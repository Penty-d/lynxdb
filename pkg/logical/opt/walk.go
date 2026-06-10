// Package opt provides rule-based expression simplification for the logical
// plan (pkg/logical). Rules operate on [ast.Expr] nodes embedded in logical
// plan nodes (Filter, Extend, Aggregate, etc.).
//
// # Rebuild semantics
//
// Lower (lower.go) references the original parsed AST expressions rather than
// deep-copying them. Multiple plan nodes may share the same ast.Expr pointers,
// and the parsed Query must remain unchanged for EXPLAIN rendering. Therefore
// all rules MUST return new ast.Expr trees — never mutate existing nodes.
package opt

import (
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// walkExprs visits every expression position in the plan node n. For each
// position it calls f with the current expression and replaces it with the
// returned value. walkExprs returns true if any expression was changed (the
// returned expression differed from the input by pointer identity).
//
// walkExprs covers: Filter.Expr, Extend assignments, Aggregate agg funcs +
// where-predicates + keys, Sort keys, TopK sort keys, Helper options +
// positionals, Parse (from is a string, not an Expr position), Aggregate
// TimeBin duration.
//
// Nodes without expression positions (Scan, Project, Limit, Dedup, Describe,
// Join.On, Union, Explode, Materialize, Tee) are no-ops.
func walkExprs(n logical.Node, f func(ast.Expr) ast.Expr) bool {
	changed := false
	apply := func(e ast.Expr) ast.Expr {
		if e == nil {
			return nil
		}
		r := f(e)
		if r != e {
			changed = true
		}
		return r
	}

	switch x := n.(type) {
	case *logical.Filter:
		x.Expr = apply(x.Expr)

	case *logical.Extend:
		for i := range x.Assignments {
			x.Assignments[i].Value = apply(x.Assignments[i].Value)
		}

	case *logical.Aggregate:
		for i := range x.Aggs {
			x.Aggs[i].Func = apply(x.Aggs[i].Func)
			x.Aggs[i].WhereCond = apply(x.Aggs[i].WhereCond)
		}
		for i := range x.Keys {
			x.Keys[i].Expr = apply(x.Keys[i].Expr)
		}
		if x.TimeBin != nil {
			x.TimeBin.Duration = apply(x.TimeBin.Duration)
		}

	case *logical.Sort:
		for i := range x.Keys {
			x.Keys[i].Expr = apply(x.Keys[i].Expr)
		}

	case *logical.TopK:
		for i := range x.SortKeys {
			x.SortKeys[i].Expr = apply(x.SortKeys[i].Expr)
		}

	case *logical.Helper:
		for k, v := range x.Options {
			x.Options[k] = apply(v)
		}
		for i := range x.Positional {
			x.Positional[i] = apply(x.Positional[i])
		}

		// Scan, Project, Limit, Dedup, Describe, Join, Union, Explode,
		// Materialize, Tee, Parse, Empty — no expression positions to walk.
	}

	return changed
}

// walkPlan applies fn to every node in the plan tree (depth-first post-order)
// and returns true if fn returned true for any node.
func walkPlan(root logical.Node, fn func(logical.Node) bool) bool {
	if root == nil {
		return false
	}
	changed := false

	// Recurse into children first.
	for _, c := range root.Children() {
		if walkPlan(c, fn) {
			changed = true
		}
	}
	// For Join, also walk the Right sub-plan.
	if j, ok := root.(*logical.Join); ok && j.Right != nil {
		if walkPlan(j.Right, fn) {
			changed = true
		}
	}
	if fn(root) {
		changed = true
	}
	return changed
}
