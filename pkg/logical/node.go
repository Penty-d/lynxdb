package logical

import (
	"fmt"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/sema"
)

// ---------------------------------------------------------------------------
// Node interface
// ---------------------------------------------------------------------------

// Node is a single operator in the logical plan tree.
//
// The tree is linear-with-branches: most nodes have exactly one input child.
// Scan has zero children, Join has left+Right, Union has N children.
type Node interface {
	// Children returns the input children of this node. For most nodes this
	// is a single-element slice (the input). Scan returns nil. Union returns
	// N elements. Join returns only the left child; the Right pipeline is
	// accessed via the Join.Right field.
	Children() []Node

	// SetChildren replaces the input children. The caller must provide the
	// same number of children as returned by Children (panics otherwise).
	SetChildren([]Node)

	// Schema returns the output schema of this node. The schema is computed
	// from the input schema and the node's operation. Callers should not
	// mutate the returned slice.
	Schema() []sema.Field

	// String returns a one-line summary of this node suitable for plan
	// rendering (e.g. "Filter(status >= 500)").
	String() string
}

// ---------------------------------------------------------------------------
// Common base: stores a single input child.
// ---------------------------------------------------------------------------

// unaryNode is embedded by nodes with exactly one input child.
type unaryNode struct {
	Input Node
}

func (u *unaryNode) Children() []Node {
	if u.Input == nil {
		return nil
	}
	return []Node{u.Input}
}

func (u *unaryNode) SetChildren(cs []Node) {
	if len(cs) != 1 {
		panic(fmt.Sprintf("logical: unaryNode.SetChildren: want 1, got %d", len(cs)))
	}
	u.Input = cs[0]
}

// inputSchema returns the schema of the input child, or nil.
func (u *unaryNode) inputSchema() []sema.Field {
	if u.Input == nil {
		return nil
	}
	return u.Input.Schema()
}

// ---------------------------------------------------------------------------
// Scan
// ---------------------------------------------------------------------------

// SourcePattern is a single source reference in a Scan node.
type SourcePattern struct {
	Kind    ast.SourceAtomKind
	Name    string
	Pattern string
}

// TimeBounds represents a time range constraint. Start/End store the ORIGINAL
// AST expression text so the plan is cacheable (not wall-clock resolved).
// For relative durations like -1h, the ast.Expr is preserved as-is.
type TimeBounds struct {
	Start ast.Expr // nil for open-ended
	End   ast.Expr // nil for open-ended
	Snap  string   // snap suffix like "@d" (empty if none)
}

// Pushdown carries storage-level hints. Empty in this PR; fields land in
// PR (c) when the optimizer rules are ported.
type Pushdown struct {
	TimeBounds      *TimeBounds
	FieldPredicates []ast.Expr
	BloomTerms      []string
	RawTerms        []string
}

// Scan reads events from one or more sources.
type Scan struct {
	Sources   []SourcePattern
	TimeRange *TimeBounds
	Pushdown  Pushdown
	// OutputSchema is set during lowering from the initial catalog schema.
	OutputSchema []sema.Field
}

func (n *Scan) Children() []Node      { return nil }
func (n *Scan) SetChildren(cs []Node) { mustEmpty(cs) }
func (n *Scan) Schema() []sema.Field  { return n.OutputSchema }

func (n *Scan) String() string {
	var b strings.Builder
	b.WriteString("Scan(")
	for i, s := range n.Sources {
		if i > 0 {
			b.WriteString(", ")
		}
		switch s.Kind {
		case ast.SourceStar:
			b.WriteByte('*')
		case ast.SourceCTE:
			b.WriteString("$" + s.Name)
		case ast.SourceNegated:
			b.WriteString("!" + s.Pattern)
		case ast.SourceGlob:
			b.WriteString(s.Pattern)
		default:
			b.WriteString(s.Name)
		}
	}
	if n.TimeRange != nil {
		b.WriteString(", ")
		b.WriteString(timeBoundsString(n.TimeRange))
	}
	b.WriteByte(')')
	return b.String()
}

