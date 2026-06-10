package opt

import (
	"sort"

	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/sema"
)

// ---------------------------------------------------------------------------
// Batch 3 plan rules: aggregation, TopK, tail-scan, limit-pushdown,
// column-pruning.
//
// Rule ordering (appended after existing rules):
//   - partial-agg:       sets Aggregate.Partial when all funcs are decomposable
//   - topk-into-agg:     annotates Aggregate with TopKHint when TopK sits above
//   - tail-scan:         converts tail through streaming nodes into reverse scan
//   - limit-pushdown:    swaps Limit below row-count-preserving per-row nodes
//   - column-pruning:    top-down required-column analysis -> Scan.Pushdown.Columns
// ---------------------------------------------------------------------------

// batch3PlanRules returns the five batch-3 plan rules in their defined order.
func batch3PlanRules() []PlanRule {
	return []PlanRule{
		{Name: "partial-agg", Apply: partialAgg},
		{Name: "topk-into-agg", Apply: topkIntoAgg},
		{Name: "tail-scan", Apply: tailScan},
		{Name: "limit-pushdown", Apply: limitPushdown},
		{Name: "column-pruning", Apply: columnPruning},
	}
}

// ---------------------------------------------------------------------------
// Rule: partial-agg
// ---------------------------------------------------------------------------
//
// Sets Aggregate.Partial = true when ALL aggregation functions are decomposable
// into a partial (per-segment) + merge (global) pair. Window variants
// (eventstats/streamstats) are NEVER partial. Conditional where-args do not
// affect decomposability.
//
// Decomposable functions:
//
//	count, sum, min, max, avg (sum+count), dc/estdc (HLL),
//	p50..p99/perc* (t-digest), stdev/var (M2), first/last/earliest/latest
//	(row pick), values (distinct set merge), rate/per_second (sum merge),
//	mode (count-map merge).
//
// Non-decomposable: list (order-dependent).
func partialAgg(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		agg, ok := n.(*logical.Aggregate)
		if !ok || agg.Partial {
			return n, false
		}
		// Window variants are never partial.
		if agg.Window != nil {
			return n, false
		}
		// Check every agg function.
		for _, a := range agg.Aggs {
			if !isDecomposableAgg(a.Func) {
				return n, false
			}
		}
		// All decomposable: set Partial.
		newAgg := *agg
		newAgg.Partial = true
		return &newAgg, true
	})
}

// isDecomposableAgg returns true if the aggregate function expression is
// decomposable into partial + merge phases.
func isDecomposableAgg(expr ast.Expr) bool {
	call, ok := expr.(*ast.Call)
	if !ok {
		return false
	}
	switch call.Callee {
	case "count", "sum", "min", "max", "avg",
		"dc", "estdc",
		"p50", "p75", "p90", "p95", "p99",
		"perc50", "perc75", "perc90", "perc95", "perc99",
		"stdev", "stdevp", "var", "varp",
		"first", "last", "earliest", "latest",
		"values",
		"rate", "per_second",
		"mode":
		return true
	case "list":
		// list is order-dependent and NOT decomposable.
		return false
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Rule: topk-into-agg
// ---------------------------------------------------------------------------
//
// When TopK sits directly above an Aggregate (no Window), and every TopK sort
// key references an output column of the Aggregate, set Aggregate.TopK hint.
// The TopK node is KEPT (final ordering); the hint lets the physical aggregate
// use a bounded heap.
func topkIntoAgg(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		topk, ok := n.(*logical.TopK)
		if !ok {
			return n, false
		}
		agg, ok := topk.Input.(*logical.Aggregate)
		if !ok {
			return n, false
		}
		// Window variants do not support TopK hint.
		if agg.Window != nil {
			return n, false
		}
		// Already hinted?
		if agg.TopK != nil {
			return n, false
		}
		// Check every sort key references an output column of the aggregate.
		aggOutputCols := aggOutputColumnNames(agg)
		for _, sk := range topk.SortKeys {
			name := exprSingleIdent(sk.Expr)
			if name == "" {
				return n, false
			}
			if !stringInSlice(name, aggOutputCols) {
				return n, false
			}
		}
		// Set the hint. Clone agg and rebuild topk.
		newAgg := *agg
		newAgg.TopK = &logical.TopKHint{
			K:        topk.K,
			SortKeys: topk.SortKeys,
		}
		newTopK := *topk
		newTopK.SetChildren([]logical.Node{&newAgg})
		return &newTopK, true
	})
}

