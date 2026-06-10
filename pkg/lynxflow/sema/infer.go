package sema

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/registry"
)

// Semantic diagnostic codes (S001+).
const (
	// CodeUnknownField is emitted when a field reference is not found in a
	// closed schema or the catalog.
	CodeUnknownField parser.DiagCode = "S001"

	// CodeTypeMismatchBinop is emitted when a binary operator has
	// incompatible operand types (e.g. string + int).
	CodeTypeMismatchBinop parser.DiagCode = "S002"

	// CodeTypeMismatchComparison is emitted when a comparison has
	// incompatible known types on both sides.
	CodeTypeMismatchComparison parser.DiagCode = "S003"

	// CodeNonBoolLogical is emitted when and/or/not operands are known
	// non-bool.
	CodeNonBoolLogical parser.DiagCode = "S004"

	// CodeAggOutsideStats is emitted when an aggregate function is used
	// outside of stats/eventstats/streamstats.
	CodeAggOutsideStats parser.DiagCode = "S005"

	// CodeUnknownFunction is emitted when a function call references an
	// unknown function.
	CodeUnknownFunction parser.DiagCode = "S006"

	// CodeWrongArity is emitted when a function call has the wrong number
	// of arguments.
	CodeWrongArity parser.DiagCode = "S007"

	// CodeUnknownByField is emitted when a stats by-key references a field
	// not in the schema.
	CodeUnknownByField parser.DiagCode = "S008"
)

// inferExpr returns the inferred type of an expression, emitting diagnostics
// for type errors.
func (a *analyzer) inferExpr(e ast.Expr) FieldType {
	if e == nil {
		return TypeAny
	}
	switch x := e.(type) {
	case *ast.Literal:
		return a.inferLiteral(x)
	case *ast.Ident:
		return a.inferIdent(x)
	case *ast.Binary:
		return a.inferBinary(x)
	case *ast.Unary:
		return a.inferUnary(x)
	case *ast.Call:
		return a.inferCall(x)
	case *ast.In:
		a.inferExpr(x.LHS)
		a.inferExpr(x.RHS)
		return TypeBool
	case *ast.Between:
		a.inferExpr(x.X)
		a.inferExpr(x.Lo)
		a.inferExpr(x.Hi)
		return TypeBool
	case *ast.Member:
		a.inferExpr(x.Object)
		return TypeAny
	case *ast.SafeMember:
		a.inferExpr(x.Object)
		return TypeAny
	case *ast.Index:
		a.inferExpr(x.Object)
		a.inferExpr(x.Idx)
		return TypeAny
	case *ast.Lambda:
		// Lambda body: infer with param bound to "any".
		a.inferExpr(x.Body)
		return TypeAny
	case *ast.Paren:
		return a.inferExpr(x.Inner)
	case *ast.Array:
		for _, elem := range x.Elems {
			a.inferExpr(elem)
		}
		return TypeArray
	case *ast.Object:
		for _, entry := range x.Entries {
			a.inferExpr(entry.Value)
		}
		return TypeObject
	case *ast.ErrorExpr:
		return TypeAny
	}
	return TypeAny
}

func (a *analyzer) inferLiteral(lit *ast.Literal) FieldType {
	switch lit.Kind {
	case ast.LitString, ast.LitRawString:
		return TypeString
	case ast.LitInt:
		return TypeInt
	case ast.LitFloat:
		return TypeFloat
	case ast.LitBool:
		return TypeBool
	case ast.LitNull:
		return TypeAny
	case ast.LitDuration:
		return TypeDuration
	}
	return TypeAny
}

func (a *analyzer) inferIdent(id *ast.Ident) FieldType {
	// Check schema.
	if typ, ok := a.schema.lookup(id.Name); ok {
		return typ
	}
	// Check catalog.
	if typ, ok := a.cat.Lookup(id.Name); ok {
		return FieldType(typ)
	}
	// Open schema -> "any" without diagnostic.
	if a.schema.open {
		return TypeAny
	}
	// Closed schema -> unknown field error with did-you-mean.
	suggestion := didYouMean(id.Name, a.allKnownFields())
	a.addDiag(CodeUnknownField, parser.SeverityError, id.Pos,
		"unknown field '"+id.Name+"'",
		suggestion)
	return TypeAny
}