func timeBoundsString(tb *TimeBounds) string {
	if tb == nil {
		return ""
	}
	var b strings.Builder
	b.WriteByte('[')
	if tb.Start != nil {
		b.WriteString(tb.Start.String())
	}
	if tb.End != nil {
		b.WriteString("..")
		b.WriteString(tb.End.String())
	}
	b.WriteByte(']')
	if tb.Snap != "" {
		b.WriteString("[@")
		b.WriteString(tb.Snap)
		b.WriteByte(']')
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Filter
// ---------------------------------------------------------------------------

// Filter applies a boolean predicate to its input.
type Filter struct {
	unaryNode
	Expr ast.Expr
}

func (n *Filter) Schema() []sema.Field { return n.inputSchema() }
func (n *Filter) String() string {
	return "Filter(" + n.Expr.String() + ")"
}

// ---------------------------------------------------------------------------
// Parse
// ---------------------------------------------------------------------------

// Capture is a typed capture field in a Parse node.
type Capture struct {
	Name string
	Type string // empty if untyped
}

// Parse extracts structure from a field (default _raw).
type Parse struct {
	unaryNode
	Format   string // json, logfmt, regex, pattern, ...
	FirstOf  []string
	From     string // field name to parse from (empty = _raw)
	Captures []Capture
	Prefix   string
	OnError  string // propagate, null, drop, strict
	// cachedSchema is lazily built from input + captures.
	cachedSchema []sema.Field
}

func (n *Parse) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	base := copySchema(n.inputSchema())
	for _, c := range n.Captures {
		typ := sema.TypeAny
		if c.Type != "" {
			typ = captureType(c.Type)
		}
		base = addField(base, c.Name, typ)
	}
	// Without captures, schema is open (dynamic output); we still return
	// the base fields but mark nothing special here.
	n.cachedSchema = base
	return n.cachedSchema
}

func (n *Parse) String() string {
	var b strings.Builder
	b.WriteString("Parse(")
	if len(n.FirstOf) > 0 {
		b.WriteString("first_of(")
		b.WriteString(strings.Join(n.FirstOf, ", "))
		b.WriteByte(')')
	} else {
		b.WriteString(n.Format)
	}
	if n.From != "" {
		b.WriteString(", from=")
		b.WriteString(n.From)
	}
	b.WriteByte(')')
	return b.String()
}

// ---------------------------------------------------------------------------
// Project (unified keep/drop/rename)
// ---------------------------------------------------------------------------

// ProjectAction classifies what a ProjectCol does.
type ProjectAction uint8

const (
	ProjectKeep   ProjectAction = iota // retain this column
	ProjectDrop                        // remove this column
	ProjectRename                      // rename From -> Name
)

// ProjectCol is a single column operation in a Project node.
type ProjectCol struct {
	Action ProjectAction
	Name   string // target name (for keep/rename)
	From   string // source name (for rename); also used as name for keep/drop
	Glob   bool   // true when Name is a glob pattern
	// StarExcept means this project was a `keep * except ...` where the
	// entries are drops.
	StarExcept bool
}

// Project unifies keep, drop, and rename into a single projection operator.
// Consecutive keep/drop/rename stages in the AST are fused into one Project.
type Project struct {
	unaryNode
	Cols         []ProjectCol
	cachedSchema []sema.Field
}

func (n *Project) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	n.cachedSchema = computeProjectSchema(n.inputSchema(), n.Cols)
	return n.cachedSchema
}

func (n *Project) String() string {
	var parts []string
	for _, c := range n.Cols {
		switch c.Action {
		case ProjectKeep:
			parts = append(parts, c.Name)
		case ProjectDrop:
			parts = append(parts, "-"+c.Name)
		case ProjectRename:
			parts = append(parts, c.From+"->"+c.Name)
		}
	}
	return "Project(" + strings.Join(parts, ", ") + ")"
}

// ---------------------------------------------------------------------------
// Extend
// ---------------------------------------------------------------------------

// Assignment is a name = expr pair.
type Assignment struct {
	Name  string
	Value ast.Expr
}

