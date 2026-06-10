package vm

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// TestGoldenFuncCompilation compiles every registered function with valid arity,
// disassembles the resulting program, and compares against golden files.
//
// To regenerate goldens, run:
//
//	go test ./pkg/vm/ -run TestGoldenFuncCompilation -update-goldens
//
// The golden files prove that the function-registry refactor produces
// byte-identical bytecode compared to the original switch-based dispatch.
func TestGoldenFuncCompilation(t *testing.T) {
	updateGoldens := os.Getenv("UPDATE_GOLDENS") == "1"

	goldenDir := filepath.Join("testdata", "goldens")
	if updateGoldens {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("mkdir goldens: %v", err)
		}
	}

	// Helper to build a literal arg.
	lit := func(v string) spl2.Expr { return &spl2.LiteralExpr{Value: v} }
	qlit := func(v string) spl2.Expr { return &spl2.LiteralExpr{Value: `"` + v + `"`} }
	field := func(name string) spl2.Expr { return &spl2.FieldExpr{Name: name} }

	type testCase struct {
		name string
		expr spl2.Expr
	}

	// Build comprehensive set of expressions — one per function, with valid arity.
	cases := []testCase{
		// Control flow / jump-patching functions
		{"if", &spl2.FuncCallExpr{Name: "IF", Args: []spl2.Expr{
			field("status"), qlit("yes"), qlit("no"),
		}}},
		{"case", &spl2.FuncCallExpr{Name: "CASE", Args: []spl2.Expr{
			field("x"), qlit("a"), field("y"), qlit("b"), qlit("default"),
		}}},
		{"case_even", &spl2.FuncCallExpr{Name: "CASE", Args: []spl2.Expr{
			field("x"), qlit("a"), field("y"), qlit("b"),
		}}},
		{"validate", &spl2.FuncCallExpr{Name: "validate", Args: []spl2.Expr{
			field("a"), qlit("err1"), field("b"), qlit("err2"),
		}}},
		{"coalesce", &spl2.FuncCallExpr{Name: "coalesce", Args: []spl2.Expr{
			field("a"), field("b"), qlit("fallback"),
		}}},
		{"null_fn", &spl2.FuncCallExpr{Name: "null", Args: nil}},
		{"nullif", &spl2.FuncCallExpr{Name: "nullif", Args: []spl2.Expr{
			field("a"), qlit("none"),
		}}},

		// Search / predicate
		{"searchmatch", &spl2.FuncCallExpr{Name: "searchmatch", Args: []spl2.Expr{qlit("error AND timeout")}}},
		{"in", &spl2.FuncCallExpr{Name: "in", Args: []spl2.Expr{
			field("status"), lit("200"), lit("201"), lit("204"),
		}}},

		// Null checks
		{"isnull", &spl2.FuncCallExpr{Name: "isnull", Args: []spl2.Expr{field("x")}}},
		{"isnotnull", &spl2.FuncCallExpr{Name: "isnotnull", Args: []spl2.Expr{field("x")}}},

		// Type conversions
		{"tonumber", &spl2.FuncCallExpr{Name: "tonumber", Args: []spl2.Expr{field("x")}}},
		{"todouble", &spl2.FuncCallExpr{Name: "todouble", Args: []spl2.Expr{field("x")}}},
		{"toint", &spl2.FuncCallExpr{Name: "toint", Args: []spl2.Expr{field("x")}}},
		{"tostring", &spl2.FuncCallExpr{Name: "tostring", Args: []spl2.Expr{field("x")}}},
		{"tobool", &spl2.FuncCallExpr{Name: "tobool", Args: []spl2.Expr{field("x")}}},

		// String functions
		{"len", &spl2.FuncCallExpr{Name: "len", Args: []spl2.Expr{field("msg")}}},
		{"lower", &spl2.FuncCallExpr{Name: "lower", Args: []spl2.Expr{field("msg")}}},
		{"upper", &spl2.FuncCallExpr{Name: "upper", Args: []spl2.Expr{field("msg")}}},
		{"substr_2arg", &spl2.FuncCallExpr{Name: "substr", Args: []spl2.Expr{field("msg"), lit("2")}}},
		{"substr_3arg", &spl2.FuncCallExpr{Name: "substr", Args: []spl2.Expr{field("msg"), lit("2"), lit("5")}}},
		{"match", &spl2.FuncCallExpr{Name: "match", Args: []spl2.Expr{field("uri"), qlit(`^/api`)}}},
		{"replace", &spl2.FuncCallExpr{Name: "replace", Args: []spl2.Expr{field("msg"), qlit(`\d+`), qlit("N")}}},
		{"trim_default", &spl2.FuncCallExpr{Name: "trim", Args: []spl2.Expr{field("msg")}}},
		{"trim_chars", &spl2.FuncCallExpr{Name: "trim", Args: []spl2.Expr{field("msg"), qlit("x")}}},
		{"ltrim_default", &spl2.FuncCallExpr{Name: "ltrim", Args: []spl2.Expr{field("msg")}}},
		{"ltrim_chars", &spl2.FuncCallExpr{Name: "ltrim", Args: []spl2.Expr{field("msg"), qlit("x")}}},
		{"rtrim_default", &spl2.FuncCallExpr{Name: "rtrim", Args: []spl2.Expr{field("msg")}}},
		{"rtrim_chars", &spl2.FuncCallExpr{Name: "rtrim", Args: []spl2.Expr{field("msg"), qlit("x")}}},
		{"split", &spl2.FuncCallExpr{Name: "split", Args: []spl2.Expr{field("msg"), qlit(",")}}},
		{"urldecode", &spl2.FuncCallExpr{Name: "urldecode", Args: []spl2.Expr{field("url")}}},

		// String predicates
		{"startswith", &spl2.FuncCallExpr{Name: "startswith", Args: []spl2.Expr{field("msg"), qlit("err")}}},
		{"endswith", &spl2.FuncCallExpr{Name: "endswith", Args: []spl2.Expr{field("msg"), qlit(".log")}}},
		{"contains", &spl2.FuncCallExpr{Name: "contains", Args: []spl2.Expr{field("msg"), qlit("error")}}},

		// LIKE
		{"like", &spl2.FuncCallExpr{Name: "like", Args: []spl2.Expr{field("msg"), qlit("%err%")}}},
		{"ilike", &spl2.FuncCallExpr{Name: "ilike", Args: []spl2.Expr{field("msg"), qlit("%err%")}}},

		// Math functions
		{"round_1arg", &spl2.FuncCallExpr{Name: "round", Args: []spl2.Expr{field("x")}}},
		{"round_2arg", &spl2.FuncCallExpr{Name: "round", Args: []spl2.Expr{field("x"), lit("2")}}},
		{"ln", &spl2.FuncCallExpr{Name: "ln", Args: []spl2.Expr{field("x")}}},
		{"log_1arg", &spl2.FuncCallExpr{Name: "log", Args: []spl2.Expr{field("x")}}},
		{"log_2arg", &spl2.FuncCallExpr{Name: "log", Args: []spl2.Expr{field("x"), lit("2")}}},
		{"exp", &spl2.FuncCallExpr{Name: "exp", Args: []spl2.Expr{field("x")}}},
		{"pow", &spl2.FuncCallExpr{Name: "pow", Args: []spl2.Expr{field("x"), lit("3")}}},
		{"abs", &spl2.FuncCallExpr{Name: "abs", Args: []spl2.Expr{field("x")}}},
		{"ceil", &spl2.FuncCallExpr{Name: "ceil", Args: []spl2.Expr{field("x")}}},
		{"ceiling", &spl2.FuncCallExpr{Name: "ceiling", Args: []spl2.Expr{field("x")}}},
		{"floor", &spl2.FuncCallExpr{Name: "floor", Args: []spl2.Expr{field("x")}}},
		{"sqrt", &spl2.FuncCallExpr{Name: "sqrt", Args: []spl2.Expr{field("x")}}},
		{"pi", &spl2.FuncCallExpr{Name: "pi", Args: nil}},
		{"random", &spl2.FuncCallExpr{Name: "random", Args: nil}},

		// Trig functions (unary math)
		{"acos", &spl2.FuncCallExpr{Name: "acos", Args: []spl2.Expr{field("x")}}},
		{"acosh", &spl2.FuncCallExpr{Name: "acosh", Args: []spl2.Expr{field("x")}}},
		{"asin", &spl2.FuncCallExpr{Name: "asin", Args: []spl2.Expr{field("x")}}},
		{"asinh", &spl2.FuncCallExpr{Name: "asinh", Args: []spl2.Expr{field("x")}}},
		{"atan", &spl2.FuncCallExpr{Name: "atan", Args: []spl2.Expr{field("x")}}},
		{"atanh", &spl2.FuncCallExpr{Name: "atanh", Args: []spl2.Expr{field("x")}}},
		{"cos", &spl2.FuncCallExpr{Name: "cos", Args: []spl2.Expr{field("x")}}},
		{"cosh", &spl2.FuncCallExpr{Name: "cosh", Args: []spl2.Expr{field("x")}}},
		{"sin", &spl2.FuncCallExpr{Name: "sin", Args: []spl2.Expr{field("x")}}},
		{"sinh", &spl2.FuncCallExpr{Name: "sinh", Args: []spl2.Expr{field("x")}}},
		{"tan", &spl2.FuncCallExpr{Name: "tan", Args: []spl2.Expr{field("x")}}},
		{"tanh", &spl2.FuncCallExpr{Name: "tanh", Args: []spl2.Expr{field("x")}}},

		// Binary math
		{"atan2", &spl2.FuncCallExpr{Name: "atan2", Args: []spl2.Expr{field("y"), field("x")}}},
		{"hypot", &spl2.FuncCallExpr{Name: "hypot", Args: []spl2.Expr{field("a"), field("b")}}},

		// Variadic math
		{"max", &spl2.FuncCallExpr{Name: "max", Args: []spl2.Expr{field("a"), field("b"), field("c")}}},
		{"min", &spl2.FuncCallExpr{Name: "min", Args: []spl2.Expr{field("a"), field("b"), field("c")}}},

		// Multivalue
		{"mvappend", &spl2.FuncCallExpr{Name: "mvappend", Args: []spl2.Expr{field("a"), field("b")}}},
		{"mvjoin", &spl2.FuncCallExpr{Name: "mvjoin", Args: []spl2.Expr{field("mv"), qlit(",")}}},
		{"mvdedup", &spl2.FuncCallExpr{Name: "mvdedup", Args: []spl2.Expr{field("mv")}}},
		{"mvcount", &spl2.FuncCallExpr{Name: "mvcount", Args: []spl2.Expr{field("mv")}}},

		// Printf
		{"printf", &spl2.FuncCallExpr{Name: "printf", Args: []spl2.Expr{qlit("%s=%d"), field("name"), field("val")}}},

		// IP functions
		{"ipmask", &spl2.FuncCallExpr{Name: "ipmask", Args: []spl2.Expr{qlit("255.255.255.0"), field("ip")}}},
		{"cidrmatch", &spl2.FuncCallExpr{Name: "cidrmatch", Args: []spl2.Expr{
			// First arg must be a literal CIDR string.
			&spl2.LiteralExpr{Value: "10.0.0.0/8"},
			field("ip"),
		}}},

		// Time functions
		{"strftime", &spl2.FuncCallExpr{Name: "strftime", Args: []spl2.Expr{field("ts"), qlit("%Y-%m-%d")}}},
		{"strptime", &spl2.FuncCallExpr{Name: "strptime", Args: []spl2.Expr{qlit("2024-01-01"), qlit("%Y-%m-%d")}}},

		// Hash functions
		{"md5", &spl2.FuncCallExpr{Name: "md5", Args: []spl2.Expr{field("msg")}}},
		{"sha1", &spl2.FuncCallExpr{Name: "sha1", Args: []spl2.Expr{field("msg")}}},
		{"sha256", &spl2.FuncCallExpr{Name: "sha256", Args: []spl2.Expr{field("msg")}}},
		{"sha512", &spl2.FuncCallExpr{Name: "sha512", Args: []spl2.Expr{field("msg")}}},

		// JSON functions
		{"json_extract", &spl2.FuncCallExpr{Name: "json_extract", Args: []spl2.Expr{field("data"), qlit("$.key")}}},
		{"spath", &spl2.FuncCallExpr{Name: "spath", Args: []spl2.Expr{field("data"), qlit("$.key")}}},
		{"json_valid", &spl2.FuncCallExpr{Name: "json_valid", Args: []spl2.Expr{field("data")}}},
		{"json_keys_1arg", &spl2.FuncCallExpr{Name: "json_keys", Args: []spl2.Expr{field("data")}}},
		{"json_keys_2arg", &spl2.FuncCallExpr{Name: "json_keys", Args: []spl2.Expr{field("data"), qlit("$.nested")}}},
		{"json_array_length_1arg", &spl2.FuncCallExpr{Name: "json_array_length", Args: []spl2.Expr{field("data")}}},
		{"json_array_length_2arg", &spl2.FuncCallExpr{Name: "json_array_length", Args: []spl2.Expr{field("data"), qlit("$.arr")}}},
		{"json_object", &spl2.FuncCallExpr{Name: "json_object", Args: []spl2.Expr{qlit("k1"), qlit("v1"), qlit("k2"), qlit("v2")}}},
		{"json_array", &spl2.FuncCallExpr{Name: "json_array", Args: []spl2.Expr{lit("1"), lit("2"), lit("3")}}},
		{"json_type_1arg", &spl2.FuncCallExpr{Name: "json_type", Args: []spl2.Expr{field("data")}}},
		{"json_type_2arg", &spl2.FuncCallExpr{Name: "json_type", Args: []spl2.Expr{field("data"), qlit("$.key")}}},
		{"json_set", &spl2.FuncCallExpr{Name: "json_set", Args: []spl2.Expr{field("data"), qlit("$.key"), qlit("val")}}},
		{"json_remove", &spl2.FuncCallExpr{Name: "json_remove", Args: []spl2.Expr{field("data"), qlit("$.key")}}},
		{"json_merge", &spl2.FuncCallExpr{Name: "json_merge", Args: []spl2.Expr{field("a"), field("b")}}},

		// Type checks
		{"isnum", &spl2.FuncCallExpr{Name: "isnum", Args: []spl2.Expr{field("x")}}},
		{"isnumeric", &spl2.FuncCallExpr{Name: "isnumeric", Args: []spl2.Expr{field("x")}}},
		{"isint", &spl2.FuncCallExpr{Name: "isint", Args: []spl2.Expr{field("x")}}},
		{"isbool", &spl2.FuncCallExpr{Name: "isbool", Args: []spl2.Expr{field("x")}}},
		{"isarray", &spl2.FuncCallExpr{Name: "isarray", Args: []spl2.Expr{field("x")}}},
		{"isobject", &spl2.FuncCallExpr{Name: "isobject", Args: []spl2.Expr{field("x")}}},
		{"isstr", &spl2.FuncCallExpr{Name: "isstr", Args: []spl2.Expr{field("x")}}},
		{"typeof", &spl2.FuncCallExpr{Name: "typeof", Args: []spl2.Expr{field("x")}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := CompileExpr(tc.expr)
			if err != nil {
				t.Fatalf("CompileExpr(%s): %v", tc.name, err)
			}
			got := Disassemble(prog)

			goldenFile := filepath.Join(goldenDir, tc.name+".golden")
			if updateGoldens {
				if err := os.WriteFile(goldenFile, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update-goldens to generate)", goldenFile, err)
			}
			if got != string(want) {
				t.Errorf("bytecode mismatch for %s.\n--- want ---\n%s\n--- got ---\n%s", tc.name, string(want), got)
			}
		})
	}
}

