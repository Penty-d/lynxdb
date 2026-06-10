// Package lint implements registry-based advisory lints for LynxFlow v2
// queries (RFC-002 Phase 3c). Lints are run over the desugared AST and
// produce advisory diagnostics. They never mutate the AST and never prevent
// query execution.
//
// Each rule is identified by a stable code (LF01, LF02, ...) and a reason
// tag classifying the advisory: "slow", "broad", "canon", or "data-quality".
// The primary entry points are [Rules] (returns the rule registry) and [Run]
// (runs all rules on a query, returning any lints found).
package lint

import (
	"strings"
	"unicode"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// Lint is an advisory diagnostic produced by a lint rule.
type Lint struct {
	Code       string   // stable code, e.g. "LF01"
	Message    string   // human-readable description
	Reason     string   // stable category: "slow" | "broad" | "canon" | "data-quality"
	Span       ast.Span // source span of the offending construct
	Suggestion string   // actionable fix-it
}

// Rule is a single lint rule in the registry.
type Rule struct {
	Code  string                    // stable code, e.g. "LF01"
	Doc   string                    // human-readable description of what the rule detects
	Check func(q *ast.Query) []Lint // the rule body; must be pure (no mutation)
}

// rules is the static set of lint rules, initialized by init-free construction.
var rules = []Rule{
	{Code: "LF01", Doc: "Leading wildcard in glob/regex pattern", Check: checkLeadingWildcard},
	{Code: "LF02", Doc: "from * without time range or _time predicate (broad scope)", Check: checkBroadScopeStar},
	{Code: "LF03", Doc: "Unbounded time range (no bracket range or _time predicate)", Check: checkUnboundedTimeRange},
	{Code: "LF04", Doc: "Regex without literal anchor (cannot prefilter efficiently)", Check: checkRegexNoLiteral},
	{Code: "LF05", Doc: "Materialize without strict parse on_error", Check: checkMaterializeStrictParse},
	{Code: "LF06", Doc: "head before sort (arbitrary rows then sorted)", Check: checkHeadBeforeSort},
	// LF07: dedup after sort by other keys — placeholder, not implemented.
	{Code: "LF08", Doc: "has() with uppercase term (has is always case-insensitive)", Check: checkHasUppercase},
}

// Rules returns a copy of the lint rule registry.
func Rules() []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	return out
}

// Run executes all lint rules on a desugared query and returns any lints.
func Run(q *ast.Query) []Lint {
	if q == nil {
		return nil
	}
	var all []Lint
	for _, r := range rules {
		all = append(all, r.Check(q)...)
	}
	return all
}

// ---------------------------------------------------------------------------
// LF01: leading wildcard in glob/regex pattern
// ---------------------------------------------------------------------------

func checkLeadingWildcard(q *ast.Query) []Lint {
	var lints []Lint

	// Walk all expressions in all stages (including CTEs).
	forEachExpr(q, func(e ast.Expr) {
		call, ok := e.(*ast.Call)
		if !ok {
			return
		}

		switch call.Callee {
		case "glob":
			// glob(field, "pattern") — check second arg for leading wildcard.
			if len(call.Args) >= 2 {
				if pat, ok := stringLitValue(call.Args[1]); ok {
					if hasLeadingGlobWildcard(pat) {
						lints = append(lints, Lint{
							Code:       "LF01",
							Message:    "leading wildcard in glob pattern: " + pat,
							Reason:     "slow",
							Span:       call.Args[1].ExprSpan(),
							Suggestion: "anchor the pattern with a literal prefix",
						})
					}
				}
			}
		case "matches":
			// matches(field, r"pattern") — check second arg for leading .* or .+.
			if len(call.Args) >= 2 {
				if pat, ok := stringLitValue(call.Args[1]); ok {
					if hasLeadingRegexWildcard(pat) {
						lints = append(lints, Lint{
							Code:       "LF01",
							Message:    "leading wildcard in regex pattern: " + pat,
							Reason:     "slow",
							Span:       call.Args[1].ExprSpan(),
							Suggestion: "anchor the pattern with a literal prefix",
						})
					}
				}
			}
		}
	})

	// Also check search sugar SearchGlobValue patterns (pre-desugar residuals
	// should not exist on desugared AST, but the from stage may still carry
	// SearchKeyValue with SearchGlobValue).
	// After desugaring, glob patterns are in glob() calls — handled above.

	return lints
}

// hasLeadingGlobWildcard returns true if the pattern starts with * or ?.
func hasLeadingGlobWildcard(pat string) bool {
	if len(pat) == 0 {
		return false
	}
	return pat[0] == '*' || pat[0] == '?'
}

