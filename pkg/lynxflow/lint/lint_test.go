package lint

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

var update = flag.Bool("update", false, "update golden files")

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustParseDesugar(t *testing.T, input string) *ast.Query {
	t.Helper()
	q, diags := parser.Parse(input)
	if len(diags) > 0 {
		t.Fatalf("Parse(%q): %d diag(s): %v", input, len(diags), diags)
	}
	if q == nil {
		t.Fatalf("Parse(%q): returned nil", input)
	}
	out, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	return out
}

func lintCodes(lints []Lint) []string {
	codes := make([]string, len(lints))
	for i, l := range lints {
		codes[i] = l.Code
	}
	return codes
}

func hasCode(lints []Lint, code string) bool {
	for _, l := range lints {
		if l.Code == code {
			return true
		}
	}
	return false
}

func assertHasCode(t *testing.T, lints []Lint, code string) {
	t.Helper()
	if !hasCode(lints, code) {
		t.Errorf("expected lint %s, got codes: %v", code, lintCodes(lints))
	}
}

func assertNoCode(t *testing.T, lints []Lint, code string) {
	t.Helper()
	if hasCode(lints, code) {
		t.Errorf("unexpected lint %s in codes: %v", code, lintCodes(lints))
	}
}

// ---------------------------------------------------------------------------
// 1. Registry invariants
// ---------------------------------------------------------------------------

func TestRegistryInvariants(t *testing.T) {
	rules := Rules()
	if len(rules) == 0 {
		t.Fatal("Rules() returned empty registry")
	}

	seen := map[string]bool{}
	for _, r := range rules {
		if r.Code == "" {
			t.Error("rule with empty Code")
		}
		if r.Doc == "" {
			t.Errorf("rule %s has empty Doc", r.Code)
		}
		if r.Check == nil {
			t.Errorf("rule %s has nil Check", r.Code)
		}
		if seen[r.Code] {
			t.Errorf("duplicate rule code: %s", r.Code)
		}
		seen[r.Code] = true
	}

	// Verify all rules produce a valid Reason tag.
	validReasons := map[string]bool{"slow": true, "broad": true, "canon": true, "data-quality": true}
	for _, r := range rules {
		q := mustParseDesugar(t, `from app[-1h] | head 1`)
		lints := r.Check(q)
		for _, l := range lints {
			if !validReasons[l.Reason] {
				t.Errorf("rule %s produced invalid reason %q", r.Code, l.Reason)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 2. Per-rule positive + negative tests
// ---------------------------------------------------------------------------

func TestLF01_LeadingWildcard(t *testing.T) {
	t.Run("positive_glob_star", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where glob(host, "*web")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF01")
		// Verify Reason.
		for _, l := range lints {
			if l.Code == "LF01" {
				if l.Reason != "slow" {
					t.Errorf("LF01 reason = %q, want slow", l.Reason)
				}
				if l.Suggestion == "" {
					t.Error("LF01 should have a suggestion")
				}
			}
		}
	})

	t.Run("positive_glob_question", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where glob(host, "?web")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF01")
	})

	t.Run("positive_regex_dotstar", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r".*error")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF01")
	})

	t.Run("positive_regex_dotplus", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r".+error")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF01")
	})

	t.Run("negative_anchored_glob", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where glob(host, "web-*")`)
		lints := Run(q)
		assertNoCode(t, lints, "LF01")
	})

	t.Run("negative_anchored_regex", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r"^/api/.*")`)
		lints := Run(q)
		assertNoCode(t, lints, "LF01")
	})

	t.Run("negative_no_glob_or_regex", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where status >= 500`)
		lints := Run(q)
		assertNoCode(t, lints, "LF01")
	})

	t.Run("positive_search_sugar_glob", func(t *testing.T) {
		// Test glob with leading wildcard via direct call (search sugar
		// glob patterns parse as glob() calls after desugaring).
		q := mustParseDesugar(t, `from app[-1h] | where glob(host, "*-prod")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF01")
	})
}

