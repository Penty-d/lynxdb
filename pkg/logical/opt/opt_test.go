package opt

import (
	"bufio"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/format"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

var update = flag.Bool("update", false, "update golden files")

// ---------------------------------------------------------------------------
// Helper: parse expression, optimize, format
// ---------------------------------------------------------------------------

func parseExpr(t *testing.T, input string) ast.Expr {
	t.Helper()
	e, diags := parser.ParseExpr(input)
	if len(diags) > 0 {
		t.Fatalf("parse %q: %v", input, diags[0].Message)
	}
	return e
}

func optimizeExpr(t *testing.T, input string) string {
	t.Helper()
	e := parseExpr(t, input)
	// Wrap in a Filter node so Optimize can visit it.
	// No input child — plan rules (filter-elim, filter-false-to-empty)
	// require a non-nil input and will skip this synthetic wrapper.
	plan := &logical.Plan{
		Root: &logical.Filter{Expr: e},
	}
	plan, _ = Optimize(plan)
	out := plan.Root.(*logical.Filter).Expr
	return format.Expr(out)
}

// optimizeExprApplied returns the formatted result and Applied slice.
func optimizeExprApplied(t *testing.T, input string) (string, []Applied) {
	t.Helper()
	e := parseExpr(t, input)
	plan := &logical.Plan{
		Root: &logical.Filter{Expr: e},
	}
	plan, applied := Optimize(plan)
	out := plan.Root.(*logical.Filter).Expr
	return format.Expr(out), applied
}

// parseDesugarLower parses a full query, desugars, and lowers it.
func parseDesugarLower(t *testing.T, query string) *logical.Plan {
	t.Helper()
	q, pDiags := parser.Parse(query)
	if len(pDiags) > 0 {
		var msgs []string
		for _, d := range pDiags {
			msgs = append(msgs, d.Message)
		}
		t.Fatalf("parse %q: %s", query, strings.Join(msgs, "; "))
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	plan, _ := logical.Lower(desugared, logical.Options{DefaultSource: "main"})
	return plan
}

// ---------------------------------------------------------------------------
// Test: const-fold-arith
// ---------------------------------------------------------------------------

func TestConstFoldArith(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// int + int
		{name: "int_add", input: "2 + 3", want: "5"},
		{name: "int_sub", input: "10 - 4", want: "6"},
		{name: "int_mul", input: "6 * 7", want: "42"},
		// int/int truncates (§5.4: 5/2=2)
		{name: "int_div_truncate", input: "5 / 2", want: "2"},
		{name: "int_div_exact", input: "10 / 5", want: "2"},
		{name: "int_div_neg_truncate", input: "-7 / 2", want: "-3"},
		{name: "int_mod", input: "10 % 3", want: "1"},
		{name: "int_mod_zero", input: "10 % 0", want: "null"},

		// float operations
		{name: "float_add", input: "1.5 + 2.5", want: "4"},
		{name: "float_sub", input: "5.0 - 1.5", want: "3.5"},
		{name: "float_mul", input: "2.0 * 3.5", want: "7"},
		{name: "float_div", input: "5.0 / 2", want: "2.5"},
		{name: "float_div_exact", input: "10.0 / 5.0", want: "2"},

		// int + float promotion
		{name: "int_float_add", input: "2 + 3.0", want: "5"},
		{name: "float_int_sub", input: "5.5 - 2", want: "3.5"},

		// division by zero -> null
		{name: "div_by_zero_int", input: "42 / 0", want: "null"},
		{name: "div_by_zero_float", input: "42.0 / 0.0", want: "null"},

		// string concatenation
		{name: "string_concat", input: `"a" + "b"`, want: `"ab"`},
		{name: "string_concat_empty", input: `"hello" + ""`, want: `"hello"`},
		{name: "string_concat_multi", input: `"a" + "b" + "c"`, want: `"abc"`},

		// duration ± duration
		{name: "dur_add", input: "1h + 30m", want: "90m"},
		{name: "dur_sub", input: "2h - 30m", want: "90m"},

		// duration * number
		{name: "dur_mul_int", input: "30m * 2", want: "1h"},
		{name: "int_mul_dur", input: "3 * 20m", want: "1h"},

		// duration / number
		{name: "dur_div_int", input: "1h / 2", want: "30m"},

		// duration / duration -> float
		{name: "dur_div_dur", input: "1h / 30m", want: "2"},

		// duration / 0 -> null
		{name: "dur_div_zero", input: "1h / 0", want: "null"},

		// Unary negation folding
		{name: "neg_int", input: "-42", want: "-42"},
		{name: "neg_float", input: "-3.14", want: "-3.14"},
		{name: "neg_dur", input: "-30s", want: "-30s"},

		// float % -> leave unfolded (float mod not supported per §5.4)
		{name: "float_mod_leave", input: "5.0 % 2.0", want: "5.0 % 2.0"},
		// Mixed-type that sema would catch -> leave
		{name: "string_plus_int_leave", input: `"a" + 1`, want: `"a" + 1`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("constFoldArith(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: const-fold-compare
// ---------------------------------------------------------------------------

func TestConstFoldCompare(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// int comparisons
		{name: "int_eq_true", input: "5 == 5", want: "true"},
		{name: "int_eq_false", input: "5 == 6", want: "false"},
		{name: "int_neq_true", input: "5 != 6", want: "true"},
		{name: "int_lt_true", input: "3 < 5", want: "true"},
		{name: "int_lt_false", input: "5 < 3", want: "false"},
		{name: "int_lte_true", input: "5 <= 5", want: "true"},
		{name: "int_gt_true", input: "5 > 3", want: "true"},
		{name: "int_gte_true", input: "5 >= 5", want: "true"},

		// float comparisons
		{name: "float_eq", input: "3.14 == 3.14", want: "true"},
		{name: "float_lt", input: "2.5 < 3.5", want: "true"},

		// int vs float promotion
		{name: "int_float_eq", input: "5 == 5.0", want: "true"},
		{name: "int_float_lt", input: "5 < 5.5", want: "true"},

		// string comparisons (lexical per §5.4)
		{name: "str_eq", input: `"abc" == "abc"`, want: "true"},
		{name: "str_lt_lex", input: `"10" < "2"`, want: "true"}, // lexical: "10" < "2"
		{name: "str_gt", input: `"b" > "a"`, want: "true"},

		// bool comparisons (== and != only)
		{name: "bool_eq_true", input: "true == true", want: "true"},
		{name: "bool_eq_false", input: "true == false", want: "false"},
		{name: "bool_neq", input: "true != false", want: "true"},
		{name: "bool_lt_leave", input: "true < false", want: "true < false"},

		// duration comparisons
		{name: "dur_eq", input: "1h == 60m", want: "true"},
		{name: "dur_lt", input: "30s < 1m", want: "true"},

		// null comparisons
		{name: "null_eq_null", input: "null == null", want: "true"},
		{name: "null_neq_null", input: "null != null", want: "false"},
		{name: "null_lt_null_leave", input: "null < null", want: "null < null"},
		// null vs non-null -> leave (three-valued)
		{name: "null_vs_int_leave", input: "null == 5", want: "null == 5"},
		{name: "int_vs_null_leave", input: "5 == null", want: "5 == null"},

		// cross-type -> leave
		{name: "str_vs_int_leave", input: `"5" == 5`, want: `"5" == 5`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("constFoldCompare(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: bool-simplify
// ---------------------------------------------------------------------------

func TestBoolSimplify(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// AND identity / annihilation
		{name: "true_and_x", input: "true and x", want: "x"},
		{name: "x_and_true", input: "x and true", want: "x"},
		{name: "false_and_x", input: "false and x", want: "false"},
		{name: "x_and_false", input: "x and false", want: "false"},

		// OR identity / annihilation
		{name: "true_or_x", input: "true or x", want: "true"},
		{name: "x_or_true", input: "x or true", want: "true"},
		{name: "false_or_x", input: "false or x", want: "x"},
		{name: "x_or_false", input: "x or false", want: "x"},

		// NOT
		{name: "not_true", input: "not true", want: "false"},
		{name: "not_false", input: "not false", want: "true"},
		{name: "not_not_x", input: "not not x", want: "x"},

		// Three-valued soundness: these are SOUND under 3VL
		// null AND false = false (so false AND X -> false is sound even if X is null)
		// null OR true = true (so true OR X -> true is sound even if X is null)
		// These are tested implicitly by the rules above since X could be null at runtime.

		// Combined: not not not x -> not x (needs two passes)
		{name: "triple_not", input: "not not not x", want: "not x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("boolSimplify(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: coalesce-fold
// ---------------------------------------------------------------------------

func TestCoalesceFold(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "non_null_lit", input: "42 ?? x", want: "42"},
		{name: "string_lit", input: `"hello" ?? x`, want: `"hello"`},
		{name: "null_coalesce", input: "null ?? x", want: "x"},
		{name: "null_coalesce_lit", input: "null ?? 99", want: "99"},
		{name: "non_lit_left", input: "y ?? x", want: "y ?? x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("coalesceFold(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: if-fold
// ---------------------------------------------------------------------------

func TestIfFold(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "if_true", input: "if(true, 1, 2)", want: "1"},
		{name: "if_false", input: "if(false, 1, 2)", want: "2"},
		{name: "if_null", input: "if(null, 1, 2)", want: "null"},
		// Non-literal condition -> leave
		{name: "if_dynamic", input: "if(x, 1, 2)", want: "if(x, 1, 2)"},
		// Wrong arity -> leave
		{name: "if_wrong_arity", input: "if(true, 1)", want: "if(true, 1)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("ifFold(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: cmp-normalize
// ---------------------------------------------------------------------------

func TestCmpNormalize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "lt_flip", input: "5 < x", want: "x > 5"},
		{name: "gt_flip", input: "5 > x", want: "x < 5"},
		{name: "eq_flip", input: "5 == x", want: "x == 5"},
		{name: "neq_flip", input: `"a" != x`, want: `x != "a"`},
		{name: "lte_flip", input: "5 <= x", want: "x >= 5"},
		{name: "gte_flip", input: "5 >= x", want: "x <= 5"},
		// Both literals -> fold (const-fold-compare fires first, then
		// cmp-normalize sees a literal, not a comparison anymore).
		{name: "both_lit_no_flip", input: "5 == 5", want: "true"},
		// Neither side literal -> no flip
		{name: "no_flip", input: "x < y", want: "x < y"},
		// Right is literal, left is not -> already canonical
		{name: "already_canonical", input: "x > 5", want: "x > 5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("cmpNormalize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: paren-strip
// ---------------------------------------------------------------------------

func TestParenStrip(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "simple_paren", input: "(x)", want: "x"},
		{name: "double_paren", input: "((x))", want: "x"},
		{name: "paren_binary", input: "(x + y)", want: "x + y"},
		{name: "nested_paren", input: "((x + y)) * z", want: "(x + y) * z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := optimizeExpr(t, tt.input)
			if got != tt.want {
				t.Errorf("parenStrip(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: plan-level (full query pipeline)
// ---------------------------------------------------------------------------

func TestPlanLevel(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains string // substring expected in Dump output
	}{
		{
			name:     "filter_true_eliminated",
			query:    "from main | where 1 == 1",
			contains: "Scan(main)", // expr simplifies to true; filter-elim removes it
		},
		{
			name:     "filter_false_empty",
			query:    "from main | where 1 == 2",
			contains: "Empty()", // expr simplifies to false; filter-false-to-empty
		},
		{
			name:     "extend_folded",
			query:    "from main | extend x = 2 + 3",
			contains: "Extend(x=5)",
		},
		{
			name:     "nested_arith_in_filter",
			query:    "from main | where status >= 200 + 300",
			contains: "Filter((status >= 500))",
		},
		{
			name:     "bool_simplify_in_filter",
			query:    "from main | where true and level == \"error\"",
			contains: `Filter((level == "error"))`,
		},
		{
			name:     "cmp_normalize_in_filter",
			query:    "from main | where 500 <= status",
			contains: "Filter((status >= 500))",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := parseDesugarLower(t, tt.query)
			optimized, _ := Optimize(plan)
			dump := optimized.Dump()
			if !strings.Contains(dump, tt.contains) {
				t.Errorf("plan dump does not contain %q\nGot:\n%s", tt.contains, dump)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: fixed-point convergence
// ---------------------------------------------------------------------------

func TestFixedPointConvergence(t *testing.T) {
	// Expression that requires multiple passes:
	// not not not not x
	// Pass 1: not not not not x -> not not x (inner not-not removed)
	// Pass 2: not not x -> x
	// Pass 3: no change -> converges.
	got := optimizeExpr(t, "not not not not x")
	if got != "x" {
		t.Errorf("fixed-point: got %q, want %q", got, "x")
	}
}

func TestFixedPointMaxPasses(t *testing.T) {
	// The driver caps at maxPasses=10. A deeply nested expression that
	// converges within 10 passes should reach its final form.
	// not^8 x = x (4 passes of removing pairs).
	got := optimizeExpr(t, "not not not not not not not not x")
	if got != "x" {
		t.Errorf("8-deep not: got %q, want %q", got, "x")
	}
}

// ---------------------------------------------------------------------------
// Test: purity (original plan unchanged)
// ---------------------------------------------------------------------------

func TestPurityOriginalPlanUnchanged(t *testing.T) {
	// Parse an expression, capture its string form, optimize in a plan, then
	// verify the original expression is unchanged.
	exprStr := "2 + 3"
	e := parseExpr(t, exprStr)
	originalStr := format.Expr(e)

	// Create a plan with this expression.
	plan := &logical.Plan{
		Root: &logical.Filter{Expr: e},
	}
	// Take a copy of the expression pointer before optimization.
	origExprPtr := e

	_, _ = Optimize(plan)

	// The original AST node must be unchanged.
	afterStr := format.Expr(origExprPtr)
	if afterStr != originalStr {
		t.Errorf("original expression changed: was %q, now %q", originalStr, afterStr)
	}

	// But the plan's filter now points to a NEW expression.
	optimizedStr := format.Expr(plan.Root.(*logical.Filter).Expr)
	if optimizedStr != "5" {
		t.Errorf("optimized result: got %q, want %q", optimizedStr, "5")
	}
}

// ---------------------------------------------------------------------------
// Test: determinism
// ---------------------------------------------------------------------------

func TestDeterminism(t *testing.T) {
	query := "from main | where 2 + 3 == 5 and true or false"
	plan1 := parseDesugarLower(t, query)
	plan2 := parseDesugarLower(t, query)

	_, applied1 := Optimize(plan1)
	_, applied2 := Optimize(plan2)

	dump1 := plan1.Dump()
	dump2 := plan2.Dump()

	if dump1 != dump2 {
		t.Errorf("non-deterministic Dump:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", dump1, dump2)
	}

	if len(applied1) != len(applied2) {
		t.Fatalf("non-deterministic Applied length: %d vs %d", len(applied1), len(applied2))
	}
	for i := range applied1 {
		if applied1[i] != applied2[i] {
			t.Errorf("non-deterministic Applied[%d]: %+v vs %+v", i, applied1[i], applied2[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Applied reporting
// ---------------------------------------------------------------------------

func TestAppliedReporting(t *testing.T) {
	_, applied := optimizeExprApplied(t, "2 + 3")
	found := false
	for _, a := range applied {
		if a.Rule == "const-fold-arith" && a.Count > 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected const-fold-arith in Applied, got %v", applied)
	}
}

// ---------------------------------------------------------------------------
// Test: walkExprs covers Aggregate, Sort, TopK, Helper nodes
// ---------------------------------------------------------------------------

func TestWalkExprsAggregate(t *testing.T) {
	// Test bin duration folding inside Aggregate group key.
	query := "from main | stats count() by bin(_time, 1h + 1h)"
	plan := parseDesugarLower(t, query)
	optimized, _ := Optimize(plan)
	dump := optimized.Dump()
	// The bin duration 1h+1h should fold to 2h.
	if !strings.Contains(dump, "2h") {
		t.Errorf("expected folded bin duration 2h in aggregate, got:\n%s", dump)
	}

	// Test that Agg.WhereCond is rewritten (even though String() doesn't show it).
	query2 := "from main | stats count(where 1 + 1 == 2)"
	plan2 := parseDesugarLower(t, query2)
	optimized2, _ := Optimize(plan2)
	agg := optimized2.Root.(*logical.Aggregate)
	if agg.Aggs[0].WhereCond == nil {
		t.Fatal("expected non-nil WhereCond after optimization")
	}
	wcStr := format.Expr(agg.Aggs[0].WhereCond)
	if wcStr != "true" {
		t.Errorf("expected WhereCond to fold to true, got %q", wcStr)
	}
}

func TestWalkExprsSort(t *testing.T) {
	query := "from main | sort 1 + 1"
	plan := parseDesugarLower(t, query)
	optimized, _ := Optimize(plan)
	dump := optimized.Dump()
	if !strings.Contains(dump, "Sort(+2)") {
		t.Errorf("expected folded sort key, got:\n%s", dump)
	}
}

// ---------------------------------------------------------------------------
// Corpus golden plan tests (optimized plans)
// ---------------------------------------------------------------------------

type corpusEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Source   string   `json:"source"`
	SPL2     string   `json:"spl2"`
	LynxFlow string   `json:"lynxflow"`
	Features []string `json:"features"`
	Notes    string   `json:"notes"`
}

func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "lynxflow", "testdata", "corpus", "corpus.jsonl"))
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var entries []corpusEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e corpusEntry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan corpus: %v", err)
	}
	return entries
}

const goldenDir = "../testdata/golden/plans"

// TestCorpus_OptimizedGoldenPlans runs all 63 corpus entries through
// parse -> desugar -> Lower -> Optimize -> Dump and compares against
// golden plan files. Run with -update to regenerate.
func TestCorpus_OptimizedGoldenPlans(t *testing.T) {
	entries := loadCorpus(t)
	if len(entries) < 50 {
		t.Fatalf("corpus has %d entries, want at least 50", len(entries))
	}

	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	for _, e := range entries {
		t.Run(e.ID, func(t *testing.T) {
			plan := parseDesugarLower(t, e.LynxFlow)

			// Apply optimization — golden plans show the optimized plan.
			plan, _ = Optimize(plan)

			got := plan.Dump()
			goldenFile := filepath.Join(goldenDir, e.ID+".txt")

			if *update {
				if err := os.WriteFile(goldenFile, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Fatalf("read golden %s: %v\n  (run with -update to generate)", goldenFile, err)
			}
			if got != string(want) {
				t.Errorf("plan mismatch for %s\n--- want ---\n%s--- got ---\n%s",
					e.ID, string(want), got)
			}
		})
	}
}
