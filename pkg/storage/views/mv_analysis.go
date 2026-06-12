package views

import (
	"fmt"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/logical/opt"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

// MVAnalysis is the analysis result for a materialized view's LynxFlow query.
// Produced by AnalyzeLynxFlow, consumed by the dispatcher, backfill, and merge
// paths.
//
// Storage format compatibility: the dispatcher serializes partial aggregation
// state via PartialGroupsToEvents using _pa_ prefixed columns keyed by
// AggSpec.Funcs[].Alias. As long as the AggSpec is identical, an SPL2-created
// view and a migrated LynxFlow view produce byte-identical partial state
// columns. This means:
//   - A view migrated from SPL2 to LynxFlow keeps serving its existing
//     materialized parts with no data rewrite.
//   - New insert-time data written by the LynxFlow dispatch path merges
//     correctly with old SPL2-written partial state during ViewAllEvents.
type MVAnalysis struct {
	// SourceIndex is the FROM clause index name (e.g., "main", "nginx").
	SourceIndex string

	// IsAggregation is true if the query contains a terminal aggregation.
	IsAggregation bool

	// AggSpec describes the partial aggregation to compute at insert time.
	// Nil for projection views.
	AggSpec *pipeline.PartialAggSpec

	// GroupBy holds the group-by field names from the aggregation command.
	GroupBy []string

	// Plan is the optimized logical plan for the view query. Non-nil only
	// for lynxflow views. Used by the dispatcher to build physical pipelines
	// for insert-time dispatch and backfill execution.
	Plan *logical.Plan

	// StreamingPlan is the sub-plan containing only the pre-aggregation
	// operators (Scan -> Filter -> Extend -> Parse). Non-nil only for
	// lynxflow aggregation views. The dispatcher builds a physical pipeline
	// from this plan (with a slice source over the incoming batch) to
	// transform events before partial aggregation.
	StreamingPlan *logical.Plan

	// TimeBin, when non-nil, indicates the Aggregate has a bin(_time, d)
	// grouping. The dispatcher adds a BinIterator after the streaming plan
	// and before partial aggregation.
	TimeBin *logical.TimeBin
}

// AnalyzeLynxFlow parses a LynxFlow query and extracts the MVAnalysis.
//
// The plan shape must be: Scan -> [Filter] -> [Parse] -> [Extend] -> Aggregate.
// Any operators after Aggregate (Sort, Limit, Project) are query-time only
// and are ignored for insert-time dispatch. Unsupported shapes (Join, Union,
// Dedup before Aggregate, window aggregates) are rejected with a clear error.
//
// The returned MVAnalysis.AggSpec uses the same PartialAggFunc format as the
// the original SPL2 implementation, so storage format is identical. See
// MVAnalysis doc comment for the storage compatibility argument.
func AnalyzeLynxFlow(query string) (*MVAnalysis, error) {
	// 1. Parse
	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("views.AnalyzeLynxFlow: parse: %s", d.Message)
		}
	}

	// 2. Desugar
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})

	// 3. Lower
	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: "main"})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("views.AnalyzeLynxFlow: lower: %s", d.Message)
		}
	}

	// 4. Optimize
	plan, _ = opt.Optimize(plan)

	if plan == nil || plan.Root == nil {
		return nil, fmt.Errorf("views.AnalyzeLynxFlow: empty plan")
	}

	// 5. Walk the plan to validate shape and extract MV metadata.
	return analyzePlan(plan)
}

