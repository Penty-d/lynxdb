// Package sema implements semantic analysis for LynxFlow v2 queries (RFC-002
// Phase 3). It operates on DESUGARED queries (core + helper + management
// stages only) and performs:
//
//   - Flow-typed schema tracking across pipeline stages
//   - Expression type inference with did-you-mean suggestions
//   - Aggregate/function misuse detection
//   - Streaming-safety classification for live tail
//
// The primary entry point is [Analyze], which takes a desugared [ast.Query]
// and a [Catalog] and produces a [Result] containing diagnostics, the output
// schema, and a streaming-safety flag.
package sema

import (
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/registry"
)

// FieldType is the semantic type of a field in the flowing schema.
type FieldType string

const (
	TypeString    FieldType = "string"
	TypeInt       FieldType = "int"
	TypeFloat     FieldType = "float"
	TypeBool      FieldType = "bool"
	TypeTimestamp FieldType = "timestamp"
	TypeDuration  FieldType = "duration"
	TypeArray     FieldType = "array"
	TypeObject    FieldType = "object"
	TypeAny       FieldType = "any" // unknown / dynamic
)

// Field is a named, typed field in the schema.
type Field struct {
	Name string
	Type FieldType
}

// Catalog provides field metadata from the storage layer (or a test stub).
// Implementations must be safe for concurrent use.
type Catalog interface {
	// Lookup returns the type and presence of a field in the catalog.
	Lookup(field string) (FieldType, bool)
	// Fields returns all known field names.
	Fields() []string
}

// MapCatalog is a simple map-based Catalog for testing.
type MapCatalog map[string]FieldType

// Lookup implements Catalog.
func (m MapCatalog) Lookup(field string) (FieldType, bool) {
	t, ok := m[field]
	return t, ok
}

// Fields implements Catalog.
func (m MapCatalog) Fields() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Result is the output of semantic analysis.
type Result struct {
	// Diags contains all diagnostics (errors and warnings) produced.
	Diags []parser.Diag
	// OutputSchema is the schema at the end of the pipeline. May be nil
	// if the schema is open (unknown fields allowed).
	OutputSchema []Field
	// StreamingSafe is true when every stage in the pipeline is row-streaming
	// (safe for live tail).
	StreamingSafe bool
}

// schema tracks the flowing schema through the pipeline.
type schema struct {
	fields []Field
	open   bool // true = unknown fields allowed without diagnostics
}

func (s *schema) lookup(name string) (FieldType, bool) {
	for _, f := range s.fields {
		if f.Name == name {
			return f.Type, true
		}
	}
	return "", false
}

func (s *schema) names() []string {
	out := make([]string, len(s.fields))
	for i, f := range s.fields {
		out[i] = f.Name
	}
	return out
}

func (s *schema) add(name string, typ FieldType) {
	for i, f := range s.fields {
		if f.Name == name {
			s.fields[i].Type = typ
			return
		}
	}
	s.fields = append(s.fields, Field{Name: name, Type: typ})
}

func (s *schema) remove(name string) {
	for i, f := range s.fields {
		if f.Name == name {
			s.fields = append(s.fields[:i], s.fields[i+1:]...)
			return
		}
	}
}

func (s *schema) rename(old, new string) {
	for i, f := range s.fields {
		if f.Name == old {
			s.fields[i].Name = new
			return
		}
	}
}

func (s *schema) clone() *schema {
	out := &schema{open: s.open}
	out.fields = make([]Field, len(s.fields))
	copy(out.fields, s.fields)
	return out
}

// analyzer holds analysis state.
type analyzer struct {
	cat    Catalog
	diags  []parser.Diag
	schema *schema
	// streamingSafe is true until we encounter a non-streaming stage.
	streamingSafe bool
	// inAggContext is true when analyzing aggregate expressions inside
	// stats/eventstats/streamstats.
	inAggContext bool
}

// Analyze performs semantic analysis on a desugared query.
// The input must contain only core, helper, and management stages.
// If a sugar-class stage is encountered, a defensive internal-error
// diagnostic is emitted (no panic).
func Analyze(q *ast.Query, cat Catalog) Result {
	if q == nil {
		return Result{StreamingSafe: true}
	}
	a := &analyzer{
		cat:           cat,
		streamingSafe: true,
	}

	// Build initial schema: builtins + catalog fields.
	a.schema = a.initialSchema()

	// Analyze Let bindings (CTEs).
	for _, l := range q.Lets {
		a.analyzeLet(l)
	}

	// Analyze the main pipeline.
	a.analyzePipeline(q.Pipeline)

	return Result{
		Diags:         a.diags,
		OutputSchema:  a.schema.fields,
		StreamingSafe: a.streamingSafe,
	}
}

