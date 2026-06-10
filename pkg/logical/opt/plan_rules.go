package opt

import (
	"strings"
	"unicode"

	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/sema"
)

// PlanRule is a single optimizer rule that rewrites a plan tree.
// Plan rules operate on [logical.Node] subtrees (not individual expressions).
// Like expression rules, plan rules MUST return new/rebuilt nodes — never
// mutate existing nodes (rebuild semantics, same as expression rules, because
// lowered plans may share AST pointers).
type PlanRule struct {
	// Name identifies this rule in EXPLAIN output and Applied reporting.
	Name string
	// Apply rewrites the plan tree rooted at root, returning the new root
	// and true if a change was made. The input node tree must not be mutated.
	Apply func(root logical.Node) (logical.Node, bool)
}

// defaultPlanRules returns the plan-level rules in deterministic order.
//
// Rule ordering matters:
//   - filter-merge runs first so that adjacent filters are combined into a
//     single Filter before predicate-pushdown analyzes conjuncts. This avoids
//     analyzing the same predicate twice across two Filter nodes.
//   - filter-elim and filter-false-to-empty run next to remove trivial filters.
//   - predicate-pushdown runs last to decompose the (now-merged) filter into
//     Scan.Pushdown hints.
func defaultPlanRules() []PlanRule {
	return []PlanRule{
		{Name: "filter-merge", Apply: filterMerge},
		{Name: "filter-elim", Apply: filterElim},
		{Name: "filter-false-to-empty", Apply: filterFalseToEmpty},
		{Name: "predicate-pushdown", Apply: predicatePushdown},
	}
}

// ---------------------------------------------------------------------------
// Rule: filter-elim
// ---------------------------------------------------------------------------
//
// Removes Filter(true) — a filter whose predicate is a true literal is a
// no-op; the node is replaced by its input child.
func filterElim(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		f, ok := n.(*logical.Filter)
		if !ok {
			return n, false
		}
		if isTrueLiteral(f.Expr) && f.Input != nil {
			return f.Input, true
		}
		return n, false
	})
}

// ---------------------------------------------------------------------------
// Rule: filter-false-to-empty
// ---------------------------------------------------------------------------
//
// Replaces Filter(false) and Filter(null) with an Empty node. Both are
// provably unsatisfiable: false never matches, and null is falsy in boolean
// context (three-valued logic: WHERE null yields no rows).
func filterFalseToEmpty(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		f, ok := n.(*logical.Filter)
		if !ok || f.Input == nil {
			return n, false
		}
		if isFalseLiteral(f.Expr) || isNullLiteral(f.Expr) {
			// Preserve the schema of the filter's input so downstream
			// operators see the expected columns.
			return &logical.Empty{OutputSchema: copySchema(f.Input.Schema())}, true
		}
		return n, false
	})
}

// ---------------------------------------------------------------------------
// Rule: filter-merge
// ---------------------------------------------------------------------------
//
// Merges adjacent Filter nodes: Filter(a) whose input is Filter(b) becomes
// Filter(a AND b) with b's input. This produces a single Filter with all
// conjuncts, which predicate-pushdown can then decompose efficiently.
func filterMerge(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		outer, ok := n.(*logical.Filter)
		if !ok {
			return n, false
		}
		inner, ok := outer.Input.(*logical.Filter)
		if !ok {
			return n, false
		}
		merged := &ast.Binary{
			Op:    ast.OpAnd,
			Left:  inner.Expr,
			Right: outer.Expr,
		}
		f := &logical.Filter{Expr: merged}
		f.SetChildren([]logical.Node{inner.Input})
		return f, true
	})
}

