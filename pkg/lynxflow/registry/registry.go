// Package registry is the single source of truth for the LynxFlow v2 language
// surface (RFC-002 §12): stage operators, scalar functions, and aggregate
// functions. It contains declarative data only — no parsing or execution
// logic. The parser, desugarer, semantic analyzer, VM compiler, autocomplete
// catalogs (/api/v1/catalog), EBNF generator, and docs generator all consume
// this package; hand-maintained duplicates are not allowed.
package registry

// Class describes how a stage operator relates to the core language.
type Class string

const (
	// ClassSource is the scan stage (`from`), only valid first in a pipeline.
	ClassSource Class = "source"
	// ClassCore stages are the orthogonal relation->relation core (RFC-002 §8).
	ClassCore Class = "core"
	// ClassSugar stages have exactly one mechanical desugaring to core stages,
	// recorded as a Rewrite and visible via --show-rewritten (RFC-002 §9.1).
	ClassSugar Class = "sugar"
	// ClassHelper stages are investigation operators with dedicated runtime
	// support and declared cost (RFC-002 §9.2).
	ClassHelper Class = "helper"
	// ClassManagement stages are not relation transforms (materialize, tee, use).
	ClassManagement Class = "management"
)

// Streaming classifies a stage for live tail and planning.
type Streaming string

const (
	// StreamingRow stages are row-at-a-time safe for live tail.
	StreamingRow Streaming = "row"
	// StreamingAcc stages accumulate (block) before emitting.
	StreamingAcc Streaming = "acc"
)

// ArgType is the surface type of a positional argument or option.
type ArgType string

const (
	ArgExpr          ArgType = "expr"           // any expression
	ArgPredicate     ArgType = "predicate"      // boolean expression
	ArgField         ArgType = "field"          // single field reference
	ArgFieldList     ArgType = "field_list"     // f1, f2, ...
	ArgFieldPatterns ArgType = "field_patterns" // names, globs, `* except ...`
	ArgAssignList    ArgType = "assign_list"    // x = expr, y = expr, ...
	ArgAggList       ArgType = "agg_list"       // agg() [as name], ...
	ArgSortList      ArgType = "sort_list"      // -f, +f, f
	ArgInt           ArgType = "int"
	ArgString        ArgType = "string"
	ArgDuration      ArgType = "duration"
	ArgFormat        ArgType = "format"       // parse format spec, incl. first_of(...)
	ArgCaptures      ArgType = "captures"     // into (name [as type], ...)
	ArgSubPipeline   ArgType = "sub_pipeline" // $cte or [ <pipeline> ]
	ArgEnum          ArgType = "enum"
	ArgBool          ArgType = "bool"
)

// Positional describes one positional argument of a stage operator.
type Positional struct {
	Name     string
	Type     ArgType
	Required bool
	Variadic bool
	Doc      string
}

// Option describes one key=value option of a stage operator.
type Option struct {
	Name     string
	Type     ArgType
	Required bool
	Default  string // canonical literal text; empty means no default
	Enum     []string
	Doc      string
}

// Operator declares one pipeline stage.
type Operator struct {
	Name        string
	Class       Class
	Streaming   Streaming
	Positionals []Positional
	Options     []Option
	// DesugarsTo is the human-readable expansion template for ClassSugar
	// operators (RFC-002 §9.1). Empty for all other classes.
	DesugarsTo string
	Doc        string
	Examples   []string
}

// ValueType is a LynxFlow value type (RFC-002 §5.1) for function signatures.
type ValueType string

const (
	TAny       ValueType = "any"
	TString    ValueType = "string"
	TInt       ValueType = "int"
	TFloat     ValueType = "float"
	TNumber    ValueType = "number" // int or float
	TBool      ValueType = "bool"
	TTimestamp ValueType = "timestamp"
	TDuration  ValueType = "duration"
	TArray     ValueType = "array"
	TObject    ValueType = "object"
	TRegex     ValueType = "regex" // raw-string regex argument
	TLambda    ValueType = "lambda"
)

// Fallibility classifies how a scalar function fails (RFC-002 §10).
type Fallibility string

const (
	Infallible    Fallibility = "infallible"
	NullOnFailure Fallibility = "null_on_failure"
)

// Param describes one parameter of a function or aggregate.
type Param struct {
	Name     string
	Type     ValueType
	Optional bool
	Variadic bool
}

// Function declares one scalar function.
type Function struct {
	Name        string
	Category    string
	Params      []Param
	Result      ValueType
	Fallibility Fallibility
	// StrictVariant means the `name!` spelling exists and fails the query
	// with row context instead of yielding null.
	StrictVariant bool
	Doc           string
}

// Aggregate declares one aggregate or window function.
type Aggregate struct {
	Name   string
	Params []Param
	// SupportsWhere means a trailing `where <predicate>` argument is accepted
	// (conditional aggregate, RFC-002 §10).
	SupportsWhere bool
	// WindowOnly aggregates are valid only in streamstats context.
	WindowOnly bool
	Result     ValueType
	Doc        string
}

// Operators returns all stage operators in canonical (documentation) order.
func Operators() []Operator { return operators }

// Functions returns all scalar functions in canonical order.
func Functions() []Function { return functions }

// Aggregates returns all aggregate and window functions in canonical order.
func Aggregates() []Aggregate { return aggregates }

// LookupOperator finds a stage operator by lowercase name.
func LookupOperator(name string) (Operator, bool) {
	for _, op := range operators {
		if op.Name == name {
			return op, true
		}
	}
	return Operator{}, false
}

// LookupFunction finds a scalar function by lowercase name.
func LookupFunction(name string) (Function, bool) {
	for _, fn := range functions {
		if fn.Name == name {
			return fn, true
		}
	}
	return Function{}, false
}

// LookupAggregate finds an aggregate function by lowercase name.
func LookupAggregate(name string) (Aggregate, bool) {
	for _, ag := range aggregates {
		if ag.Name == name {
			return ag, true
		}
	}
	return Aggregate{}, false
}

// ParseFormats is the closed set of parse-stage formats (RFC-002 §7.1).
// first_of is a combinator over these, not a format itself.
func ParseFormats() []string {
	return []string{
		"json", "logfmt", "kv", "pattern", "regex",
		"syslog", "combined", "clf", "nginx_error", "cef", "docker",
		"redis", "apache_error", "postgres", "mysql_slow", "haproxy",
		"leef", "w3c",
	}
}