func TestLF02_BroadScopeStar(t *testing.T) {
	t.Run("positive_star_no_range_no_time", func(t *testing.T) {
		q := mustParseDesugar(t, `from * | stats count()`)
		lints := Run(q)
		assertHasCode(t, lints, "LF02")
		for _, l := range lints {
			if l.Code == "LF02" && l.Reason != "broad" {
				t.Errorf("LF02 reason = %q, want broad", l.Reason)
			}
		}
	})

	t.Run("negative_star_with_range", func(t *testing.T) {
		q := mustParseDesugar(t, `from *[-1h] | stats count()`)
		lints := Run(q)
		assertNoCode(t, lints, "LF02")
	})

	t.Run("negative_star_with_time_predicate", func(t *testing.T) {
		q := mustParseDesugar(t, `from * | where _time > now() - 1h | stats count()`)
		lints := Run(q)
		assertNoCode(t, lints, "LF02")
	})

	t.Run("negative_named_source", func(t *testing.T) {
		// Named source should NOT trigger LF02 (that's LF03's domain).
		q := mustParseDesugar(t, `from app | stats count()`)
		lints := Run(q)
		assertNoCode(t, lints, "LF02")
	})
}

func TestLF03_UnboundedTimeRange(t *testing.T) {
	t.Run("positive_named_no_range_no_time", func(t *testing.T) {
		q := mustParseDesugar(t, `from app | stats count()`)
		lints := Run(q)
		assertHasCode(t, lints, "LF03")
		for _, l := range lints {
			if l.Code == "LF03" && l.Reason != "broad" {
				t.Errorf("LF03 reason = %q, want broad", l.Reason)
			}
		}
	})

	t.Run("negative_with_range", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | stats count()`)
		lints := Run(q)
		assertNoCode(t, lints, "LF03")
	})

	t.Run("negative_with_time_predicate", func(t *testing.T) {
		q := mustParseDesugar(t, `from app | where _time > now() - 1h | stats count()`)
		lints := Run(q)
		assertNoCode(t, lints, "LF03")
	})

	t.Run("suppressed_for_star_sources", func(t *testing.T) {
		// Star sources should trigger LF02, not LF03.
		q := mustParseDesugar(t, `from * | stats count()`)
		lints := Run(q)
		assertNoCode(t, lints, "LF03")
		assertHasCode(t, lints, "LF02")
	})
}

func TestLF04_RegexNoLiteral(t *testing.T) {
	t.Run("positive_no_literal", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r"\d+")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF04")
		for _, l := range lints {
			if l.Code == "LF04" && l.Reason != "slow" {
				t.Errorf("LF04 reason = %q, want slow", l.Reason)
			}
		}
	})

	t.Run("positive_short_literal", func(t *testing.T) {
		// Only 2-char literal run "ab" is not enough (need >= 3).
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r"ab\d+")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF04")
	})

	t.Run("negative_has_literal_anchor", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r"api/v1/\d+")`)
		lints := Run(q)
		assertNoCode(t, lints, "LF04")
	})

	t.Run("negative_long_literal", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where matches(path, r"^/api/users/\d+")`)
		lints := Run(q)
		assertNoCode(t, lints, "LF04")
	})

	t.Run("positive_extract_no_literal", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | extend x = extract(msg, r"(\d+)")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF04")
	})
}

func TestLF05_MaterializeStrictParse(t *testing.T) {
	t.Run("positive_parse_without_strict", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | parse json | stats count() by service | materialize "mv_test" retention=90d`)
		lints := Run(q)
		assertHasCode(t, lints, "LF05")
		for _, l := range lints {
			if l.Code == "LF05" && l.Reason != "data-quality" {
				t.Errorf("LF05 reason = %q, want data-quality", l.Reason)
			}
		}
	})

	t.Run("negative_parse_with_strict", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | parse json on_error strict | stats count() by service | materialize "mv_test" retention=90d`)
		lints := Run(q)
		assertNoCode(t, lints, "LF05")
	})

	t.Run("negative_no_materialize", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | parse json | stats count() by service`)
		lints := Run(q)
		assertNoCode(t, lints, "LF05")
	})

	t.Run("negative_materialize_no_parse", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | stats count() by service | materialize "mv_test"`)
		lints := Run(q)
		assertNoCode(t, lints, "LF05")
	})
}

func TestLF06_HeadBeforeSort(t *testing.T) {
	t.Run("positive_head_then_sort", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | head 10 | sort -status`)
		lints := Run(q)
		assertHasCode(t, lints, "LF06")
		for _, l := range lints {
			if l.Code == "LF06" && l.Reason != "canon" {
				t.Errorf("LF06 reason = %q, want canon", l.Reason)
			}
		}
	})

	t.Run("positive_head_where_sort", func(t *testing.T) {
		// head | where | sort — where is not stats-like, so LF06 fires.
		q := mustParseDesugar(t, `from app[-1h] | head 10 | where status >= 500 | sort -status`)
		lints := Run(q)
		assertHasCode(t, lints, "LF06")
	})

	t.Run("negative_sort_then_head", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | sort -status | head 10`)
		lints := Run(q)
		assertNoCode(t, lints, "LF06")
	})

	t.Run("negative_head_stats_sort", func(t *testing.T) {
		// head | stats | sort — stats intervenes, so LF06 does NOT fire.
		q := mustParseDesugar(t, `from app[-1h] | head 10 | stats count() by level | sort -count`)
		lints := Run(q)
		assertNoCode(t, lints, "LF06")
	})

	t.Run("negative_no_sort", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | head 10`)
		lints := Run(q)
		assertNoCode(t, lints, "LF06")
	})
}