// Extend adds or replaces computed columns.
type Extend struct {
	unaryNode
	Assignments  []Assignment
	cachedSchema []sema.Field
}

func (n *Extend) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	base := copySchema(n.inputSchema())
	for _, a := range n.Assignments {
		base = addField(base, a.Name, sema.TypeAny) // TODO: infer from expr
	}
	n.cachedSchema = base
	return n.cachedSchema
}

func (n *Extend) String() string {
	var parts []string
	for _, a := range n.Assignments {
		parts = append(parts, a.Name+"="+a.Value.String())
	}
	return "Extend(" + strings.Join(parts, ", ") + ")"
}

// ---------------------------------------------------------------------------
// Aggregate
// ---------------------------------------------------------------------------

// Agg is a single aggregate expression.
type Agg struct {
	Func      ast.Expr // the aggregate call (typically *ast.Call)
	WhereCond ast.Expr // conditional: count(where p)
	Alias     string
}

// Key is a group-by key.
type Key struct {
	Expr ast.Expr
	Name string // resolved name for the output column
}

// TimeBin is an extracted bin(_time, d) from the group-by list.
type TimeBin struct {
	Duration ast.Expr // the duration expression
}

// WindowVariant distinguishes plain stats from eventstats/streamstats.
type WindowVariant uint8

const (
	WindowNone        WindowVariant = iota // plain stats
	WindowEventstats                       // eventstats
	WindowStreamstats                      // streamstats
)

// WindowSpec carries extra options for eventstats/streamstats variants.
type WindowSpec struct {
	Variant WindowVariant
	Window  *int  // streamstats window (nil = unbounded)
	Current *bool // streamstats current row inclusion
}

// Aggregate groups and aggregates its input.
type Aggregate struct {
	unaryNode
	Aggs    []Agg
	Keys    []Key
	TimeBin *TimeBin
	Partial bool
	Window  *WindowSpec // nil = plain stats
	// cachedSchema built from keys + aggs
	cachedSchema []sema.Field
}

func (n *Aggregate) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	if n.Window != nil {
		// eventstats/streamstats: input schema + agg fields
		base := copySchema(n.inputSchema())
		for _, a := range n.Aggs {
			name := a.Alias
			if name == "" {
				name = aggAutoName(a)
			}
			base = addField(base, name, sema.TypeAny)
		}
		n.cachedSchema = base
		return n.cachedSchema
	}
	// plain stats: keys + aggs replace schema
	var fields []sema.Field
	if n.TimeBin != nil {
		fields = append(fields, sema.Field{Name: "_time", Type: sema.TypeTimestamp})
	}
	for _, k := range n.Keys {
		fields = append(fields, sema.Field{Name: k.Name, Type: sema.TypeAny})
	}
	for _, a := range n.Aggs {
		name := a.Alias
		if name == "" {
			name = aggAutoName(a)
		}
		fields = append(fields, sema.Field{Name: name, Type: sema.TypeAny})
	}
	n.cachedSchema = fields
	return n.cachedSchema
}

func (n *Aggregate) String() string {
	var b strings.Builder
	b.WriteString("Aggregate(")
	for i, a := range n.Aggs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Func.String())
		if a.Alias != "" {
			b.WriteString(" as ")
			b.WriteString(a.Alias)
		}
	}
	if len(n.Keys) > 0 || n.TimeBin != nil {
		b.WriteString(" by ")
		first := true
		for _, k := range n.Keys {
			if !first {
				b.WriteString(", ")
			}
			b.WriteString(k.Name)
			first = false
		}
		if n.TimeBin != nil {
			if !first {
				b.WriteString(", ")
			}
			b.WriteString("bin(_time, ")
			b.WriteString(n.TimeBin.Duration.String())
			b.WriteByte(')')
		}
	}
	if n.Window != nil {
		switch n.Window.Variant {
		case WindowEventstats:
			b.WriteString(" [eventstats]")
		case WindowStreamstats:
			b.WriteString(" [streamstats")
			if n.Window.Window != nil {
				b.WriteString(fmt.Sprintf(" window=%d", *n.Window.Window))
			}
			b.WriteByte(']')
		}
	}
	b.WriteByte(')')
	return b.String()
}