func (a *analyzer) inferBinary(b *ast.Binary) FieldType {
	lt := a.inferExpr(b.Left)
	rt := a.inferExpr(b.Right)

	switch b.Op {
	case ast.OpAnd, ast.OpOr:
		// Logical: operands should be bool.
		if lt != TypeBool && lt != TypeAny {
			a.addDiag(CodeNonBoolLogical, parser.SeverityWarning, b.Left.ExprSpan(),
				fmt.Sprintf("operand of '%s' has type %s, expected bool", binaryOpName(b.Op), lt),
				"")
		}
		if rt != TypeBool && rt != TypeAny {
			a.addDiag(CodeNonBoolLogical, parser.SeverityWarning, b.Right.ExprSpan(),
				fmt.Sprintf("operand of '%s' has type %s, expected bool", binaryOpName(b.Op), rt),
				"")
		}
		return TypeBool

	case ast.OpEq, ast.OpNotEq, ast.OpLt, ast.OpLtEq, ast.OpGt, ast.OpGtEq:
		return a.inferComparison(b, lt, rt)

	case ast.OpAdd:
		return a.inferAdd(b, lt, rt)

	case ast.OpSub:
		return a.inferSub(b, lt, rt)

	case ast.OpMul:
		return a.inferMul(b, lt, rt)

	case ast.OpDiv:
		return a.inferDiv(b, lt, rt)

	case ast.OpMod:
		// int only.
		if lt == TypeInt && rt == TypeInt {
			return TypeInt
		}
		if lt == TypeAny || rt == TypeAny {
			return TypeAny
		}
		return TypeAny

	case ast.OpCoalesce:
		// ?? -> common type or "any".
		return commonType(lt, rt)
	}

	return TypeAny
}

func (a *analyzer) inferComparison(b *ast.Binary, lt, rt FieldType) FieldType {
	// If either side is "any", allow.
	if lt == TypeAny || rt == TypeAny {
		return TypeBool
	}
	// Same type: OK.
	if lt == rt {
		return TypeBool
	}
	// int/float mixed: OK.
	if isNumeric(lt) && isNumeric(rt) {
		return TypeBool
	}
	// Incompatible known types: error with fix-it.
	suggestion := a.comparisonFixIt(b, lt, rt)
	a.addDiag(CodeTypeMismatchComparison, parser.SeverityError, b.Pos,
		fmt.Sprintf("cannot compare %s %s %s", lt, binaryOpName(b.Op), rt),
		suggestion)
	return TypeBool
}

// comparisonFixIt generates a suggestion for type-mismatched comparisons.
// Per RFC-002 D8/§5.4: when one side is a literal whose text could parse as
// the other side's type, suggest the retyped literal FIRST. When the literal
// can't be retyped, suggest casting the field or fixing the literal.
func (a *analyzer) comparisonFixIt(b *ast.Binary, lt, rt FieldType) string {
	op := binaryOpName(b.Op)

	// Check if the right side is a literal.
	if rLit, ok := b.Right.(*ast.Literal); ok {
		leftName := exprName(b.Left)
		litText := literalText(rLit)

		// Can the literal's text parse as the left side's type?
		if retyped, ok := retypeLiteral(litText, lt); ok {
			return fmt.Sprintf("did you mean %s %s %s?", leftName, op, retyped)
		}
		// Literal can't be retyped: suggest casting the field to the
		// literal's type, or casting the literal to the field's type.
		return fmt.Sprintf("did you mean %s(%s) %s %s, or %s %s %s(%s)?",
			rt, leftName, op, litText,
			leftName, op, lt, litText)
	}

	// Check if the left side is a literal.
	if lLit, ok := b.Left.(*ast.Literal); ok {
		rightName := exprName(b.Right)
		litText := literalText(lLit)

		if retyped, ok := retypeLiteral(litText, rt); ok {
			return fmt.Sprintf("did you mean %s %s %s?", retyped, op, rightName)
		}
		return fmt.Sprintf("did you mean %s(%s) %s %s, or %s(%s) %s %s?",
			rt, litText, op, rightName,
			lt, rightName, op, litText)
	}

	// Both non-literals: suggest casting one side.
	leftName := exprName(b.Left)
	rightName := exprName(b.Right)
	return fmt.Sprintf("did you mean %s(%s) %s %s, or %s %s %s(%s)?",
		rt, leftName, op, rightName,
		leftName, op, lt, rightName)
}

