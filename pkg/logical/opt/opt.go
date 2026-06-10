package opt

import (
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// maxPasses is the upper bound on fixed-point iterations.
const maxPasses = 10

// Rule is a single optimizer rule that rewrites an expression tree.
// Rules MUST return a new ast.Expr (rebuild semantics — see package doc).
type Rule struct {
	// Name identifies this rule in EXPLAIN output and Applied reporting.
	Name string
	// Apply rewrites an expression, returning the new expression and true
	// if a change was made. The input expression must not be mutated.
	Apply func(e ast.Expr) (ast.Expr, bool)
}

// Applied records how many times a rule fired during optimization.
type Applied struct {
	Rule  string
	Count int
}

// Optimize runs expression-simplification rules and plan-level rules on p.
// It applies rules in a fixed-point loop (deterministic rule order, maximum
// [maxPasses] passes) until no rule fires.
//
// Each pass runs in two phases:
//  1. Expression rules: visit every expression position in every plan node,
//     applying each rule bottom-up.
//  2. Plan rules: visit the plan tree bottom-up, applying each rule to the
//     node structure (filter elimination, merge, predicate pushdown, etc.).
//
// Optimize does NOT modify the input Plan's expression AST nodes. It replaces
// the Expr pointers on plan nodes with new trees (rebuild semantics). Plan
// rules rebuild plan Node pointers (same rebuild semantics).
//
// The returned Applied slice contains one entry per rule that fired at least
// once, in the order the rules are defined (expression rules first, then plan
// rules).
func Optimize(p *logical.Plan) (*logical.Plan, []Applied) {
	exprRules := defaultRules()
	planRules := defaultPlanRules()
	exprCounts := make([]int, len(exprRules))
	planCounts := make([]int, len(planRules))

	for pass := 0; pass < maxPasses; pass++ {
		anyChanged := false

		// Phase 1: Expression rules.
		for ri, rule := range exprRules {
			ruleApply := rule.Apply
			nodeChanged := walkPlanExprs(p, func(e ast.Expr) (ast.Expr, bool) {
				return ruleApply(e)
			})
			if nodeChanged {
				exprCounts[ri]++
				anyChanged = true
			}
		}

		// Phase 2: Plan rules (run after expression rules each pass so
		// they see simplified expressions, e.g. const-folded true/false).
		for ri, rule := range planRules {
			planChanged := applyPlanRule(p, rule)
			if planChanged {
				planCounts[ri]++
				anyChanged = true
			}
		}

		if !anyChanged {
			break
		}
	}

	var applied []Applied
	for i, c := range exprCounts {
		if c > 0 {
			applied = append(applied, Applied{Rule: exprRules[i].Name, Count: c})
		}
	}
	for i, c := range planCounts {
		if c > 0 {
			applied = append(applied, Applied{Rule: planRules[i].Name, Count: c})
		}
	}

	return p, applied
}

// applyPlanRule applies a single plan rule to the entire plan (main root +
// CTE sub-plans). Returns true if any change was made.
func applyPlanRule(p *logical.Plan, rule PlanRule) bool {
	changed := false

	// Walk CTE sub-plans.
	for name, letPlan := range p.Lets {
		newRoot, c := rule.Apply(letPlan.Root)
		if c {
			p.Lets[name] = &logical.Plan{Root: newRoot, Lets: letPlan.Lets}
			changed = true
		}
	}

	// Walk main plan.
	newRoot, c := rule.Apply(p.Root)
	if c {
		p.Root = newRoot
		changed = true
	}

	return changed
}

// walkPlanExprs applies the rewrite function to every expression position in
// every node of the plan (including CTE sub-plans). It returns true if any
// expression was changed.
func walkPlanExprs(p *logical.Plan, f func(ast.Expr) (ast.Expr, bool)) bool {
	changed := false

	// Walk CTE sub-plans.
	for _, letPlan := range p.Lets {
		if walkPlanExprsNode(letPlan.Root, f) {
			changed = true
		}
	}

	// Walk main plan.
	if walkPlanExprsNode(p.Root, f) {
		changed = true
	}

	return changed
}

// walkPlanExprsNode applies the rewrite function to every expression position
// in the node tree rooted at n.
func walkPlanExprsNode(root logical.Node, f func(ast.Expr) (ast.Expr, bool)) bool {
	changed := false
	walkPlan(root, func(n logical.Node) bool {
		nodeChanged := walkExprs(n, func(e ast.Expr) ast.Expr {
			r, c := rewriteBottomUp(e, f)
			if c {
				changed = true
			}
			return r
		})
		if nodeChanged {
			changed = true
		}
		return false // walkPlan return ignored by outer; we track via changed
	})
	return changed
}

// rewriteBottomUp walks the expression tree bottom-up, applying f to each
// node after its children have been rewritten. Returns the new tree and
// whether any change occurred.
func rewriteBottomUp(e ast.Expr, f func(ast.Expr) (ast.Expr, bool)) (ast.Expr, bool) {
	if e == nil {
		return nil, false
	}

	anyChanged := false

	// Rebuild children bottom-up.
	switch x := e.(type) {
	case *ast.Unary:
		operand, c := rewriteBottomUp(x.Operand, f)
		if c {
			anyChanged = true
			e = &ast.Unary{Op: x.Op, Operand: operand, Pos: x.Pos}
		}

	case *ast.Binary:
		left, lc := rewriteBottomUp(x.Left, f)
		right, rc := rewriteBottomUp(x.Right, f)
		if lc || rc {
			anyChanged = true
			e = &ast.Binary{Op: x.Op, Left: left, Right: right, Pos: x.Pos}
		}

	case *ast.Call:
		var recv ast.Expr
		recvChanged := false
		if x.Receiver != nil {
			recv, recvChanged = rewriteBottomUp(x.Receiver, f)
		}
		args := x.Args
		argsChanged := false
		for i, a := range x.Args {
			r, c := rewriteBottomUp(a, f)
			if c {
				if !argsChanged {
					// Copy-on-write.
					args = make([]ast.Expr, len(x.Args))
					copy(args, x.Args)
					argsChanged = true
				}
				args[i] = r
			}
		}
		if recvChanged || argsChanged {
			anyChanged = true
			e = &ast.Call{
				Receiver: recv,
				SafeNav:  x.SafeNav,
				Callee:   x.Callee,
				Bang:     x.Bang,
				Args:     args,
				Pos:      x.Pos,
			}
			if !recvChanged && x.Receiver != nil {
				e.(*ast.Call).Receiver = x.Receiver
			}
		}

	case *ast.Paren:
		inner, c := rewriteBottomUp(x.Inner, f)
		if c {
			anyChanged = true
			e = &ast.Paren{Inner: inner, Pos: x.Pos}
		}

	case *ast.In:
		lhs, lc := rewriteBottomUp(x.LHS, f)
		rhs, rc := rewriteBottomUp(x.RHS, f)
		if lc || rc {
			anyChanged = true
			e = &ast.In{LHS: lhs, RHS: rhs, Pos: x.Pos}
		}

	case *ast.Between:
		xExpr, xc := rewriteBottomUp(x.X, f)
		lo, loc := rewriteBottomUp(x.Lo, f)
		hi, hic := rewriteBottomUp(x.Hi, f)
		if xc || loc || hic {
			anyChanged = true
			e = &ast.Between{X: xExpr, Lo: lo, Hi: hi, Pos: x.Pos}
		}

	case *ast.Member:
		obj, c := rewriteBottomUp(x.Object, f)
		if c {
			anyChanged = true
			e = &ast.Member{Object: obj, Field: x.Field, Pos: x.Pos}
		}

	case *ast.SafeMember:
		obj, c := rewriteBottomUp(x.Object, f)
		if c {
			anyChanged = true
			e = &ast.SafeMember{Object: obj, Field: x.Field, Pos: x.Pos}
		}

	case *ast.Index:
		obj, oc := rewriteBottomUp(x.Object, f)
		idx, ic := rewriteBottomUp(x.Idx, f)
		if oc || ic {
			anyChanged = true
			e = &ast.Index{Object: obj, Idx: idx, Pos: x.Pos}
		}

	case *ast.Lambda:
		body, c := rewriteBottomUp(x.Body, f)
		if c {
			anyChanged = true
			e = &ast.Lambda{Param: x.Param, Body: body, Pos: x.Pos}
		}

	case *ast.Array:
		elems := x.Elems
		elemsChanged := false
		for i, el := range x.Elems {
			r, c := rewriteBottomUp(el, f)
			if c {
				if !elemsChanged {
					elems = make([]ast.Expr, len(x.Elems))
					copy(elems, x.Elems)
					elemsChanged = true
				}
				elems[i] = r
			}
		}
		if elemsChanged {
			anyChanged = true
			e = &ast.Array{Elems: elems, Pos: x.Pos}
		}

	case *ast.Object:
		entries := x.Entries
		entriesChanged := false
		for i, ent := range x.Entries {
			r, c := rewriteBottomUp(ent.Value, f)
			if c {
				if !entriesChanged {
					entries = make([]ast.ObjectEntry, len(x.Entries))
					copy(entries, x.Entries)
					entriesChanged = true
				}
				entries[i].Value = r
			}
		}
		if entriesChanged {
			anyChanged = true
			e = &ast.Object{Entries: entries, Pos: x.Pos}
		}

		// Leaf nodes: *ast.Ident, *ast.Literal, *ast.ErrorExpr,
		// *ast.SearchGlobValue — no children.
	}

	// Apply f to the (possibly rebuilt) node.
	result, c := f(e)
	if c {
		return result, true
	}
	return e, anyChanged
}

// defaultRules returns the expression-simplification rules in deterministic
// order. Rule order matters for convergence: paren-strip first so downstream
// rules see clean trees; const-fold before bool-simplify so folded
// comparisons feed into boolean absorption.
func defaultRules() []Rule {
	return []Rule{
		{Name: "paren-strip", Apply: parenStrip},
		{Name: "const-fold-arith", Apply: constFoldArith},
		{Name: "const-fold-compare", Apply: constFoldCompare},
		{Name: "bool-simplify", Apply: boolSimplify},
		{Name: "coalesce-fold", Apply: coalesceFold},
		{Name: "if-fold", Apply: ifFold},
		{Name: "cmp-normalize", Apply: cmpNormalize},
	}
}
