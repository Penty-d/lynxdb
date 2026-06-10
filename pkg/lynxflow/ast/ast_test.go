package ast

import (
	"testing"
	"time"
)

func TestLiteral_String(t *testing.T) {
	tests := []struct {
		name string
		lit  Literal
		want string
	}{
		{"string", Literal{Kind: LitString, Raw: `"hello"`, Value: "hello"}, `"hello"`},
		{"raw string", Literal{Kind: LitRawString, Raw: `r"\d+"`, Value: `\d+`}, `r"\d+"`},
		{"int", Literal{Kind: LitInt, Raw: "42", Value: int64(42)}, "42"},
		{"hex int", Literal{Kind: LitInt, Raw: "0x2A", Value: int64(42)}, "42"},
		{"float", Literal{Kind: LitFloat, Raw: "3.14", Value: 3.14}, "3.14"},
		{"bool true", Literal{Kind: LitBool, Raw: "true", Value: true}, "true"},
		{"bool false", Literal{Kind: LitBool, Raw: "false", Value: false}, "false"},
		{"null", Literal{Kind: LitNull, Raw: "null", Value: nil}, "null"},
		{"duration seconds", Literal{Kind: LitDuration, Raw: "30s", Value: 30 * time.Second}, "30s"},
		{"duration ms", Literal{Kind: LitDuration, Raw: "100ms", Value: 100 * time.Millisecond}, "100ms"},
		{"duration hours", Literal{Kind: LitDuration, Raw: "1h", Value: time.Hour}, "1h"},
		{"duration minutes", Literal{Kind: LitDuration, Raw: "5m", Value: 5 * time.Minute}, "5m"},
		{"duration days", Literal{Kind: LitDuration, Raw: "7d", Value: 7 * 24 * time.Hour}, "168h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.lit.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIdent_String(t *testing.T) {
	bare := &Ident{Name: "status", Quoted: false}
	if bare.String() != "status" {
		t.Errorf("bare ident: got %q", bare.String())
	}
	quoted := &Ident{Name: "field-with-dash", Quoted: true}
	if quoted.String() != "`field-with-dash`" {
		t.Errorf("quoted ident: got %q", quoted.String())
	}
}

func TestUnary_String(t *testing.T) {
	notExpr := &Unary{Op: OpNot, Operand: &Ident{Name: "x"}}
	if notExpr.String() != "(not x)" {
		t.Errorf("not: got %q", notExpr.String())
	}
	negExpr := &Unary{Op: OpNeg, Operand: &Ident{Name: "x"}}
	if negExpr.String() != "(-x)" {
		t.Errorf("neg: got %q", negExpr.String())
	}
}

func TestBinary_String(t *testing.T) {
	tests := []struct {
		op   BinaryOp
		want string
	}{
		{OpOr, "(a or b)"},
		{OpAnd, "(a and b)"},
		{OpEq, "(a == b)"},
		{OpNotEq, "(a != b)"},
		{OpLt, "(a < b)"},
		{OpLtEq, "(a <= b)"},
		{OpGt, "(a > b)"},
		{OpGtEq, "(a >= b)"},
		{OpAdd, "(a + b)"},
		{OpSub, "(a - b)"},
		{OpMul, "(a * b)"},
		{OpDiv, "(a / b)"},
		{OpMod, "(a % b)"},
		{OpCoalesce, "(a ?? b)"},
	}
	for _, tt := range tests {
		b := &Binary{Op: tt.op, Left: &Ident{Name: "a"}, Right: &Ident{Name: "b"}}
		if b.String() != tt.want {
			t.Errorf("Binary(%v): got %q, want %q", tt.op, b.String(), tt.want)
		}
	}
}

func TestIn_String(t *testing.T) {
	n := &In{
		LHS: &Ident{Name: "x"},
		RHS: &Array{Elems: []Expr{
			&Literal{Kind: LitInt, Value: int64(1)},
			&Literal{Kind: LitInt, Value: int64(2)},
		}},
	}
	want := "(x in [1, 2])"
	if n.String() != want {
		t.Errorf("got %q, want %q", n.String(), want)
	}
}

func TestBetween_String(t *testing.T) {
	n := &Between{
		X:  &Ident{Name: "x"},
		Lo: &Literal{Kind: LitInt, Value: int64(1)},
		Hi: &Literal{Kind: LitInt, Value: int64(10)},
	}
	want := "(x between 1 and 10)"
	if n.String() != want {
		t.Errorf("got %q, want %q", n.String(), want)
	}
}

func TestCall_String(t *testing.T) {
	// Simple call.
	c := &Call{Callee: "count"}
	if c.String() != "count()" {
		t.Errorf("empty call: got %q", c.String())
	}

	// Call with args.
	c2 := &Call{Callee: "avg", Args: []Expr{&Ident{Name: "dur"}}}
	if c2.String() != "avg(dur)" {
		t.Errorf("call with arg: got %q", c2.String())
	}

	// Strict cast.
	c3 := &Call{Callee: "int", Bang: true, Args: []Expr{&Ident{Name: "x"}}}
	if c3.String() != "int!(x)" {
		t.Errorf("strict cast: got %q", c3.String())
	}

	// Call with receiver (member chain).
	c4 := &Call{
		Receiver: &Ident{Name: "obj"},
		Callee:   "method",
		Args:     []Expr{&Literal{Kind: LitInt, Value: int64(1)}},
	}
	if c4.String() != "obj.method(1)" {
		t.Errorf("method call: got %q", c4.String())
	}

	// Call with receiver (safe nav).
	c5 := &Call{
		Receiver: &Ident{Name: "obj"},
		SafeNav:  true,
		Callee:   "method",
	}
	if c5.String() != "obj?.method()" {
		t.Errorf("safe method call: got %q", c5.String())
	}
}

func TestMember_String(t *testing.T) {
	m := &Member{Object: &Ident{Name: "a"}, Field: "b"}
	if m.String() != "a.b" {
		t.Errorf("got %q", m.String())
	}
}

func TestSafeMember_String(t *testing.T) {
	sm := &SafeMember{Object: &Ident{Name: "a"}, Field: "b"}
	if sm.String() != "a?.b" {
		t.Errorf("got %q", sm.String())
	}
}

func TestIndex_String(t *testing.T) {
	idx := &Index{
		Object: &Ident{Name: "arr"},
		Idx:    &Literal{Kind: LitInt, Value: int64(0)},
	}
	if idx.String() != "arr[0]" {
		t.Errorf("got %q", idx.String())
	}
}

func TestLambda_String(t *testing.T) {
	l := &Lambda{
		Param: "t",
		Body:  &Binary{Op: OpEq, Left: &Member{Object: &Ident{Name: "t"}, Field: "name"}, Right: &Literal{Kind: LitString, Raw: `"vip"`, Value: "vip"}},
	}
	want := `(t -> (t.name == "vip"))`
	if l.String() != want {
		t.Errorf("got %q, want %q", l.String(), want)
	}
}

func TestErrorExpr_String(t *testing.T) {
	e := &ErrorExpr{Message: "test error"}
	if e.String() != "<error>" {
		t.Errorf("got %q", e.String())
	}
}

func TestArray_String(t *testing.T) {
	a := &Array{Elems: []Expr{
		&Literal{Kind: LitInt, Value: int64(1)},
		&Literal{Kind: LitString, Raw: `"x"`, Value: "x"},
	}}
	if a.String() != `[1, "x"]` {
		t.Errorf("got %q", a.String())
	}

	empty := &Array{}
	if empty.String() != "[]" {
		t.Errorf("empty array: got %q", empty.String())
	}
}

func TestObject_String(t *testing.T) {
	o := &Object{Entries: []ObjectEntry{
		{Key: "a", Value: &Literal{Kind: LitInt, Value: int64(1)}},
		{Key: "b", Value: &Literal{Kind: LitBool, Value: true}},
	}}
	if o.String() != "{a: 1, b: true}" {
		t.Errorf("got %q", o.String())
	}

	empty := &Object{}
	if empty.String() != "{}" {
		t.Errorf("empty object: got %q", empty.String())
	}
}

func TestParen_String(t *testing.T) {
	// Paren delegates to inner for debug rendering.
	p := &Paren{Inner: &Ident{Name: "x"}}
	if p.String() != "x" {
		t.Errorf("got %q, want 'x'", p.String())
	}
}

func TestSpan(t *testing.T) {
	s := Span{Start: 5, End: 10}
	n := &Ident{Name: "x", Pos: s}
	if n.ExprSpan() != s {
		t.Errorf("ExprSpan() = %+v, want %+v", n.ExprSpan(), s)
	}
}

func TestWalk_AllNodes(t *testing.T) {
	// Build a tree with every node type.
	tree := &Binary{
		Op: OpAnd,
		Left: &Unary{
			Op: OpNot,
			Operand: &In{
				LHS: &Ident{Name: "x"},
				RHS: &Array{Elems: []Expr{&Literal{Kind: LitInt, Value: int64(1)}}},
			},
		},
		Right: &Between{
			X:  &Member{Object: &Ident{Name: "a"}, Field: "b"},
			Lo: &Literal{Kind: LitInt, Value: int64(0)},
			Hi: &Call{Callee: "max", Args: []Expr{&Ident{Name: "y"}}},
		},
	}

	count := 0
	Walk(tree, func(n Expr) bool {
		count++
		return true
	})
	// Binary(And), Unary(Not), In, Ident(x), Array, Literal(1),
	// Between, Member, Ident(a), Literal(0), Call(max), Ident(y) = 12
	if count != 12 {
		t.Errorf("Walk visited %d nodes, want 12", count)
	}
}

func TestWalk_NilSafe(t *testing.T) {
	// Walk(nil, ...) should not panic.
	Walk(nil, func(n Expr) bool {
		t.Error("should not be called for nil")
		return true
	})
}

func TestInspect_AllVisited(t *testing.T) {
	tree := &Lambda{
		Param: "t",
		Body: &SafeMember{
			Object: &Index{
				Object: &Ident{Name: "arr"},
				Idx:    &Literal{Kind: LitInt, Value: int64(0)},
			},
			Field: "name",
		},
	}
	count := 0
	Inspect(tree, func(n Expr) {
		count++
	})
	// Lambda, SafeMember, Index, Ident(arr), Literal(0) = 5
	if count != 5 {
		t.Errorf("Inspect visited %d nodes, want 5", count)
	}
}

func TestWalk_ObjectEntries(t *testing.T) {
	obj := &Object{
		Entries: []ObjectEntry{
			{Key: "a", Value: &Ident{Name: "x"}},
			{Key: "b", Value: &Literal{Kind: LitInt, Value: int64(1)}},
		},
	}
	count := 0
	Walk(obj, func(n Expr) bool {
		count++
		return true
	})
	// Object, Ident(x), Literal(1) = 3
	if count != 3 {
		t.Errorf("Walk visited %d nodes, want 3", count)
	}
}

func TestWalk_Paren(t *testing.T) {
	p := &Paren{Inner: &Ident{Name: "x"}}
	count := 0
	Walk(p, func(n Expr) bool {
		count++
		return true
	})
	// Paren, Ident = 2
	if count != 2 {
		t.Errorf("Walk visited %d nodes, want 2", count)
	}
}

func TestWalk_CallWithReceiver(t *testing.T) {
	c := &Call{
		Receiver: &Ident{Name: "obj"},
		Callee:   "method",
		Args:     []Expr{&Literal{Kind: LitInt, Value: int64(1)}},
	}
	count := 0
	Walk(c, func(n Expr) bool {
		count++
		return true
	})
	// Call, Ident(obj), Literal(1) = 3
	if count != 3 {
		t.Errorf("Walk visited %d nodes, want 3", count)
	}
}

func TestWalk_ErrorExprLeaf(t *testing.T) {
	e := &ErrorExpr{Message: "test"}
	count := 0
	Walk(e, func(n Expr) bool {
		count++
		return true
	})
	if count != 1 {
		t.Errorf("Walk on ErrorExpr visited %d nodes, want 1", count)
	}
}