func (a *analyzer) inferAdd(b *ast.Binary, lt, rt FieldType) FieldType {
	// String concatenation.
	if lt == TypeString && rt == TypeString {
		return TypeString
	}
	// Numeric.
	if lt == TypeInt && rt == TypeInt {
		return TypeInt
	}
	if isNumeric(lt) && isNumeric(rt) {
		return TypeFloat
	}
	// Timestamp + duration.
	if lt == TypeTimestamp && rt == TypeDuration {
		return TypeTimestamp
	}
	if lt == TypeDuration && rt == TypeTimestamp {
		return TypeTimestamp
	}
	// Duration + duration.
	if lt == TypeDuration && rt == TypeDuration {
		return TypeDuration
	}
	// Any.
	if lt == TypeAny || rt == TypeAny {
		return TypeAny
	}
	// Type error: string + number.
	if lt == TypeString && isNumeric(rt) {
		a.addDiag(CodeTypeMismatchBinop, parser.SeverityError, b.Pos,
			fmt.Sprintf("cannot add %s + %s", lt, rt),
			fmt.Sprintf("did you mean string(%s), or %s + string(%s)?",
				exprName(b.Right), exprName(b.Left), exprName(b.Right)))
		return TypeAny
	}
	if isNumeric(lt) && rt == TypeString {
		a.addDiag(CodeTypeMismatchBinop, parser.SeverityError, b.Pos,
			fmt.Sprintf("cannot add %s + %s", lt, rt),
			fmt.Sprintf("did you mean string(%s) + %s?",
				exprName(b.Left), exprName(b.Right)))
		return TypeAny
	}
	return TypeAny
}

func (a *analyzer) inferSub(b *ast.Binary, lt, rt FieldType) FieldType {
	if lt == TypeInt && rt == TypeInt {
		return TypeInt
	}
	if isNumeric(lt) && isNumeric(rt) {
		return TypeFloat
	}
	// timestamp - timestamp -> duration
	if lt == TypeTimestamp && rt == TypeTimestamp {
		return TypeDuration
	}
	// timestamp - duration -> timestamp
	if lt == TypeTimestamp && rt == TypeDuration {
		return TypeTimestamp
	}
	// duration - duration -> duration
	if lt == TypeDuration && rt == TypeDuration {
		return TypeDuration
	}
	if lt == TypeAny || rt == TypeAny {
		return TypeAny
	}
	_ = b
	return TypeAny
}

func (a *analyzer) inferMul(_ *ast.Binary, lt, rt FieldType) FieldType {
	if lt == TypeInt && rt == TypeInt {
		return TypeInt
	}
	if isNumeric(lt) && isNumeric(rt) {
		return TypeFloat
	}
	// duration * number -> duration
	if lt == TypeDuration && isNumeric(rt) {
		return TypeDuration
	}
	if isNumeric(lt) && rt == TypeDuration {
		return TypeDuration
	}
	if lt == TypeAny || rt == TypeAny {
		return TypeAny
	}
	return TypeAny
}

func (a *analyzer) inferDiv(_ *ast.Binary, lt, rt FieldType) FieldType {
	if lt == TypeInt && rt == TypeInt {
		return TypeInt
	}
	if isNumeric(lt) && isNumeric(rt) {
		return TypeFloat
	}
	// duration / number -> duration
	if lt == TypeDuration && isNumeric(rt) {
		return TypeDuration
	}
	// duration / duration -> float
	if lt == TypeDuration && rt == TypeDuration {
		return TypeFloat
	}
	if lt == TypeAny || rt == TypeAny {
		return TypeAny
	}
	return TypeAny
}

