// Package format provides canonical formatting for the LynxFlow v2 AST.
//
// The formatter produces valid LynxFlow text from a parsed AST with these
// guarantees:
//   - Lowercase keywords, single spaces, | stage separators.
//   - Multi-stage pipelines: each stage on its own line prefixed with "| ".
//   - Single-stage pipelines: one line.
//   - Minimal parentheses: re-derived from operator precedence.
//   - Literal preservation: number/duration Raw text kept where lossless;
//     strings re-quoted with Go escaping; raw strings preserved as r"...".
//   - Backtick identifiers preserved.
//   - let-CTEs each on their own line ending ";".
//
// Fixpoint contract: format(parse(format(parse(q)))) == format(parse(q)).
package format

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// Query formats a complete Query AST into canonical LynxFlow text.
func Query(q *ast.Query) string {
	var b strings.Builder
	for _, l := range q.Lets {
		formatLet(&b, &l)
	}
	formatPipeline(&b, &q.Pipeline, len(q.Lets) > 0)
	return b.String()
}

// Expr formats a single expression into canonical LynxFlow text.
func Expr(e ast.Expr) string {
	var b strings.Builder
	formatExpr(&b, e, precTop)
	return b.String()
}

// ---------------------------------------------------------------------------
// Precedence levels (higher = tighter binding)
// ---------------------------------------------------------------------------

type prec int

const (
	precTop   prec = iota // top-level: no parens needed
	precOr                // or
	precAnd               // and
	precNot               // not (unary)
	precCmp               // ==, !=, <, <=, >, >=, in, between
	precCoal              // ??
	precAdd               // +, -
	precMul               // *, /, %
	precUnary             // - (unary)
	precAtom              // literals, idents, calls, postfix
)

// opPrec returns the precedence of a binary operator.
func opPrec(op ast.BinaryOp) prec {
	switch op {
	case ast.OpOr:
		return precOr
	case ast.OpAnd:
		return precAnd
	case ast.OpEq, ast.OpNotEq, ast.OpLt, ast.OpLtEq, ast.OpGt, ast.OpGtEq:
		return precCmp
	case ast.OpCoalesce:
		return precCoal
	case ast.OpAdd, ast.OpSub:
		return precAdd
	case ast.OpMul, ast.OpDiv, ast.OpMod:
		return precMul
	}
	return precTop
}