// ---------------------------------------------------------------------------
// TopK (fused sort + head)
// ---------------------------------------------------------------------------

// SortKey is a sort key with direction.
type SortKey struct {
	Expr ast.Expr
	Desc bool
}

// TopK is a fused sort+head operator.
type TopK struct {
	unaryNode
	K        int64
	SortKeys []SortKey
}

func (n *TopK) Schema() []sema.Field { return n.inputSchema() }
func (n *TopK) String() string {
	var keys []string
	for _, k := range n.SortKeys {
		prefix := "+"
		if k.Desc {
			prefix = "-"
		}
		keys = append(keys, prefix+k.Expr.String())
	}
	return fmt.Sprintf("TopK(%d, %s)", n.K, strings.Join(keys, ", "))
}

// ---------------------------------------------------------------------------
// Sort
// ---------------------------------------------------------------------------

// Sort orders its input by one or more keys.
type Sort struct {
	unaryNode
	Keys []SortKey
}

func (n *Sort) Schema() []sema.Field { return n.inputSchema() }
func (n *Sort) String() string {
	var keys []string
	for _, k := range n.Keys {
		prefix := "+"
		if k.Desc {
			prefix = "-"
		}
		keys = append(keys, prefix+k.Expr.String())
	}
	return "Sort(" + strings.Join(keys, ", ") + ")"
}

// ---------------------------------------------------------------------------
// Limit
// ---------------------------------------------------------------------------

// Limit returns the first (or last, if Tail) N rows.
type Limit struct {
	unaryNode
	N    int64
	Tail bool
}

func (n *Limit) Schema() []sema.Field { return n.inputSchema() }
func (n *Limit) String() string {
	if n.Tail {
		return fmt.Sprintf("Limit(tail %d)", n.N)
	}
	return fmt.Sprintf("Limit(%d)", n.N)
}

// ---------------------------------------------------------------------------
// Dedup
// ---------------------------------------------------------------------------

// Dedup keeps the first N rows per unique key combination.
type Dedup struct {
	unaryNode
	N      int64
	Fields []string
}

func (n *Dedup) Schema() []sema.Field { return n.inputSchema() }
func (n *Dedup) String() string {
	return fmt.Sprintf("Dedup(%d, %s)", n.N, strings.Join(n.Fields, ", "))
}

// ---------------------------------------------------------------------------
// Join
// ---------------------------------------------------------------------------

// Join combines the left input with a right sub-plan.
type Join struct {
	unaryNode             // Left input
	Type         string   // "inner", "left", "outer"
	On           []string // join key field names
	Right        Node     // right-side sub-plan
	cachedSchema []sema.Field
}

func (n *Join) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	left := copySchema(n.inputSchema())
	if n.Right != nil {
		for _, f := range n.Right.Schema() {
			left = addFieldIfAbsent(left, f.Name, f.Type)
		}
	}
	n.cachedSchema = left
	return n.cachedSchema
}

func (n *Join) String() string {
	return fmt.Sprintf("Join(%s, on=[%s])", n.Type, strings.Join(n.On, ", "))
}

// ---------------------------------------------------------------------------
// Union
// ---------------------------------------------------------------------------

// Union appends rows from N inputs. Schemas merge by name with null-padding.
type Union struct {
	Inputs       []Node
	cachedSchema []sema.Field
}

func (n *Union) Children() []Node { return n.Inputs }
func (n *Union) SetChildren(cs []Node) {
	n.Inputs = cs
	n.cachedSchema = nil
}

func (n *Union) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	if len(n.Inputs) == 0 {
		return nil
	}
	merged := copySchema(n.Inputs[0].Schema())
	for _, inp := range n.Inputs[1:] {
		for _, f := range inp.Schema() {
			merged = addFieldIfAbsent(merged, f.Name, f.Type)
		}
	}
	n.cachedSchema = merged
	return n.cachedSchema
}