func (a *analyzer) inferUnary(u *ast.Unary) FieldType {
	ot := a.inferExpr(u.Operand)
	switch u.Op {
	case ast.OpNot:
		if ot != TypeBool && ot != TypeAny {
			a.addDiag(CodeNonBoolLogical, parser.SeverityWarning, u.Operand.ExprSpan(),
				fmt.Sprintf("operand of 'not' has type %s, expected bool", ot),
				"")
		}
		return TypeBool
	case ast.OpNeg:
		if ot == TypeInt {
			return TypeInt
		}
		if ot == TypeFloat {
			return TypeFloat
		}
		if ot == TypeDuration {
			return TypeDuration
		}
		return TypeAny
	}
	return TypeAny
}

func (a *analyzer) inferCall(c *ast.Call) FieldType {
	callee := strings.ToLower(c.Callee)

	// Check if it's an aggregate function.
	if agg, ok := registry.LookupAggregate(callee); ok {
		if !a.inAggContext {
			a.addDiag(CodeAggOutsideStats, parser.SeverityError, c.Pos,
				fmt.Sprintf("aggregate function '%s' is only valid in stats/eventstats/streamstats", callee),
				"aggregate functions are only valid inside stats, eventstats, or streamstats")
		}
		// Type-check args.
		for _, arg := range c.Args {
			a.inferExpr(arg)
		}
		return registryTypeToFieldType(agg.Result)
	}

	// Check if it's a scalar function.
	if fn, ok := registry.LookupFunction(callee); ok {
		// Arity check.
		a.checkArity(c, fn)
		// Type-check args.
		for _, arg := range c.Args {
			a.inferExpr(arg)
		}
		return registryTypeToFieldType(fn.Result)
	}

	// Check strict-cast variants (int!, float!, etc.).
	if c.Bang {
		baseName := callee
		if fn, ok := registry.LookupFunction(baseName); ok && fn.StrictVariant {
			a.checkArity(c, fn)
			for _, arg := range c.Args {
				a.inferExpr(arg)
			}
			return registryTypeToFieldType(fn.Result)
		}
	}

	// Unknown function.
	allNames := allFunctionAndAggregateNames()
	suggestion := didYouMean(callee, allNames)
	a.addDiag(CodeUnknownFunction, parser.SeverityError, c.Pos,
		fmt.Sprintf("unknown function '%s'", callee),
		suggestion)
	for _, arg := range c.Args {
		a.inferExpr(arg)
	}
	return TypeAny
}

// inferAgg infers the type of an aggregate expression in a stats context.
func (a *analyzer) inferAgg(agg ast.AggExpr) FieldType {
	if call, ok := agg.Func.(*ast.Call); ok {
		callee := strings.ToLower(call.Callee)
		if ag, ok := registry.LookupAggregate(callee); ok {
			// Type-check arguments.
			for _, arg := range call.Args {
				a.inferExpr(arg)
			}
			return registryTypeToFieldType(ag.Result)
		}
		// Might be a scalar function used as an agg (error).
		if _, ok := registry.LookupFunction(callee); ok {
			a.addDiag(CodeAggOutsideStats, parser.SeverityError, call.Pos,
				fmt.Sprintf("'%s' is a scalar function, not an aggregate; use it in extend", callee),
				"")
		}
	}
	return a.inferExpr(agg.Func)
}

