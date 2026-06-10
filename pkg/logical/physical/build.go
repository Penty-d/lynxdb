// Package physical translates a logical IR plan (pkg/logical) into a chain
// of pipeline iterators (pkg/engine/pipeline). This is the LynxFlow v2
// counterpart of the old BuildProgram dispatch in pipeline.go. The old code
// path is NOT modified; this builder constructs the same iterator types from
// the typed logical nodes produced by Lower + Optimize.
//
// The Source callback in BuildOptions is the seam between this builder and the
// storage layer: callers supply a function that turns a logical.Scan into a
// pipeline.Iterator. Tests use a simple slice-backed source; the server plugs
// the real segment-scan path in the next PR.
package physical

import (
	"context"
	"fmt"
	"strings"
	"time"

	lfast "github.com/lynxbase/lynxdb/pkg/lynxflow/ast"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/engine/unpack"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/memgov"
	"github.com/lynxbase/lynxdb/pkg/vm"
)

// BuildOptions configures the physical builder.
type BuildOptions struct {
	// Source turns a logical.Scan node into an iterator. Tests supply a
	// slice-backed source; the server plugs the real storage scan path.
	Source func(scan *logical.Scan) (pipeline.Iterator, error)

	// Coordinator is the optional per-query MemoryCoordinator for spillable
	// operators. When nil, all memory accounts are nop.
	Coordinator *pipeline.MemoryCoordinator

	// BatchSize is the number of rows per batch. Zero means DefaultBatchSize.
	BatchSize int

	// Now is the reference time for resolving relative time bounds in Scan
	// nodes (e.g. -1h, -7d). Injected for testability. Zero means time.Now()
	// at build time (resolved lazily in the Source callback).
	Now time.Time
}

func (o *BuildOptions) batchSize() int {
	if o.BatchSize > 0 {
		return o.BatchSize
	}
	return pipeline.DefaultBatchSize
}

// Build translates a logical Plan into a pipeline Iterator.
//
// CTE sub-plans (Plan.Lets) are materialized eagerly into row slices and fed
// through the pipeline as RowScanIterators when referenced by Scan nodes with
// SourceCTE kind. The Source hook is only called for non-CTE scans.
func Build(plan *logical.Plan, opts BuildOptions) (pipeline.Iterator, error) {
	if plan == nil || plan.Root == nil {
		return newEmptyIterator(), nil
	}
	b := &builder{
		opts: opts,
		lets: make(map[string][]map[string]event.Value),
	}

	// Materialize CTEs eagerly (simple sequential for now; DAG parallelism
	// is a future optimization).
	for name, letPlan := range plan.Lets {
		iter, err := b.buildNode(letPlan.Root)
		if err != nil {
			return nil, fmt.Errorf("physical.Build: CTE $%s: %w", name, err)
		}
		rows, err := pipeline.CollectAll(context.Background(), iter)
		if err != nil {
			return nil, fmt.Errorf("physical.Build: CTE $%s collect: %w", name, err)
		}
		b.lets[name] = rows
	}

	return b.buildNode(plan.Root)
}

// builder holds state during the recursive plan-to-iterator translation.
type builder struct {
	opts BuildOptions
	lets map[string][]map[string]event.Value // materialized CTE rows
}

// buildNode dispatches on the concrete logical node type.
func (b *builder) buildNode(n logical.Node) (pipeline.Iterator, error) {
	switch nd := n.(type) {
	case *logical.Scan:
		return b.buildScan(nd)
	case *logical.Empty:
		return newEmptyIterator(), nil
	case *logical.Filter:
		return b.buildFilter(nd)
	case *logical.Extend:
		return b.buildExtend(nd)
	case *logical.Project:
		return b.buildProject(nd)
	case *logical.Aggregate:
		return b.buildAggregate(nd)
	case *logical.TopK:
		return b.buildTopK(nd)
	case *logical.Sort:
		return b.buildSort(nd)
	case *logical.Limit:
		return b.buildLimit(nd)
	case *logical.Dedup:
		return b.buildDedup(nd)
	case *logical.Join:
		return b.buildJoin(nd)
	case *logical.Union:
		return b.buildUnion(nd)
	case *logical.Explode:
		return b.buildExplode(nd)
	case *logical.Describe:
		return b.buildDescribe(nd)
	case *logical.Parse:
		return b.buildParse(nd)
	case *logical.Helper:
		return b.buildHelper(nd)
	case *logical.Materialize:
		return nil, &NotYetImplementedError{Feature: "Materialize (Phase 8)"}
	case *logical.Tee:
		return nil, &NotYetImplementedError{Feature: "Tee (Phase 9)"}
	default:
		return nil, fmt.Errorf("physical.Build: unknown node type %T", n)
	}
}