// aggOutputColumnNames collects output column names from an Aggregate node.
func aggOutputColumnNames(agg *logical.Aggregate) []string {
	var names []string
	if agg.TimeBin != nil {
		names = append(names, "_time")
	}
	for _, k := range agg.Keys {
		names = append(names, k.Name)
	}
	for _, a := range agg.Aggs {
		name := a.Alias
		if name == "" {
			name = aggAutoName(a)
		}
		names = append(names, name)
	}
	return names
}

// aggAutoName matches the logic in node.go.
func aggAutoName(a logical.Agg) string {
	if call, ok := a.Func.(*ast.Call); ok {
		if len(call.Args) > 0 {
			return call.Callee + "(" + call.Args[0].String() + ")"
		}
		return call.Callee + "()"
	}
	return "?"
}

// exprSingleIdent returns the field name if expr is a plain Ident, else "".
func exprSingleIdent(e ast.Expr) string {
	id, ok := e.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

func stringInSlice(s string, ss []string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Rule: tail-scan
// ---------------------------------------------------------------------------
//
// Converts Limit{Tail:true, N} into Limit{Tail:false, N} (head) on a
// reversed Scan, when the entire path from the Limit down to the Scan
// consists ONLY of row-order-preserving row-streaming nodes:
// Filter, Project, Extend, Parse.
//
// Any Sort, Aggregate, Join, Union, Dedup, TopK, Helper, Explode,
// Materialize, Tee, Describe, or Empty on the path blocks the rule.
func tailScan(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		lim, ok := n.(*logical.Limit)
		if !ok || !lim.Tail {
			return n, false
		}
		// Walk down from lim.Input to find the Scan.
		if !pathToScanIsStreamingPreserving(lim.Input) {
			return n, false
		}
		scan := findScan(lim.Input)
		if scan == nil || scan.Reverse {
			// Already reversed, or no Scan found.
			return n, false
		}
		// Rebuild: clone the chain, set Scan.Reverse = true, flip Limit.Tail.
		newChain := cloneChainSetReverse(lim.Input)
		newLim := *lim
		newLim.Tail = false
		newLim.SetChildren([]logical.Node{newChain})
		return &newLim, true
	})
}

// pathToScanIsStreamingPreserving checks that every node from n down to
// (and including) the Scan is a row-order-preserving streaming node.
func pathToScanIsStreamingPreserving(n logical.Node) bool {
	for {
		switch n.(type) {
		case *logical.Scan:
			return true
		case *logical.Filter, *logical.Project, *logical.Extend, *logical.Parse:
			children := n.Children()
			if len(children) != 1 {
				return false
			}
			n = children[0]
		default:
			return false
		}
	}
}

// findScan walks a linear chain and returns the terminal Scan, or nil.
func findScan(n logical.Node) *logical.Scan {
	for {
		if s, ok := n.(*logical.Scan); ok {
			return s
		}
		children := n.Children()
		if len(children) != 1 {
			return nil
		}
		n = children[0]
	}
}

// cloneChainSetReverse clones a linear chain of nodes, setting Reverse=true
// on the terminal Scan. Assumes the chain is valid (streaming-preserving).
func cloneChainSetReverse(n logical.Node) logical.Node {
	if s, ok := n.(*logical.Scan); ok {
		ns := cloneScan(s)
		ns.Reverse = true
		return ns
	}
	newNode := cloneNode(n)
	children := n.Children()
	newChild := cloneChainSetReverse(children[0])
	newNode.SetChildren([]logical.Node{newChild})
	return newNode
}

// ---------------------------------------------------------------------------
// Rule: limit-pushdown
// ---------------------------------------------------------------------------
//
// Swaps Limit{Tail:false} below row-count-preserving per-row nodes:
// Extend and Project.
//
// NOT below Filter (changes row count) or Parse with on_error=drop
// (changes row count). Parse with other on_error modes preserves count.
//
// Repeats to fixed point via the driver.
func limitPushdown(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		lim, ok := n.(*logical.Limit)
		if !ok || lim.Tail {
			return n, false
		}
		child := lim.Input
		if child == nil {
			return n, false
		}
		switch c := child.(type) {
		case *logical.Extend:
			return swapLimitBelowUnary(lim, c), true
		case *logical.Project:
			return swapLimitBelowUnary(lim, c), true
		case *logical.Parse:
			// Parse with on_error=drop changes row count.
			if c.OnError == "drop" {
				return n, false
			}
			return swapLimitBelowUnary(lim, c), true
		default:
			return n, false
		}
	})
}

