package parser

import (
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/format"
)

// ---------------------------------------------------------------------------
// Precedence golden table
// ---------------------------------------------------------------------------

func TestPrecedenceGolden(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// 1. or vs and: and binds tighter
		{"a or b and c", "(a or (b and c))"},
		// 2. and vs not: not binds tighter
		{"not a and b", "((not a) and b)"},
		// 3. not vs comparison: not binds looser than comparison
		{"not a == b", "(not (a == b))"},
		// 4. comparison vs coalesce
		{"a ?? 0 == b ?? 1", "((a ?? 0) == (b ?? 1))"},
		// 5. coalesce vs additive
		{"a + b ?? c", "((a + b) ?? c)"},
		// 6. additive vs multiplicative
		{"a + b * c", "(a + (b * c))"},
		// 7. multiplicative vs unary minus
		{"-a * b", "((-a) * b)"},
		// 8. unary minus vs postfix call
		{"-f(x)", "(-f(x))"},
		// 9. postfix member chain
		{"a.b.c", "a.b.c"},
		// 10. postfix index chain
		{"a[0][1]", "a[0][1]"},
		// 11. mixed postfix: a.b[0]?.c(1).d
		{"a.b[0]?.c(1).d", "a.b[0]?.c(1).d"},
		// 12. in with array
		{"x in [1, 2, 3]", "(x in [1, 2, 3])"},
		// 13. between
		{"x between 1 and 10", "(x between 1 and 10)"},
		// 14. coalesce chaining (left-associative)
		{"a ?? b ?? c", "((a ?? b) ?? c)"},
		// 15. or chaining
		{"a or b or c", "((a or b) or c)"},
		// 16. and chaining
		{"a and b and c", "((a and b) and c)"},
		// 17. not-not
		{"not not a", "(not (not a))"},
		// 18. complex: not a == b parses as not(a == b)
		{"not x > 5 and y < 10", "((not (x > 5)) and (y < 10))"},
		// 19. object literal
		{`{service: "api", retry: true}`, `{service: "api", retry: true}`},
		// 20. array literal
		{"[1, 2, 3]", "[1, 2, 3]"},
		// 21. nested lambda
		{`any(tags, t -> t.name == "vip")`, `any(tags, (t -> (t.name == "vip")))`},
		// 22. duration in arithmetic
		{"now() - 1h", "(now() - 1h)"},
		// 23. hex int
		{"0x2A + 1", "(42 + 1)"},
		// 24. raw string
		{`matches(_raw, r"\d+")`, `matches(_raw, r"\d+")`},
		// 25. parenthesized precedence override
		{"(a or b) and c", "((a or b) and c)"},
		// 26. subtraction vs unary minus
		{"a - -b", "(a - (-b))"},
		// 27. mod operator
		{"a % b + c", "((a % b) + c)"},
		// 28. division
		{"a / b / c", "((a / b) / c)"},
		// 29. equality vs comparison
		{"a == b != c", "(a == b)"}, // chained comparison generates error; first one returns
		// 30. nested calls
		{"f(g(x), h(y))", "f(g(x), h(y))"},
		// 31. bool literals
		{"true and false", "(true and false)"},
		// 32. null coalesce
		{"amount ?? 0", "(amount ?? 0)"},
		// 33. string literal
		{`"hello" + " world"`, `("hello" + " world")`},
		// 34. empty call
		{"count()", "count()"},
		// 35. strict cast bang
		{"int!(x)", "int!(x)"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr, diags := ParseExpr(tt.input)
			// For test case 29 (chained comparison), diags are expected.
			if tt.input != "a == b != c" {
				if len(diags) > 0 {
					t.Errorf("ParseExpr(%q): unexpected diags: %v", tt.input, diagMessages(diags))
				}
			}
			got := expr.String()
			if got != tt.want {
				t.Errorf("ParseExpr(%q).String():\n  got  %s\n  want %s", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Error cases with exact message/suggestion assertions
// ---------------------------------------------------------------------------

func TestError_SingleEquals(t *testing.T) {
	_, diags := ParseExpr("a = b")
	requireDiag(t, diags, CodeSingleEquals, "'=' is assignment; use '==' for comparison", "replace = with ==")
}

func TestError_ChainedComparison(t *testing.T) {
	_, diags := ParseExpr("a < b < c")
	requireDiag(t, diags, CodeChainedComparison, "chained comparisons are not allowed", "use 'and' to combine: a < b and b < c")
}

func TestError_UnterminatedParen(t *testing.T) {
	_, diags := ParseExpr("(a + b")
	requireDiagContaining(t, diags, "')'")
}

func TestError_SingleQuoteString(t *testing.T) {
	// Single quote produces a lexer error surfaced as E004.
	_, diags := ParseExpr("'hello'")
	requireDiag(t, diags, CodeLexerError, "single quotes are not allowed", "")
}

func TestError_KeywordMisuse(t *testing.T) {
	// "where not" is fine (not is a unary prefix), but "a and or b" errors.
	_, diags := ParseExpr("a and or b")
	if len(diags) == 0 {
		t.Fatal("expected diagnostic for 'a and or b'")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "unexpected keyword") || strings.Contains(d.Message, "expected expression") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected keyword-misuse diagnostic, got: %v", diagMessages(diags))
	}
}

func TestError_TrailingOperator(t *testing.T) {
	_, diags := ParseExpr("a +")
	if len(diags) == 0 {
		t.Fatal("expected diagnostic for trailing operator")
	}
}

func TestError_WhereNotOk(t *testing.T) {
	// "not a" is valid.
	expr, diags := ParseExpr("not a")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags for 'not a': %v", diagMessages(diags))
	}
	if expr.String() != "(not a)" {
		t.Errorf("got %s, want (not a)", expr.String())
	}
}

// ---------------------------------------------------------------------------
// Span assertions
// ---------------------------------------------------------------------------

func TestSpan_BinaryCoversOperands(t *testing.T) {
	tests := []struct {
		input     string
		wantStart int
		wantEnd   int
	}{
		{"a + b", 0, 5},
		{"a == b", 0, 6},
		{"x or y", 0, 6},
		{"a * b + c", 0, 9}, // (a * b) + c
		{"not x", 0, 5},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			expr, diags := ParseExpr(tt.input)
			if len(diags) > 0 {
				t.Fatalf("unexpected diags: %v", diagMessages(diags))
			}
			span := expr.ExprSpan()
			if span.Start != tt.wantStart || span.End != tt.wantEnd {
				t.Errorf("span = [%d, %d), want [%d, %d)", span.Start, span.End, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestSpan_IdentAndLiteral(t *testing.T) {
	// "  foo  " — ident starts at 2, ends at 5.
	expr, diags := ParseExpr("  foo  ")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	span := expr.ExprSpan()
	if span.Start != 2 || span.End != 5 {
		t.Errorf("span = [%d, %d), want [2, 5)", span.Start, span.End)
	}
}

func TestSpan_CallExpression(t *testing.T) {
	// "f(x, y)" — spans from f to closing paren.
	expr, diags := ParseExpr("f(x, y)")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	span := expr.ExprSpan()
	if span.Start != 0 || span.End != 7 {
		t.Errorf("span = [%d, %d), want [0, 7)", span.Start, span.End)
	}
}

func TestSpan_ParenExpression(t *testing.T) {
	// "(a + b)" — spans from ( to ).
	expr, diags := ParseExpr("(a + b)")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	span := expr.ExprSpan()
	if span.Start != 0 || span.End != 7 {
		t.Errorf("span = [%d, %d), want [0, 7)", span.Start, span.End)
	}
}

func TestSpan_ArrayLiteral(t *testing.T) {
	// "[1, 2]" — spans from [ to ].
	expr, diags := ParseExpr("[1, 2]")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	span := expr.ExprSpan()
	if span.Start != 0 || span.End != 6 {
		t.Errorf("span = [%d, %d), want [0, 6)", span.Start, span.End)
	}
}

// ---------------------------------------------------------------------------
// Soft keyword test (D29)
// ---------------------------------------------------------------------------

func TestSoftKeywords(t *testing.T) {
	// rate, latency, top — all stage-starting keywords, but ordinary idents
	// in expression position.
	expr, diags := ParseExpr("rate + latency * top")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	want := "(rate + (latency * top))"
	if expr.String() != want {
		t.Errorf("got %s, want %s", expr.String(), want)
	}
}

func TestSoftKeyword_AsIdent(t *testing.T) {
	// All these soft keywords should parse as identifiers.
	keywords := []string{
		"stats", "sort", "head", "tail", "top", "rare", "rate", "latency",
		// "where" is excluded: it marks conditional-aggregate args.
		"from", "parse", "extend", "keep", "drop", "rename",
		"join", "union", "explode", "describe", "every", "dedup",
		"as", "by", "with", "on", "except",
	}
	for _, kw := range keywords {
		t.Run(kw, func(t *testing.T) {
			expr, diags := ParseExpr(kw)
			if len(diags) > 0 {
				t.Fatalf("ParseExpr(%q): unexpected diags: %v", kw, diagMessages(diags))
			}
			ident, ok := expr.(*ast.Ident)
			if !ok {
				t.Fatalf("expected *ast.Ident, got %T", expr)
			}
			if ident.Name != strings.ToLower(kw) {
				t.Errorf("name = %q, want %q", ident.Name, strings.ToLower(kw))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Corpus smoke: RFC-002 example expressions
// ---------------------------------------------------------------------------

func TestCorpusExpressions(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"filtered aggregate predicate", `status >= 500 and has(_raw, "timeout")`},
		{"nested json with lambda", `user.role == "admin" and any(tags, t -> t.name == "vip")`},
		{"null coalesce", `amount ?? 0`},
		{"conditional with aggregates", `if(stdev_f > 0, delta_f / stdev_f, null)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, diags := ParseExpr(tt.input)
			if len(diags) > 0 {
				t.Errorf("ParseExpr(%q): %d diag(s):", tt.input, len(diags))
				for _, d := range diags {
					t.Errorf("  [%s] %s (span %d..%d)", d.Code, d.Message, d.Span.Start, d.Span.End)
				}
			}
			if expr == nil {
				t.Fatal("expr is nil")
			}
			// Verify it round-trips to a non-empty string.
			s := expr.String()
			if s == "" || s == "<error>" {
				t.Errorf("String() = %q, expected a real expression", s)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Specific feature tests
// ---------------------------------------------------------------------------

func TestLambda_Nested(t *testing.T) {
	// any(tags, t -> t.name == "vip")
	expr, diags := ParseExpr(`any(tags, t -> t.name == "vip")`)
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	want := `any(tags, (t -> (t.name == "vip")))`
	if expr.String() != want {
		t.Errorf("got %s, want %s", expr.String(), want)
	}
}

func TestLambda_NestedDouble(t *testing.T) {
	// map(arr, x -> filter(x, y -> y > 0))
	expr, diags := ParseExpr("map(arr, x -> filter(x, y -> y > 0))")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	want := "map(arr, (x -> filter(x, (y -> (y > 0)))))"
	if expr.String() != want {
		t.Errorf("got %s, want %s", expr.String(), want)
	}
}

func TestStrictCastBang(t *testing.T) {
	expr, diags := ParseExpr("int!(x)")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	call, ok := expr.(*ast.Call)
	if !ok {
		t.Fatalf("expected *ast.Call, got %T", expr)
	}
	if !call.Bang {
		t.Error("expected Bang=true")
	}
	if call.Callee != "int" {
		t.Errorf("callee = %q, want 'int'", call.Callee)
	}
	if call.String() != "int!(x)" {
		t.Errorf("String() = %q, want 'int!(x)'", call.String())
	}
}

func TestObjectLiteral(t *testing.T) {
	expr, diags := ParseExpr(`{service: "api", count: 42}`)
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	obj, ok := expr.(*ast.Object)
	if !ok {
		t.Fatalf("expected *ast.Object, got %T", expr)
	}
	if len(obj.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(obj.Entries))
	}
	if obj.Entries[0].Key != "service" {
		t.Errorf("entry[0].Key = %q", obj.Entries[0].Key)
	}
	if obj.Entries[1].Key != "count" {
		t.Errorf("entry[1].Key = %q", obj.Entries[1].Key)
	}
}

func TestArrayLiteral(t *testing.T) {
	expr, diags := ParseExpr("[1, 2, 3]")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	arr, ok := expr.(*ast.Array)
	if !ok {
		t.Fatalf("expected *ast.Array, got %T", expr)
	}
	if len(arr.Elems) != 3 {
		t.Fatalf("expected 3 elems, got %d", len(arr.Elems))
	}
}

func TestInWithArray(t *testing.T) {
	expr, diags := ParseExpr(`level in ["error", "fatal"]`)
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	want := `(level in ["error", "fatal"])`
	if expr.String() != want {
		t.Errorf("got %s, want %s", expr.String(), want)
	}
}

func TestBetween(t *testing.T) {
	expr, diags := ParseExpr("x between 1 and 10")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	want := "(x between 1 and 10)"
	if expr.String() != want {
		t.Errorf("got %s, want %s", expr.String(), want)
	}
}

func TestDurationArithmetic(t *testing.T) {
	expr, diags := ParseExpr("now() - 1h")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	want := "(now() - 1h)"
	if expr.String() != want {
		t.Errorf("got %s, want %s", expr.String(), want)
	}
}

func TestHexInt(t *testing.T) {
	expr, diags := ParseExpr("0x2A")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	lit, ok := expr.(*ast.Literal)
	if !ok {
		t.Fatalf("expected *ast.Literal, got %T", expr)
	}
	if lit.Value.(int64) != 42 {
		t.Errorf("value = %v, want 42", lit.Value)
	}
}

func TestSafeMember(t *testing.T) {
	expr, diags := ParseExpr("a?.b")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	if expr.String() != "a?.b" {
		t.Errorf("got %s, want a?.b", expr.String())
	}
}

func TestBacktickIdent(t *testing.T) {
	expr, diags := ParseExpr("`field-with-dash`")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		t.Fatalf("expected *ast.Ident, got %T", expr)
	}
	if !ident.Quoted {
		t.Error("expected Quoted=true")
	}
	if ident.Name != "field-with-dash" {
		t.Errorf("Name = %q, want 'field-with-dash'", ident.Name)
	}
}

func TestEmptyArrayLiteral(t *testing.T) {
	expr, diags := ParseExpr("[]")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	arr, ok := expr.(*ast.Array)
	if !ok {
		t.Fatalf("expected *ast.Array, got %T", expr)
	}
	if len(arr.Elems) != 0 {
		t.Errorf("expected 0 elems, got %d", len(arr.Elems))
	}
}

func TestEmptyObjectLiteral(t *testing.T) {
	expr, diags := ParseExpr("{}")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	obj, ok := expr.(*ast.Object)
	if !ok {
		t.Fatalf("expected *ast.Object, got %T", expr)
	}
	if len(obj.Entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(obj.Entries))
	}
}

func TestNullLiteral(t *testing.T) {
	expr, diags := ParseExpr("null")
	if len(diags) > 0 {
		t.Fatalf("unexpected diags: %v", diagMessages(diags))
	}
	lit, ok := expr.(*ast.Literal)
	if !ok {
		t.Fatalf("expected *ast.Literal, got %T", expr)
	}
	if lit.Kind != ast.LitNull {
		t.Errorf("kind = %v, want LitNull", lit.Kind)
	}
}

// ---------------------------------------------------------------------------
// Walk / Inspect
// ---------------------------------------------------------------------------

func TestWalk(t *testing.T) {
	expr, _ := ParseExpr("a + b * c")
	var names []string
	ast.Walk(expr, func(n ast.Expr) bool {
		if id, ok := n.(*ast.Ident); ok {
			names = append(names, id.Name)
		}
		return true
	})
	want := "a b c"
	got := strings.Join(names, " ")
	if got != want {
		t.Errorf("Walk idents: got %q, want %q", got, want)
	}
}

func TestWalk_ShortCircuit(t *testing.T) {
	expr, _ := ParseExpr("a + b * c")
	count := 0
	ast.Walk(expr, func(n ast.Expr) bool {
		count++
		// Stop at the multiplication subtree.
		if _, ok := n.(*ast.Binary); ok {
			if b := n.(*ast.Binary); b.Op == ast.OpMul {
				return false
			}
		}
		return true
	})
	// Should visit: Binary(+), Ident(a), Binary(*) — then stop. = 3
	if count != 3 {
		t.Errorf("visited %d nodes, want 3", count)
	}
}

func TestInspect(t *testing.T) {
	expr, _ := ParseExpr("f(x)")
	count := 0
	ast.Inspect(expr, func(n ast.Expr) {
		count++
	})
	// Call, Ident(x) = 2
	if count != 2 {
		t.Errorf("Inspect visited %d nodes, want 2", count)
	}
}

// ---------------------------------------------------------------------------
// ErrorExpr recovery
// ---------------------------------------------------------------------------

func TestErrorExpr_RecoveryAtComma(t *testing.T) {
	// f(, x) — missing first argument produces ErrorExpr, then x is parsed.
	expr, diags := ParseExpr("f(, x)")
	if len(diags) == 0 {
		t.Fatal("expected diagnostic")
	}
	call, ok := expr.(*ast.Call)
	if !ok {
		t.Fatalf("expected *ast.Call, got %T", expr)
	}
	// Should still have some arguments parsed despite the error.
	_ = call
}

func TestErrorExpr_EmptyInput(t *testing.T) {
	expr, diags := ParseExpr("")
	if len(diags) == 0 {
		t.Fatal("expected diagnostic for empty input")
	}
	if _, ok := expr.(*ast.ErrorExpr); !ok {
		t.Errorf("expected *ast.ErrorExpr for empty input, got %T", expr)
	}
}

// ---------------------------------------------------------------------------
// Fuzz test
// ---------------------------------------------------------------------------

func FuzzParseExpr(f *testing.F) {
	// Seed with golden table inputs.
	seeds := []string{
		"a or b and c",
		"not a and b",
		"not a == b",
		"a ?? 0 == b ?? 1",
		"a + b ?? c",
		"a + b * c",
		"-a * b",
		"-f(x)",
		"a.b.c",
		"a[0][1]",
		"a.b[0]?.c(1).d",
		"x in [1, 2, 3]",
		"x between 1 and 10",
		"a ?? b ?? c",
		"a or b or c",
		"a and b and c",
		"not not a",
		"not x > 5 and y < 10",
		`{service: "api", retry: true}`,
		"[1, 2, 3]",
		`any(tags, t -> t.name == "vip")`,
		"now() - 1h",
		"0x2A + 1",
		`matches(_raw, r"\d+")`,
		"(a or b) and c",
		"a - -b",
		"a % b + c",
		"a / b / c",
		"f(g(x), h(y))",
		"true and false",
		"amount ?? 0",
		`"hello" + " world"`,
		"count()",
		"int!(x)",
		`status >= 500 and has(_raw, "timeout")`,
		`user.role == "admin" and any(tags, t -> t.name == "vip")`,
		`if(stdev_f > 0, delta_f / stdev_f, null)`,
		"rate + latency * top",
		"a = b",
		"a < b < c",
		"(a + b",
		"'hello'",
		"a and or b",
		"a +",
		"",
		"null",
		`map(arr, x -> filter(x, y -> y > 0))`,
		`level in ["error", "fatal"]`,
		"a?.b ?? c",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		expr, diags := ParseExpr(input)

		// Property 1: never panics (if we got here, it didn't).

		// Property 2: expr is never nil.
		if expr == nil {
			t.Fatal("ParseExpr returned nil expr")
		}

		// Property 3: all diagnostic spans are within bounds.
		for i, d := range diags {
			if d.Span.Start < 0 {
				t.Errorf("diag[%d] Span.Start < 0: %d", i, d.Span.Start)
			}
			if d.Span.End < d.Span.Start {
				t.Errorf("diag[%d] Span.End < Start: %d < %d", i, d.Span.End, d.Span.Start)
			}
			if d.Span.End > len(input)+1 {
				t.Errorf("diag[%d] Span.End > len(input)+1: %d > %d", i, d.Span.End, len(input)+1)
			}
		}

		// Property 4: all node spans are within bounds.
		ast.Walk(expr, func(n ast.Expr) bool {
			sp := n.ExprSpan()
			if sp.Start < 0 {
				t.Errorf("node %T Span.Start < 0: %d", n, sp.Start)
			}
			if sp.End < sp.Start {
				t.Errorf("node %T Span.End < Start: %d < %d", n, sp.End, sp.Start)
			}
			// Allow End == len+1 for error nodes that may reference EOF position.
			if sp.End > len(input)+1 {
				t.Errorf("node %T Span.End > len(input)+1: %d > %d", n, sp.End, len(input)+1)
			}
			return true
		})

		// Property 5: String() never panics.
		_ = expr.String()

		// Property 6 (format-roundtrip): if parse succeeds with zero diags,
		// formatting and re-parsing must also produce zero diags and the
		// same formatted output (fixpoint).
		if len(diags) == 0 {
			formatted := format.Expr(expr)
			expr2, diags2 := ParseExpr(formatted)
			if len(diags2) > 0 {
				t.Errorf("format-roundtrip: re-parse has diags:\n  input: %q\n  formatted: %q\n  diags: %v",
					input, formatted, diagMessages(diags2))
				return
			}
			formatted2 := format.Expr(expr2)
			if formatted != formatted2 {
				t.Errorf("format-roundtrip: fixpoint violated:\n  input: %q\n  f1: %q\n  f2: %q",
					input, formatted, formatted2)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func diagMessages(diags []Diag) []string {
	msgs := make([]string, len(diags))
	for i, d := range diags {
		msgs[i] = d.Message
	}
	return msgs
}

func requireDiag(t *testing.T, diags []Diag, code DiagCode, msgSubstr, suggestion string) {
	t.Helper()
	for _, d := range diags {
		if d.Code == code && strings.Contains(d.Message, msgSubstr) {
			if suggestion != "" && d.Suggestion != suggestion {
				t.Errorf("diag %s: suggestion = %q, want %q", code, d.Suggestion, suggestion)
			}
			return
		}
	}
	t.Errorf("expected diag with code=%s containing %q, got: %v", code, msgSubstr, diagMessages(diags))
}

func requireDiagContaining(t *testing.T, diags []Diag, substr string) {
	t.Helper()
	for _, d := range diags {
		if strings.Contains(d.Message, substr) || containsExpected(d.Expected, substr) {
			return
		}
	}
	t.Errorf("expected diag containing %q, got: %v", substr, diagMessages(diags))
}

func containsExpected(expected []string, substr string) bool {
	for _, e := range expected {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}