func (n *Union) String() string {
	return fmt.Sprintf("Union(%d inputs)", len(n.Inputs))
}

// ---------------------------------------------------------------------------
// Explode
// ---------------------------------------------------------------------------

// Explode produces one row per array element.
type Explode struct {
	unaryNode
	Field        string
	As           string // alias for element (empty = reuse field name)
	cachedSchema []sema.Field
}

func (n *Explode) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	base := copySchema(n.inputSchema())
	name := n.As
	if name == "" {
		name = n.Field
	}
	base = addField(base, name, sema.TypeAny)
	n.cachedSchema = base
	return n.cachedSchema
}

func (n *Explode) String() string {
	if n.As != "" {
		return fmt.Sprintf("Explode(%s as %s)", n.Field, n.As)
	}
	return fmt.Sprintf("Explode(%s)", n.Field)
}

// ---------------------------------------------------------------------------
// Describe
// ---------------------------------------------------------------------------

// Describe emits stream schema/coverage summary.
type Describe struct {
	unaryNode
}

func (n *Describe) Schema() []sema.Field {
	return []sema.Field{
		{Name: "field", Type: sema.TypeString},
		{Name: "type", Type: sema.TypeString},
		{Name: "coverage", Type: sema.TypeFloat},
		{Name: "distinct_est", Type: sema.TypeInt},
		{Name: "top_values", Type: sema.TypeArray},
	}
}

func (n *Describe) String() string { return "Describe()" }

// ---------------------------------------------------------------------------
// Helper (passthrough for investigation helpers)
// ---------------------------------------------------------------------------

// Helper is a passthrough node for investigation operators (patterns, compare,
// outliers, sessionize, transaction, trace, topology, correlate, rollup,
// xyseries) that are preserved as-is for the physical planner.
type Helper struct {
	unaryNode
	Name       string
	Options    map[string]ast.Expr
	Positional []ast.Expr
	// extraFields are fields this helper adds to the schema.
	extraFields  []sema.Field
	cachedSchema []sema.Field
}

func (n *Helper) Schema() []sema.Field {
	if n.cachedSchema != nil {
		return n.cachedSchema
	}
	base := copySchema(n.inputSchema())
	for _, f := range n.extraFields {
		base = addField(base, f.Name, f.Type)
	}
	n.cachedSchema = base
	return n.cachedSchema
}

func (n *Helper) String() string {
	return "Helper(" + n.Name + ")"
}

// ---------------------------------------------------------------------------
// Materialize
// ---------------------------------------------------------------------------

// Materialize creates a materialized view from its input.
type Materialize struct {
	unaryNode
	Name        string
	Retention   string // duration text
	PartitionBy []string
}

func (n *Materialize) Schema() []sema.Field { return n.inputSchema() }
func (n *Materialize) String() string {
	return fmt.Sprintf("Materialize(%q)", n.Name)
}

// ---------------------------------------------------------------------------
// Tee
// ---------------------------------------------------------------------------

// Tee sends a copy of the stream to a side sink.
type Tee struct {
	unaryNode
	Sink string
}