// NotYetImplementedError is returned for nodes whose physical mapping is
// deferred to a future PR. Callers can type-assert to distinguish "not yet
// implemented" from genuine errors.
type NotYetImplementedError struct {
	Feature string
}

func (e *NotYetImplementedError) Error() string {
	return fmt.Sprintf("physical.Build: not yet implemented: %s", e.Feature)
}

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

func (b *builder) buildScan(nd *logical.Scan) (pipeline.Iterator, error) {
	// CTE reference: return materialized rows.
	if len(nd.Sources) == 1 && nd.Sources[0].Kind == lfast.SourceCTE {
		cteName := nd.Sources[0].Name
		rows, ok := b.lets[cteName]
		if !ok {
			return nil, fmt.Errorf("physical.Build: unknown CTE $%s", cteName)
		}
		return pipeline.NewRowScanIterator(rows, b.opts.batchSize()), nil
	}

	if b.opts.Source == nil {
		return nil, fmt.Errorf("physical.Build: no Source hook provided for scan %s", nd)
	}
	return b.opts.Source(nd)
}

// ---------------------------------------------------------------------------
// Filter
// ---------------------------------------------------------------------------

func (b *builder) buildFilter(nd *logical.Filter) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	prog, err := vm.CompileLynxFlow(nd.Expr)
	if err != nil {
		return nil, fmt.Errorf("physical.Build: compile filter: %w", err)
	}
	return pipeline.NewFilterIterator(child, prog), nil
}

// ---------------------------------------------------------------------------
// Extend (eval-style)
// ---------------------------------------------------------------------------

func (b *builder) buildExtend(nd *logical.Extend) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	assigns := make([]pipeline.EvalAssignment, len(nd.Assignments))
	for i, a := range nd.Assignments {
		prog, err := vm.CompileLynxFlow(a.Value)
		if err != nil {
			return nil, fmt.Errorf("physical.Build: compile extend %s: %w", a.Name, err)
		}
		assigns[i] = pipeline.EvalAssignment{Field: a.Name, Program: prog}
	}
	return pipeline.NewEvalIterator(child, assigns), nil
}

// ---------------------------------------------------------------------------
// Project (keep/drop/rename)
// ---------------------------------------------------------------------------

func (b *builder) buildProject(nd *logical.Project) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}

	// Determine operation mode from the ProjectCols.
	hasKeep := false
	hasDrop := false
	hasRename := false
	for _, c := range nd.Cols {
		switch c.Action {
		case logical.ProjectKeep:
			hasKeep = true
		case logical.ProjectDrop:
			hasDrop = true
		case logical.ProjectRename:
			hasRename = true
		}
	}

	var iter pipeline.Iterator = child

	if hasKeep || hasDrop {
		if hasKeep && !hasDrop {
			// Keep mode: fields list in order.
			fields := make([]string, 0, len(nd.Cols))
			for _, c := range nd.Cols {
				if c.Action == logical.ProjectKeep {
					fields = append(fields, c.Name)
				}
			}
			iter = pipeline.NewProjectIterator(iter, fields, false)
		} else if hasDrop && !hasKeep {
			// Drop mode.
			fields := make([]string, 0, len(nd.Cols))
			for _, c := range nd.Cols {
				if c.Action == logical.ProjectDrop {
					fields = append(fields, c.Name)
				}
			}
			iter = pipeline.NewProjectIterator(iter, fields, true)
		} else {
			// Mixed keep+drop: process drops first, then keep.
			drops := make([]string, 0)
			keeps := make([]string, 0)
			for _, c := range nd.Cols {
				switch c.Action {
				case logical.ProjectDrop:
					drops = append(drops, c.Name)
				case logical.ProjectKeep:
					keeps = append(keeps, c.Name)
				}
			}
			if len(drops) > 0 {
				iter = pipeline.NewProjectIterator(iter, drops, true)
			}
			if len(keeps) > 0 {
				iter = pipeline.NewProjectIterator(iter, keeps, false)
			}
		}
	}

	if hasRename {
		renames := make(map[string]string)
		for _, c := range nd.Cols {
			if c.Action == logical.ProjectRename {
				renames[c.From] = c.Name
			}
		}
		iter = pipeline.NewRenameIterator(iter, renames)
	}

	return iter, nil
}