// swapLimitBelowUnary moves the limit below the child node.
// Before: Limit -> Child -> X
// After:  Child -> Limit -> X
func swapLimitBelowUnary(lim *logical.Limit, child logical.Node) logical.Node {
	grandChildren := child.Children()
	if len(grandChildren) != 1 {
		// Safety: should not happen for unary nodes, but be defensive.
		return lim
	}
	newLim := *lim
	newLim.SetChildren(grandChildren)
	newChild := cloneNode(child)
	newChild.SetChildren([]logical.Node{&newLim})
	return newChild
}

// ---------------------------------------------------------------------------
// Rule: column-pruning
// ---------------------------------------------------------------------------
//
// Top-down required-column analysis. Computes the minimal set of columns
// needed from the Scan by walking the plan tree and propagating required
// columns downward through each node.
//
// At each node, we compute what columns it needs from its input based on:
//   - What the parent needs from this node's output.
//   - What expressions this node evaluates (which read from the input).
//
// Key design: Aggregate and plain-stats Project RESET the required set
// because they define entirely new output columns; columns above them
// that reference agg-output names must NOT propagate through. Extend ADDS
// the columns its expressions reference but REMOVES the names it produces
// (they don't need to come from the Scan). Filter, Sort, TopK, Limit,
// Dedup pass through requirements plus add their own references.
//
// Nodes that make the set OPEN (unbounded): Parse without into, Describe,
// Helper, glob-pattern Project, Join, Union. When the set is open, pruning
// is conservatively disabled (Columns stays nil).
//
// Built-in fields _time, _raw, _source are always included when pruning
// is enabled.
func columnPruning(root logical.Node) (logical.Node, bool) {
	// Walk the linear path from root to Scan, computing required columns
	// at each level. For non-linear plans (Join, Union), disable pruning.
	required, ok := computeRequiredAtScan(root)
	if !ok {
		return root, false
	}

	// Add builtins and set on the Scan.
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		scan, ok := n.(*logical.Scan)
		if !ok {
			return n, false
		}
		if scan.Pushdown.Columns != nil {
			return n, false
		}
		cols := addBuiltins(required)
		// If the required set covers all columns in the Scan's output
		// schema, there is nothing to prune — skip annotation.
		if coversFullSchema(cols, scan.OutputSchema) {
			return n, false
		}
		sort.Strings(cols)
		ns := cloneScan(scan)
		ns.Pushdown.Columns = cols
		return ns, true
	})
}

// builtinColumns that are always included when pruning is possible.
var builtinColumns = []string{"_time", "_raw", "_source"}

// addBuiltins ensures the built-in columns are in the set.
func addBuiltins(cols map[string]bool) []string {
	result := make(map[string]bool, len(cols)+len(builtinColumns))
	for c := range cols {
		result[c] = true
	}
	for _, b := range builtinColumns {
		result[b] = true
	}
	out := make([]string, 0, len(result))
	for c := range result {
		out = append(out, c)
	}
	return out
}

// coversFullSchema returns true if cols (with builtins) covers every column
// in the scan's output schema. When true, pruning would not reduce anything.
func coversFullSchema(cols []string, schema []sema.Field) bool {
	colSet := make(map[string]bool, len(cols))
	for _, c := range cols {
		colSet[c] = true
	}
	for _, f := range schema {
		if !colSet[f.Name] {
			return false
		}
	}
	return true
}

// computeRequiredAtScan walks from root to Scan, computing the minimal set
// of columns needed from the Scan. Returns ok=false when pruning cannot be
// determined (open set).
//
// The algorithm tracks a "needed" set that starts as ALL output columns of
// the root. At each node we transform the set:
//   - Remove columns that this node PRODUCES (they don't come from below).
//   - Add columns that this node's expressions READ from its input.
//   - Aggregate (plain stats, not window) RESETS the needed set entirely:
//     columns above it refer to agg outputs, not scan columns. Below the
//     aggregate, only the agg's own key/func expressions matter.
func computeRequiredAtScan(root logical.Node) (map[string]bool, bool) {
	// Walk the tree top-down.
	return propagateDown(root, nil)
}