// hasLeadingRegexWildcard returns true if the pattern starts with .* or .+.
func hasLeadingRegexWildcard(pat string) bool {
	if len(pat) < 2 {
		return false
	}
	if pat[0] == '.' && (pat[1] == '*' || pat[1] == '+') {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// LF02: from * with no time range and no _time predicate (broad scope)
// ---------------------------------------------------------------------------

func checkBroadScopeStar(q *ast.Query) []Lint {
	var lints []Lint

	// Check main pipeline.
	if l := checkBroadScopeStarPipeline(q.Pipeline); l != nil {
		lints = append(lints, *l)
	}

	// Check CTEs.
	for _, let := range q.Lets {
		if l := checkBroadScopeStarPipeline(let.Pipeline); l != nil {
			lints = append(lints, *l)
		}
	}

	return lints
}

func checkBroadScopeStarPipeline(p ast.Pipeline) *Lint {
	if p.Source == nil {
		return nil
	}

	if !hasSourceStar(p.Source) {
		return nil
	}

	// Has bracket time range?
	if len(p.Source.TimeRanges) > 0 {
		return nil
	}

	// Has a where stage referencing _time?
	if hasTimeWhereStage(p.Stages) {
		return nil
	}

	return &Lint{
		Code:       "LF02",
		Message:    "from * without time bound scans all sources and all time",
		Reason:     "broad",
		Span:       p.Source.Pos,
		Suggestion: "add a time range like from *[-1h] or a | where _time > ... predicate",
	}
}

// ---------------------------------------------------------------------------
// LF03: unbounded time range (non-star source, no bracket range or _time
// predicate). Suppressed when LF02 fires on the same pipeline.
// ---------------------------------------------------------------------------

func checkUnboundedTimeRange(q *ast.Query) []Lint {
	var lints []Lint

	if l := checkUnboundedPipeline(q.Pipeline); l != nil {
		lints = append(lints, *l)
	}
	for _, let := range q.Lets {
		if l := checkUnboundedPipeline(let.Pipeline); l != nil {
			lints = append(lints, *l)
		}
	}

	return lints
}

func checkUnboundedPipeline(p ast.Pipeline) *Lint {
	if p.Source == nil {
		return nil
	}

	// Suppress for star sources (LF02 handles those).
	if hasSourceStar(p.Source) {
		return nil
	}

	// Has bracket time range?
	if len(p.Source.TimeRanges) > 0 {
		return nil
	}

	// Has a where stage referencing _time?
	if hasTimeWhereStage(p.Stages) {
		return nil
	}

	return &Lint{
		Code:       "LF03",
		Message:    "query has no time bound (no bracket range or _time predicate)",
		Reason:     "broad",
		Span:       p.Source.Pos,
		Suggestion: "add a time range like from source[-1h] or a | where _time > ... predicate",
	}
}

// ---------------------------------------------------------------------------
// LF04: regex without literal anchor
// ---------------------------------------------------------------------------

func checkRegexNoLiteral(q *ast.Query) []Lint {
	var lints []Lint

	forEachExpr(q, func(e ast.Expr) {
		call, ok := e.(*ast.Call)
		if !ok {
			return
		}

		switch call.Callee {
		case "matches", "extract", "extract_all":
			// The pattern is the second argument (first is the field).
			if len(call.Args) < 2 {
				return
			}
			pat, ok := stringLitValue(call.Args[1])
			if !ok {
				return
			}
			if !hasLiteralAnchor(pat, 3) {
				lints = append(lints, Lint{
					Code:       "LF04",
					Message:    "regex pattern has no literal anchor of >= 3 characters",
					Reason:     "slow",
					Span:       call.Args[1].ExprSpan(),
					Suggestion: "include a literal substring of >= 3 characters so the index can prefilter",
				})
			}
		}
	})

	return lints
}

// hasLiteralAnchor returns true if the regex pattern contains a run of
// consecutive non-metacharacter characters of at least minLen.
func hasLiteralAnchor(pattern string, minLen int) bool {
	// Regex metacharacters (RE2/PCRE common set).
	const metaChars = `\.+*?^$|(){}` + "\\"
	run := 0
	i := 0
	for i < len(pattern) {
		ch := pattern[i]

		// Skip character classes: [...]
		if ch == '[' {
			run = 0
			// Advance past the closing ].
			i++
			if i < len(pattern) && pattern[i] == '^' {
				i++ // negated class
			}
			if i < len(pattern) && pattern[i] == ']' {
				i++ // literal ] at start of class
			}
			for i < len(pattern) && pattern[i] != ']' {
				if pattern[i] == '\\' && i+1 < len(pattern) {
					i++ // skip escaped char inside class
				}
				i++
			}
			if i < len(pattern) {
				i++ // skip closing ]
			}
			continue
		}

		if ch == '\\' && i+1 < len(pattern) {
			// Escaped character: the ESCAPED char is literal only if it is
			// not a metaclass (\d, \w, \s, etc.). We count \\ as literal
			// backslash, and literal-escaped chars like \/ as literal.
			next := pattern[i+1]
			if isRegexMetaclass(next) {
				run = 0
			} else {
				run++
			}
			i += 2
			if run >= minLen {
				return true
			}
			continue
		}
		if strings.ContainsRune(metaChars, rune(ch)) {
			run = 0
		} else {
			run++
		}
		if run >= minLen {
			return true
		}
		i++
	}
	return false
}

// isRegexMetaclass returns true for characters that, when preceded by \,
// represent a character class rather than a literal character (e.g. \d, \w).
func isRegexMetaclass(ch byte) bool {
	switch ch {
	case 'd', 'D', 'w', 'W', 's', 'S', 'b', 'B', 'A', 'z', 'Z', 'p', 'P':
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// LF05: materialize without strict parse on_error
// ---------------------------------------------------------------------------

func checkMaterializeStrictParse(q *ast.Query) []Lint {
	var lints []Lint

	// Check main pipeline.
	if l := checkMaterializePipeline(q.Pipeline); l != nil {
		lints = append(lints, *l)
	}
	for _, let := range q.Lets {
		if l := checkMaterializePipeline(let.Pipeline); l != nil {
			lints = append(lints, *l)
		}
	}

	return lints
}

func checkMaterializePipeline(p ast.Pipeline) *Lint {
	// Check if pipeline ends with materialize.
	hasMaterialize := false
	var matSpan ast.Span
	for _, s := range p.Stages {
		if s.Name == "materialize" {
			hasMaterialize = true
			matSpan = s.Pos
		}
	}
	if !hasMaterialize {
		return nil
	}

	// Check if any parse stage has on_error != "strict".
	for _, s := range p.Stages {
		if s.Name == "parse" && s.Parse != nil {
			if s.Parse.OnError != "strict" {
				return &Lint{
					Code:       "LF05",
					Message:    "materialize pipeline contains parse without on_error strict",
					Reason:     "data-quality",
					Span:       matSpan,
					Suggestion: "add on_error strict to the parse stage for materialized views",
				}
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// LF06: head before sort (arbitrary then sorted)
// ---------------------------------------------------------------------------

func checkHeadBeforeSort(q *ast.Query) []Lint {
	var lints []Lint

	// Check main pipeline.
	lints = append(lints, checkHeadBeforeSortStages(q.Pipeline.Stages)...)
	for _, let := range q.Lets {
		lints = append(lints, checkHeadBeforeSortStages(let.Pipeline.Stages)...)
	}

	return lints
}

func checkHeadBeforeSortStages(stages []ast.Stage) []Lint {
	var lints []Lint

	for i := 0; i < len(stages); i++ {
		if stages[i].Name != "head" {
			continue
		}
		headSpan := stages[i].Pos

		// Look for a sort stage after head with no stats-like stage between.
		for j := i + 1; j < len(stages); j++ {
			name := stages[j].Name
			if isStatsLikeStage(name) {
				break // stats-like stage intervenes; stop looking.
			}
			if name == "sort" {
				lints = append(lints, Lint{
					Code:       "LF06",
					Message:    "head before sort: truncates to arbitrary rows then sorts; did you mean sort | head?",
					Reason:     "canon",
					Span:       headSpan,
					Suggestion: "place sort before head: | sort ... | head N",
				})
				break
			}
		}
	}

	return lints
}

// isStatsLikeStage returns true for stages that produce aggregate output
// (meaning a subsequent sort would operate on a different row set).
func isStatsLikeStage(name string) bool {
	switch name {
	case "stats", "eventstats", "streamstats":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// LF08: has() with uppercase term
// ---------------------------------------------------------------------------

func checkHasUppercase(q *ast.Query) []Lint {
	var lints []Lint

	forEachExpr(q, func(e ast.Expr) {
		call, ok := e.(*ast.Call)
		if !ok || call.Callee != "has" {
			return
		}
		if len(call.Args) < 2 {
			return
		}
		term, ok := stringLitValue(call.Args[1])
		if !ok {
			return
		}
		if containsUpper(term) {
			lints = append(lints, Lint{
				Code:       "LF08",
				Message:    "has() is always case-insensitive; uppercase letters in \"" + term + "\" will still match lowercase",
				Reason:     "canon",
				Span:       call.Args[1].ExprSpan(),
				Suggestion: "has() matches case-insensitively; this is informational for Splunk migrators",
			})
		}
	})

	return lints
}

// containsUpper returns true if s contains any uppercase letter.
func containsUpper(s string) bool {
	for _, r := range s {
		if unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// AST walking helpers
// ---------------------------------------------------------------------------

// forEachExpr calls fn for every expression in the query's stages (including
// CTE pipelines). It walks where predicates, extend values, stats aggs/by
// keys, sort keys, dedup fields, join/union sub-pipelines, and parse format
// args.
func forEachExpr(q *ast.Query, fn func(ast.Expr)) {
	forEachExprPipeline(q.Pipeline, fn)
	for _, let := range q.Lets {
		forEachExprPipeline(let.Pipeline, fn)
	}
}

func forEachExprPipeline(p ast.Pipeline, fn func(ast.Expr)) {
	for _, s := range p.Stages {
		forEachExprStage(s, fn)
	}
}

func forEachExprStage(s ast.Stage, fn func(ast.Expr)) {
	switch {
	case s.Where != nil && s.Where.Expr != nil:
		walkExprDeep(s.Where.Expr, fn)
	case s.Extend != nil:
		for _, a := range s.Extend.Assignments {
			walkExprDeep(a.Value, fn)
		}
	case s.Stats != nil:
		forEachExprStats(s.Stats, fn)
	case s.Eventstats != nil:
		forEachExprStats(s.Eventstats, fn)
	case s.Streamstats != nil:
		forEachExprStats(&s.Streamstats.StatsPayload, fn)
	case s.Sort != nil:
		for _, k := range s.Sort.Keys {
			walkExprDeep(k.Field, fn)
		}
	case s.Dedup != nil:
		for _, f := range s.Dedup.Fields {
			walkExprDeep(f, fn)
		}
	case s.Parse != nil:
		for _, a := range s.Parse.FormatArgs {
			walkExprDeep(a, fn)
		}
		if s.Parse.From != nil {
			walkExprDeep(s.Parse.From, fn)
		}
	case s.Join != nil:
		for _, f := range s.Join.On {
			walkExprDeep(f, fn)
		}
		if s.Join.Right != nil && s.Join.Right.Pipeline != nil {
			forEachExprPipeline(*s.Join.Right.Pipeline, fn)
		}
	case s.Union != nil:
		for _, src := range s.Union.Sources {
			if src.Pipeline != nil {
				forEachExprPipeline(*src.Pipeline, fn)
			}
		}
	case s.Explode != nil:
		if s.Explode.Array != nil {
			walkExprDeep(s.Explode.Array, fn)
		}
	case s.Transaction != nil:
		for _, f := range s.Transaction.Fields {
			walkExprDeep(f, fn)
		}
		if s.Transaction.MaxSpan != nil {
			walkExprDeep(s.Transaction.MaxSpan, fn)
		}
		if s.Transaction.StartsWith != nil {
			walkExprDeep(s.Transaction.StartsWith, fn)
		}
		if s.Transaction.EndsWith != nil {
			walkExprDeep(s.Transaction.EndsWith, fn)
		}
	case s.Correlate != nil:
		if s.Correlate.Field1 != nil {
			walkExprDeep(s.Correlate.Field1, fn)
		}
		if s.Correlate.Field2 != nil {
			walkExprDeep(s.Correlate.Field2, fn)
		}
	}
}

func forEachExprStats(sp *ast.StatsPayload, fn func(ast.Expr)) {
	for _, agg := range sp.Aggs {
		walkExprDeep(agg.Func, fn)
		if agg.WhereCond != nil {
			walkExprDeep(agg.WhereCond, fn)
		}
	}
	for _, by := range sp.By {
		walkExprDeep(by, fn)
	}
}

// walkExprDeep calls fn for e and every sub-expression using ast.Inspect.
func walkExprDeep(e ast.Expr, fn func(ast.Expr)) {
	ast.Inspect(e, fn)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// hasSourceStar returns true if the from stage contains a SourceStar atom.
func hasSourceStar(f *ast.FromStage) bool {
	for _, s := range f.Sources {
		if s.Kind == ast.SourceStar {
			return true
		}
	}
	return false
}

// hasTimeWhereStage returns true if any where stage in stages references
// _time in its predicate expression.
func hasTimeWhereStage(stages []ast.Stage) bool {
	for _, s := range stages {
		if s.Name == "where" && s.Where != nil && s.Where.Expr != nil {
			if exprReferencesField(s.Where.Expr, "_time") {
				return true
			}
		}
	}
	return false
}

// exprReferencesField returns true if the expression references the given
// field name (via Ident nodes).
func exprReferencesField(e ast.Expr, field string) bool {
	found := false
	ast.Walk(e, func(n ast.Expr) bool {
		if found {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == field {
			found = true
			return false
		}
		return true
	})
	return found
}

// stringLitValue returns the string value of a string or raw-string literal.
func stringLitValue(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.Literal)
	if !ok {
		return "", false
	}
	switch lit.Kind {
	case ast.LitString, ast.LitRawString:
		if s, ok := lit.Value.(string); ok {
			return s, true
		}
	}
	return "", false
}