func TestLF08_HasUppercase(t *testing.T) {
	t.Run("positive_uppercase_term", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where has(level, "ERROR")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF08")
		for _, l := range lints {
			if l.Code == "LF08" && l.Reason != "canon" {
				t.Errorf("LF08 reason = %q, want canon", l.Reason)
			}
		}
	})

	t.Run("positive_mixed_case", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where has(_raw, "TimeoutError")`)
		lints := Run(q)
		assertHasCode(t, lints, "LF08")
	})

	t.Run("negative_lowercase_term", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where has(level, "error")`)
		lints := Run(q)
		assertNoCode(t, lints, "LF08")
	})

	t.Run("negative_not_has", func(t *testing.T) {
		q := mustParseDesugar(t, `from app[-1h] | where contains(level, "ERROR")`)
		lints := Run(q)
		assertNoCode(t, lints, "LF08")
	})

	t.Run("positive_search_sugar_bare_upper", func(t *testing.T) {
		// Bare word "ERROR" desugars to has(_raw, "ERROR").
		q := mustParseDesugar(t, `from app[-1h] ERROR`)
		lints := Run(q)
		assertHasCode(t, lints, "LF08")
	})
}

// ---------------------------------------------------------------------------
// 3. Span sanity
// ---------------------------------------------------------------------------

func TestSpanSanity(t *testing.T) {
	// Verify all lints have non-negative span values.
	queries := []string{
		`from * | stats count()`,
		`from app | where glob(host, "*web")`,
		`from app[-1h] | where matches(path, r"\d+")`,
		`from app[-1h] | head 10 | sort -status`,
		`from app[-1h] | where has(level, "ERROR")`,
	}

	for _, input := range queries {
		q := mustParseDesugar(t, input)
		lints := Run(q)
		for _, l := range lints {
			if l.Span.Start < 0 || l.Span.End < 0 {
				t.Errorf("[%s] lint %s has negative span: %+v", input, l.Code, l.Span)
			}
			if l.Span.End < l.Span.Start {
				t.Errorf("[%s] lint %s has inverted span: %+v", input, l.Code, l.Span)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Corpus golden test
// ---------------------------------------------------------------------------

type corpusEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	LynxFlow string   `json:"lynxflow"`
	Features []string `json:"features"`
}

func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "corpus", "corpus.jsonl"))
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

func TestCorpusLints_NoPanics(t *testing.T) {
	entries := loadCorpus(t)
	if len(entries) < 50 {
		t.Fatalf("corpus has %d entries, want >= 50", len(entries))
	}

	opts := desugar.Options{DefaultSource: "main"}

	for _, e := range entries {
		t.Run(e.ID+"_"+e.Name, func(t *testing.T) {
			q, diags := parser.Parse(e.LynxFlow)
			if len(diags) > 0 || q == nil {
				t.Skipf("parse diags for %s (skip lint test)", e.ID)
				return
			}

			out, _ := desugar.Desugar(q, opts)

			// Property: no panic.
			lints := Run(out)

			// Verify all lints have valid fields.
			for _, l := range lints {
				if l.Code == "" {
					t.Errorf("[%s] lint with empty Code", e.ID)
				}
				if l.Message == "" {
					t.Errorf("[%s] lint %s with empty Message", e.ID, l.Code)
				}
				if l.Reason == "" {
					t.Errorf("[%s] lint %s with empty Reason", e.ID, l.Code)
				}
			}
		})
	}
}

func TestCorpusLints_Golden(t *testing.T) {
	entries := loadCorpus(t)
	opts := desugar.Options{DefaultSource: "main"}

	var lines []string
	for _, e := range entries {
		q, diags := parser.Parse(e.LynxFlow)
		if len(diags) > 0 || q == nil {
			continue
		}
		out, _ := desugar.Desugar(q, opts)
		lints := Run(out)
		if len(lints) == 0 {
			continue
		}
		codes := lintCodes(lints)
		sort.Strings(codes)
		// Deduplicate codes.
		deduped := dedupStrings(codes)
		lines = append(lines, fmt.Sprintf("%s %s", e.ID, strings.Join(deduped, ",")))
	}
	sort.Strings(lines)

	golden := strings.Join(lines, "\n") + "\n"

	goldenPath := filepath.Join("..", "testdata", "golden", "lints.txt")

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(golden), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to generate)", goldenPath, err)
	}

	if string(want) != golden {
		t.Errorf("golden mismatch (-want +got):\n--- want:\n%s\n--- got:\n%s", string(want), golden)
	}
}