func (a *analyzer) initialSchema() *schema {
	s := &schema{}
	// Builtins.
	s.add("_time", TypeTimestamp)
	s.add("_raw", TypeString)
	s.add("_source", TypeString)
	s.add("_sourcetype", TypeString)
	s.add("host", TypeString)
	s.add("index", TypeString)
	// Catalog fields.
	for _, name := range a.cat.Fields() {
		typ, _ := a.cat.Lookup(name)
		s.add(name, FieldType(typ))
	}
	return s
}

func (a *analyzer) addDiag(code parser.DiagCode, sev parser.Severity, span ast.Span, msg, suggestion string) {
	a.diags = append(a.diags, parser.Diag{
		Code:       code,
		Severity:   sev,
		Message:    msg,
		Span:       span,
		Suggestion: suggestion,
	})
}

func (a *analyzer) analyzeLet(l ast.Let) {
	// CTEs are analyzed with the initial schema (they don't see prior CTEs'
	// schema modifications — they have their own pipeline).
	saved := a.schema
	a.schema = a.initialSchema()
	a.analyzePipeline(l.Pipeline)
	// Restore the outer schema.
	a.schema = saved
}

func (a *analyzer) analyzePipeline(p ast.Pipeline) {
	for _, s := range p.Stages {
		a.analyzeStage(s)
	}
}

func (a *analyzer) analyzeStage(s ast.Stage) {
	// Check for sugar stages (defensive — should have been desugared).
	if op, found := registry.LookupOperator(s.Name); found {
		if op.Class == registry.ClassSugar {
			a.addDiag("S999", parser.SeverityError, s.Pos,
				"internal error: sugar stage '"+s.Name+"' should have been desugared before semantic analysis",
				"run the desugarer before the semantic analyzer")
			return
		}
		// Track streaming safety.
		if op.Streaming == registry.StreamingAcc {
			a.streamingSafe = false
		}
	}

	switch s.Name {
	case "where":
		a.analyzeWhere(s)
	case "extend":
		a.analyzeExtend(s)
	case "keep":
		a.analyzeKeep(s)
	case "drop":
		a.analyzeDrop(s)
	case "rename":
		a.analyzeRename(s)
	case "stats":
		a.analyzeStats(s)
	case "eventstats":
		a.analyzeEventstats(s)
	case "streamstats":
		a.analyzeStreamstats(s)
	case "sort":
		a.analyzeSort(s)
	case "head", "tail":
		// Schema unchanged.
	case "dedup":
		a.analyzeDedup(s)
	case "join":
		a.analyzeJoin(s)
	case "union":
		a.analyzeUnion(s)
	case "explode":
		a.analyzeExplode(s)
	case "describe":
		a.analyzeDescribe(s)
	case "parse":
		a.analyzeParse(s)
	// Helpers: keep schema + add documented fields as "any".
	case "compare":
		a.analyzeCompare(s)
	case "patterns":
		a.analyzePatterns(s)
	case "outliers":
		a.analyzeOutliers(s)
	case "sessionize":
		a.analyzeSessionize(s)
	case "transaction":
		a.analyzeTransaction(s)
	case "trace":
		a.analyzeTrace(s)
	case "topology":
		a.analyzeTopology(s)
	case "correlate":
		a.analyzeCorrelate(s)
	case "rollup":
		a.analyzeRollup(s)
	case "xyseries":
		a.analyzeXyseries(s)
	// Management stages: schema unchanged.
	case "materialize", "tee", "use":
		// No schema effect.
	default:
		// Unknown stage: leave schema open, no error (parser already handles unknown stages).
	}
}

// ---------------------------------------------------------------------------
// Stage analyzers
// ---------------------------------------------------------------------------

func (a *analyzer) analyzeWhere(s ast.Stage) {
	if s.Where == nil {
		return
	}
	// Type-check the predicate; result should be bool.
	a.inferExpr(s.Where.Expr)
}

