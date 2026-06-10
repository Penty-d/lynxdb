// Package ast query-level nodes for the LynxFlow v2 AST.
//
// Every node carries a [Span] for diagnostics and formatter round-trip.
// The hierarchy is: Query -> Let* + Pipeline -> FromStage? + Stage*.
package ast

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Query (top-level)
// ---------------------------------------------------------------------------

// Query is the root AST node for a LynxFlow v2 query.
//
//	[let $name = <pipeline> ;]*
//	from <source-list>[<time-range>] [<search-sugar-terms>]
//	| <stage>
//	| <stage> ...
type Query struct {
	Lets     []Let
	Pipeline Pipeline
	Pos      Span
}

// String returns a compact debug rendering of the full query.
func (q *Query) String() string {
	var b strings.Builder
	for _, l := range q.Lets {
		b.WriteString(l.String())
		b.WriteString("; ")
	}
	b.WriteString(q.Pipeline.String())
	return b.String()
}

// ---------------------------------------------------------------------------
// Let (CTE binding)
// ---------------------------------------------------------------------------

// Let is a CTE binding: let $name = <pipeline>.
type Let struct {
	Name     string // without the $ prefix
	NameSpan Span
	Pipeline Pipeline
	Pos      Span // from 'let' keyword to end of pipeline (before ';')
}