// TestGoldenArityErrors verifies that arity-error messages are preserved
// byte-for-byte after the registry refactor.
func TestGoldenArityErrors(t *testing.T) {
	updateGoldens := os.Getenv("UPDATE_GOLDENS") == "1"

	goldenDir := filepath.Join("testdata", "goldens")
	if updateGoldens {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatalf("mkdir goldens: %v", err)
		}
	}

	lit := func(v string) spl2.Expr { return &spl2.LiteralExpr{Value: v} }
	field := func(name string) spl2.Expr { return &spl2.FieldExpr{Name: name} }
	_ = lit
	_ = math.Pi // suppress unused import if needed

	type errCase struct {
		name string
		expr spl2.Expr
	}

	errCases := []errCase{
		// IF: wrong arity
		{"err_if_0args", &spl2.FuncCallExpr{Name: "IF", Args: nil}},
		{"err_if_2args", &spl2.FuncCallExpr{Name: "IF", Args: []spl2.Expr{field("a"), field("b")}}},
		// null: wrong arity
		{"err_null_1arg", &spl2.FuncCallExpr{Name: "null", Args: []spl2.Expr{field("a")}}},
		// nullif: wrong arity
		{"err_nullif_1arg", &spl2.FuncCallExpr{Name: "nullif", Args: []spl2.Expr{field("a")}}},
		// isnull: wrong arity
		{"err_isnull_0arg", &spl2.FuncCallExpr{Name: "isnull", Args: nil}},
		// tonumber: wrong arity
		{"err_tonumber_0arg", &spl2.FuncCallExpr{Name: "tonumber", Args: nil}},
		// round: wrong arity
		{"err_round_0arg", &spl2.FuncCallExpr{Name: "round", Args: nil}},
		{"err_round_3arg", &spl2.FuncCallExpr{Name: "round", Args: []spl2.Expr{field("a"), field("b"), field("c")}}},
		// validate: wrong arity (0 args)
		{"err_validate_0arg", &spl2.FuncCallExpr{Name: "validate", Args: nil}},
		// validate: odd args
		{"err_validate_1arg", &spl2.FuncCallExpr{Name: "validate", Args: []spl2.Expr{field("a")}}},
		// match: wrong arity
		{"err_match_1arg", &spl2.FuncCallExpr{Name: "match", Args: []spl2.Expr{field("a")}}},
		// unknown function
		{"err_unknown_func", &spl2.FuncCallExpr{Name: "foobar_nonexistent", Args: nil}},
		// pi: wrong arity
		{"err_pi_1arg", &spl2.FuncCallExpr{Name: "pi", Args: []spl2.Expr{field("a")}}},
		// random: wrong arity
		{"err_random_1arg", &spl2.FuncCallExpr{Name: "random", Args: []spl2.Expr{field("a")}}},
		// in: too few args
		{"err_in_1arg", &spl2.FuncCallExpr{Name: "in", Args: []spl2.Expr{field("a")}}},
		// searchmatch: wrong arity
		{"err_searchmatch_0arg", &spl2.FuncCallExpr{Name: "searchmatch", Args: nil}},
		// max/min: too few args
		{"err_max_1arg", &spl2.FuncCallExpr{Name: "max", Args: []spl2.Expr{field("a")}}},
		{"err_min_1arg", &spl2.FuncCallExpr{Name: "min", Args: []spl2.Expr{field("a")}}},
		// json_object: odd args
		{"err_json_object_odd", &spl2.FuncCallExpr{Name: "json_object", Args: []spl2.Expr{field("a"), field("b"), field("c")}}},
		// substr: wrong arity
		{"err_substr_1arg", &spl2.FuncCallExpr{Name: "substr", Args: []spl2.Expr{field("a")}}},
		{"err_substr_4arg", &spl2.FuncCallExpr{Name: "substr", Args: []spl2.Expr{field("a"), field("b"), field("c"), field("d")}}},
	}

	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CompileExpr(tc.expr)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			got := err.Error()

			goldenFile := filepath.Join(goldenDir, tc.name+".golden")
			if updateGoldens {
				if err := os.WriteFile(goldenFile, []byte(got), 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update-goldens to generate)", goldenFile, err)
			}
			if got != string(want) {
				t.Errorf("error message mismatch for %s.\n--- want ---\n%s\n--- got ---\n%s", tc.name, string(want), got)
			}
		})
	}
}