// analyzePlan walks the optimized logical plan and extracts MV metadata.
func analyzePlan(plan *logical.Plan) (*MVAnalysis, error) {
	// Flatten the plan into a linear chain for analysis.
	chain := flattenChain(plan.Root)

	result := &MVAnalysis{
		Plan: plan,
	}

	// Extract source index from the Scan node (must be first).
	if len(chain) == 0 {
		return nil, fmt.Errorf("views.AnalyzeLynxFlow: empty plan chain")
	}
	scan, ok := chain[0].(*logical.Scan)
	if !ok {
		return nil, fmt.Errorf("views.AnalyzeLynxFlow: plan must start with Scan, got %T", chain[0])
	}
	if len(scan.Sources) == 1 && scan.Sources[0].Kind == ast.SourceName {
		result.SourceIndex = scan.Sources[0].Name
	}

	// Find the Aggregate node and validate the plan shape.
	aggIdx := -1
	for i, node := range chain {
		if err := validateNodeForMV(node, i == 0); err != nil {
			return nil, err
		}
		if _, isAgg := node.(*logical.Aggregate); isAgg {
			aggIdx = i
			break
		}
	}

	if aggIdx == -1 {
		// Projection view: no aggregation.
		result.IsAggregation = false
		return result, nil
	}

	aggNode := chain[aggIdx].(*logical.Aggregate)

	// Validate the aggregate node.
	if aggNode.Window != nil {
		return nil, fmt.Errorf("views.AnalyzeLynxFlow: eventstats/streamstats not supported for MV")
	}

	// Build AggSpec from the Aggregate node.
	spec, groupBy, err := buildAggSpecFromIR(aggNode)
	if err != nil {
		return nil, err
	}

	result.IsAggregation = true
	result.AggSpec = spec
	result.GroupBy = groupBy

	// Build the streaming sub-plan: everything before the Aggregate, plus a
	// TimeBin if present. The streaming plan transforms events before partial
	// aggregation. If the aggregate has a TimeBin (e.g., bin(_time, 1h)), we
	// append an Aggregate node with ONLY the TimeBin (no aggs, just the bin)
	// — but actually the physical builder handles TimeBin by prepending a
	// BinIterator. So we build a minimal Aggregate with TimeBin as a separate
	// step.
	//
	// Simpler approach: mark the MVAnalysis with the TimeBin info so the
	// dispatcher can add a BinIterator to the pipeline.
	if aggIdx > 0 || aggNode.TimeBin != nil {
		streamChain := make([]logical.Node, len(chain[:aggIdx]))
		copy(streamChain, chain[:aggIdx])
		var streamRoot logical.Node
		if len(streamChain) > 0 {
			streamRoot = rebuildChain(streamChain)
		} else {
			streamRoot = chain[0] // Scan
		}
		result.StreamingPlan = &logical.Plan{Root: streamRoot}
	}

	// Store the TimeBin info separately for the dispatcher to add a
	// BinIterator before partial aggregation.
	result.TimeBin = aggNode.TimeBin

	return result, nil
}

// flattenChain linearizes a logical plan tree into a slice, walking the single
// child of unary nodes. Stops at branching nodes (Join, Union) or leaves (Scan).
func flattenChain(n logical.Node) []logical.Node {
	var chain []logical.Node
	for n != nil {
		chain = append(chain, n)
		children := n.Children()
		if len(children) != 1 {
			// Reverse so Scan is first.
			break
		}
		n = children[0]
	}
	// Reverse: we walked top-down, but want Scan first.
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain
}

// rebuildChain creates a new linear plan from a chain of nodes (Scan first).
// Each node's child is set to the previous node in the chain.
func rebuildChain(chain []logical.Node) logical.Node {
	if len(chain) == 0 {
		return nil
	}
	if len(chain) == 1 {
		return chain[0]
	}
	// chain[0] is Scan (leaf). chain[1..n] are unary nodes.
	// Set each node's child to the previous.
	for i := 1; i < len(chain); i++ {
		chain[i].SetChildren([]logical.Node{chain[i-1]})
	}
	return chain[len(chain)-1]
}

// validateNodeForMV checks if a node is allowed in an MV plan before the
// aggregate split point.
func validateNodeForMV(node logical.Node, isScan bool) error {
	switch node.(type) {
	case *logical.Scan:
		if !isScan {
			return fmt.Errorf("views.AnalyzeLynxFlow: unexpected Scan in middle of plan")
		}
		return nil
	case *logical.Filter, *logical.Extend, *logical.Parse, *logical.Project:
		return nil
	case *logical.Aggregate:
		return nil // handled by caller
	case *logical.Join:
		return fmt.Errorf("views.AnalyzeLynxFlow: join not supported for MV")
	case *logical.Union:
		return fmt.Errorf("views.AnalyzeLynxFlow: union not supported for MV")
	case *logical.Dedup:
		return fmt.Errorf("views.AnalyzeLynxFlow: dedup before aggregation not supported for MV")
	case *logical.Sort, *logical.Limit, *logical.TopK:
		// These are post-agg, but if they appear before the agg, reject.
		return fmt.Errorf("views.AnalyzeLynxFlow: sort/limit/topk before aggregation not supported for MV")
	case *logical.Helper:
		h := node.(*logical.Helper)
		switch h.Name {
		case "transaction":
			return fmt.Errorf("views.AnalyzeLynxFlow: transaction not supported for MV")
		}
		return nil
	default:
		return fmt.Errorf("views.AnalyzeLynxFlow: unsupported node type %T for MV", node)
	}
}