func (n *Tee) Schema() []sema.Field { return n.inputSchema() }
func (n *Tee) String() string {
	return fmt.Sprintf("Tee(%q)", n.Sink)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustEmpty(cs []Node) {
	if len(cs) != 0 {
		panic(fmt.Sprintf("logical: SetChildren on leaf node: want 0, got %d", len(cs)))
	}
}

func copySchema(in []sema.Field) []sema.Field {
	if in == nil {
		return nil
	}
	out := make([]sema.Field, len(in))
	copy(out, in)
	return out
}

func addField(schema []sema.Field, name string, typ sema.FieldType) []sema.Field {
	for i, f := range schema {
		if f.Name == name {
			schema[i].Type = typ
			return schema
		}
	}
	return append(schema, sema.Field{Name: name, Type: typ})
}

func addFieldIfAbsent(schema []sema.Field, name string, typ sema.FieldType) []sema.Field {
	for _, f := range schema {
		if f.Name == name {
			return schema
		}
	}
	return append(schema, sema.Field{Name: name, Type: typ})
}

func aggAutoName(a Agg) string {
	if call, ok := a.Func.(*ast.Call); ok {
		if len(call.Args) > 0 {
			return call.Callee + "(" + call.Args[0].String() + ")"
		}
		return call.Callee + "()"
	}
	return "?"
}

func captureType(typ string) sema.FieldType {
	switch strings.ToLower(typ) {
	case "string":
		return sema.TypeString
	case "int":
		return sema.TypeInt
	case "float":
		return sema.TypeFloat
	case "bool":
		return sema.TypeBool
	case "timestamp":
		return sema.TypeTimestamp
	case "duration":
		return sema.TypeDuration
	case "array":
		return sema.TypeArray
	case "object":
		return sema.TypeObject
	default:
		return sema.TypeAny
	}
}

func computeProjectSchema(input []sema.Field, cols []ProjectCol) []sema.Field {
	// Determine the overall operation mode.
	// If any col is a keep, we build a keep-list.
	// If all cols are drops, we drop from the input.
	// If there are renames, apply them.
	hasKeep := false
	hasDrop := false
	hasStarExcept := false
	for _, c := range cols {
		switch c.Action {
		case ProjectKeep:
			hasKeep = true
			if c.StarExcept {
				hasStarExcept = true
			}
		case ProjectDrop:
			hasDrop = true
		}
	}

	if hasStarExcept || (hasDrop && !hasKeep) {
		// Drop mode: start from input, remove named fields.
		result := copySchema(input)
		for _, c := range cols {
			if c.Action == ProjectDrop || (c.Action == ProjectKeep && c.StarExcept) {
				// Actually the StarExcept drops are stored as keep cols
				// with StarExcept=true, but the names are the ones to drop.
				// Let's handle this properly: if StarExcept, keep all EXCEPT named.
			}
			if c.Action == ProjectDrop {
				if c.Glob {
					result = removeGlobFields(result, c.Name)
				} else {
					result = removeField(result, c.Name)
				}
			}
		}
		// Apply renames.
		for _, c := range cols {
			if c.Action == ProjectRename {
				result = renameField(result, c.From, c.Name)
			}
		}
		return result
	}

	if hasKeep {
		// Keep mode: build from the keep list.
		var result []sema.Field
		for _, c := range cols {
			if c.Action == ProjectKeep {
				if c.Glob {
					for _, f := range input {
						if globMatch(c.Name, f.Name) {
							result = addFieldIfAbsent(result, f.Name, f.Type)
						}
					}
				} else {
					typ := findFieldType(input, c.Name)
					result = addFieldIfAbsent(result, c.Name, typ)
				}
			}
		}
		// Apply renames.
		for _, c := range cols {
			if c.Action == ProjectRename {
				result = renameField(result, c.From, c.Name)
			}
		}
		return result
	}

	// Only renames.
	result := copySchema(input)
	for _, c := range cols {
		if c.Action == ProjectRename {
			result = renameField(result, c.From, c.Name)
		}
	}
	return result
}

func removeField(schema []sema.Field, name string) []sema.Field {
	for i, f := range schema {
		if f.Name == name {
			return append(schema[:i], schema[i+1:]...)
		}
	}
	return schema
}

func removeGlobFields(schema []sema.Field, pattern string) []sema.Field {
	var result []sema.Field
	for _, f := range schema {
		if !globMatch(pattern, f.Name) {
			result = append(result, f)
		}
	}
	return result
}

func renameField(schema []sema.Field, from, to string) []sema.Field {
	for i, f := range schema {
		if f.Name == from {
			schema[i].Name = to
			return schema
		}
	}
	return schema
}

func findFieldType(schema []sema.Field, name string) sema.FieldType {
	for _, f := range schema {
		if f.Name == name {
			return f.Type
		}
	}
	return sema.TypeAny
}

// globMatch does simple glob matching (supports * and ?).
func globMatch(pattern, name string) bool {
	return matchGlob(pattern, name)
}

func matchGlob(pattern, name string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
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
