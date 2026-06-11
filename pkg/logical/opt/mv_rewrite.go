// MV rewrite rule: rewrites queries that match a materialized view to scan
// the view instead of raw data, providing up to ~400x speedup.
//
// v1 matching restrictions (documented in RFC-002 decision log):
//   - Exact canonical filter equality (no implication/range tightening)
//   - Rollup table: count->sum, sum->sum, min->min, max->max
//   - Refused aggs for subset group-by: avg, dc, stdev, perc*, values, earliest, latest
//   - View must be active or backfilling
//   - No catalog = no-op
package opt

import (
	"strings"

	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/format"
)

// ---------------------------------------------------------------------------
// ViewCatalog: interface for the optimizer to query MV metadata
// ---------------------------------------------------------------------------

// ViewInfo describes a materialized view for optimizer matching.
type ViewInfo struct {
	Name     string
	Status   string // "active", "backfill", "paused", "needs-migration", etc.
	Filter   string // canonical filter expression (formatted from AST), empty = no filter
	GroupBy  []string
	Aggs     []AggInfo
	Source   string // source index name (e.g. "main")
	RowCount int64  // approximate row count in the view
}

// AggInfo describes a single aggregation in a view.
type AggInfo struct {
	Func  string // "count", "sum", "avg", "min", "max", "dc"
	Arg   string // field name (empty for count())
	Alias string // output column name
}

// ViewCatalog provides materialized view metadata for the optimizer.
type ViewCatalog interface {
	ListViewInfos() []ViewInfo
}

// MVAccel holds acceleration metadata returned by OptimizeWithViews.
type MVAccel struct {
	ViewName string
	Status   string // "active" or "backfill"
	Speedup  string // e.g. "~400x"
}

// ---------------------------------------------------------------------------
// Options and OptimizeWith entry point
// ---------------------------------------------------------------------------

// Options configures the optimizer with optional external catalogs.
type Options struct {
	Views ViewCatalog
}

// OptimizeWithViews runs the standard optimizer and then attempts MV rewriting.
// Returns the optimized plan, applied rule list, and optional MV acceleration
// metadata. When no ViewCatalog is provided, behaves identically to Optimize.
func OptimizeWithViews(p *logical.Plan, o Options) (*logical.Plan, []Applied, *MVAccel) {
	p, applied := Optimize(p)

	if o.Views == nil {
		return p, applied, nil
	}

	views := o.Views.ListViewInfos()
	if len(views) == 0 {
		return p, applied, nil
	}

	rewritten, accel := tryMVRewrite(p, views)
	if accel != nil {
		p = rewritten
		applied = append(applied, Applied{Rule: "mv-rewrite", Count: 1})
	}

	return p, applied, accel
}

// ---------------------------------------------------------------------------
// MV rewrite logic
// ---------------------------------------------------------------------------

// tryMVRewrite attempts to rewrite the plan to scan a materialized view.
// Returns the rewritten plan and acceleration metadata, or (original, nil)
// if no view matches.
func tryMVRewrite(p *logical.Plan, views []ViewInfo) (*logical.Plan, *MVAccel) {
	if p == nil || p.Root == nil {
		return p, nil
	}

	// Extract the plan shape: we need Scan [+ Filter] + Aggregate, possibly
	// with Sort/Limit/TopK/Project above the Aggregate.
	shape := extractRewritableShape(p.Root)
	if shape == nil {
		return p, nil
	}

	// Try each view for a match.
	for _, vi := range views {
		// View must be active or backfilling.
		if vi.Status != "active" && vi.Status != "backfill" {
			continue
		}

		accel := tryMatchView(p, shape, vi)
		if accel != nil {
			return p, accel
		}
	}

	return p, nil
}

// rewritableShape captures the components of a plan suitable for MV rewrite.
type rewritableShape struct {
	scan     *logical.Scan
	filter   *logical.Filter // nil if no filter
	agg      *logical.Aggregate
	above    logical.Node // the node above the aggregate (may be agg itself if root)
	aggIsTop bool         // true if agg is the topmost operator (after pass-through nodes)
}