// isLeftAssoc reports whether a binary operator is left-associative.
// Comparisons are non-associative; all others are left-associative.
func isLeftAssoc(op ast.BinaryOp) bool {
	switch op {
	case ast.OpEq, ast.OpNotEq, ast.OpLt, ast.OpLtEq, ast.OpGt, ast.OpGtEq:
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Expression formatting
// ---------------------------------------------------------------------------

// formatExpr writes the expression e into b, inserting parentheses only when
// the surrounding context (parentPrec) demands it.
func formatExpr(b *strings.Builder, e ast.Expr, parentPrec prec) {
	if e == nil {
		b.WriteString("<nil>")
		return
	}
	switch n := e.(type) {
	case *ast.Ident:
		if n.Quoted {
			b.WriteByte('`')
			b.WriteString(n.Name)
			b.WriteByte('`')
		} else {
			b.WriteString(n.Name)
		}

	case *ast.Literal:
		formatLiteral(b, n)

	case *ast.Array:
		b.WriteByte('[')
		for i, el := range n.Elems {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, el, precTop)
		}
		b.WriteByte(']')

	case *ast.Object:
		b.WriteByte('{')
		for i, ent := range n.Entries {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(ent.Key)
			b.WriteString(": ")
			formatExpr(b, ent.Value, precTop)
		}
		b.WriteByte('}')

	case *ast.Binary:
		myPrec := opPrec(n.Op)
		needParens := myPrec < parentPrec
		if needParens {
			b.WriteByte('(')
		}
		// Left child: needs parens if its precedence is lower, OR if it is
		// equal and the operator is right-associative or non-associative.
		leftPrec := myPrec
		if !isLeftAssoc(n.Op) {
			leftPrec = myPrec + 1
		}
		formatExpr(b, n.Left, leftPrec)
		b.WriteByte(' ')
		b.WriteString(binaryOpString(n.Op))
		b.WriteByte(' ')
		// Right child: needs parens if its precedence is lower, OR if it is
		// equal and the operator is left-associative.
		rightPrec := myPrec
		if isLeftAssoc(n.Op) {
			rightPrec = myPrec + 1
		}
		formatExpr(b, n.Right, rightPrec)
		if needParens {
			b.WriteByte(')')
		}

	case *ast.Unary:
		switch n.Op {
		case ast.OpNot:
			needParens := precNot < parentPrec
			if needParens {
				b.WriteByte('(')
			}
			b.WriteString("not ")
			formatExpr(b, n.Operand, precNot)
			if needParens {
				b.WriteByte(')')
			}
		case ast.OpNeg:
			needParens := precUnary < parentPrec
			if needParens {
				b.WriteByte('(')
			}
			b.WriteByte('-')
			formatExpr(b, n.Operand, precUnary)
			if needParens {
				b.WriteByte(')')
			}
		}

	case *ast.In:
		needParens := precCmp < parentPrec
		if needParens {
			b.WriteByte('(')
		}
		formatExpr(b, n.LHS, precCmp+1)
		b.WriteString(" in ")
		formatExpr(b, n.RHS, precCmp+1)
		if needParens {
			b.WriteByte(')')
		}

	case *ast.Between:
		needParens := precCmp < parentPrec
		if needParens {
			b.WriteByte('(')
		}
		formatExpr(b, n.X, precCmp+1)
		b.WriteString(" between ")
		formatExpr(b, n.Lo, precCmp+1)
		b.WriteString(" and ")
		formatExpr(b, n.Hi, precCmp+1)
		if needParens {
			b.WriteByte(')')
		}

	case *ast.Call:
		if n.Receiver != nil {
			formatExpr(b, n.Receiver, precAtom)
			if n.SafeNav {
				b.WriteString("?.")
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteString(strings.ToLower(n.Callee))
		if n.Bang {
			b.WriteByte('!')
		}
		b.WriteByte('(')
		for i, a := range n.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, a, precTop)
		}
		b.WriteByte(')')

	case *ast.Member:
		formatExpr(b, n.Object, precAtom)
		b.WriteByte('.')
		b.WriteString(n.Field)

	case *ast.SafeMember:
		formatExpr(b, n.Object, precAtom)
		b.WriteString("?.")
		b.WriteString(n.Field)

	case *ast.Index:
		formatExpr(b, n.Object, precAtom)
		b.WriteByte('[')
		formatExpr(b, n.Idx, precTop)
		b.WriteByte(']')

	case *ast.Lambda:
		needParens := parentPrec > precTop
		if needParens {
			b.WriteByte('(')
		}
		b.WriteString(n.Param)
		b.WriteString(" -> ")
		formatExpr(b, n.Body, precTop)
		if needParens {
			b.WriteByte(')')
		}

	case *ast.Paren:
		// Drop redundant parens — the formatter is canonical.
		formatExpr(b, n.Inner, parentPrec)

	case *ast.ErrorExpr:
		b.WriteString("<error>")

	case *ast.SearchGlobValue:
		b.WriteString(n.Pattern)

	default:
		b.WriteString("<unknown>")
	}
}

func binaryOpString(op ast.BinaryOp) string {
	switch op {
	case ast.OpOr:
		return "or"
	case ast.OpAnd:
		return "and"
	case ast.OpEq:
		return "=="
	case ast.OpNotEq:
		return "!="
	case ast.OpLt:
		return "<"
	case ast.OpLtEq:
		return "<="
	case ast.OpGt:
		return ">"
	case ast.OpGtEq:
		return ">="
	case ast.OpAdd:
		return "+"
	case ast.OpSub:
		return "-"
	case ast.OpMul:
		return "*"
	case ast.OpDiv:
		return "/"
	case ast.OpMod:
		return "%"
	case ast.OpCoalesce:
		return "??"
	}
	return "?"
}

// formatLiteral writes a literal in canonical form.
func formatLiteral(b *strings.Builder, n *ast.Literal) {
	switch n.Kind {
	case ast.LitString:
		// Re-quote with Go escaping for canonical form.
		s, ok := n.Value.(string)
		if !ok {
			b.WriteString(n.Raw)
			return
		}
		b.WriteString(quoteString(s))
	case ast.LitRawString:
		// Preserve r"..." form.
		b.WriteString(n.Raw)
	case ast.LitInt:
		// Preserve user's raw spelling (e.g. 0x2A stays as-is).
		b.WriteString(n.Raw)
	case ast.LitFloat:
		// Preserve user's raw spelling.
		b.WriteString(n.Raw)
	case ast.LitBool:
		if v, ok := n.Value.(bool); ok {
			if v {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		} else {
			b.WriteString(n.Raw)
		}
	case ast.LitNull:
		b.WriteString("null")
	case ast.LitDuration:
		// Preserve user's raw spelling (e.g. 30s, 1.5h).
		b.WriteString(n.Raw)
	default:
		b.WriteString(n.Raw)
	}
}

// quoteString produces a double-quoted string with standard LynxFlow escaping.
func quoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u{%04X}`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------------------
// Let formatting
// ---------------------------------------------------------------------------

func formatLet(b *strings.Builder, l *ast.Let) {
	b.WriteString("let $")
	b.WriteString(l.Name)
	b.WriteString(" = ")
	formatPipelineInline(b, &l.Pipeline)
	b.WriteString(";\n")
}

// ---------------------------------------------------------------------------
// Pipeline formatting
// ---------------------------------------------------------------------------

func formatPipeline(b *strings.Builder, p *ast.Pipeline, afterLets bool) {
	totalStages := len(p.Stages)
	hasSource := p.Source != nil
	// Count total parts that produce lines.
	totalParts := totalStages
	if hasSource {
		totalParts++
	}
	multiLine := totalParts > 1

	if hasSource {
		formatFromStage(b, p.Source)
		if totalStages > 0 {
			b.WriteByte('\n')
		}
	}

	for i, s := range p.Stages {
		if multiLine {
			b.WriteString("| ")
		}
		formatStage(b, &s)
		if i < totalStages-1 {
			b.WriteByte('\n')
		}
	}
}

// formatPipelineInline formats a pipeline on a single line (for let bodies and sub-pipelines).
func formatPipelineInline(b *strings.Builder, p *ast.Pipeline) {
	if p.Source != nil {
		formatFromStage(b, p.Source)
	}
	for i, s := range p.Stages {
		if p.Source != nil || i > 0 {
			b.WriteString(" | ")
		}
		formatStage(b, &s)
	}
}

// ---------------------------------------------------------------------------
// From stage
// ---------------------------------------------------------------------------

func formatFromStage(b *strings.Builder, f *ast.FromStage) {
	b.WriteString("from")
	if len(f.Sources) > 0 || len(f.TimeRanges) > 0 || f.SugarTerms != nil {
		b.WriteByte(' ')
	}
	for i, src := range f.Sources {
		if i > 0 {
			b.WriteString(", ")
		}
		formatSourceAtom(b, &src)
	}
	for _, tr := range f.TimeRanges {
		formatTimeRange(b, &tr)
	}
	if f.SugarTerms != nil {
		b.WriteByte(' ')
		formatSearchExpr(b, f.SugarTerms)
	}
}

func formatSourceAtom(b *strings.Builder, s *ast.SourceAtom) {
	switch s.Kind {
	case ast.SourceStar:
		b.WriteByte('*')
	case ast.SourceCTE:
		b.WriteByte('$')
		b.WriteString(s.Name)
	case ast.SourceNegated:
		b.WriteByte('!')
		if s.Pattern != "" {
			b.WriteString(s.Pattern)
		} else if needsQuoting(s.Name) {
			b.WriteByte('`')
			b.WriteString(s.Name)
			b.WriteByte('`')
		} else {
			b.WriteString(s.Name)
		}
	case ast.SourceGlob:
		b.WriteString(s.Pattern)
	default:
		if s.Quoted || needsQuoting(s.Name) {
			b.WriteByte('`')
			b.WriteString(s.Name)
			b.WriteByte('`')
		} else {
			b.WriteString(s.Name)
		}
	}
}