func (a *analyzer) analyzeExtend(s ast.Stage) {
	if s.Extend == nil {
		return
	}
	for _, assign := range s.Extend.Assignments {
		typ := a.inferExpr(assign.Value)
		a.schema.add(assign.Name, typ)
	}
}

func (a *analyzer) analyzeKeep(s ast.Stage) {
	if s.Keep == nil {
		return
	}
	if s.Keep.StarExcept {
		// keep * except f1, f2: remove the named fields, keep rest.
		for _, p := range s.Keep.Patterns {
			if !p.Glob {
				a.schema.remove(p.Name)
			} else {
				// Glob pattern: remove matching fields.
				for _, f := range a.schema.fields {
					if globMatch(p.Name, f.Name) {
						a.schema.remove(f.Name)
					}
				}
			}
		}
		return
	}
	// Explicit keep: build new schema from the listed fields.
	var newFields []Field
	for _, p := range s.Keep.Patterns {
		if p.Glob {
			for _, f := range a.schema.fields {
				if globMatch(p.Name, f.Name) {
					newFields = append(newFields, f)
				}
			}
		} else {
			if typ, ok := a.schema.lookup(p.Name); ok {
				newFields = append(newFields, Field{Name: p.Name, Type: typ})
			} else if a.schema.open {
				newFields = append(newFields, Field{Name: p.Name, Type: TypeAny})
			}
			// If the field doesn't exist and schema is closed, keep doesn't add it.
		}
	}
	a.schema.fields = newFields
	// After explicit keep, schema becomes closed (only listed fields remain).
}

func (a *analyzer) analyzeDrop(s ast.Stage) {
	if s.Drop == nil {
		return
	}
	for _, p := range s.Drop.Patterns {
		if p.Glob {
			// Remove matching fields.
			var remaining []Field
			for _, f := range a.schema.fields {
				if !globMatch(p.Name, f.Name) {
					remaining = append(remaining, f)
				}
			}
			a.schema.fields = remaining
		} else {
			a.schema.remove(p.Name)
		}
	}
}

func (a *analyzer) analyzeRename(s ast.Stage) {
	if s.Rename == nil {
		return
	}
	for _, r := range s.Rename.Renames {
		a.schema.rename(r.Old, r.New)
	}
}

func (a *analyzer) analyzeStats(s ast.Stage) {
	if s.Stats == nil {
		return
	}
	a.analyzeStatsPayload(s.Stats, s.Pos, true)
}

func (a *analyzer) analyzeEventstats(s ast.Stage) {
	if s.Eventstats == nil {
		return
	}
	a.analyzeStatsPayload(s.Eventstats, s.Pos, false)
}

func (a *analyzer) analyzeStreamstats(s ast.Stage) {
	if s.Streamstats == nil {
		return
	}
	a.analyzeStatsPayload(&s.Streamstats.StatsPayload, s.Pos, false)
}

func (a *analyzer) analyzeStatsPayload(sp *ast.StatsPayload, stageSpan ast.Span, replacesSchema bool) {
	// Validate by-keys exist in the current schema.
	var byFields []Field
	for _, byExpr := range sp.By {
		// Check for bin(_time, d) special case.
		if call, ok := byExpr.(*ast.Call); ok && call.Callee == "bin" {
			if len(call.Args) >= 1 {
				if ident, ok := call.Args[0].(*ast.Ident); ok && ident.Name == "_time" {
					byFields = append(byFields, Field{Name: "_time", Type: TypeTimestamp})
					continue
				}
			}
		}
		// Standard by-key: check field existence.
		if ident, ok := byExpr.(*ast.Ident); ok {
			if _, found := a.schema.lookup(ident.Name); !found && !a.schema.open {
				suggestion := didYouMean(ident.Name, a.allKnownFields())
				a.addDiag("S008", parser.SeverityError, byExpr.ExprSpan(),
					"unknown field '"+ident.Name+"' in group-by clause",
					suggestion)
			}
			typ := a.fieldType(ident.Name)
			byFields = append(byFields, Field{Name: ident.Name, Type: typ})
		} else {
			// Complex by-expression: infer type.
			typ := a.inferExpr(byExpr)
			name := exprFieldName(byExpr)
			byFields = append(byFields, Field{Name: name, Type: typ})
		}
	}

	// Validate and type aggregation expressions.
	savedAggCtx := a.inAggContext
	a.inAggContext = true
	var aggFields []Field
	for _, agg := range sp.Aggs {
		typ := a.inferAgg(agg)
		name := aggAlias(agg)
		aggFields = append(aggFields, Field{Name: name, Type: typ})
		// Type-check where condition.
		if agg.WhereCond != nil {
			a.inferExpr(agg.WhereCond)
		}
	}
	a.inAggContext = savedAggCtx

	if replacesSchema {
		// stats replaces schema with by-keys + agg aliases.
		a.schema.fields = nil
		a.schema.open = false
		for _, f := range byFields {
			a.schema.add(f.Name, f.Type)
		}
		for _, f := range aggFields {
			a.schema.add(f.Name, f.Type)
		}
	} else {
		// eventstats/streamstats add agg fields.
		for _, f := range aggFields {
			a.schema.add(f.Name, f.Type)
		}
	}
}