// propagateDown walks from n toward the Scan, transforming the required set.
// parentNeeded is nil for the root (meaning "everything this node outputs").
// Returns the set needed at the Scan, or ok=false if indeterminate.
func propagateDown(n logical.Node, parentNeeded map[string]bool) (map[string]bool, bool) {
	if n == nil {
		return nil, true
	}

	switch x := n.(type) {
	case *logical.Scan:
		// Terminal. The parentNeeded IS what we need from the scan.
		if parentNeeded == nil {
			// Root is Scan — need all (no pruning possible without knowing
			// what downstream wants).
			return nil, false
		}
		return parentNeeded, true

	case *logical.Empty:
		return make(map[string]bool), true

	case *logical.Filter:
		// Filter passes through all columns, and additionally reads its expr.
		needed := cloneSet(parentNeeded)
		if needed == nil {
			// Parent needs everything — propagate that plus our expr.
			needed = make(map[string]bool)
			// When parent needs everything we can't prune, but let's try:
			// filter as root? Unusual. Be safe: collect schema.
			for _, f := range x.Schema() {
				needed[f.Name] = true
			}
		}
		collectExprIdents(x.Expr, needed)
		return propagateDown(x.Input, needed)

	case *logical.Extend:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		// Extend PRODUCES its assignment names and READS their expressions.
		// Remove produced names from needed (they come from the Extend, not
		// from below), then add what the expressions reference.
		for _, a := range x.Assignments {
			delete(needed, a.Name)
			collectExprIdents(a.Value, needed)
		}
		return propagateDown(x.Input, needed)

	case *logical.Project:
		// Project restructures columns. Compute what it needs from input.
		needed := make(map[string]bool)
		for _, c := range x.Cols {
			switch c.Action {
			case logical.ProjectKeep:
				if c.Glob {
					return nil, false
				}
				needed[c.Name] = true
			case logical.ProjectDrop:
				if c.Glob {
					return nil, false
				}
				// Drop: we need everything the input has EXCEPT the dropped
				// cols. But we don't know the full input schema precisely
				// at this stage. Conservative: if Project is only drops,
				// pass through the parent's needed set (minus the drops don't
				// help us prune).
			case logical.ProjectRename:
				needed[c.From] = true
			}
		}
		// If there are no keeps (only drops/renames), we need everything
		// the parent needs, which is wider than just renames.
		hasKeep := false
		for _, c := range x.Cols {
			if c.Action == logical.ProjectKeep {
				hasKeep = true
				break
			}
		}
		if !hasKeep {
			// Drop-only or rename-only project: pass through parent's set
			// plus the rename sources.
			if parentNeeded != nil {
				for k := range parentNeeded {
					needed[k] = true
				}
			} else {
				// Parent needs everything — use schema.
				for _, f := range x.Schema() {
					needed[f.Name] = true
				}
			}
		}
		return propagateDown(x.Input, needed)

	case *logical.Aggregate:
		// Window variants (eventstats/streamstats) pass through all input
		// columns plus add their own, so they don't reset the set.
		if x.Window != nil {
			needed := cloneSet(parentNeeded)
			if needed == nil {
				needed = schemaSet(x)
			}
			// Remove produced agg names; add agg expression refs.
			for _, a := range x.Aggs {
				name := a.Alias
				if name == "" {
					name = aggAutoName(a)
				}
				delete(needed, name)
				collectExprIdents(a.Func, needed)
				if a.WhereCond != nil {
					collectExprIdents(a.WhereCond, needed)
				}
			}
			// Keys are passed through.
			for _, k := range x.Keys {
				collectExprIdents(k.Expr, needed)
			}
			if x.TimeBin != nil {
				needed["_time"] = true
			}
			return propagateDown(x.Input, needed)
		}
		// Plain stats: RESETS the required set. Only the aggregate's own
		// key/func expressions matter — columns above it refer to agg
		// output names (count, avg(x), etc.) that are produced here.
		needed := make(map[string]bool)
		for _, a := range x.Aggs {
			collectExprIdents(a.Func, needed)
			if a.WhereCond != nil {
				collectExprIdents(a.WhereCond, needed)
			}
		}
		for _, k := range x.Keys {
			collectExprIdents(k.Expr, needed)
		}
		if x.TimeBin != nil {
			needed["_time"] = true
		}
		return propagateDown(x.Input, needed)

	case *logical.Sort:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		for _, k := range x.Keys {
			collectExprIdents(k.Expr, needed)
		}
		return propagateDown(x.Input, needed)

	case *logical.TopK:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		for _, k := range x.SortKeys {
			collectExprIdents(k.Expr, needed)
		}
		return propagateDown(x.Input, needed)

	case *logical.Limit:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		return propagateDown(x.Input, needed)

	case *logical.Dedup:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		for _, f := range x.Fields {
			needed[f] = true
		}
		return propagateDown(x.Input, needed)

	case *logical.Parse:
		if x.From != "" {
			// known from-field
		} else {
			// reads _raw
		}
		if len(x.Captures) == 0 {
			// No into clause: produces unknown fields -> open set.
			return nil, false
		}
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		// Remove produced capture names; add the from field.
		for _, c := range x.Captures {
			delete(needed, c.Name)
		}
		if x.From != "" {
			needed[x.From] = true
		} else {
			needed["_raw"] = true
		}
		return propagateDown(x.Input, needed)

	case *logical.Explode:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		needed[x.Field] = true
		return propagateDown(x.Input, needed)

	case *logical.Join:
		// Join involves two sub-plans with different scans.
		// Conservative: disable.
		return nil, false

	case *logical.Union:
		// Multiple inputs. Conservative: disable.
		return nil, false

	case *logical.Describe:
		// Reads ALL columns.
		return nil, false

	case *logical.Helper:
		// Unknown semantics.
		return nil, false

	case *logical.Materialize:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		return propagateDown(x.Input, needed)

	case *logical.Tee:
		needed := cloneSet(parentNeeded)
		if needed == nil {
			needed = schemaSet(x)
		}
		return propagateDown(x.Input, needed)

	default:
		return nil, false
	}
}