// needsQuoting reports whether a source name needs backtick quoting to
// survive a round-trip through format-parse. Allowlist, not blocklist: only
// names the parser reassembles as bare tokens (ident start, then ident chars
// or dashes) may render unquoted; everything else gets backticks.
func needsQuoting(name string) bool {
	if name == "" {
		return true
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case i > 0 && (c >= '0' && c <= '9' || c == '-'):
		default:
			return true
		}
	}
	return false
}

func formatTimeRange(b *strings.Builder, tr *ast.TimeRange) {
	b.WriteByte('[')
	if tr.Start != nil {
		formatExpr(b, tr.Start, precTop)
	}
	if tr.End != nil {
		b.WriteString("..")
		formatExpr(b, tr.End, precTop)
	}
	b.WriteByte(']')
	if tr.Snap != "" {
		b.WriteByte('[')
		b.WriteString(tr.Snap)
		b.WriteByte(']')
	}
}

// ---------------------------------------------------------------------------
// Search sugar formatting
// ---------------------------------------------------------------------------

func formatSearchExpr(b *strings.Builder, se ast.SearchExpr) {
	if se == nil {
		return
	}
	formatSearchOr(b, se)
}

func formatSearchOr(b *strings.Builder, se ast.SearchExpr) {
	switch s := se.(type) {
	case *ast.SearchBinary:
		if s.Op == "or" {
			formatSearchOr(b, s.Left)
			b.WriteString(" or ")
			formatSearchAnd(b, s.Right)
			return
		}
		formatSearchAnd(b, se)
	default:
		formatSearchAnd(b, se)
	}
}