// ---------------------------------------------------------------------------
// Rule: predicate-pushdown
// ---------------------------------------------------------------------------
//
// Decomposes a Filter's predicate into storage-level hints on the Scan node
// below it. The Filter must be DIRECTLY above a Scan (v1: no push through
// Parse/Extend/Project; only push through other Filters, which filter-merge
// will have already collapsed).
//
// For each top-level AND conjunct in the Filter predicate:
//
//   - _time comparison (>=, <=, >, <, between) -> Scan.Pushdown.TimeBounds.
//     The conjunct is CONSUMED (removed from the Filter) because time bounds
//     are exact, not probabilistic. When a bracket time range already exists
//     on the Scan, the pushdown intersects: only when both forms are directly
//     representable (same bound direction) is the tighter bound kept; otherwise
//     the conjunct stays in the Filter.
//
//   - has(_raw, "lit") -> tokenize the literal (lowercase, split on non-alnum),
//     append tokens to Scan.Pushdown.RawTerms. The conjunct is KEPT in the
//     Filter because term-index lookup is a candidate filter (false positives
//     possible at segment level; row-level verification required).
//
//   - field == literal, field != literal, field < literal, etc. on scalar
//     literals -> append the expression to Scan.Pushdown.FieldPredicates.
//     KEPT in Filter (RG-level skipping is a hint, row verification stays).
//
//   - contains(_raw, "lit"), glob(_raw, pat), matches(_raw, r"...") -> extract
//     required literal substrings >= 3 chars, tokenize, append tokens to
//     Scan.Pushdown.BloomTerms. KEPT in Filter.
//
//   - Everything else stays in the Filter untouched.
//
// The Filter node is removed only when ALL conjuncts were fully consumed.
func predicatePushdown(root logical.Node) (logical.Node, bool) {
	return rewritePlanBottomUp(root, func(n logical.Node) (logical.Node, bool) {
		f, ok := n.(*logical.Filter)
		if !ok {
			return n, false
		}
		scan, ok := f.Input.(*logical.Scan)
		if !ok {
			return n, false
		}

		// Idempotency guard: if the Scan already has non-empty pushdown
		// hints, a prior pass already decomposed this Filter. Do not
		// re-push (the Filter's conjuncts are deliberately kept for
		// row-level verification, so the tree shape is unchanged on
		// subsequent passes).
		if scanHasPushdown(scan) {
			return n, false
		}

		conjuncts := flattenAnd(f.Expr)
		if len(conjuncts) == 0 {
			return n, false
		}

		// We rebuild the Scan with merged pushdown.
		newScan := cloneScan(scan)
		anyPushed := false

		// Remaining conjuncts that stay in the Filter.
		var remaining []ast.Expr

		for _, conj := range conjuncts {
			pushed, consumed := pushConjunct(newScan, conj)
			if pushed {
				anyPushed = true
			}
			if !consumed {
				remaining = append(remaining, conj)
			}
		}

		if !anyPushed {
			return n, false
		}

		// Rebuild the pipeline: newScan as the input.
		if len(remaining) == 0 {
			// All conjuncts consumed — remove the Filter entirely.
			return newScan, true
		}

		// Rebuild the Filter with remaining conjuncts.
		remainingExpr := rebuildAnd(remaining)
		newFilter := &logical.Filter{Expr: remainingExpr}
		newFilter.SetChildren([]logical.Node{newScan})
		return newFilter, true
	})
}