// buildAggSpecFromIR extracts a PartialAggSpec from a logical.Aggregate node.
// The returned spec uses the same PartialAggFunc format as the original
// SPL2 implementation, ensuring storage compatibility for migrated views.
func buildAggSpecFromIR(agg *logical.Aggregate) (*pipeline.PartialAggSpec, []string, error) {
	funcs := make([]pipeline.PartialAggFunc, 0, len(agg.Aggs))

	for _, a := range agg.Aggs {
		call, ok := a.Func.(*ast.Call)
		if !ok {
			return nil, nil, fmt.Errorf("views.AnalyzeLynxFlow: agg func must be *ast.Call, got %T", a.Func)
		}

		name := strings.ToLower(call.Callee)
		if err := validateAggFuncForMV(name); err != nil {
			return nil, nil, err
		}

		alias := a.Alias
		if alias == "" {
			if len(call.Args) > 0 {
				if ident, ok := call.Args[0].(*ast.Ident); ok {
					alias = call.Callee + "(" + ident.Name + ")"
				} else {
					alias = call.Callee + "(" + call.Args[0].String() + ")"
				}
			} else {
				alias = call.Callee + "()"
			}
		}

		field := ""
		if len(call.Args) > 0 {
			if ident, ok := call.Args[0].(*ast.Ident); ok {
				field = ident.Name
			} else {
				return nil, nil, fmt.Errorf("views.AnalyzeLynxFlow: materialized views do not support expression arguments in %s(); "+
					"pre-compute with extend: | extend err = if(status >= 500, 1, 0) | stats sum(err) as errors", name)
			}
		}

		funcs = append(funcs, pipeline.PartialAggFunc{
			Name:  name,
			Field: field,
			Alias: alias,
		})
	}

	// Auto-inject hidden count for avg (required for weighted merge).
	hasAvg, hasCount := false, false
	for _, fn := range funcs {
		if fn.Name == "avg" {
			hasAvg = true
		}
		if fn.Name == "count" {
			hasCount = true
		}
	}
	if hasAvg && !hasCount {
		funcs = append(funcs, pipeline.PartialAggFunc{
			Name:   "count",
			Field:  "",
			Alias:  MVAutoCountAlias,
			Hidden: true,
		})
	}

	// Build group-by keys.
	groupBy := make([]string, 0, len(agg.Keys)+1)
	if agg.TimeBin != nil {
		groupBy = append(groupBy, "_time")
	}
	for _, k := range agg.Keys {
		groupBy = append(groupBy, k.Name)
	}

	spec := &pipeline.PartialAggSpec{
		GroupBy: groupBy,
		Funcs:   funcs,
	}

	return spec, groupBy, nil
}

// validateAggFuncForMV checks if an aggregation function name is supported
// for materialized view partial state.
func validateAggFuncForMV(name string) error {
	switch name {
	case "count", "sum", "avg", "min", "max", "dc":
		return nil
	case "values", "list":
		return fmt.Errorf("views.AnalyzeLynxFlow: values()/list() not supported for MV (unbounded memory)")
	case "earliest", "latest":
		return fmt.Errorf("views.AnalyzeLynxFlow: earliest()/latest() not supported for MV")
	case "stdev", "stdevp", "var", "varp":
		return fmt.Errorf("views.AnalyzeLynxFlow: stdev/var not supported for MV")
	case "perc25", "perc50", "perc75", "perc90", "perc95", "perc99",
		"p25", "p50", "p75", "p90", "p95", "p99":
		return fmt.Errorf("views.AnalyzeLynxFlow: percentile functions not supported for MV")
	default:
		return fmt.Errorf("views.AnalyzeLynxFlow: unsupported aggregation function %q for MV", name)
	}
}
