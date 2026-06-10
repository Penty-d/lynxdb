// Package ast defines the expression AST nodes for the LynxFlow v2 query
// language (RFC-002 Phase 2). Every node carries a [Span] (byte-offset range
// into the original source) for diagnostics, formatter round-trip, and caret
// rendering.
package ast

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Span is a half-open byte range [Start, End) into the source text.
// It is re-exported from the lexer package for convenience so that consumers
// of the AST do not need to import the lexer.
type Span struct {
	Start int
	End   int
}

// Expr is the interface implemented by all expression AST nodes.
// Every implementation must carry a Span and produce a deterministic debug
// rendering via String().
type Expr interface {
	// ExprSpan returns the source span covering the entire expression.
	ExprSpan() Span
	// String returns a compact, unambiguous debug rendering suitable for
	// golden-test assertions.
	String() string
}

// ---------------------------------------------------------------------------
// Identifier
// ---------------------------------------------------------------------------

// Ident is a bare or backtick-quoted identifier.
type Ident struct {
	Name   string // the resolved name (without backticks)
	Quoted bool   // true when the source was backtick-quoted
	Pos    Span
}

func (n *Ident) ExprSpan() Span { return n.Pos }
func (n *Ident) String() string {
	if n.Quoted {
		return "`" + n.Name + "`"
	}
	return n.Name
}

// ---------------------------------------------------------------------------
// Literals
// ---------------------------------------------------------------------------

// LitKind classifies the kind of a literal value.
type LitKind uint8

const (
	LitString    LitKind = iota // "double-quoted"
	LitRawString                // r"raw"
	LitInt                      // 42, 0x2A
	LitFloat                    // 3.14, 1e-6
	LitBool                     // true, false
	LitNull                     // null
	LitDuration                 // 30s, 1.5h, 100ms
)

// Literal is a scalar literal value: string, raw string, int, float, bool,
// null, or duration. The parsed Go value is stored in Value.
type Literal struct {
	Kind  LitKind
	Raw   string      // original source text (e.g. `"hello"`, `0x2A`, `30s`)
	Value interface{} // parsed Go value: string, int64, float64, bool, nil, time.Duration
	Pos   Span
}

func (n *Literal) ExprSpan() Span { return n.Pos }
func (n *Literal) String() string {
	switch n.Kind {
	case LitString:
		return n.Raw
	case LitRawString:
		return n.Raw
	case LitInt:
		if v, ok := n.Value.(int64); ok {
			return strconv.FormatInt(v, 10)
		}
		return n.Raw
	case LitFloat:
		if v, ok := n.Value.(float64); ok {
			return strconv.FormatFloat(v, 'g', -1, 64)
		}
		return n.Raw
	case LitBool:
		if v, ok := n.Value.(bool); ok {
			if v {
				return "true"
			}
			return "false"
		}
		return n.Raw
	case LitNull:
		return "null"
	case LitDuration:
		if v, ok := n.Value.(time.Duration); ok {
			return formatDuration(v)
		}
		return n.Raw
	}
	return n.Raw
}

// formatDuration renders a time.Duration in the most natural LynxFlow unit.
func formatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	neg := ""
	if d < 0 {
		neg = "-"
		d = -d
	}
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return fmt.Sprintf("%s%dh", neg, d/time.Hour)
	case d%time.Minute == 0 && d >= time.Minute:
		return fmt.Sprintf("%s%dm", neg, d/time.Minute)
	case d%time.Second == 0 && d >= time.Second:
		return fmt.Sprintf("%s%ds", neg, d/time.Second)
	case d%time.Millisecond == 0 && d >= time.Millisecond:
		return fmt.Sprintf("%s%dms", neg, d/time.Millisecond)
	case d%(time.Microsecond) == 0 && d >= time.Microsecond:
		return fmt.Sprintf("%s%dus", neg, d/time.Microsecond)
	default:
		return fmt.Sprintf("%s%dns", neg, d/time.Nanosecond)
	}
}

// ---------------------------------------------------------------------------
// Composite literals
// ---------------------------------------------------------------------------

// Array is an array literal: [expr, expr, ...].
type Array struct {
	Elems []Expr
	Pos   Span // spans from [ to ]
}

func (n *Array) ExprSpan() Span { return n.Pos }
func (n *Array) String() string {
	var b strings.Builder
	b.WriteByte('[')
	for i, e := range n.Elems {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.String())
	}
	b.WriteByte(']')
	return b.String()
}

// ObjectEntry is a key:value pair inside an object literal.
type ObjectEntry struct {
	Key     string // resolved key name
	KeySpan Span   // span of the key token (ident or string)
	Value   Expr
}