// pushConjunct analyzes a single conjunct and, if recognized, pushes
// hints into the scan's Pushdown. Returns (pushed, consumed) where pushed
// means something was added to Pushdown, and consumed means the conjunct
// should be removed from the Filter (only time bounds are consumed).
func pushConjunct(scan *logical.Scan, expr ast.Expr) (pushed, consumed bool) {
	// 1. Time bounds: _time >= expr, _time <= expr, _time > expr, _time < expr.
	if tb, ok := extractTimeBound(expr); ok {
		// When the Scan already has a bracket time range (from syntax like
		// from main[-1h]), we must intersect with it. Use the bracket range
		// as the base, then layer the pushdown on top.
		base := scan.Pushdown.TimeBounds
		if base == nil && scan.TimeRange != nil {
			base = scan.TimeRange
		}
		merged := mergeTimeBounds(base, tb)
		if merged != nil {
			scan.Pushdown.TimeBounds = merged
			return true, true
		}
		// Could not intersect representably — keep conjunct.
		return false, false
	}

	// 2. has(_raw, "lit") -> RawTerms.
	if terms, ok := extractHasRawTerms(expr); ok {
		scan.Pushdown.RawTerms = append(scan.Pushdown.RawTerms, terms...)
		return true, false // KEEP in filter
	}

	// 3. field cmp literal -> FieldPredicates.
	if isFieldCmpLiteral(expr) {
		scan.Pushdown.FieldPredicates = append(scan.Pushdown.FieldPredicates, expr)
		return true, false // KEEP in filter
	}

	// 4. contains/glob/matches on _raw -> BloomTerms via literal extraction.
	if terms := extractBloomTerms(expr); len(terms) > 0 {
		scan.Pushdown.BloomTerms = append(scan.Pushdown.BloomTerms, terms...)
		return true, false // KEEP in filter
	}

	return false, false
}

// ---------------------------------------------------------------------------
// Time bound extraction
// ---------------------------------------------------------------------------

// extractTimeBound checks if expr is a comparison of _time against a
// literal/duration expression. Returns a TimeBounds with either Start or End
// populated (not both).
func extractTimeBound(expr ast.Expr) (*logical.TimeBounds, bool) {
	b, ok := expr.(*ast.Binary)
	if !ok {
		return nil, false
	}
	// Check for _time on left: _time >= val, _time <= val, etc.
	if isTimeIdent(b.Left) {
		switch b.Op {
		case ast.OpGtEq, ast.OpGt:
			return &logical.TimeBounds{Start: b.Right}, true
		case ast.OpLtEq, ast.OpLt:
			return &logical.TimeBounds{End: b.Right}, true
		case ast.OpEq:
			// _time == val -> both bounds.
			return &logical.TimeBounds{Start: b.Right, End: b.Right}, true
		}
	}
	// Check for _time on right: val <= _time, val >= _time, etc.
	if isTimeIdent(b.Right) {
		switch b.Op {
		case ast.OpLtEq, ast.OpLt:
			return &logical.TimeBounds{Start: b.Left}, true
		case ast.OpGtEq, ast.OpGt:
			return &logical.TimeBounds{End: b.Left}, true
		case ast.OpEq:
			return &logical.TimeBounds{Start: b.Left, End: b.Left}, true
		}
	}
	return nil, false
}

// mergeTimeBounds intersects a new time bound with an existing one.
// Returns the merged result, or nil if the intersection is not representable
// (which signals the caller to leave the conjunct in the Filter).
func mergeTimeBounds(existing *logical.TimeBounds, incoming *logical.TimeBounds) *logical.TimeBounds {
	if existing == nil {
		return incoming
	}
	// Merge field by field: for each of Start/End, when both are present
	// we can only represent the intersection if the new bound is "the same
	// direction" (both are starts, or both are ends). We keep the incoming
	// value when the existing slot is nil; otherwise we cannot safely merge
	// at plan time (would need runtime evaluation) — return nil to signal
	// "keep conjunct in filter".
	result := &logical.TimeBounds{
		Start: existing.Start,
		End:   existing.End,
		Snap:  existing.Snap,
	}

	if incoming.Start != nil {
		if result.Start != nil {
			// Both have a start bound — we cannot statically determine which
			// is tighter without evaluating the expressions. Keep both: leave
			// the conjunct in the filter and do not merge.
			return nil
		}
		result.Start = incoming.Start
	}
	if incoming.End != nil {
		if result.End != nil {
			return nil
		}
		result.End = incoming.End
	}
	return result
}

// scanHasPushdown returns true if the Scan already has any pushdown hints.
func scanHasPushdown(s *logical.Scan) bool {
	pd := &s.Pushdown
	return pd.TimeBounds != nil ||
		len(pd.FieldPredicates) > 0 ||
		len(pd.BloomTerms) > 0 ||
		len(pd.RawTerms) > 0
}

func isTimeIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "_time"
}

// ---------------------------------------------------------------------------
// has(_raw, "lit") extraction
// ---------------------------------------------------------------------------

// extractHasRawTerms checks if expr is has(_raw, "literal") and returns
// lowercased tokens from the literal argument per the tokenizer contract
// (§6.1: lowercase, runs of ASCII alphanumerics and Unicode letters/digits).
func extractHasRawTerms(expr ast.Expr) ([]string, bool) {
	c, ok := expr.(*ast.Call)
	if !ok || c.Callee != "has" || len(c.Args) != 2 {
		return nil, false
	}
	// First arg must be _raw.
	id, ok := c.Args[0].(*ast.Ident)
	if !ok || id.Name != "_raw" {
		return nil, false
	}
	// Second arg must be a string literal.
	lit, ok := c.Args[1].(*ast.Literal)
	if !ok || (lit.Kind != ast.LitString && lit.Kind != ast.LitRawString) {
		return nil, false
	}
	s, ok := lit.Value.(string)
	if !ok {
		return nil, false
	}
	tokens := tokenize(strings.ToLower(s))
	if len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
}

// ---------------------------------------------------------------------------
// Field comparison extraction
// ---------------------------------------------------------------------------

// isFieldCmpLiteral returns true if expr is a binary comparison of an
// identifier against a scalar literal: field == "x", field >= 5, etc.
// Does NOT match _time comparisons (those are handled by time bounds).
func isFieldCmpLiteral(expr ast.Expr) bool {
	b, ok := expr.(*ast.Binary)
	if !ok {
		return false
	}
	switch b.Op {
	case ast.OpEq, ast.OpNotEq, ast.OpLt, ast.OpLtEq, ast.OpGt, ast.OpGtEq:
		// continue
	default:
		return false
	}
	// field op literal (canonical form after cmp-normalize).
	leftID, leftIsIdent := b.Left.(*ast.Ident)
	_, rightIsLit := b.Right.(*ast.Literal)
	if leftIsIdent && rightIsLit {
		// Exclude _time — handled by time bounds.
		return leftID.Name != "_time"
	}
	return false
}

// ---------------------------------------------------------------------------
// Bloom term extraction (contains/glob/matches)
// ---------------------------------------------------------------------------

// extractBloomTerms extracts required literal substrings from contains, glob,
// and matches calls on _raw, tokenizes them, and returns tokens >= 3 chars
// suitable for bloom filter lookup.
func extractBloomTerms(expr ast.Expr) []string {
	c, ok := expr.(*ast.Call)
	if !ok || len(c.Args) < 2 {
		return nil
	}

	// First arg must be _raw.
	id, ok := c.Args[0].(*ast.Ident)
	if !ok || id.Name != "_raw" {
		return nil
	}

	var literalStr string
	var isPattern bool

	switch c.Callee {
	case "contains", "contains_cs":
		lit, ok := c.Args[1].(*ast.Literal)
		if !ok || (lit.Kind != ast.LitString && lit.Kind != ast.LitRawString) {
			return nil
		}
		s, ok := lit.Value.(string)
		if !ok {
			return nil
		}
		literalStr = s
		isPattern = false

	case "glob":
		lit, ok := c.Args[1].(*ast.Literal)
		if !ok || (lit.Kind != ast.LitString && lit.Kind != ast.LitRawString) {
			return nil
		}
		s, ok := lit.Value.(string)
		if !ok {
			return nil
		}
		literalStr = s
		isPattern = true

	case "matches":
		lit, ok := c.Args[1].(*ast.Literal)
		if !ok || (lit.Kind != ast.LitString && lit.Kind != ast.LitRawString) {
			return nil
		}
		s, ok := lit.Value.(string)
		if !ok {
			return nil
		}
		literalStr = s
		isPattern = true

	default:
		return nil
	}

	var substrs []string
	if isPattern {
		switch c.Callee {
		case "glob":
			substrs = extractGlobLiterals(literalStr)
		case "matches":
			substrs = extractRegexLiterals(literalStr)
		}
	} else {
		// contains: the entire argument is a literal substring.
		substrs = []string{literalStr}
	}

	// Tokenize and filter to tokens >= 3 chars.
	var result []string
	seen := make(map[string]bool)
	for _, s := range substrs {
		tokens := tokenize(strings.ToLower(s))
		for _, tok := range tokens {
			if len(tok) >= 3 && !seen[tok] {
				seen[tok] = true
				result = append(result, tok)
			}
		}
	}
	return result
}