func (a *analyzer) analyzeSort(s ast.Stage) {
	if s.Sort == nil {
		return
	}
	for _, k := range s.Sort.Keys {
		a.inferExpr(k.Field)
	}
}

func (a *analyzer) analyzeDedup(s ast.Stage) {
	if s.Dedup == nil {
		return
	}
	for _, f := range s.Dedup.Fields {
		a.inferExpr(f)
	}
}

func (a *analyzer) analyzeJoin(s ast.Stage) {
	if s.Join == nil {
		return
	}
	// Analyze the right-side pipeline.
	if s.Join.Right != nil && s.Join.Right.Pipeline != nil {
		rightAnalyzer := &analyzer{
			cat:           a.cat,
			streamingSafe: true,
			schema:        a.initialSchema(),
		}
		rightAnalyzer.analyzePipeline(*s.Join.Right.Pipeline)
		// Merge right pipeline's output schema into current.
		for _, f := range rightAnalyzer.schema.fields {
			if _, exists := a.schema.lookup(f.Name); !exists {
				a.schema.add(f.Name, f.Type)
			}
		}
		// Propagate any diags from the right side.
		a.diags = append(a.diags, rightAnalyzer.diags...)
	}
}

func (a *analyzer) analyzeUnion(s ast.Stage) {
	if s.Union == nil {
		return
	}
	for _, src := range s.Union.Sources {
		if src.Pipeline != nil {
			rightAnalyzer := &analyzer{
				cat:           a.cat,
				streamingSafe: true,
				schema:        a.initialSchema(),
			}
			rightAnalyzer.analyzePipeline(*src.Pipeline)
			// Union schema: merge by name; conflicting known types -> "any".
			for _, f := range rightAnalyzer.schema.fields {
				if existing, ok := a.schema.lookup(f.Name); ok {
					if existing != f.Type && existing != TypeAny && f.Type != TypeAny {
						a.schema.add(f.Name, TypeAny)
					}
				} else {
					a.schema.add(f.Name, f.Type)
				}
			}
			a.diags = append(a.diags, rightAnalyzer.diags...)
		}
	}
}

func (a *analyzer) analyzeExplode(s ast.Stage) {
	if s.Explode == nil {
		return
	}
	// The element field becomes "any".
	if s.Explode.As != "" {
		a.schema.add(s.Explode.As, TypeAny)
	} else {
		// If no alias, the array field name is reused.
		if ident, ok := s.Explode.Array.(*ast.Ident); ok {
			a.schema.add(ident.Name, TypeAny)
		}
	}
}

func (a *analyzer) analyzeDescribe(_ ast.Stage) {
	// describe replaces the schema with its fixed output.
	a.schema.fields = []Field{
		{Name: "field", Type: TypeString},
		{Name: "type", Type: TypeString},
		{Name: "coverage", Type: TypeFloat},
		{Name: "distinct_est", Type: TypeInt},
		{Name: "top_values", Type: TypeArray},
	}
	a.schema.open = false
}

func (a *analyzer) analyzeParse(s ast.Stage) {
	if s.Parse == nil {
		return
	}
	if len(s.Parse.Into) > 0 {
		// With into: add typed capture fields.
		for _, c := range s.Parse.Into {
			typ := TypeAny
			if c.Type != "" {
				typ = captureType(c.Type)
			}
			a.schema.add(c.Name, typ)
		}
	} else {
		// Without into: schema becomes OPEN (dynamic parse outputs).
		a.schema.open = true
	}
}