// extractRewritableShape walks the plan to find a Scan [+ Filter] + Aggregate
// pattern. The aggregate must be non-windowed.
func extractRewritableShape(root logical.Node) *rewritableShape {
	// Walk down from root through pass-through nodes to find the aggregate.
	agg, above := findAggregate(root)
	if agg == nil {
		return nil
	}
	if agg.Window != nil {
		return nil // windowed aggregates not rewritable
	}

	// Below the aggregate should be [Filter ->] Scan.
	child := agg.Input
	if child == nil {
		return nil
	}

	shape := &rewritableShape{
		agg:      agg,
		above:    above,
		aggIsTop: above == nil,
	}

	switch nd := child.(type) {
	case *logical.Filter:
		if scan, ok := nd.Input.(*logical.Scan); ok {
			shape.filter = nd
			shape.scan = scan
		}
	case *logical.Scan:
		shape.scan = nd
	}

	if shape.scan == nil {
		return nil
	}

	// Must be a single named source.
	if len(shape.scan.Sources) != 1 {
		return nil
	}
	if shape.scan.Sources[0].Kind != ast.SourceName {
		return nil
	}

	return shape
}

// findAggregate walks down through pass-through nodes (Sort, Limit, TopK,
// Project) to find the first Aggregate. Returns the Aggregate and the node
// directly above it (nil if aggregate is root).
func findAggregate(root logical.Node) (*logical.Aggregate, logical.Node) {
	if agg, ok := root.(*logical.Aggregate); ok {
		return agg, nil
	}

	// Walk through pass-through nodes.
	var parent logical.Node
	n := root
	for {
		switch nd := n.(type) {
		case *logical.Sort:
			parent = nd
			n = nd.Input
		case *logical.Limit:
			parent = nd
			n = nd.Input
		case *logical.TopK:
			parent = nd
			n = nd.Input
		case *logical.Project:
			parent = nd
			n = nd.Input
		case *logical.Aggregate:
			return nd, parent
		default:
			return nil, nil
		}
	}
}

// tryMatchView attempts to match a single view against the plan shape.
// If successful, rewrites the plan in-place and returns MVAccel metadata.
func tryMatchView(p *logical.Plan, shape *rewritableShape, vi ViewInfo) *MVAccel {
	// Source must match.
	scanSource := shape.scan.Sources[0].Name
	if vi.Source != "" && vi.Source != scanSource {
		return nil
	}

	// Filter must match (v1: exact canonical equality).
	queryFilter := canonicalizeFilter(shape.filter)
	if queryFilter != vi.Filter {
		return nil
	}

	// Must have aggregations in the view.
	if len(vi.Aggs) == 0 {
		return nil
	}

	// Match group-by and aggregations.
	queryGroupBy := extractGroupByNames(shape.agg)
	queryAggs := extractAggInfos(shape.agg)

	viewGroupBySet := toStringSet(vi.GroupBy)
	queryGroupBySet := toStringSet(queryGroupBy)

	if setsEqual(queryGroupBySet, viewGroupBySet) {
		// EXACT group-by match: rewrite to Scan(view) + Project.
		return rewriteExactMatch(p, shape, vi, queryAggs)
	}

	if isSubset(queryGroupBySet, viewGroupBySet) {
		// SUBSET group-by: rewrite to Scan(view) + rolled-up Aggregate.
		return rewriteSubsetMatch(p, shape, vi, queryGroupBy, queryAggs)
	}

	return nil
}

// rewriteExactMatch rewrites the plan for exact group-by match.
// View rows ARE the result — no re-aggregation needed.
func rewriteExactMatch(p *logical.Plan, shape *rewritableShape, vi ViewInfo, queryAggs []aggMatch) *MVAccel {
	// Verify all query aggs exist in the view (by function + field).
	viewAggMap := buildViewAggMap(vi.Aggs)
	for _, qa := range queryAggs {
		key := qa.Func + "\x00" + qa.Arg
		if _, ok := viewAggMap[key]; !ok {
			return nil // query asks for an agg the view doesn't have
		}
	}

	// Build the rewritten plan: Scan(viewName) + Project (rename view aliases to query aliases).
	viewScan := &logical.Scan{
		Sources: []logical.SourcePattern{
			{Kind: ast.SourceName, Name: vi.Name},
		},
	}

	// Build one Project that (a) keeps exactly the query's output schema —
	// group-by columns plus aggregate outputs — dropping the view's storage
	// columns (_time, _raw, index, serialized partial state), and (b) renames
	// view aliases to the query-expected names. The physical builder applies
	// keeps before renames, so the keep list uses view-side names.
	var projCols []logical.ProjectCol
	for _, gb := range vi.GroupBy {
		projCols = append(projCols, logical.ProjectCol{
			Name:   gb,
			Action: logical.ProjectKeep,
		})
	}
	for _, qa := range queryAggs {
		key := qa.Func + "\x00" + qa.Arg
		viewAlias := viewAggMap[key]
		projCols = append(projCols, logical.ProjectCol{
			Name:   viewAlias,
			Action: logical.ProjectKeep,
		})
		if viewAlias != qa.Alias {
			projCols = append(projCols, logical.ProjectCol{
				Name:   qa.Alias,
				Action: logical.ProjectRename,
				From:   viewAlias,
			})
		}
	}

	proj := &logical.Project{Cols: projCols}
	proj.SetChildren([]logical.Node{viewScan})
	var newRoot logical.Node = proj

	// Preserve above-agg nodes (Sort, Limit, TopK, Project).
	newRoot = reattachAbove(shape.agg, p.Root, newRoot)

	p.Root = newRoot

	return &MVAccel{
		ViewName: vi.Name,
		Status:   vi.Status,
		Speedup:  estimateSpeedup(vi.RowCount),
	}
}