// cloneSet returns a copy of the set, or nil if input is nil.
func cloneSet(s map[string]bool) map[string]bool {
	if s == nil {
		return nil
	}
	out := make(map[string]bool, len(s))
	for k := range s {
		out[k] = true
	}
	return out
}

// schemaSet builds a set from a node's output schema.
func schemaSet(n logical.Node) map[string]bool {
	s := make(map[string]bool)
	for _, f := range n.Schema() {
		s[f.Name] = true
	}
	return s
}

// collectExprIdents walks an expression tree and adds all Ident names to the
// set.
func collectExprIdents(e ast.Expr, cols map[string]bool) {
	if e == nil {
		return
	}
	switch x := e.(type) {
	case *ast.Ident:
		cols[x.Name] = true
	case *ast.Unary:
		collectExprIdents(x.Operand, cols)
	case *ast.Binary:
		collectExprIdents(x.Left, cols)
		collectExprIdents(x.Right, cols)
	case *ast.Call:
		if x.Receiver != nil {
			collectExprIdents(x.Receiver, cols)
		}
		for _, a := range x.Args {
			collectExprIdents(a, cols)
		}
	case *ast.Paren:
		collectExprIdents(x.Inner, cols)
	case *ast.In:
		collectExprIdents(x.LHS, cols)
		collectExprIdents(x.RHS, cols)
	case *ast.Between:
		collectExprIdents(x.X, cols)
		collectExprIdents(x.Lo, cols)
		collectExprIdents(x.Hi, cols)
	case *ast.Member:
		collectExprIdents(x.Object, cols)
	case *ast.SafeMember:
		collectExprIdents(x.Object, cols)
	case *ast.Index:
		collectExprIdents(x.Object, cols)
		collectExprIdents(x.Idx, cols)
	case *ast.Lambda:
		collectExprIdents(x.Body, cols)
	case *ast.Array:
		for _, el := range x.Elems {
			collectExprIdents(el, cols)
		}
	case *ast.Object:
		for _, ent := range x.Entries {
			collectExprIdents(ent.Value, cols)
		}
		// Literal, ErrorExpr, SearchGlobValue — no idents.
	}
}