// extractGlobLiterals extracts the longest literal runs between glob
// metacharacters (* and ?). Returns runs that are non-empty.
func extractGlobLiterals(pattern string) []string {
	var result []string
	var current strings.Builder
	for _, r := range pattern {
		if r == '*' || r == '?' {
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		} else {
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}
	return result
}

// extractRegexLiterals extracts maximal runs of unescaped literal characters
// from a regex pattern, outside character classes, groups, and alternations.
// This is a conservative extraction: if the regex is too complex, we return
// nothing rather than risk incorrect bloom candidates.
//
// Extraction rules:
//   - Characters inside [...] are skipped (character class).
//   - The | alternation operator terminates extraction for that branch
//     (we cannot require literals from either side of an alternation).
//   - Backslash-escaped characters contribute the escaped char (e.g., \. -> .).
//   - Quantifiers (*, +, ?, {n,m}) following a literal char mean that char
//     is not required; the preceding char is removed from the current run.
//   - Anchors (^, $) are skipped.
//   - Groups (() ) are treated transparently for the literal chars inside them,
//     but their content is not guaranteed to be required (due to quantifiers
//     on the group), so we flush the current run when entering a group.
//
// This is intentionally conservative. We prefer false negatives (missing a
// bloom term) over false positives (adding a bloom term that is not required).
func extractRegexLiterals(pattern string) []string {
	var result []string
	var current []rune
	runes := []rune(pattern)
	i := 0

	flush := func() {
		if len(current) > 0 {
			result = append(result, string(current))
			current = current[:0]
		}
	}

	// skipQuantifier advances i past a quantifier if one is present.
	skipQuantifier := func() {
		if i < len(runes) && isQuantifier(runes[i]) {
			q := runes[i]
			i++
			if q == '{' {
				for i < len(runes) && runes[i] != '}' {
					i++
				}
				if i < len(runes) {
					i++ // skip }
				}
			}
			// Also skip a trailing ? for non-greedy (e.g., *? +? ??)
			if i < len(runes) && runes[i] == '?' {
				i++
			}
		}
	}

	for i < len(runes) {
		r := runes[i]

		// Character class: skip everything inside [...].
		if r == '[' {
			flush()
			i++
			// Handle negation [^...]
			if i < len(runes) && runes[i] == '^' {
				i++
			}
			for i < len(runes) {
				if runes[i] == '\\' && i+1 < len(runes) {
					i += 2
					continue
				}
				if runes[i] == ']' {
					i++
					break
				}
				i++
			}
			skipQuantifier()
			continue
		}

		// Alternation: we cannot require literals from either branch.
		if r == '|' {
			flush()
			i++
			continue
		}

		// Group open: flush (group contents may be optional due to
		// quantifiers on the group).
		if r == '(' {
			flush()
			i++
			// Skip non-capturing group modifiers like ?:, ?=, ?!, etc.
			if i < len(runes) && runes[i] == '?' {
				i++
				for i < len(runes) && runes[i] != ')' && runes[i] != ':' {
					i++
				}
				if i < len(runes) && runes[i] == ':' {
					i++
				}
			}
			continue
		}

		// Group close.
		if r == ')' {
			flush()
			i++
			skipQuantifier()
			continue
		}

		// Backslash escape.
		if r == '\\' && i+1 < len(runes) {
			next := runes[i+1]
			i += 2
			switch next {
			case 'd', 'D', 'w', 'W', 's', 'S', 'b', 'B', 'A', 'z', 'Z':
				// Shorthand character class — not a literal char. Flush
				// and skip any trailing quantifier.
				flush()
				skipQuantifier()
				continue
			default:
				// Escaped literal: \. -> ., \- -> -, etc.
				// Check if followed by a quantifier.
				if i < len(runes) && isQuantifier(runes[i]) {
					q := runes[i]
					if q == '+' {
						// + means at least one — the char is required.
						current = append(current, next)
						i++
					} else if q == '{' {
						// Cannot easily determine min count; be conservative.
						flush()
						skipQuantifier()
					} else {
						// * or ? — char is optional.
						flush()
						i++
					}
					continue
				}
				current = append(current, next)
				continue
			}
		}

		// Anchors.
		if r == '^' || r == '$' {
			flush()
			i++
			continue
		}

		// Dot: matches any char — not literal.
		if r == '.' {
			flush()
			i++
			skipQuantifier()
			continue
		}

		// Regular literal char.
		i++
		// Check if followed by a quantifier.
		if i < len(runes) && isQuantifier(runes[i]) {
			q := runes[i]
			if q == '+' {
				// + means at least one occurrence — the char IS required.
				current = append(current, r)
				i++
			} else if q == '{' {
				// Cannot easily determine if {n,m} with n>=1; be conservative.
				flush()
				skipQuantifier()
			} else {
				// * or ? — char is optional, don't include.
				flush()
				i++
			}
			continue
		}
		current = append(current, r)
	}
	flush()
	return result
}

func isQuantifier(r rune) bool {
	return r == '*' || r == '+' || r == '?' || r == '{'
}

// ---------------------------------------------------------------------------
// Tokenizer (matches §6.1 contract)
// ---------------------------------------------------------------------------

// tokenize splits s into tokens per the tokenizer contract (§6.1):
// tokens are runs of ASCII alphanumerics and Unicode letters/digits;
// everything else delimits. Input should already be lowercased.
func tokenize(s string) []string {
	var tokens []string
	var current strings.Builder
	for _, r := range s {
		if isTokenChar(r) {
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// isTokenChar returns true for characters that are part of a token:
// ASCII alphanumerics and Unicode letters/digits.
func isTokenChar(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	// Unicode letters/digits beyond ASCII.
	if r > 127 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// AST helpers
// ---------------------------------------------------------------------------

// flattenAnd collects top-level AND conjuncts from an expression tree.
func flattenAnd(expr ast.Expr) []ast.Expr {
	b, ok := expr.(*ast.Binary)
	if ok && b.Op == ast.OpAnd {
		return append(flattenAnd(b.Left), flattenAnd(b.Right)...)
	}
	return []ast.Expr{expr}
}

// rebuildAnd rebuilds an AND tree from a slice of conjuncts.
func rebuildAnd(exprs []ast.Expr) ast.Expr {
	if len(exprs) == 0 {
		return litBool(true)
	}
	result := exprs[0]
	for _, e := range exprs[1:] {
		result = &ast.Binary{Op: ast.OpAnd, Left: result, Right: e}
	}
	return result
}

func isTrueLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.Literal)
	return ok && lit.Kind == ast.LitBool && lit.Value == true
}

func isFalseLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.Literal)
	return ok && lit.Kind == ast.LitBool && lit.Value == false
}

func isNullLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.Literal)
	return ok && lit.Kind == ast.LitNull
}