// ---------------------------------------------------------------------------
// Aggregate
// ---------------------------------------------------------------------------

// aggNameMapping maps LynxFlow v2 aggregate function names (lowercase) to the
// pipeline's internal aggregate name constants.
var aggNameMapping = map[string]string{
	"count":         "count",
	"sum":           "sum",
	"avg":           "avg",
	"min":           "min",
	"max":           "max",
	"dc":            "dc",
	"estdc":         "dc",
	"estdc_error":   "estdc_error",
	"p25":           "perc25",
	"p50":           "perc50",
	"p75":           "perc75",
	"p90":           "perc90",
	"p95":           "perc95",
	"p99":           "perc99",
	"perc25":        "perc25",
	"perc50":        "perc50",
	"perc75":        "perc75",
	"perc90":        "perc90",
	"perc95":        "perc95",
	"perc99":        "perc99",
	"stdev":         "stdev",
	"stdevp":        "stdevp",
	"var":           "var",
	"varp":          "varp",
	"mode":          "mode",
	"first":         "first",
	"last":          "last",
	"earliest":      "earliest",
	"latest":        "latest",
	"values":        "values",
	"list":          "list",
	"rate":          "rate",
	"per_second":    "per_second",
	"per_minute":    "per_minute",
	"per_hour":      "per_hour",
	"per_day":       "per_day",
	"range":         "range",
	"sumsq":         "sumsq",
	"earliest_time": "earliest_time",
	"latest_time":   "latest_time",
}

func (b *builder) buildAggregate(nd *logical.Aggregate) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}

	// Window variant dispatch.
	if nd.Window != nil {
		return b.buildWindowAggregate(child, nd)
	}

	// TimeBin: prepend a BinIterator for bin(_time, d).
	if nd.TimeBin != nil {
		dur, err := exprToDuration(nd.TimeBin.Duration)
		if err != nil {
			return nil, fmt.Errorf("physical.Build: aggregate time bin: %w", err)
		}
		child = pipeline.NewBinIterator(child, "_time", "_time", dur)
	}

	aggs, err := b.convertAggs(nd.Aggs)
	if err != nil {
		return nil, err
	}

	groupBy := make([]string, 0, len(nd.Keys))
	if nd.TimeBin != nil {
		groupBy = append(groupBy, "_time")
	}
	for _, k := range nd.Keys {
		groupBy = append(groupBy, k.Name)
	}

	return pipeline.NewAggregateIterator(child, aggs, groupBy, memgov.NopAccount()), nil
}

func (b *builder) buildWindowAggregate(child pipeline.Iterator, nd *logical.Aggregate) (pipeline.Iterator, error) {
	aggs, err := b.convertAggs(nd.Aggs)
	if err != nil {
		return nil, err
	}

	groupBy := make([]string, 0, len(nd.Keys))
	for _, k := range nd.Keys {
		groupBy = append(groupBy, k.Name)
	}

	switch nd.Window.Variant {
	case logical.WindowEventstats:
		return pipeline.NewEventStatsIterator(child, aggs, groupBy, b.opts.batchSize()), nil
	case logical.WindowStreamstats:
		window := 0
		if nd.Window.Window != nil {
			window = *nd.Window.Window
		}
		current := true
		if nd.Window.Current != nil {
			current = *nd.Window.Current
		}
		return pipeline.NewStreamStatsIterator(child, aggs, groupBy, window, current), nil
	default:
		return nil, fmt.Errorf("physical.Build: unknown window variant %d", nd.Window.Variant)
	}
}