// TestGoldenRegisteredFunctions verifies the RegisteredFunctions list is
// comprehensive and sorted.
func TestGoldenRegisteredFunctions(t *testing.T) {
	fns := RegisteredFunctions()
	if len(fns) == 0 {
		t.Fatal("RegisteredFunctions() returned empty list")
	}
	if !sort.StringsAreSorted(fns) {
		t.Error("RegisteredFunctions() is not sorted")
	}

	// Verify no duplicates.
	seen := make(map[string]bool)
	for _, f := range fns {
		if seen[f] {
			t.Errorf("duplicate function name: %s", f)
		}
		seen[f] = true
	}

	// Spot-check critical functions.
	critical := []string{
		"if", "case", "validate", "coalesce", "null", "nullif",
		"in", "isnull", "isnotnull", "tonumber", "todouble", "tostring",
		"len", "lower", "upper", "substr", "match", "replace",
		"round", "ln", "exp", "pow", "abs", "ceil", "floor", "sqrt",
		"pi", "random", "max", "min", "mvappend", "mvjoin", "mvdedup",
		"printf", "ipmask", "cidrmatch", "strftime", "strptime",
		"md5", "sha256", "json_extract", "json_valid", "like",
		"startswith", "endswith", "contains", "typeof",
		"acos", "sin", "atan2", "hypot",
	}
	for _, name := range critical {
		found := false
		for _, f := range fns {
			if f == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("RegisteredFunctions() missing critical function: %s", name)
		}
	}
}

// TestUnknownFuncErrorPreserved checks the exact error format for unknown
// functions. The spl2.error_hints layer and other consumers depend on this.
func TestUnknownFuncErrorPreserved(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string
	}{
		{"foobar", "unknown function: foobar"},
		{"FOOBAR", "unknown function: FOOBAR"},
		{"notAFunc", "unknown function: notAFunc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr := &spl2.FuncCallExpr{Name: tt.name, Args: nil}
			_, err := CompileExpr(expr)
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestCIDRMatchErrorPreserved verifies the cidrmatch error wrapping.
func TestCIDRMatchErrorPreserved(t *testing.T) {
	// Invalid CIDR should error at compile time.
	expr := &spl2.FuncCallExpr{
		Name: "cidrmatch",
		Args: []spl2.Expr{
			&spl2.LiteralExpr{Value: "not-a-cidr"},
			&spl2.FieldExpr{Name: "ip"},
		},
	}
	_, err := CompileExpr(expr)
	if err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
	if !strings.Contains(err.Error(), "cidrmatch") {
		t.Errorf("error should mention cidrmatch: %v", err)
	}
}