// ---------------------------------------------------------------------------
// Plan node helpers
// ---------------------------------------------------------------------------

// cloneScan creates a shallow copy of a Scan node with a deep copy of the
// Pushdown struct so that modifications do not affect the original.
func cloneScan(s *logical.Scan) *logical.Scan {
	newScan := &logical.Scan{
		Sources:      s.Sources,
		TimeRange:    s.TimeRange,
		OutputSchema: s.OutputSchema,
		Pushdown: logical.Pushdown{
			FieldPredicates: cloneExprs(s.Pushdown.FieldPredicates),
			BloomTerms:      cloneStrings(s.Pushdown.BloomTerms),
			RawTerms:        cloneStrings(s.Pushdown.RawTerms),
		},
	}
	if s.Pushdown.TimeBounds != nil {
		tb := *s.Pushdown.TimeBounds
		newScan.Pushdown.TimeBounds = &tb
	}
	return newScan
}

func cloneExprs(in []ast.Expr) []ast.Expr {
	if in == nil {
		return nil
	}
	out := make([]ast.Expr, len(in))
	copy(out, in)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copySchema(in []sema.Field) []sema.Field {
	if in == nil {
		return nil
	}
	out := make([]sema.Field, len(in))
	copy(out, in)
	return out
}

// rewritePlanBottomUp walks the plan tree bottom-up and applies fn to each
// node. If fn returns a different node, the parent is rebuilt to reference the
// new child. Returns the (possibly new) root and whether any change was made.
func rewritePlanBottomUp(root logical.Node, fn func(logical.Node) (logical.Node, bool)) (logical.Node, bool) {
	if root == nil {
		return nil, false
	}
	anyChanged := false

	// Recurse into children.
	children := root.Children()
	newChildren := make([]logical.Node, len(children))
	childrenChanged := false
	for i, c := range children {
		nc, changed := rewritePlanBottomUp(c, fn)
		newChildren[i] = nc
		if changed {
			childrenChanged = true
			anyChanged = true
		}
	}

	// For Join, also recurse into Right sub-plan.
	if j, ok := root.(*logical.Join); ok && j.Right != nil {
		newRight, rightChanged := rewritePlanBottomUp(j.Right, fn)
		if rightChanged {
			anyChanged = true
			// Clone the join with new right.
			newJoin := *j
			newJoin.Right = newRight
			if childrenChanged {
				newJoin.SetChildren(newChildren)
			}
			result, changed := fn(&newJoin)
			if changed {
				anyChanged = true
			}
			return result, anyChanged
		}
	}

	current := root
	if childrenChanged {
		// Rebuild the current node with new children.
		current = cloneNode(root)
		current.SetChildren(newChildren)
		// For Join, preserve the Right sub-plan.
		if j, ok := root.(*logical.Join); ok {
			if newJ, ok := current.(*logical.Join); ok {
				newJ.Right = j.Right
			}
		}
	}

	// Apply fn to the current node.
	result, changed := fn(current)
	if changed {
		anyChanged = true
	}
	return result, anyChanged
}

// cloneNode creates a shallow clone of a logical.Node so that SetChildren
// does not mutate the original.
func cloneNode(n logical.Node) logical.Node {
	switch x := n.(type) {
	case *logical.Filter:
		c := *x
		return &c
	case *logical.Scan:
		c := *x
		return &c
	case *logical.Empty:
		c := *x
		return &c
	case *logical.Extend:
		c := *x
		return &c
	case *logical.Parse:
		c := *x
		return &c
	case *logical.Project:
		c := *x
		return &c
	case *logical.Aggregate:
		c := *x
		return &c
	case *logical.TopK:
		c := *x
		return &c
	case *logical.Sort:
		c := *x
		return &c
	case *logical.Limit:
		c := *x
		return &c
	case *logical.Dedup:
		c := *x
		return &c
	case *logical.Join:
		c := *x
		return &c
	case *logical.Union:
		c := *x
		return &c
	case *logical.Explode:
		c := *x
		return &c
	case *logical.Describe:
		c := *x
		return &c
	case *logical.Helper:
		c := *x
		return &c
	case *logical.Materialize:
		c := *x
		return &c
	case *logical.Tee:
		c := *x
		return &c
	default:
		// Unknown node type — return as-is. This should not happen in
		// practice but avoids a panic.
		return n
	}
}