func (b *builder) convertAggs(aggs []logical.Agg) ([]pipeline.AggFunc, error) {
	result := make([]pipeline.AggFunc, len(aggs))
	for i, a := range aggs {
		call, ok := a.Func.(*lfast.Call)
		if !ok {
			return nil, fmt.Errorf("physical.Build: agg %d: expected *ast.Call, got %T", i, a.Func)
		}
		name := strings.ToLower(call.Callee)
		mapped, ok := aggNameMapping[name]
		if !ok {
			return nil, fmt.Errorf("physical.Build: unsupported aggregate function %q", name)
		}

		alias := a.Alias
		if alias == "" {
			alias = aggAutoAlias(call)
		}

		field := ""
		var prog *vm.Program

		if len(call.Args) > 0 {
			// If the argument is a simple identifier, use it as a field name.
			// Otherwise compile it as an expression.
			if ident, ok := call.Args[0].(*lfast.Ident); ok {
				field = ident.Name
			} else {
				compiled, err := vm.CompileLynxFlow(call.Args[0])
				if err != nil {
					return nil, fmt.Errorf("physical.Build: compile agg %s arg: %w", name, err)
				}
				prog = compiled
			}
		}

		// Conditional aggregation: count(x, where=predicate)
		var condProg *vm.Program
		if a.WhereCond != nil {
			compiled, err := vm.CompileLynxFlow(a.WhereCond)
			if err != nil {
				return nil, fmt.Errorf("physical.Build: compile agg %s where cond: %w", name, err)
			}
			condProg = compiled
		}

		result[i] = pipeline.AggFunc{
			Name:        mapped,
			Field:       field,
			Alias:       alias,
			Program:     prog,
			CondProgram: condProg,
		}
	}
	return result, nil
}

// aggAutoAlias generates a default alias like "count()" or "sum(x)".
func aggAutoAlias(call *lfast.Call) string {
	if len(call.Args) > 0 {
		if ident, ok := call.Args[0].(*lfast.Ident); ok {
			return call.Callee + "(" + ident.Name + ")"
		}
		return call.Callee + "(" + call.Args[0].String() + ")"
	}
	return call.Callee + "()"
}

// ---------------------------------------------------------------------------
// TopK
// ---------------------------------------------------------------------------

func (b *builder) buildTopK(nd *logical.TopK) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	fields := make([]pipeline.SortField, len(nd.SortKeys))
	for i, k := range nd.SortKeys {
		fields[i] = pipeline.SortField{
			Name: exprFieldName(k.Expr),
			Desc: k.Desc,
		}
	}
	// TopK = sort + head. Use TopN iterator (heap-based) when available.
	return pipeline.NewTopNIterator(child, fields, int(nd.K), b.opts.batchSize()), nil
}

// ---------------------------------------------------------------------------
// Sort
// ---------------------------------------------------------------------------

func (b *builder) buildSort(nd *logical.Sort) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	fields := make([]pipeline.SortField, len(nd.Keys))
	for i, k := range nd.Keys {
		fields[i] = pipeline.SortField{
			Name: exprFieldName(k.Expr),
			Desc: k.Desc,
		}
	}
	return pipeline.NewSortIterator(child, fields, b.opts.batchSize()), nil
}

// ---------------------------------------------------------------------------
// Limit (head/tail)
// ---------------------------------------------------------------------------

func (b *builder) buildLimit(nd *logical.Limit) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	if nd.Tail {
		return pipeline.NewTailIterator(child, int(nd.N), b.opts.batchSize()), nil
	}
	return pipeline.NewLimitIterator(child, int(nd.N)), nil
}

// ---------------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------------