// String returns a debug rendering.
func (l *Let) String() string {
	return "let $" + l.Name + " = " + l.Pipeline.String()
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// Pipeline is a source stage followed by zero or more pipeline stages.
type Pipeline struct {
	Source *FromStage // nil when implicit (query starts with | or stage name)
	Stages []Stage
	Pos    Span
}

// String returns a compact debug rendering.
func (p *Pipeline) String() string {
	var b strings.Builder
	if p.Source != nil {
		b.WriteString(p.Source.String())
	}
	for _, s := range p.Stages {
		if b.Len() > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(s.String())
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// FromStage (source stage)
// ---------------------------------------------------------------------------

// FromStage is the scan stage: from <sources>[range] [sugar-terms].
type FromStage struct {
	Sources    []SourceAtom
	TimeRanges []TimeRange
	SugarTerms SearchExpr // nil when no search sugar present
	Pos        Span
}

// String returns a debug rendering.
func (f *FromStage) String() string {
	var b strings.Builder
	b.WriteString("from ")
	for i, s := range f.Sources {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s.String())
	}
	for _, tr := range f.TimeRanges {
		b.WriteString(tr.String())
	}
	if f.SugarTerms != nil {
		b.WriteByte(' ')
		b.WriteString(f.SugarTerms.String())
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// SourceAtom (individual source reference in from)
// ---------------------------------------------------------------------------

// SourceAtomKind classifies a source reference.
type SourceAtomKind uint8

const (
	SourceName    SourceAtomKind = iota // bare name or backtick-quoted
	SourceGlob                          // name with wildcard: logs*
	SourceNegated                       // !-prefixed exclude: !logs-debug*
	SourceStar                          // * (all sources)
	SourceCTE                           // $name CTE reference
)

// SourceAtom is a single source reference inside a from stage.
type SourceAtom struct {
	Kind    SourceAtomKind
	Name    string // resolved name (without $, !, backticks)
	Pattern string // original pattern text (for globs)
	Quoted  bool   // true when backtick-quoted
	Pos     Span
}

// String returns a debug rendering.
func (s *SourceAtom) String() string {
	switch s.Kind {
	case SourceStar:
		return "*"
	case SourceCTE:
		return "$" + s.Name
	case SourceNegated:
		if s.Pattern != "" {
			return "!" + s.Pattern
		}
		return "!" + s.Name
	case SourceGlob:
		return s.Pattern
	default:
		if s.Quoted {
			return "`" + s.Name + "`"
		}
		return s.Name
	}
}

// ---------------------------------------------------------------------------
// TimeRange
// ---------------------------------------------------------------------------

// TimeRange represents a bracket time range: [-1h], [-7d..-1d], [@d],
// [-1h][@h], [2026-06-01T00:00:00Z..2026-06-02T00:00:00Z].
type TimeRange struct {
	Start    Expr   // nil for open-ended
	End      Expr   // nil for open-ended
	Snap     string // snap suffix like "@d", "@h" (empty if none)
	SnapSpan Span   // span of the snap token
	Pos      Span   // from [ to ]
}

// String returns a debug rendering.
func (tr *TimeRange) String() string {
	var b strings.Builder
	b.WriteByte('[')
	if tr.Start != nil {
		b.WriteString(tr.Start.String())
	}
	if tr.End != nil {
		b.WriteString("..")
		b.WriteString(tr.End.String())
	}
	b.WriteByte(']')
	if tr.Snap != "" {
		b.WriteByte('[')
		b.WriteString(tr.Snap)
		b.WriteByte(']')
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Search sugar expressions (§3.1)
// ---------------------------------------------------------------------------

// SearchExpr is the interface for search-sugar expressions in the from stage.
// These are NOT desugared here — PR (d) does that. The parser represents them
// faithfully with spans.
type SearchExpr interface {
	SearchExprSpan() Span
	String() string
}

// SearchBareWord is a bare search term: timeout, error.
type SearchBareWord struct {
	Word string
	Pos  Span
}

func (s *SearchBareWord) SearchExprSpan() Span { return s.Pos }
func (s *SearchBareWord) String() string       { return s.Word }

// SearchPhrase is a quoted phrase: "connection reset".
type SearchPhrase struct {
	Text string // the text without quotes
	Raw  string // original source including quotes
	Pos  Span
}

func (s *SearchPhrase) SearchExprSpan() Span { return s.Pos }
func (s *SearchPhrase) String() string       { return s.Raw }

// SearchKeyValue is a key=value (or key!=, key>, etc.) term.
type SearchKeyValue struct {
	Key   string
	Op    string // "=", "!=", "<", "<=", ">", ">="
	Value Expr   // literal, glob pattern, or array for 'in'
	Pos   Span
}

func (s *SearchKeyValue) SearchExprSpan() Span { return s.Pos }
func (s *SearchKeyValue) String() string {
	val := "<nil>"
	if s.Value != nil {
		val = s.Value.String()
	}
	return s.Key + s.Op + val
}

// SearchIn is key in (...) in search sugar.
type SearchIn struct {
	Key    string
	Values []Expr
	Pos    Span
}

func (s *SearchIn) SearchExprSpan() Span { return s.Pos }
func (s *SearchIn) String() string {
	var b strings.Builder
	b.WriteString(s.Key)
	b.WriteString(" in (")
	for i, v := range s.Values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(v.String())
	}
	b.WriteByte(')')
	return b.String()
}

// SearchBinary is an and/or binary search expression.
type SearchBinary struct {
	Op    string // "and", "or"
	Left  SearchExpr
	Right SearchExpr
	Pos   Span
}

func (s *SearchBinary) SearchExprSpan() Span { return s.Pos }
func (s *SearchBinary) String() string {
	return "(" + s.Left.String() + " " + s.Op + " " + s.Right.String() + ")"
}

// SearchNot is a not-prefixed search expression.
type SearchNot struct {
	Operand SearchExpr
	Pos     Span
}

func (s *SearchNot) SearchExprSpan() Span { return s.Pos }
func (s *SearchNot) String() string {
	return "(not " + s.Operand.String() + ")"
}

// SearchParen is a parenthesized search expression.
type SearchParen struct {
	Inner SearchExpr
	Pos   Span
}

func (s *SearchParen) SearchExprSpan() Span { return s.Pos }
func (s *SearchParen) String() string       { return s.Inner.String() }

// SearchGlobValue is a glob value in search sugar: host=web-*.
type SearchGlobValue struct {
	Pattern string
	Pos     Span
}

func (s *SearchGlobValue) ExprSpan() Span { return s.Pos }
func (s *SearchGlobValue) String() string { return s.Pattern }

// ---------------------------------------------------------------------------
// Stage (generic pipeline stage)
// ---------------------------------------------------------------------------

// Stage is a single pipeline stage: where, stats, extend, etc.
type Stage struct {
	Name    string
	NamePos Span
	// Typed payloads for stages that need structured representation.
	// Only one of these is non-nil at a time.
	Where       *WherePayload
	Extend      *AssignPayload
	Rename      *RenamePayload
	Stats       *StatsPayload
	Eventstats  *StatsPayload
	Streamstats *StreamstatsPayload
	Sort        *SortPayload
	Head        *IntPayload
	Tail        *IntPayload
	Dedup       *DedupPayload
	Keep        *FieldPatternsPayload
	Drop        *FieldPatternsPayload
	Join        *JoinPayload
	Union       *UnionPayload
	Explode     *ExplodePayload
	Describe    *DescribePayload
	Parse       *ParsePayload
	Top         *TopRarePayload
	Rare        *TopRarePayload
	Every       *EveryPayload
	Rate        *RatePayload
	Latency     *LatencyPayload
	Percentiles *PercentilesPayload
	Proportion  *ProportionPayload
	Facets      *FacetsPayload
	Impact      *ImpactPayload
	Baseline    *BaselinePayload
	Changes     *ChangesPayload
	Exemplars   *ExemplarsPayload
	Materialize *MaterializePayload
	Tee         *TeePayload
	Use         *UsePayload
	Compare     *ComparePayload
	Transaction *TransactionPayload
	Patterns    *GenericOptionsPayload
	Outliers    *GenericOptionsPayload
	Sessionize  *GenericOptionsPayload
	Trace       *GenericOptionsPayload
	Topology    *GenericOptionsPayload
	Correlate   *CorrelatePayload
	Rollup      *RollupPayload
	Xyseries    *XYSeriesPayload
	Generic     *GenericOptionsPayload // fallback for unknown-but-recovered stages
	Pos         Span                   // from stage name to end of stage
	HasError    bool                   // true if parsing this stage produced errors
}

// String returns a compact debug rendering.
func (s *Stage) String() string {
	var b strings.Builder
	b.WriteString(s.Name)
	switch {
	case s.Where != nil:
		b.WriteByte(' ')
		b.WriteString(s.Where.Expr.String())
	case s.Extend != nil:
		b.WriteByte(' ')
		b.WriteString(s.Extend.String())
	case s.Rename != nil:
		b.WriteByte(' ')
		b.WriteString(s.Rename.String())
	case s.Stats != nil:
		b.WriteByte(' ')
		b.WriteString(s.Stats.String())
	case s.Eventstats != nil:
		b.WriteByte(' ')
		b.WriteString(s.Eventstats.String())
	case s.Streamstats != nil:
		b.WriteByte(' ')
		b.WriteString(s.Streamstats.String())
	case s.Sort != nil:
		b.WriteByte(' ')
		b.WriteString(s.Sort.String())
	case s.Head != nil:
		b.WriteByte(' ')
		b.WriteString(fmt.Sprintf("%d", s.Head.N))
	case s.Tail != nil:
		b.WriteByte(' ')
		b.WriteString(fmt.Sprintf("%d", s.Tail.N))
	case s.Dedup != nil:
		b.WriteByte(' ')
		b.WriteString(s.Dedup.String())
	case s.Keep != nil:
		b.WriteByte(' ')
		b.WriteString(s.Keep.String())
	case s.Drop != nil:
		b.WriteByte(' ')
		b.WriteString(s.Drop.String())
	case s.Join != nil:
		b.WriteByte(' ')
		b.WriteString(s.Join.String())
	case s.Union != nil:
		b.WriteByte(' ')
		b.WriteString(s.Union.String())
	case s.Explode != nil:
		b.WriteByte(' ')
		b.WriteString(s.Explode.String())
	case s.Parse != nil:
		b.WriteByte(' ')
		b.WriteString(s.Parse.String())
	case s.Top != nil:
		b.WriteByte(' ')
		b.WriteString(s.Top.String())
	case s.Rare != nil:
		b.WriteByte(' ')
		b.WriteString(s.Rare.String())
	case s.Every != nil:
		b.WriteByte(' ')
		b.WriteString(s.Every.String())
	case s.Rate != nil:
		b.WriteByte(' ')
		b.WriteString(s.Rate.String())
	case s.Latency != nil:
		b.WriteByte(' ')
		b.WriteString(s.Latency.String())
	case s.Percentiles != nil:
		b.WriteByte(' ')
		b.WriteString(s.Percentiles.String())
	case s.Proportion != nil:
		b.WriteByte(' ')
		b.WriteString(s.Proportion.String())
	case s.Facets != nil:
		b.WriteByte(' ')
		b.WriteString(s.Facets.String())
	case s.Impact != nil:
		b.WriteByte(' ')
		b.WriteString(s.Impact.String())
	case s.Baseline != nil:
		b.WriteByte(' ')
		b.WriteString(s.Baseline.String())
	case s.Changes != nil:
		b.WriteByte(' ')
		b.WriteString(s.Changes.String())
	case s.Exemplars != nil:
		b.WriteByte(' ')
		b.WriteString(s.Exemplars.String())
	case s.Materialize != nil:
		b.WriteByte(' ')
		b.WriteString(s.Materialize.String())
	case s.Tee != nil:
		b.WriteByte(' ')
		b.WriteString(s.Tee.String())
	case s.Use != nil:
		b.WriteByte(' ')
		b.WriteString(s.Use.String())
	case s.Compare != nil:
		b.WriteByte(' ')
		b.WriteString(s.Compare.String())
	case s.Transaction != nil:
		b.WriteByte(' ')
		b.WriteString(s.Transaction.String())
	case s.Correlate != nil:
		b.WriteByte(' ')
		b.WriteString(s.Correlate.String())
	case s.Rollup != nil:
		b.WriteByte(' ')
		b.WriteString(s.Rollup.String())
	case s.Xyseries != nil:
		b.WriteByte(' ')
		b.WriteString(s.Xyseries.String())
	case s.Patterns != nil || s.Outliers != nil || s.Sessionize != nil ||
		s.Trace != nil || s.Topology != nil || s.Generic != nil:
		payload := s.Patterns
		if payload == nil {
			payload = s.Outliers
		}
		if payload == nil {
			payload = s.Sessionize
		}
		if payload == nil {
			payload = s.Trace
		}
		if payload == nil {
			payload = s.Topology
		}
		if payload == nil {
			payload = s.Generic
		}
		if payload != nil && len(payload.Options) > 0 {
			b.WriteByte(' ')
			b.WriteString(payload.String())
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Stage payloads
// ---------------------------------------------------------------------------

// WherePayload is the typed payload for a where stage.
type WherePayload struct {
	Expr Expr
}

// AssignPayload is the typed payload for extend.
type AssignPayload struct {
	Assignments []Assignment
}

func (a *AssignPayload) String() string {
	var b strings.Builder
	for i, assign := range a.Assignments {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(assign.String())
	}
	return b.String()
}

// Assignment is a name = expr pair.
type Assignment struct {
	Name     string
	NameSpan Span
	Value    Expr
	Pos      Span
}

func (a *Assignment) String() string {
	return a.Name + " = " + a.Value.String()
}

// RenamePayload is the typed payload for rename.
type RenamePayload struct {
	Renames []RenameEntry
}

func (r *RenamePayload) String() string {
	var b strings.Builder
	for i, re := range r.Renames {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(re.String())
	}
	return b.String()
}

// RenameEntry is old as new.
type RenameEntry struct {
	Old     string
	OldSpan Span
	New     string
	NewSpan Span
	Pos     Span
}

func (r *RenameEntry) String() string {
	return r.Old + " as " + r.New
}

// StatsPayload is the typed payload for stats/eventstats.
type StatsPayload struct {
	Aggs []AggExpr
	By   []Expr // group keys (may include bin() calls)
}

func (s *StatsPayload) String() string {
	var b strings.Builder
	for i, agg := range s.Aggs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(agg.String())
	}
	if len(s.By) > 0 {
		b.WriteString(" by ")
		for i, k := range s.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// AggExpr is an aggregate call with optional alias and conditional where.
type AggExpr struct {
	Func      Expr   // the aggregate call expression (typically *Call)
	WhereCond Expr   // conditional aggregate: count(where p) / sum(x, where p)
	Alias     string // as name (empty if no alias)
	AliasSpan Span
	Pos       Span
}

func (a *AggExpr) String() string {
	var b strings.Builder
	// Render the aggregate call, injecting 'where' arg into call representation.
	if call, ok := a.Func.(*Call); ok && a.WhereCond != nil {
		b.WriteString(call.Callee)
		if call.Bang {
			b.WriteByte('!')
		}
		b.WriteByte('(')
		for i, arg := range call.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(arg.String())
		}
		if len(call.Args) > 0 {
			b.WriteString(", ")
		}
		b.WriteString("where ")
		b.WriteString(a.WhereCond.String())
		b.WriteByte(')')
	} else {
		b.WriteString(a.Func.String())
	}
	if a.Alias != "" {
		b.WriteString(" as ")
		b.WriteString(a.Alias)
	}
	return b.String()
}

// StreamstatsPayload extends StatsPayload with window options.
type StreamstatsPayload struct {
	StatsPayload
	Window  *int  // nil means not specified
	Current *bool // nil means not specified
}

func (s *StreamstatsPayload) String() string {
	var b strings.Builder
	if s.Window != nil {
		b.WriteString(fmt.Sprintf("window=%d ", *s.Window))
	}
	if s.Current != nil {
		b.WriteString(fmt.Sprintf("current=%t ", *s.Current))
	}
	b.WriteString(s.StatsPayload.String())
	return b.String()
}

// SortPayload is the typed payload for sort.
type SortPayload struct {
	Keys []SortKey
}

func (s *SortPayload) String() string {
	var b strings.Builder
	for i, k := range s.Keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k.String())
	}
	return b.String()
}

// SortKey is a sort key with direction prefix.
type SortKey struct {
	Field Expr
	Desc  bool // true for -prefix
	Pos   Span
}

func (k *SortKey) String() string {
	if k.Desc {
		return "-" + k.Field.String()
	}
	return k.Field.String()
}

// IntPayload holds a single int argument (head N, tail N).
type IntPayload struct {
	N   int64
	Pos Span
}

// DedupPayload is the typed payload for dedup.
type DedupPayload struct {
	N      int64 // default 1
	Fields []Expr
}

func (d *DedupPayload) String() string {
	var b strings.Builder
	if d.N > 1 {
		b.WriteString(fmt.Sprintf("%d ", d.N))
	}
	for i, f := range d.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(f.String())
	}
	return b.String()
}

// FieldPatternsPayload is for keep/drop.
type FieldPatternsPayload struct {
	StarExcept bool // true for `* except f1, f2`
	Patterns   []FieldPattern
}

func (f *FieldPatternsPayload) String() string {
	var b strings.Builder
	if f.StarExcept {
		b.WriteString("* except ")
	}
	for i, p := range f.Patterns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(p.String())
	}
	return b.String()
}

// FieldPattern is a field reference or glob pattern in keep/drop.
type FieldPattern struct {
	Name string // field name or glob pattern
	Glob bool   // true if contains wildcard
	Pos  Span
}

func (f *FieldPattern) String() string { return f.Name }

// JoinPayload is the typed payload for join.
type JoinPayload struct {
	Type     string // "inner", "left", "outer" (default "inner")
	TypeSpan Span
	On       []Expr
	Right    *SubPipeline
}

func (j *JoinPayload) String() string {
	var b strings.Builder
	if j.Type != "" && j.Type != "inner" {
		b.WriteString("type=")
		b.WriteString(j.Type)
		b.WriteByte(' ')
	}
	if len(j.On) > 0 {
		b.WriteString("on ")
		for i, f := range j.On {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(f.String())
		}
	}
	if j.Right != nil {
		b.WriteString(" with ")
		b.WriteString(j.Right.String())
	}
	return b.String()
}

// SubPipeline is either a $cte reference or an inline [pipeline].
type SubPipeline struct {
	CTERef   string    // non-empty for $name references
	Pipeline *Pipeline // non-nil for inline [pipeline]
	Pos      Span
}

func (s *SubPipeline) String() string {
	if s.CTERef != "" {
		return "$" + s.CTERef
	}
	if s.Pipeline != nil {
		return "[" + s.Pipeline.String() + "]"
	}
	return "<error>"
}

// UnionPayload is the typed payload for union.
type UnionPayload struct {
	Sources []SubPipeline
}

func (u *UnionPayload) String() string {
	var b strings.Builder
	for i, s := range u.Sources {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(s.String())
	}
	return b.String()
}

// ExplodePayload is the typed payload for explode.
type ExplodePayload struct {
	Array Expr
	As    string // alias for element (may be empty)
	AsPos Span
}

func (e *ExplodePayload) String() string {
	arr := "<nil>"
	if e.Array != nil {
		arr = e.Array.String()
	}
	if e.As != "" {
		return arr + " as " + e.As
	}
	return arr
}

// DescribePayload is an empty payload for describe.
type DescribePayload struct{}

// ParsePayload is the typed payload for parse.
type ParsePayload struct {
	Format    string // format name: json, logfmt, regex, pattern, etc.
	FormatPos Span
	// FirstOf contains format names if this is a first_of(...) chain.
	FirstOf    []string
	FirstOfPos Span
	FormatArgs []Expr // parenthesized args for kv(...), pattern "...", regex r"..."
	From       Expr   // field to parse from (nil = _raw default)
	Into       []CaptureField
	Prefix     string // prefix string (empty if none)
	OnError    string // propagate, null, drop, strict (empty = default)
}

func (p *ParsePayload) String() string {
	var b strings.Builder
	if len(p.FirstOf) > 0 {
		b.WriteString("first_of(")
		b.WriteString(strings.Join(p.FirstOf, ", "))
		b.WriteByte(')')
	} else {
		b.WriteString(p.Format)
		for _, a := range p.FormatArgs {
			b.WriteByte(' ')
			b.WriteString(a.String())
		}
	}
	if p.From != nil {
		b.WriteString(" from ")
		b.WriteString(p.From.String())
	}
	if len(p.Into) > 0 {
		b.WriteString(" into (")
		for i, c := range p.Into {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c.String())
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
	return b.String()
}

// CaptureField is a typed capture in a parse into clause.
type CaptureField struct {
	Name string
	Type string // empty if untyped
	Pos  Span
}

func (c *CaptureField) String() string {
	if c.Type != "" {
		return c.Name + " as " + c.Type
	}
	return c.Name
}

// TopRarePayload is the typed payload for top/rare.
type TopRarePayload struct {
	N     *int64 // nil = default (10)
	Field Expr
}

func (t *TopRarePayload) String() string {
	if t.N != nil {
		return fmt.Sprintf("%d %s", *t.N, t.Field.String())
	}
	return t.Field.String()
}

// EveryPayload is the typed payload for every.
type EveryPayload struct {
	Span Expr   // duration
	By   []Expr // optional by keys
	Aggs []AggExpr
}

func (e *EveryPayload) String() string {
	var b strings.Builder
	b.WriteString(e.Span.String())
	if len(e.By) > 0 {
		b.WriteString(" by ")
		for i, k := range e.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	b.WriteString(" stats ")
	for i, agg := range e.Aggs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(agg.String())
	}
	return b.String()
}

// RatePayload is the typed payload for rate.
type RatePayload struct {
	Per Expr   // duration (nil = default 1m)
	By  []Expr // optional
}

func (r *RatePayload) String() string {
	var b strings.Builder
	if r.Per != nil {
		b.WriteString("per ")
		b.WriteString(r.Per.String())
	}
	if len(r.By) > 0 {
		b.WriteString(" by ")
		for i, k := range r.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// LatencyPayload is the typed payload for latency.
type LatencyPayload struct {
	Field Expr
	Every Expr   // duration (nil if not specified)
	By    []Expr // optional
}

func (l *LatencyPayload) String() string {
	var b strings.Builder
	b.WriteString(l.Field.String())
	if l.Every != nil {
		b.WriteString(" every ")
		b.WriteString(l.Every.String())
	}
	if len(l.By) > 0 {
		b.WriteString(" by ")
		for i, k := range l.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// PercentilesPayload is the typed payload for percentiles.
type PercentilesPayload struct {
	Field Expr
	By    []Expr
}

func (p *PercentilesPayload) String() string {
	var b strings.Builder
	b.WriteString(p.Field.String())
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// ProportionPayload is the typed payload for proportion.
type ProportionPayload struct {
	Predicate Expr
	Alias     string
	AliasSpan Span
	Every     Expr   // duration (nil if not specified)
	By        []Expr // optional
}

func (p *ProportionPayload) String() string {
	var b strings.Builder
	b.WriteString(p.Predicate.String())
	b.WriteString(" as ")
	b.WriteString(p.Alias)
	if p.Every != nil {
		b.WriteString(" every ")
		b.WriteString(p.Every.String())
	}
	if len(p.By) > 0 {
		b.WriteString(" by ")
		for i, k := range p.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// FacetsPayload is the typed payload for facets.
type FacetsPayload struct {
	Fields []Expr
	Limit  *int64 // nil = default 10
}

func (f *FacetsPayload) String() string {
	var b strings.Builder
	for i, field := range f.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(field.String())
	}
	if f.Limit != nil {
		b.WriteString(fmt.Sprintf(" limit=%d", *f.Limit))
	}
	return b.String()
}

// ImpactPayload is the typed payload for impact.
type ImpactPayload struct {
	Agg *AggExpr // nil = default count()
	By  []Expr
}

func (im *ImpactPayload) String() string {
	var b strings.Builder
	if im.Agg != nil {
		b.WriteString(im.Agg.String())
		b.WriteByte(' ')
	}
	b.WriteString("by ")
	for i, k := range im.By {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k.String())
	}
	return b.String()
}

// BaselinePayload is the typed payload for baseline.
type BaselinePayload struct {
	Field  Expr
	Window int64
	By     []Expr
}

func (bl *BaselinePayload) String() string {
	var b strings.Builder
	b.WriteString(bl.Field.String())
	b.WriteString(fmt.Sprintf(" window=%d", bl.Window))
	if len(bl.By) > 0 {
		b.WriteString(" by ")
		for i, k := range bl.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// ChangesPayload is the typed payload for changes.
type ChangesPayload struct {
	Field Expr
	By    []Expr
}

func (c *ChangesPayload) String() string {
	var b strings.Builder
	b.WriteString(c.Field.String())
	if len(c.By) > 0 {
		b.WriteString(" by ")
		for i, k := range c.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// ExemplarsPayload is the typed payload for exemplars.
type ExemplarsPayload struct {
	N  *int64 // nil = default 3
	By []Expr
}

func (e *ExemplarsPayload) String() string {
	var b strings.Builder
	if e.N != nil {
		b.WriteString(fmt.Sprintf("%d", *e.N))
	}
	if len(e.By) > 0 {
		b.WriteString(" by ")
		for i, k := range e.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// MaterializePayload is the typed payload for materialize.
type MaterializePayload struct {
	Name      string
	Retention Expr // duration (nil if not specified)
}

func (m *MaterializePayload) String() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%q", m.Name))
	if m.Retention != nil {
		b.WriteString(" retention=")
		b.WriteString(m.Retention.String())
	}
	return b.String()
}

// TeePayload is the typed payload for tee.
type TeePayload struct {
	Sink string
}

func (t *TeePayload) String() string {
	return fmt.Sprintf("%q", t.Sink)
}

// UsePayload is the typed payload for use.
type UsePayload struct {
	Fragment string
}

func (u *UsePayload) String() string {
	return u.Fragment
}

// ComparePayload is the typed payload for compare.
type ComparePayload struct {
	Previous bool // whether 'previous' keyword was present
	Shift    Expr // duration
}

func (c *ComparePayload) String() string {
	var b strings.Builder
	if c.Previous {
		b.WriteString("previous ")
	}
	if c.Shift != nil {
		b.WriteString(c.Shift.String())
	}
	return b.String()
}

// TransactionPayload is the typed payload for transaction.
type TransactionPayload struct {
	Fields     []Expr
	MaxSpan    Expr // duration (nil if not specified)
	StartsWith Expr // predicate (nil if not specified)
	EndsWith   Expr // predicate (nil if not specified)
}

func (t *TransactionPayload) String() string {
	var b strings.Builder
	for i, f := range t.Fields {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(f.String())
	}
	if t.MaxSpan != nil {
		b.WriteString(" maxspan=")
		b.WriteString(t.MaxSpan.String())
	}
	if t.StartsWith != nil {
		b.WriteString(" startswith=")
		b.WriteString(t.StartsWith.String())
	}
	if t.EndsWith != nil {
		b.WriteString(" endswith=")
		b.WriteString(t.EndsWith.String())
	}
	return b.String()
}

// CorrelatePayload is the typed payload for correlate.
type CorrelatePayload struct {
	Field1 Expr
	Field2 Expr
	Method string // "pearson", "spearman" (empty = default)
}

func (c *CorrelatePayload) String() string {
	var b strings.Builder
	b.WriteString(c.Field1.String())
	b.WriteByte(' ')
	b.WriteString(c.Field2.String())
	if c.Method != "" && c.Method != "pearson" {
		b.WriteString(" method=")
		b.WriteString(c.Method)
	}
	return b.String()
}

// RollupPayload is the typed payload for rollup.
type RollupPayload struct {
	Resolutions []Expr
	By          []Expr
}

func (r *RollupPayload) String() string {
	var b strings.Builder
	for i, res := range r.Resolutions {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(res.String())
	}
	if len(r.By) > 0 {
		b.WriteString(" by ")
		for i, k := range r.By {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(k.String())
		}
	}
	return b.String()
}

// XYSeriesPayload is the typed payload for xyseries.
type XYSeriesPayload struct {
	X     Expr
	Y     Expr
	Value Expr
}

func (x *XYSeriesPayload) String() string {
	xStr, yStr, vStr := "<nil>", "<nil>", "<nil>"
	if x.X != nil {
		xStr = x.X.String()
	}
	if x.Y != nil {
		yStr = x.Y.String()
	}
	if x.Value != nil {
		vStr = x.Value.String()
	}
	return xStr + " " + yStr + " " + vStr
}

// GenericOptionsPayload is for stages that are fully option-driven.
type GenericOptionsPayload struct {
	Positionals []Expr
	Options     []Option
}

func (g *GenericOptionsPayload) String() string {
	var b strings.Builder
	for i, p := range g.Positionals {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p.String())
	}
	for _, o := range g.Options {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(o.String())
	}
	return b.String()
}

// Option is a name=value pair in a stage.
type Option struct {
	Name     string
	NameSpan Span
	Value    Expr
	ValuePos Span
}

func (o *Option) String() string {
	return o.Name + "=" + o.Value.String()
}