func formatSearchAnd(b *strings.Builder, se ast.SearchExpr) {
	switch s := se.(type) {
	case *ast.SearchBinary:
		if s.Op == "and" {
			formatSearchAnd(b, s.Left)
			b.WriteString(" and ")
			formatSearchNot(b, s.Right)
			return
		}
		if s.Op == "or" {
			b.WriteByte('(')
			formatSearchOr(b, se)
			b.WriteByte(')')
			return
		}
		formatSearchNot(b, se)
	default:
		formatSearchNot(b, se)
	}
}

func formatSearchNot(b *strings.Builder, se ast.SearchExpr) {
	switch s := se.(type) {
	case *ast.SearchNot:
		b.WriteString("not ")
		formatSearchNot(b, s.Operand)
	default:
		formatSearchPrimary(b, se)
	}
}

func formatSearchPrimary(b *strings.Builder, se ast.SearchExpr) {
	switch s := se.(type) {
	case *ast.SearchBareWord:
		b.WriteString(s.Word)
	case *ast.SearchPhrase:
		b.WriteString(s.Raw)
	case *ast.SearchKeyValue:
		writeFieldName(b, s.Key)
		b.WriteString(s.Op)
		if s.Value != nil {
			formatExpr(b, s.Value, precTop)
		}
	case *ast.SearchIn:
		writeFieldName(b, s.Key)
		b.WriteString(" in (")
		for i, v := range s.Values {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, v, precTop)
		}
		b.WriteByte(')')
	case *ast.SearchBinary:
		// Must wrap in parens since we are at primary level.
		b.WriteByte('(')
		formatSearchOr(b, s)
		b.WriteByte(')')
	case *ast.SearchNot:
		b.WriteString("not ")
		formatSearchNot(b, s.Operand)
	case *ast.SearchParen:
		b.WriteByte('(')
		formatSearchOr(b, s.Inner)
		b.WriteByte(')')
	}
}