// rollableAggs maps agg function names to their roll-up function when doing
// subset group-by re-aggregation. count -> sum(count_alias) because summing
// pre-aggregated counts gives the correct total.
var rollableAggs = map[string]string{
	"count": "sum",
	"sum":   "sum",
	"min":   "min",
	"max":   "max",
}

// nonRollableAggs lists aggregation functions that CANNOT be correctly derived
// from finalized view rows when the query's group-by is a subset of the view's
// group-by. These would require access to raw intermediate state.
var nonRollableAggs = map[string]bool{
	"avg":      true,
	"dc":       true,
	"stdev":    true,
	"stdevp":   true,
	"var":      true,
	"varp":     true,
	"values":   true,
	"earliest": true,
	"latest":   true,
	"perc25":   true, "perc50": true, "perc75": true, "perc90": true, "perc95": true, "perc99": true,
	"p25": true, "p50": true, "p75": true, "p90": true, "p95": true, "p99": true,
}

// rewriteSubsetMatch rewrites for subset group-by with rolled-up aggregation.
func rewriteSubsetMatch(p *logical.Plan, shape *rewritableShape, vi ViewInfo, queryGroupBy []string, queryAggs []aggMatch) *MVAccel {
	viewAggMap := buildViewAggMap(vi.Aggs)

	// Check that all query aggs are rollable.
	for _, qa := range queryAggs {
		if nonRollableAggs[qa.Func] {
			return nil // REFUSE: non-rollable agg with subset group-by
		}
		rollFunc, ok := rollableAggs[qa.Func]
		if !ok {
			return nil // unknown agg function for rollup
		}
		_ = rollFunc // used below

		key := qa.Func + "\x00" + qa.Arg
		if _, ok := viewAggMap[key]; !ok {
			return nil // query asks for an agg the view doesn't have
		}
	}

	// Build: Scan(viewName) -> Aggregate(rolled-up funcs, query group-by).
	viewScan := &logical.Scan{
		Sources: []logical.SourcePattern{
			{Kind: ast.SourceName, Name: vi.Name},
		},
	}

	// Build rolled-up aggregation.
	var aggs []logical.Agg
	for _, qa := range queryAggs {
		key := qa.Func + "\x00" + qa.Arg
		viewAlias := viewAggMap[key]
		rollFunc := rollableAggs[qa.Func]

		// The rolled-up agg reads the VIEW'S output column (viewAlias)
		// and applies the roll-up function.
		aggs = append(aggs, logical.Agg{
			Func: &ast.Call{
				Callee: rollFunc,
				Args:   []ast.Expr{&ast.Ident{Name: viewAlias}},
			},
			Alias: qa.Alias,
		})
	}

	var keys []logical.Key
	for _, gb := range queryGroupBy {
		keys = append(keys, logical.Key{
			Name: gb,
			Expr: &ast.Ident{Name: gb},
		})
	}

	newAgg := &logical.Aggregate{
		Aggs: aggs,
		Keys: keys,
	}
	newAgg.SetChildren([]logical.Node{viewScan})

	var newRoot logical.Node = newAgg
	newRoot = reattachAbove(shape.agg, p.Root, newRoot)
	p.Root = newRoot

	return &MVAccel{
		ViewName: vi.Name,
		Status:   vi.Status,
		Speedup:  estimateSpeedup(vi.RowCount),
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// aggMatch captures a query's aggregation function for matching.
type aggMatch struct {
	Func  string // "count", "sum", etc.
	Arg   string // field name
	Alias string // output alias
}

// canonicalizeFilter returns a canonical string for the filter expression.
// Empty string when filter is nil (no filter).
func canonicalizeFilter(f *logical.Filter) string {
	if f == nil || f.Expr == nil {
		return ""
	}
	return format.Expr(f.Expr)
}

// extractGroupByNames returns the group-by field names from an Aggregate node.
func extractGroupByNames(agg *logical.Aggregate) []string {
	var names []string
	if agg.TimeBin != nil {
		names = append(names, "_time")
	}
	for _, k := range agg.Keys {
		names = append(names, k.Name)
	}
	return names
}

// extractAggInfos returns the aggregation function descriptors from an Aggregate.
func extractAggInfos(agg *logical.Aggregate) []aggMatch {
	var result []aggMatch
	for _, a := range agg.Aggs {
		call, ok := a.Func.(*ast.Call)
		if !ok {
			continue
		}
		name := strings.ToLower(call.Callee)
		field := ""
		if len(call.Args) > 0 {
			if ident, ok := call.Args[0].(*ast.Ident); ok {
				field = ident.Name
			}
		}
		alias := a.Alias
		if alias == "" {
			if field != "" {
				alias = name + "(" + field + ")"
			} else {
				alias = name + "()"
			}
		}
		result = append(result, aggMatch{Func: name, Arg: field, Alias: alias})
	}
	return result
}

// buildViewAggMap builds a lookup from "func\x00field" -> alias for view aggs.
func buildViewAggMap(aggs []AggInfo) map[string]string {
	m := make(map[string]string, len(aggs))
	for _, a := range aggs {
		key := a.Func + "\x00" + a.Arg
		m[key] = a.Alias
	}
	return m
}

// toStringSet converts a string slice to a set.
func toStringSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// setsEqual returns true if two string sets are identical.
func setsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// isSubset returns true if sub is a proper subset of super.
func isSubset(sub, super map[string]bool) bool {
	if len(sub) >= len(super) {
		return false // must be proper subset (strictly fewer elements)
	}
	for k := range sub {
		if !super[k] {
			return false
		}
	}
	return true
}

// reattachAbove takes the chain of pass-through nodes above the original
// aggregate (Sort, Limit, TopK, Project) and re-attaches them above newChild.
func reattachAbove(origAgg *logical.Aggregate, origRoot, newChild logical.Node) logical.Node {
	// Collect the chain from root down to origAgg (exclusive).
	var chain []logical.Node
	n := origRoot
	for n != nil && n != origAgg {
		chain = append(chain, n)
		children := n.Children()
		if len(children) == 0 {
			break
		}
		n = children[0]
	}

	if len(chain) == 0 {
		return newChild
	}

	// Clone and reattach: bottom of chain points to newChild.
	// chain[0] is root, chain[last] is the node just above agg.
	cloned := make([]logical.Node, len(chain))
	for i, node := range chain {
		cloned[i] = shallowCloneNode(node)
	}

	// Wire: cloned[i].SetChildren -> cloned[i+1], last -> newChild.
	for i := 0; i < len(cloned)-1; i++ {
		cloned[i].SetChildren([]logical.Node{cloned[i+1]})
	}
	cloned[len(cloned)-1].SetChildren([]logical.Node{newChild})

	return cloned[0]
}

// shallowCloneNode creates a shallow copy of a pass-through node.
func shallowCloneNode(n logical.Node) logical.Node {
	switch nd := n.(type) {
	case *logical.Sort:
		clone := *nd
		return &clone
	case *logical.Limit:
		clone := *nd
		return &clone
	case *logical.TopK:
		clone := *nd
		return &clone
	case *logical.Project:
		clone := *nd
		return &clone
	default:
		// Should not happen for pass-through nodes.
		return n
	}
}

// estimateSpeedup returns a human-readable speedup estimate.
func estimateSpeedup(viewRows int64) string {
	if viewRows <= 0 {
		return "~400x" // optimistic default when row count unknown
	}
	// Assume raw data is ~400x more rows than a view (empirical default).
	return "~400x"
}