func dedupStrings(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := []string{ss[0]}
	for _, s := range ss[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 5. Nil-safety
// ---------------------------------------------------------------------------

func TestRunNilQuery(t *testing.T) {
	lints := Run(nil)
	if len(lints) != 0 {
		t.Errorf("Run(nil) returned %d lints, want 0", len(lints))
	}
}

// ---------------------------------------------------------------------------
// 6. Internal helper tests
// ---------------------------------------------------------------------------

func TestHasLiteralAnchor(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{`api/v1/`, true},   // "api" is 3 chars
		{`\d+`, false},      // no literal
		{`ab\d+`, false},    // only 2 literal chars
		{`abc\d+`, true},    // 3 literal chars
		{`^/api/.*`, true},  // "/ap" after ^, then "i/" — actually "/api" = 3
		{`.*`, false},       // no literal
		{`[a-z]+`, false},   // all metacharacters
		{`user=\w+`, true},  // "user=" is 5 literal chars
		{`\d\d\d`, false},   // all metaclass
		{`(foo|bar)`, true}, // "foo" is 3 literal chars
		{`(?:abc)`, true},   // "abc" is 3 literal chars inside group syntax... wait
		{`(?i)error`, true}, // "error" is 5 literal chars
		{`a.b`, false},      // . is meta, so runs are "a" (1) and "b" (1)
	}

	for _, tt := range tests {
		got := hasLiteralAnchor(tt.pattern, 3)
		if got != tt.want {
			t.Errorf("hasLiteralAnchor(%q, 3) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestHasLeadingGlobWildcard(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{"*web", true},
		{"?web", true},
		{"web*", false},
		{"web", false},
		{"", false},
	}

	for _, tt := range tests {
		got := hasLeadingGlobWildcard(tt.pattern)
		if got != tt.want {
			t.Errorf("hasLeadingGlobWildcard(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestHasLeadingRegexWildcard(t *testing.T) {
	tests := []struct {
		pattern string
		want    bool
	}{
		{".*error", true},
		{".+error", true},
		{"^error", false},
		{"error", false},
		{".error", false}, // . is meta but not followed by * or +
		{"", false},
		{".", false},
	}

	for _, tt := range tests {
		got := hasLeadingRegexWildcard(tt.pattern)
		if got != tt.want {
			t.Errorf("hasLeadingRegexWildcard(%q) = %v, want %v", tt.pattern, got, tt.want)
		}
	}
}

func TestContainsUpper(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"error", false},
		{"ERROR", true},
		{"Error", true},
		{"123", false},
		{"", false},
	}

	for _, tt := range tests {
		got := containsUpper(tt.s)
		if got != tt.want {
			t.Errorf("containsUpper(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