// ---------------------------------------------------------------------------
// Helper stage analyzers
// ---------------------------------------------------------------------------

func (a *analyzer) analyzeCompare(_ ast.Stage) {
	// compare adds previous_*/change_* fields as "any".
	for _, f := range a.schema.fields {
		a.schema.add("previous_"+f.Name, TypeAny)
		a.schema.add("change_"+f.Name, TypeAny)
	}
}

func (a *analyzer) analyzePatterns(_ ast.Stage) {
	a.schema.add("_pattern", TypeString)
	a.schema.add("_pattern_count", TypeInt)
}

func (a *analyzer) analyzeOutliers(_ ast.Stage) {
	a.schema.add("_outlier", TypeBool)
	a.schema.add("_outlier_score", TypeFloat)
}

func (a *analyzer) analyzeSessionize(_ ast.Stage) {
	a.schema.add("_session_id", TypeString)
	a.schema.add("_session_start", TypeTimestamp)
	a.schema.add("_session_end", TypeTimestamp)
}

func (a *analyzer) analyzeTransaction(_ ast.Stage) {
	a.schema.add("duration", TypeDuration)
	a.schema.add("eventcount", TypeInt)
}

func (a *analyzer) analyzeTrace(_ ast.Stage) {
	a.schema.add("_depth", TypeInt)
	a.schema.add("_tree", TypeString)
}

func (a *analyzer) analyzeTopology(_ ast.Stage) {
	a.schema.add("_source_node", TypeString)
	a.schema.add("_dest_node", TypeString)
	a.schema.add("_edge_weight", TypeFloat)
}

func (a *analyzer) analyzeCorrelate(_ ast.Stage) {
	a.schema.add("_correlation", TypeFloat)
}

func (a *analyzer) analyzeRollup(_ ast.Stage) {
	a.schema.add("_resolution", TypeDuration)
}

func (a *analyzer) analyzeXyseries(_ ast.Stage) {
	// xyseries pivots rows; output schema is dynamic.
	a.schema.open = true
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// allKnownFields returns all field names from the schema and catalog,
// deduplicated.
func (a *analyzer) allKnownFields() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range a.schema.fields {
		if !seen[f.Name] {
			out = append(out, f.Name)
			seen[f.Name] = true
		}
	}
	for _, f := range a.cat.Fields() {
		if !seen[f] {
			out = append(out, f)
			seen[f] = true
		}
	}
	return out
}

// fieldType returns the type of a field from the schema (or catalog).
func (a *analyzer) fieldType(name string) FieldType {
	if typ, ok := a.schema.lookup(name); ok {
		return typ
	}
	if typ, ok := a.cat.Lookup(name); ok {
		return FieldType(typ)
	}
	return TypeAny
}

// captureType converts a parse-into type name to a FieldType.
func captureType(typ string) FieldType {
	switch strings.ToLower(typ) {
	case "string":
		return TypeString
	case "int":
		return TypeInt
	case "float":
		return TypeFloat
	case "bool":
		return TypeBool
	case "timestamp":
		return TypeTimestamp
	case "duration":
		return TypeDuration
	case "array":
		return TypeArray
	case "object":
		return TypeObject
	default:
		return TypeAny
	}
}

// aggAlias returns the output field name for an aggregate expression.
func aggAlias(agg ast.AggExpr) string {
	if agg.Alias != "" {
		return agg.Alias
	}
	if call, ok := agg.Func.(*ast.Call); ok {
		if len(call.Args) > 0 {
			return call.Callee + "(" + exprFieldName(call.Args[0]) + ")"
		}
		return call.Callee + "()"
	}
	return "?"
}

// exprFieldName extracts a simple field name from an expression.
func exprFieldName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.Member:
		return exprFieldName(x.Object) + "." + x.Field
	case *ast.Call:
		if len(x.Args) > 0 {
			return x.Callee + "(" + exprFieldName(x.Args[0]) + ")"
		}
		return x.Callee + "()"
	}
	return e.String()
}

// globMatch performs simple glob matching (supports * and ?).
func globMatch(pattern, name string) bool {
	return matchGlob(pattern, name)
}

func matchGlob(pattern, name string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			// Skip consecutive stars.
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if matchGlob(pattern, name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		default:
			if len(name) == 0 || pattern[0] != name[0] {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		}
	}
	return len(name) == 0
}