// ---------------------------------------------------------------------------
// Stage formatting
// ---------------------------------------------------------------------------

func formatStage(b *strings.Builder, s *ast.Stage) {
	b.WriteString(strings.ToLower(s.Name))
	switch {
	case s.Where != nil:
		b.WriteByte(' ')
		formatExpr(b, s.Where.Expr, precTop)
	case s.Extend != nil:
		b.WriteByte(' ')
		formatAssignPayload(b, s.Extend)
	case s.Rename != nil:
		b.WriteByte(' ')
		formatRenamePayload(b, s.Rename)
	case s.Stats != nil:
		b.WriteByte(' ')
		formatStatsPayload(b, s.Stats)
	case s.Eventstats != nil:
		b.WriteByte(' ')
		formatStatsPayload(b, s.Eventstats)
	case s.Streamstats != nil:
		b.WriteByte(' ')
		formatStreamstatsPayload(b, s.Streamstats)
	case s.Sort != nil:
		b.WriteByte(' ')
		formatSortPayload(b, s.Sort)
	case s.Head != nil:
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(s.Head.N, 10))
	case s.Tail != nil:
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(s.Tail.N, 10))
	case s.Dedup != nil:
		b.WriteByte(' ')
		formatDedupPayload(b, s.Dedup)
	case s.Keep != nil:
		b.WriteByte(' ')
		formatFieldPatternsPayload(b, s.Keep)
	case s.Drop != nil:
		b.WriteByte(' ')
		formatFieldPatternsPayload(b, s.Drop)
	case s.Join != nil:
		b.WriteByte(' ')
		formatJoinPayload(b, s.Join)
	case s.Union != nil:
		b.WriteByte(' ')
		formatUnionPayload(b, s.Union)
	case s.Explode != nil:
		b.WriteByte(' ')
		formatExplodePayload(b, s.Explode)
	case s.Describe != nil:
		// no payload
	case s.Parse != nil:
		b.WriteByte(' ')
		formatParsePayload(b, s.Parse)
	case s.Top != nil:
		b.WriteByte(' ')
		formatTopRarePayload(b, s.Top)
	case s.Rare != nil:
		b.WriteByte(' ')
		formatTopRarePayload(b, s.Rare)
	case s.Every != nil:
		b.WriteByte(' ')
		formatEveryPayload(b, s.Every)
	case s.Rate != nil:
		b.WriteByte(' ')
		formatRatePayload(b, s.Rate)
	case s.Latency != nil:
		b.WriteByte(' ')
		formatLatencyPayload(b, s.Latency)
	case s.Percentiles != nil:
		b.WriteByte(' ')
		formatPercentilesPayload(b, s.Percentiles)
	case s.Proportion != nil:
		b.WriteByte(' ')
		formatProportionPayload(b, s.Proportion)
	case s.Facets != nil:
		b.WriteByte(' ')
		formatFacetsPayload(b, s.Facets)
	case s.Impact != nil:
		b.WriteByte(' ')
		formatImpactPayload(b, s.Impact)
	case s.Baseline != nil:
		b.WriteByte(' ')
		formatBaselinePayload(b, s.Baseline)
	case s.Changes != nil:
		b.WriteByte(' ')
		formatChangesPayload(b, s.Changes)
	case s.Exemplars != nil:
		b.WriteByte(' ')
		formatExemplarsPayload(b, s.Exemplars)
	case s.Materialize != nil:
		b.WriteByte(' ')
		formatMaterializePayload(b, s.Materialize)
	case s.Tee != nil:
		b.WriteByte(' ')
		b.WriteString(quoteString(s.Tee.Sink))
	case s.Use != nil:
		b.WriteByte(' ')
		b.WriteString(s.Use.Fragment)
	case s.Compare != nil:
		b.WriteByte(' ')
		formatComparePayload(b, s.Compare)
	case s.Transaction != nil:
		b.WriteByte(' ')
		formatTransactionPayload(b, s.Transaction)
	case s.Correlate != nil:
		b.WriteByte(' ')
		formatCorrelatePayload(b, s.Correlate)
	case s.Rollup != nil:
		b.WriteByte(' ')
		formatRollupPayload(b, s.Rollup)
	case s.Xyseries != nil:
		b.WriteByte(' ')
		formatXYSeriesPayload(b, s.Xyseries)
	case s.Patterns != nil:
		formatGenericPayload(b, s.Patterns)
	case s.Outliers != nil:
		formatGenericPayload(b, s.Outliers)
	case s.Sessionize != nil:
		formatGenericPayload(b, s.Sessionize)
	case s.Trace != nil:
		formatGenericPayload(b, s.Trace)
	case s.Topology != nil:
		formatGenericPayload(b, s.Topology)
	case s.Generic != nil:
		formatGenericPayload(b, s.Generic)
	}
}