// Object is an object literal: {key: expr, ...}.
type Object struct {
	Entries []ObjectEntry
	Pos     Span // spans from { to }
}

func (n *Object) ExprSpan() Span { return n.Pos }
func (n *Object) String() string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range n.Entries {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.Key)
		b.WriteString(": ")
		b.WriteString(e.Value.String())
	}
	b.WriteByte('}')
	return b.String()
}

// ---------------------------------------------------------------------------
// Operators
// ---------------------------------------------------------------------------

// UnaryOp identifies a unary operator.
type UnaryOp uint8

const (
	OpNot UnaryOp = iota // not
	OpNeg                // - (unary minus)
)

// Unary is a unary operator expression: not expr, -expr.
type Unary struct {
	Op      UnaryOp
	Operand Expr
	Pos     Span // spans from operator to end of operand
}

func (n *Unary) ExprSpan() Span { return n.Pos }
func (n *Unary) String() string {
	switch n.Op {
	case OpNot:
		return "(not " + n.Operand.String() + ")"
	case OpNeg:
		return "(-" + n.Operand.String() + ")"
	}
	return "(?" + n.Operand.String() + ")"
}

// BinaryOp identifies a binary operator.
type BinaryOp uint8

const (
	OpOr       BinaryOp = iota // or
	OpAnd                      // and
	OpEq                       // ==
	OpNotEq                    // !=
	OpLt                       // <
	OpLtEq                     // <=
	OpGt                       // >
	OpGtEq                     // >=
	OpAdd                      // +
	OpSub                      // -
	OpMul                      // *
	OpDiv                      // /
	OpMod                      // %
	OpCoalesce                 // ??
)

var binaryOpNames = [...]string{
	OpOr:       "or",
	OpAnd:      "and",
	OpEq:       "==",
	OpNotEq:    "!=",
	OpLt:       "<",
	OpLtEq:     "<=",
	OpGt:       ">",
	OpGtEq:     ">=",
	OpAdd:      "+",
	OpSub:      "-",
	OpMul:      "*",
	OpDiv:      "/",
	OpMod:      "%",
	OpCoalesce: "??",
}

// Binary is a binary operator expression: expr op expr.
type Binary struct {
	Op    BinaryOp
	Left  Expr
	Right Expr
	Pos   Span // spans from start of left to end of right
}

func (n *Binary) ExprSpan() Span { return n.Pos }
func (n *Binary) String() string {
	op := "?"
	if int(n.Op) < len(binaryOpNames) {
		op = binaryOpNames[n.Op]
	}
	return "(" + n.Left.String() + " " + op + " " + n.Right.String() + ")"
}

// ---------------------------------------------------------------------------
// In / Between
// ---------------------------------------------------------------------------

// In is an `expr in expr` expression. The RHS is typically an Array literal
// but may be any expression.
type In struct {
	LHS Expr
	RHS Expr // typically *Array, but may be any expr
	Pos Span
}

func (n *In) ExprSpan() Span { return n.Pos }
func (n *In) String() string {
	return "(" + n.LHS.String() + " in " + n.RHS.String() + ")"
}

// Between is an `expr between lo and hi` expression.
type Between struct {
	X   Expr
	Lo  Expr
	Hi  Expr
	Pos Span
}

func (n *Between) ExprSpan() Span { return n.Pos }
func (n *Between) String() string {
	return "(" + n.X.String() + " between " + n.Lo.String() + " and " + n.Hi.String() + ")"
}

// ---------------------------------------------------------------------------
// Call
// ---------------------------------------------------------------------------

// Call is a function call: name(args...) or name!(args...) for strict casts.
// When a call follows a member or safe-member access (method-call chains like
// a.b[0]?.c(1)), Receiver holds the object expression and SafeNav indicates
// whether ?.  was used. For standalone function calls, Receiver is nil.
type Call struct {
	Receiver Expr   // optional: object in member-call chains; nil for standalone calls
	SafeNav  bool   // true when the call followed ?. rather than .
	Callee   string // function name (lowercase-normalized by parser)
	Bang     bool   // true for strict-cast: int!(x)
	Args     []Expr
	Pos      Span // spans from callee/receiver start to closing paren
}

func (n *Call) ExprSpan() Span { return n.Pos }
func (n *Call) String() string {
	var b strings.Builder
	if n.Receiver != nil {
		b.WriteString(n.Receiver.String())
		if n.SafeNav {
			b.WriteString("?.")
		} else {
			b.WriteByte('.')
		}
	}
	b.WriteString(n.Callee)
	if n.Bang {
		b.WriteByte('!')
	}
	b.WriteByte('(')
	for i, a := range n.Args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.String())
	}
	b.WriteByte(')')
	return b.String()
}