func (a *analyzer) checkArity(c *ast.Call, fn registry.Function) {
	nArgs := len(c.Args)
	minArgs, maxArgs := arityRange(fn.Params)

	if nArgs < minArgs || (maxArgs >= 0 && nArgs > maxArgs) {
		sig := formatSignature(fn)
		a.addDiag(CodeWrongArity, parser.SeverityError, c.Pos,
			fmt.Sprintf("'%s' expects %s, got %d", c.Callee, arityDescription(minArgs, maxArgs), nArgs),
			"signature: "+sig)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func isNumeric(t FieldType) bool {
	return t == TypeInt || t == TypeFloat
}

func commonType(a, b FieldType) FieldType {
	if a == b {
		return a
	}
	if a == TypeAny {
		return b
	}
	if b == TypeAny {
		return a
	}
	if isNumeric(a) && isNumeric(b) {
		return TypeFloat
	}
	return TypeAny
}

func binaryOpName(op ast.BinaryOp) string {
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

func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.Literal:
		return literalText(x)
	}
	return e.String()
}

func literalText(lit *ast.Literal) string {
	switch lit.Kind {
	case ast.LitString:
		return lit.Raw
	case ast.LitRawString:
		return lit.Raw
	case ast.LitInt:
		if v, ok := lit.Value.(int64); ok {
			return strconv.FormatInt(v, 10)
		}
		return lit.Raw
	case ast.LitFloat:
		if v, ok := lit.Value.(float64); ok {
			return strconv.FormatFloat(v, 'g', -1, 64)
		}
		return lit.Raw
	case ast.LitBool:
		if v, ok := lit.Value.(bool); ok {
			if v {
				return "true"
			}
			return "false"
		}
		return lit.Raw
	case ast.LitNull:
		return "null"
	case ast.LitDuration:
		return lit.Raw
	}
	return lit.Raw
}

// retypeLiteral tries to express the literal text as a value of the target
// type. Returns the new literal text and true if possible.
func retypeLiteral(text string, target FieldType) (string, bool) {
	switch target {
	case TypeInt:
		// Try parsing the literal as int.
		unquoted := strings.Trim(text, "\"")
		if _, err := strconv.ParseInt(unquoted, 10, 64); err == nil {
			return unquoted, true
		}
		return "", false
	case TypeFloat:
		unquoted := strings.Trim(text, "\"")
		if _, err := strconv.ParseFloat(unquoted, 64); err == nil {
			return unquoted, true
		}
		return "", false
	case TypeString:
		// Wrap in quotes.
		if !strings.HasPrefix(text, "\"") {
			return fmt.Sprintf("%q", text), true
		}
		return text, true
	case TypeBool:
		unquoted := strings.ToLower(strings.Trim(text, "\""))
		if unquoted == "true" || unquoted == "false" {
			return unquoted, true
		}
		return "", false
	}
	return "", false
}

func registryTypeToFieldType(vt registry.ValueType) FieldType {
	switch vt {
	case registry.TString:
		return TypeString
	case registry.TInt:
		return TypeInt
	case registry.TFloat:
		return TypeFloat
	case registry.TNumber:
		// Number could be int or float — use "any" to be safe,
		// but in practice callers can refine.
		return TypeAny
	case registry.TBool:
		return TypeBool
	case registry.TTimestamp:
		return TypeTimestamp
	case registry.TDuration:
		return TypeDuration
	case registry.TArray:
		return TypeArray
	case registry.TObject:
		return TypeObject
	case registry.TAny:
		return TypeAny
	}
	return TypeAny
}

func arityRange(params []registry.Param) (min, max int) {
	for _, p := range params {
		if p.Variadic {
			if !p.Optional {
				min++
			}
			return min, -1 // unlimited
		}
		if !p.Optional {
			min++
		}
		max++
	}
	return min, max
}

func arityDescription(min, max int) string {
	if max < 0 {
		if min == 0 {
			return "0 or more arguments"
		}
		return fmt.Sprintf("at least %d argument(s)", min)
	}
	if min == max {
		return fmt.Sprintf("%d argument(s)", min)
	}
	return fmt.Sprintf("%d to %d arguments", min, max)
}

func formatSignature(fn registry.Function) string {
	var b strings.Builder
	b.WriteString(fn.Name)
	b.WriteByte('(')
	for i, p := range fn.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		if p.Optional {
			b.WriteByte('[')
		}
		b.WriteString(p.Name)
		b.WriteString(": ")
		b.WriteString(string(p.Type))
		if p.Variadic {
			b.WriteString("...")
		}
		if p.Optional {
			b.WriteByte(']')
		}
	}
	b.WriteString(") -> ")
	b.WriteString(string(fn.Result))
	return b.String()
}

func allFunctionNames() []string {
	fns := registry.Functions()
	out := make([]string, len(fns))
	for i, fn := range fns {
		out[i] = fn.Name
	}
	return out
}

func allAggregateNames() []string {
	aggs := registry.Aggregates()
	out := make([]string, len(aggs))
	for i, ag := range aggs {
		out[i] = ag.Name
	}
	return out
}

func allFunctionAndAggregateNames() []string {
	return append(allFunctionNames(), allAggregateNames()...)
}