// ---------------------------------------------------------------------------
// Payload formatters
// ---------------------------------------------------------------------------

func formatAssignPayload(b *strings.Builder, p *ast.AssignPayload) {
	for i, a := range p.Assignments {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Name)
		b.WriteString(" = ")
		formatExpr(b, a.Value, precTop)
	}
}

func formatRenamePayload(b *strings.Builder, p *ast.RenamePayload) {
	for i, r := range p.Renames {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(r.Old)
		b.WriteString(" as ")
		b.WriteString(r.New)
	}
}

func formatStatsPayload(b *strings.Builder, p *ast.StatsPayload) {
	for i, agg := range p.Aggs {
		if i > 0 {
			b.WriteString(", ")
		}
		formatAggExpr(b, &agg)
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatAggExpr(b *strings.Builder, agg *ast.AggExpr) {
	if call, ok := agg.Func.(*ast.Call); ok && agg.WhereCond != nil {
		b.WriteString(strings.ToLower(call.Callee))
		if call.Bang {
			b.WriteByte('!')
		}
		b.WriteByte('(')
		for i, arg := range call.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, arg, precTop)
		}
		if len(call.Args) > 0 {
			b.WriteString(", ")
		}
		b.WriteString("where ")
		formatExpr(b, agg.WhereCond, precTop)
		b.WriteByte(')')
	} else {
		formatExpr(b, agg.Func, precTop)
	}
	if agg.Alias != "" {
		b.WriteString(" as ")
		b.WriteString(agg.Alias)
	}
}

func formatStreamstatsPayload(b *strings.Builder, p *ast.StreamstatsPayload) {
	if p.Window != nil {
		b.WriteString(fmt.Sprintf("window=%d ", *p.Window))
	}
	if p.Current != nil {
		if *p.Current {
			b.WriteString("current=true ")
		} else {
			b.WriteString("current=false ")
		}
	}
	formatStatsPayload(b, &p.StatsPayload)
}

func formatSortPayload(b *strings.Builder, p *ast.SortPayload) {
	for i, k := range p.Keys {
		if i > 0 {
			b.WriteString(", ")
		}
		if k.Desc {
			b.WriteByte('-')
		}
		formatExpr(b, k.Field, precTop)
	}
}

func formatDedupPayload(b *strings.Builder, p *ast.DedupPayload) {
	if p.N > 1 {
		b.WriteString(strconv.FormatInt(p.N, 10))
		b.WriteByte(' ')
	}
	for i, f := range p.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		formatExpr(b, f, precTop)
	}
}

func formatFieldPatternsPayload(b *strings.Builder, p *ast.FieldPatternsPayload) {
	if p.StarExcept {
		b.WriteString("* except ")
	}
	for i, pat := range p.Patterns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(pat.Name)
	}
}