func (b *builder) buildDedup(nd *logical.Dedup) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	return pipeline.NewDedupIterator(child, nd.Fields, int(nd.N)), nil
}

// ---------------------------------------------------------------------------
// Join
// ---------------------------------------------------------------------------

func (b *builder) buildJoin(nd *logical.Join) (pipeline.Iterator, error) {
	left, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}

	joinType := nd.Type
	if joinType == "outer" {
		return nil, &NotYetImplementedError{Feature: "outer join"}
	}

	if nd.Right == nil {
		return left, nil
	}

	right, err := b.buildNode(nd.Right)
	if err != nil {
		return nil, fmt.Errorf("physical.Build: join right: %w", err)
	}

	// The old JoinIterator takes a single field name. The logical Join has
	// []string On. Use the first key; multi-key join uses comma-joined key.
	field := ""
	if len(nd.On) > 0 {
		field = nd.On[0]
	}

	return pipeline.NewJoinIterator(left, right, field, joinType), nil
}

// ---------------------------------------------------------------------------
// Union
// ---------------------------------------------------------------------------

func (b *builder) buildUnion(nd *logical.Union) (pipeline.Iterator, error) {
	iters := make([]pipeline.Iterator, len(nd.Inputs))
	for i, input := range nd.Inputs {
		iter, err := b.buildNode(input)
		if err != nil {
			return nil, fmt.Errorf("physical.Build: union branch %d: %w", i, err)
		}
		iters[i] = iter
	}
	return pipeline.NewUnionIterator(iters), nil
}

// ---------------------------------------------------------------------------
// Explode
// ---------------------------------------------------------------------------

func (b *builder) buildExplode(nd *logical.Explode) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	fields := []string{nd.Field}
	return pipeline.NewUnrollIterator(child, fields, b.opts.batchSize()), nil
}

// ---------------------------------------------------------------------------
// Describe
// ---------------------------------------------------------------------------

func (b *builder) buildDescribe(nd *logical.Describe) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}
	return NewDescribeSummaryIterator(child, b.opts.batchSize()), nil
}

// ---------------------------------------------------------------------------
// Parse
// ---------------------------------------------------------------------------

func (b *builder) buildParse(nd *logical.Parse) (pipeline.Iterator, error) {
	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}

	// Not-yet-implemented options.
	if nd.OnError != "" && nd.OnError != "propagate" {
		return nil, &NotYetImplementedError{Feature: fmt.Sprintf("parse on_error=%q", nd.OnError)}
	}
	if len(nd.FirstOf) > 0 {
		return nil, &NotYetImplementedError{Feature: "parse first_of"}
	}

	format := nd.Format
	if format == "" {
		format = "json" // default
	}

	parser, err := unpack.NewParser(format)
	if err != nil {
		return nil, fmt.Errorf("physical.Build: parse format %q: %w", format, err)
	}

	from := nd.From
	if from == "" {
		from = "_raw"
	}

	var fields []string
	for _, c := range nd.Captures {
		fields = append(fields, c.Name)
	}

	return pipeline.NewUnpackIterator(child, parser, from, fields, nd.Prefix, false), nil
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

// helperNotYetImplemented lists helper names that need query-replay context
// or are otherwise not yet wirable.
var helperNotYetImplemented = map[string]string{
	"compare": "compare requires query-replay context",
}

func (b *builder) buildHelper(nd *logical.Helper) (pipeline.Iterator, error) {
	if reason, ok := helperNotYetImplemented[nd.Name]; ok {
		return nil, &NotYetImplementedError{Feature: fmt.Sprintf("helper %q: %s", nd.Name, reason)}
	}

	child, err := b.buildChild(nd)
	if err != nil {
		return nil, err
	}

	// Dispatch to known helper implementations.
	switch nd.Name {
	case "xyseries":
		return b.buildHelperXYSeries(child, nd)
	case "transaction":
		return b.buildHelperTransaction(child, nd)
	case "patterns":
		return b.buildHelperPatterns(child, nd)
	case "outliers":
		return b.buildHelperOutliers(child, nd)
	case "trace":
		return b.buildHelperTrace(child, nd)
	case "topology":
		return b.buildHelperTopology(child, nd)
	case "correlate":
		return b.buildHelperCorrelate(child, nd)
	case "rollup":
		return b.buildHelperRollup(child, nd)
	case "sessionize":
		return b.buildHelperSessionize(child, nd)
	default:
		return nil, &NotYetImplementedError{Feature: fmt.Sprintf("helper %q", nd.Name)}
	}
}