// ---------------------------------------------------------------------------
// Member / SafeMember / Index
// ---------------------------------------------------------------------------

// Member is a dot-access expression: expr.ident.
type Member struct {
	Object Expr
	Field  string
	Pos    Span // from start of object to end of field ident
}

func (n *Member) ExprSpan() Span { return n.Pos }
func (n *Member) String() string {
	return n.Object.String() + "." + n.Field
}

// SafeMember is a safe-navigation expression: expr?.ident.
type SafeMember struct {
	Object Expr
	Field  string
	Pos    Span
}

func (n *SafeMember) ExprSpan() Span { return n.Pos }
func (n *SafeMember) String() string {
	return n.Object.String() + "?." + n.Field
}

// Index is a subscript expression: expr[index].
type Index struct {
	Object Expr
	Idx    Expr
	Pos    Span // from start of object to closing ]
}

func (n *Index) ExprSpan() Span { return n.Pos }
func (n *Index) String() string {
	return n.Object.String() + "[" + n.Idx.String() + "]"
}

// ---------------------------------------------------------------------------
// Lambda
// ---------------------------------------------------------------------------

// Lambda is a lambda expression: param -> body.
type Lambda struct {
	Param string // parameter name
	Body  Expr
	Pos   Span // from param ident to end of body
}

func (n *Lambda) ExprSpan() Span { return n.Pos }
func (n *Lambda) String() string {
	return "(" + n.Param + " -> " + n.Body.String() + ")"
}

// ---------------------------------------------------------------------------
// Paren
// ---------------------------------------------------------------------------

// Paren is a parenthesized expression. Kept in the AST for span fidelity
// and formatter round-trip.
type Paren struct {
	Inner Expr
	Pos   Span // from ( to )
}

func (n *Paren) ExprSpan() Span { return n.Pos }

// String renders the inner expression. Paren nodes exist for span fidelity
// and formatter round-trip; the debug rendering delegates to the child since
// Binary.String() already parenthesizes for precedence clarity.
func (n *Paren) String() string {
	return n.Inner.String()
}

// ---------------------------------------------------------------------------
// ErrorExpr
// ---------------------------------------------------------------------------

// ErrorExpr is a placeholder node inserted when the parser encounters an
// error and cannot produce a valid subtree. It carries the span of the
// erroneous tokens so that downstream consumers (formatter, semantic
// analyzer) can report meaningful locations. The parser continues after
// inserting an ErrorExpr, collecting further diagnostics.
type ErrorExpr struct {
	Message string
	Pos     Span
}

func (n *ErrorExpr) ExprSpan() Span { return n.Pos }
func (n *ErrorExpr) String() string {
	return "<error>"
}

// ---------------------------------------------------------------------------
// Walk / Inspect
// ---------------------------------------------------------------------------

// Visitor is called by Walk for each node in a depth-first traversal.
// If the function returns false, children of that node are not visited.
type Visitor func(n Expr) bool

// Walk traverses the expression tree in depth-first pre-order, calling fn
// for every node. If fn returns false for a node, that node's children are
// not visited.
func Walk(n Expr, fn Visitor) {
	if n == nil || !fn(n) {
		return
	}
	switch x := n.(type) {
	case *Unary:
		Walk(x.Operand, fn)
	case *Binary:
		Walk(x.Left, fn)
		Walk(x.Right, fn)
	case *In:
		Walk(x.LHS, fn)
		Walk(x.RHS, fn)
	case *Between:
		Walk(x.X, fn)
		Walk(x.Lo, fn)
		Walk(x.Hi, fn)
	case *Call:
		if x.Receiver != nil {
			Walk(x.Receiver, fn)
		}
		for _, a := range x.Args {
			Walk(a, fn)
		}
	case *Member:
		Walk(x.Object, fn)
	case *SafeMember:
		Walk(x.Object, fn)
	case *Index:
		Walk(x.Object, fn)
		Walk(x.Idx, fn)
	case *Lambda:
		Walk(x.Body, fn)
	case *Paren:
		Walk(x.Inner, fn)
	case *Array:
		for _, e := range x.Elems {
			Walk(e, fn)
		}
	case *Object:
		for _, e := range x.Entries {
			Walk(e.Value, fn)
		}
		// Leaf nodes: *Ident, *Literal, *ErrorExpr — no children.
	}
}

// Inspect traverses the tree calling fn for every node. Unlike Walk, it does
// not support short-circuiting; every node is visited.
func Inspect(n Expr, fn func(Expr)) {
	Walk(n, func(e Expr) bool {
		fn(e)
		return true
	})
}