func formatJoinPayload(b *strings.Builder, p *ast.JoinPayload) {
	if p.Type != "" && p.Type != "inner" {
		b.WriteString("type=")
		b.WriteString(p.Type)
		b.WriteByte(' ')
	}
	if len(p.On) > 0 {
		b.WriteString("on ")
		for i, f := range p.On {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, f, precTop)
		}
	}
	if p.Right != nil {
		b.WriteString(" with ")
		formatSubPipeline(b, p.Right)
	}
}

func formatSubPipeline(b *strings.Builder, sp *ast.SubPipeline) {
	if sp.CTERef != "" {
		b.WriteByte('$')
		b.WriteString(sp.CTERef)
		return
	}
	if sp.Pipeline != nil {
		b.WriteByte('[')
		formatPipelineInline(b, sp.Pipeline)
		b.WriteByte(']')
		return
	}
	b.WriteString("<error>")
}

func formatUnionPayload(b *strings.Builder, p *ast.UnionPayload) {
	for i, s := range p.Sources {
		if i > 0 {
			b.WriteString(", ")
		}
		formatSubPipeline(b, &s)
	}
}

func formatExplodePayload(b *strings.Builder, p *ast.ExplodePayload) {
	formatExpr(b, p.Array, precTop)
	if p.As != "" {
		b.WriteString(" as ")
		b.WriteString(p.As)
	}
}

func formatParsePayload(b *strings.Builder, p *ast.ParsePayload) {
	if len(p.FirstOf) > 0 {
		b.WriteString("first_of(")
		b.WriteString(strings.Join(p.FirstOf, ", "))
		b.WriteByte(')')
	} else {
		b.WriteString(p.Format)
		for _, a := range p.FormatArgs {
			b.WriteByte(' ')
			formatExpr(b, a, precTop)
		}
	}
	if p.From != nil {
		b.WriteString(" from ")
		formatExpr(b, p.From, precTop)
	}
	if len(p.Into) > 0 {
		b.WriteString(" into (")
		for i, c := range p.Into {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c.Name)
			if c.Type != "" {
				b.WriteString(" as ")
				b.WriteString(c.Type)
			}
		}
		b.WriteByte(')')
	}
	if p.Prefix != "" {
		b.WriteString(" prefix ")
		b.WriteString(p.Prefix)
	}
	if p.OnError != "" {
		b.WriteString(" on_error ")
		b.WriteString(p.OnError)
	}
}

func formatTopRarePayload(b *strings.Builder, p *ast.TopRarePayload) {
	if p.N != nil {
		b.WriteString(strconv.FormatInt(*p.N, 10))
		b.WriteByte(' ')
	}
	formatExpr(b, p.Field, precTop)
}

func formatEveryPayload(b *strings.Builder, p *ast.EveryPayload) {
	formatExpr(b, p.Span, precTop)
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
	b.WriteString(" stats ")
	for i, agg := range p.Aggs {
		if i > 0 {
			b.WriteString(", ")
		}
		formatAggExpr(b, &agg)
	}
}