func (b *builder) buildHelperXYSeries(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	if len(nd.Positional) < 3 {
		return nil, fmt.Errorf("physical.Build: xyseries requires 3 positional args, got %d", len(nd.Positional))
	}
	x := exprFieldName(nd.Positional[0])
	y := exprFieldName(nd.Positional[1])
	v := exprFieldName(nd.Positional[2])
	return pipeline.NewXYSeriesIterator(child, x, y, v, b.opts.batchSize()), nil
}

func (b *builder) buildHelperTransaction(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	var field string
	if len(nd.Positional) > 0 {
		field = exprFieldName(nd.Positional[0])
	}
	var dur time.Duration
	if ms, ok := nd.Options["maxspan"]; ok {
		d, err := exprToDuration(ms)
		if err == nil {
			dur = d
		}
	}
	var startsWith, endsWith string
	if sw, ok := nd.Options["startswith"]; ok {
		startsWith = sw.String()
	}
	if ew, ok := nd.Options["endswith"]; ok {
		endsWith = ew.String()
	}

	// Transaction requires sorted input.
	sorted := pipeline.NewSortIterator(child, []pipeline.SortField{{Name: "_time"}}, b.opts.batchSize())
	return pipeline.NewTransactionIterator(sorted, field, dur, startsWith, endsWith, b.opts.batchSize()), nil
}

func (b *builder) buildHelperPatterns(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	field := "_raw"
	if len(nd.Positional) > 0 {
		field = exprFieldName(nd.Positional[0])
	}
	if f, ok := nd.Options["field"]; ok {
		field = exprFieldName(f)
	}
	var maxTemplates int
	if mt, ok := nd.Options["max_templates"]; ok {
		if lit, ok := mt.(*lfast.Literal); ok {
			if n, ok := lit.Value.(int64); ok {
				maxTemplates = int(n)
			}
		}
	}
	var similarity float64
	if sim, ok := nd.Options["similarity"]; ok {
		if lit, ok := sim.(*lfast.Literal); ok {
			if f, ok := lit.Value.(float64); ok {
				similarity = f
			}
		}
	}
	return pipeline.NewPatternsIterator(child, field, maxTemplates, similarity), nil
}

func (b *builder) buildHelperOutliers(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	field := ""
	if len(nd.Positional) > 0 {
		field = exprFieldName(nd.Positional[0])
	}
	if f, ok := nd.Options["field"]; ok {
		field = exprFieldName(f)
	}
	var method string
	if m, ok := nd.Options["method"]; ok {
		method = exprFieldName(m)
	}
	var threshold float64
	if th, ok := nd.Options["threshold"]; ok {
		if lit, ok := th.(*lfast.Literal); ok {
			if f, ok := lit.Value.(float64); ok {
				threshold = f
			}
		}
	}
	return pipeline.NewOutliersIterator(child, field, method, threshold), nil
}

func (b *builder) buildHelperTrace(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	traceField, spanField, parentField := "trace_id", "span_id", "parent_span_id"
	if f, ok := nd.Options["trace_id"]; ok {
		traceField = exprFieldName(f)
	}
	if f, ok := nd.Options["span_id"]; ok {
		spanField = exprFieldName(f)
	}
	if f, ok := nd.Options["parent_id"]; ok {
		parentField = exprFieldName(f)
	}
	return pipeline.NewTraceIterator(child, traceField, spanField, parentField), nil
}

func (b *builder) buildHelperTopology(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	src, dst, weight := "source", "dest", ""
	maxNodes := 0
	if f, ok := nd.Options["source"]; ok {
		src = exprFieldName(f)
	}
	if f, ok := nd.Options["dest"]; ok {
		dst = exprFieldName(f)
	}
	if f, ok := nd.Options["weight"]; ok {
		weight = exprFieldName(f)
	}
	if n, ok := nd.Options["max_nodes"]; ok {
		if lit, ok := n.(*lfast.Literal); ok {
			if v, ok := lit.Value.(int64); ok {
				maxNodes = int(v)
			}
		}
	}
	return pipeline.NewTopologyIterator(child, src, dst, weight, maxNodes, b.opts.batchSize()), nil
}

func (b *builder) buildHelperCorrelate(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	if len(nd.Positional) < 2 {
		return nil, fmt.Errorf("physical.Build: correlate requires 2 positional args")
	}
	f1 := exprFieldName(nd.Positional[0])
	f2 := exprFieldName(nd.Positional[1])
	method := ""
	if m, ok := nd.Options["method"]; ok {
		method = exprFieldName(m)
	}
	return pipeline.NewCorrelateIterator(child, f1, f2, method), nil
}

func (b *builder) buildHelperRollup(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	var spans []string
	for _, p := range nd.Positional {
		spans = append(spans, p.String())
	}
	var groupBy []string
	// rollup doesn't have groupBy in the generic options; pass empty.
	return pipeline.NewRollupIterator(child, spans, groupBy, b.opts.batchSize()), nil
}

func (b *builder) buildHelperSessionize(child pipeline.Iterator, nd *logical.Helper) (pipeline.Iterator, error) {
	var maxPause string
	if mp, ok := nd.Options["max_pause"]; ok {
		maxPause = mp.String()
	}
	var groupBy []string
	if gb, ok := nd.Options["by"]; ok {
		groupBy = append(groupBy, exprFieldName(gb))
	}
	return pipeline.NewSessionizeIterator(child, maxPause, groupBy, b.opts.batchSize()), nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildChild builds the single input child of a unary node.
func (b *builder) buildChild(n logical.Node) (pipeline.Iterator, error) {
	children := n.Children()
	if len(children) == 0 {
		return nil, fmt.Errorf("physical.Build: node %T has no children", n)
	}
	return b.buildNode(children[0])
}

// exprFieldName extracts a field name from a LynxFlow AST expression.
func exprFieldName(e lfast.Expr) string {
	switch x := e.(type) {
	case *lfast.Ident:
		return x.Name
	case *lfast.Member:
		return exprFieldName(x.Object) + "." + x.Field
	case *lfast.Call:
		if len(x.Args) > 0 {
			return x.Callee + "(" + exprFieldName(x.Args[0]) + ")"
		}
		return x.Callee + "()"
	}
	return e.String()
}

// exprToDuration extracts a time.Duration from a LynxFlow expression node.
func exprToDuration(e lfast.Expr) (time.Duration, error) {
	if lit, ok := e.(*lfast.Literal); ok && lit.Kind == lfast.LitDuration {
		if d, ok := lit.Value.(time.Duration); ok {
			return d, nil
		}
	}
	// Fall back to parsing the string representation.
	s := e.String()
	// Try Go duration format.
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// SPL2-style: "1d", "7d"
	if len(s) > 1 && s[len(s)-1] == 'd' {
		n := 0
		for _, c := range s[:len(s)-1] {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		if n > 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return 0, fmt.Errorf("cannot convert %q to duration", s)
}

// ---------------------------------------------------------------------------
// emptyIterator
// ---------------------------------------------------------------------------

type emptyIterator struct{}

func newEmptyIterator() pipeline.Iterator { return &emptyIterator{} }

func (e *emptyIterator) Init(_ context.Context) error                    { return nil }
func (e *emptyIterator) Next(_ context.Context) (*pipeline.Batch, error) { return nil, nil }
func (e *emptyIterator) Close() error                                    { return nil }
func (e *emptyIterator) Schema() []pipeline.FieldInfo                    { return nil }

// Ensure imports are used by type references in emptyIterator.
var _ context.Context