func formatRatePayload(b *strings.Builder, p *ast.RatePayload) {
	if p.Per != nil {
		b.WriteString("per ")
		formatExpr(b, p.Per, precTop)
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatLatencyPayload(b *strings.Builder, p *ast.LatencyPayload) {
	formatExpr(b, p.Field, precTop)
	if p.Every != nil {
		b.WriteString(" every ")
		formatExpr(b, p.Every, precTop)
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatPercentilesPayload(b *strings.Builder, p *ast.PercentilesPayload) {
	formatExpr(b, p.Field, precTop)
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatProportionPayload(b *strings.Builder, p *ast.ProportionPayload) {
	formatExpr(b, p.Predicate, precTop)
	b.WriteString(" as ")
	b.WriteString(p.Alias)
	if p.Every != nil {
		b.WriteString(" every ")
		formatExpr(b, p.Every, precTop)
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatFacetsPayload(b *strings.Builder, p *ast.FacetsPayload) {
	for i, f := range p.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		formatExpr(b, f, precTop)
	}
	if p.Limit != nil {
		b.WriteString(fmt.Sprintf(" limit=%d", *p.Limit))
	}
}

func formatImpactPayload(b *strings.Builder, p *ast.ImpactPayload) {
	if p.Agg != nil {
		formatAggExpr(b, p.Agg)
		b.WriteByte(' ')
	}
	b.WriteString("by ")
	for i, k := range p.By {
		if i > 0 {
			b.WriteString(", ")
		}
		formatExpr(b, k, precTop)
	}
}

func formatBaselinePayload(b *strings.Builder, p *ast.BaselinePayload) {
	formatExpr(b, p.Field, precTop)
	b.WriteString(fmt.Sprintf(" window=%d", p.Window))
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatChangesPayload(b *strings.Builder, p *ast.ChangesPayload) {
	formatExpr(b, p.Field, precTop)
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatExemplarsPayload(b *strings.Builder, p *ast.ExemplarsPayload) {
	if p.N != nil {
		b.WriteString(strconv.FormatInt(*p.N, 10))
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatMaterializePayload(b *strings.Builder, p *ast.MaterializePayload) {
	b.WriteString(quoteString(p.Name))
	if p.Retention != nil {
		b.WriteString(" retention=")
		formatExpr(b, p.Retention, precTop)
	}
}

func formatComparePayload(b *strings.Builder, p *ast.ComparePayload) {
	if p.Previous {
		b.WriteString("previous ")
	}
	if p.Shift != nil {
		formatExpr(b, p.Shift, precTop)
	}
}

func formatTransactionPayload(b *strings.Builder, p *ast.TransactionPayload) {
	for i, f := range p.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		formatExpr(b, f, precTop)
	}
	if p.MaxSpan != nil {
		b.WriteString(" maxspan=")
		formatExpr(b, p.MaxSpan, precTop)
	}
	if p.StartsWith != nil {
		b.WriteString(" startswith=")
		formatExpr(b, p.StartsWith, precTop)
	}
	if p.EndsWith != nil {
		b.WriteString(" endswith=")
		formatExpr(b, p.EndsWith, precTop)
	}
}

func formatCorrelatePayload(b *strings.Builder, p *ast.CorrelatePayload) {
	formatExpr(b, p.Field1, precTop)
	b.WriteByte(' ')
	formatExpr(b, p.Field2, precTop)
	if p.Method != "" && p.Method != "pearson" {
		b.WriteString(" method=")
		b.WriteString(p.Method)
	}
}

func formatRollupPayload(b *strings.Builder, p *ast.RollupPayload) {
	for i, r := range p.Resolutions {
		if i > 0 {
			b.WriteString(", ")
		}
		formatExpr(b, r, precTop)
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			formatExpr(b, k, precTop)
		}
	}
}

func formatXYSeriesPayload(b *strings.Builder, p *ast.XYSeriesPayload) {
	formatExpr(b, p.X, precTop)
	b.WriteByte(' ')
	formatExpr(b, p.Y, precTop)
	b.WriteByte(' ')
	formatExpr(b, p.Value, precTop)
}

func formatGenericPayload(b *strings.Builder, p *ast.GenericOptionsPayload) {
	if p == nil || (len(p.Positionals) == 0 && len(p.Options) == 0) {
		return
	}
	b.WriteByte(' ')
	for i, pos := range p.Positionals {
		if i > 0 {
			b.WriteByte(' ')
		}
		formatExpr(b, pos, precTop)
	}
	for i, opt := range p.Options {
		if i > 0 || len(p.Positionals) > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(opt.Name)
		b.WriteByte('=')
		formatExpr(b, opt.Value, precTop)
	}
}

// writeFieldName renders a field name, backtick-quoting it when it is not a
// bare-safe token (re-derived; the AST does not retain quotedness for search
// keys).
func writeFieldName(b *strings.Builder, name string) {
	if needsQuoting(name) {
		b.WriteByte('`')
		b.WriteString(name)
		b.WriteByte('`')
		return
	}
	b.WriteString(name)
}
