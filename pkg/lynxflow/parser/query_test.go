package parser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// ---------------------------------------------------------------------------
// 1. Corpus gate: all 63 lynxflow values parse with ZERO diags
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
	f, err := os.Open("../testdata/corpus/corpus.jsonl")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer f.Close()

	var entries []corpusEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e corpusEntry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", line, err)
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan corpus: %v", err)
	}
	return entries
}

func TestCorpusParse(t *testing.T) {
	entries := loadCorpus(t)
	if len(entries) < 50 {
		t.Fatalf("corpus has %d entries, want at least 50", len(entries))
	}

	passed := 0
	for _, e := range entries {
		t.Run(e.ID+"_"+e.Name, func(t *testing.T) {
			// The lynxflow field may contain multi-line queries with \n.
			input := e.LynxFlow
			q, diags := Parse(input)
			if len(diags) > 0 {
				t.Errorf("Parse(%q): %d diag(s):", e.ID, len(diags))
				for _, d := range diags {
					t.Errorf("  [%s] %s (span %d..%d)", d.Code, d.Message, d.Span.Start, d.Span.End)
				}
				return
			}
			if q == nil {
				t.Fatalf("Parse(%q): returned nil Query", e.ID)
			}
			// Verify String() doesn't panic.
			_ = q.String()
			passed++
		})
	}
	t.Logf("corpus result: %d/%d passed", passed, len(entries))
}

// ---------------------------------------------------------------------------
// 2. RFC-002 §13 "after" examples
// ---------------------------------------------------------------------------

func TestRFC002Examples(t *testing.T) {
	examples := []string{
		// 1. search sugar
		`from app[-1h] error timeout`,
		// 2. search sugar with comparisons
		`from nginx[-1h] timeout status>=500`,
		// 3. parse json
		`| parse json | where status >= 500`,
		// 4. parse logfmt prefix
		`| parse logfmt prefix log.`,
		// 5. parse regex with into
		`| parse regex r"user=(?<user>\w+) ip=(?<ip>[\d.]+)" into (ip as string)`,
		// 6a. keep
		`| keep host, status`,
		// 6b. drop
		`| drop _raw`,
		// 6c. keep * except
		`| keep * except _raw`,
		// 7. rename
		`| rename duration_ms as latency`,
		// 8. extend boolean
		`| extend is_err = status >= 500`,
		// 9. stats with count()
		`| stats count(), avg(dur) by service`,
		// 10. every sugar
		`| every 5m by service stats count()`,
		// 11. proportion sugar
		`| stats count(where status >= 500) as errors, count() as total by service | extend error_rate = errors / total`,
		// 12. latency sugar
		`| latency dur every 5m by endpoint`,
		// 13. parse json + nested
		`| parse json | where user.role == "admin" and any(tags, t -> t.name == "vip")`,
		// 14. exists + coalesce
		`| where exists(amount)`,
		// 15. parse error debugging
		`| parse json | where exists(_error) | keep _error, _error_detail, _raw | head 20`,
		// 17. parse combined
		`from nginx | parse combined | where status == 500 and has(_raw, "upstream")`,
		// 18. parse first_of
		`from app[-1h] | parse first_of(json, logfmt) | keep _time, service, status, dur | sort -dur | head 50`,
	}
	for i, ex := range examples {
		t.Run(fmt.Sprintf("example_%d", i+1), func(t *testing.T) {
			q, diags := Parse(ex)
			if len(diags) > 0 {
				t.Errorf("Parse(%q): %d diag(s):", ex, len(diags))
				for _, d := range diags {
					t.Errorf("  [%s] %s (span %d..%d)", d.Code, d.Message, d.Span.Start, d.Span.End)
				}
			}
			if q == nil {
				t.Fatal("returned nil Query")
			}
			_ = q.String()
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Golden structure tests
// ---------------------------------------------------------------------------

func TestGoldenStructure(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple_where",
			input: `| where status >= 500`,
			want:  `where (status >= 500)`,
		},
		{
			name:  "stats_by",
			input: `| stats count() by service`,
			want:  `stats count() by service`,
		},
		{
			name:  "extend_multi",
			input: `| extend x = 1, y = 2`,
			want:  `extend x = 1, y = 2`,
		},
		{
			name:  "sort_prefix",
			input: `| sort -count, service`,
			want:  `sort -count, service`,
		},
		{
			name:  "head",
			input: `| head 10`,
			want:  `head 10`,
		},
		{
			name:  "dedup_n",
			input: `| dedup 3 service, host`,
			want:  `dedup 3 service, host`,
		},
		{
			name:  "rename",
			input: `| rename service as svc, level as severity`,
			want:  `rename service as svc, level as severity`,
		},
		{
			name:  "keep_star_except",
			input: `| keep * except _raw`,
			want:  `keep * except _raw`,
		},
		{
			name:  "drop_glob",
			input: `| drop _raw`,
			want:  `drop _raw`,
		},
		{
			name:  "top",
			input: `| top 10 uri`,
			want:  `top 10 uri`,
		},
		{
			name:  "rare",
			input: `| rare 3 service`,
			want:  `rare 3 service`,
		},
		{
			name:  "every_sugar",
			input: `| every 5m by service stats count()`,
			want:  `every 5m by service stats count()`,
		},
		{
			name:  "cte_let",
			input: `let $errs = from main | where level == "ERROR"; from $errs | stats count()`,
			want:  `let $errs = from main | where (level == "ERROR"); from $errs | stats count()`,
		},
		{
			name:  "pipeline_multi",
			input: `| where status >= 500 | stats count() by service | sort -count | head 10`,
			want:  `where (status >= 500) | stats count() by service | sort -count | head 10`,
		},
		{
			name:  "stats_cond_agg",
			input: `| stats count(where status >= 500) as errors, count() as total`,
			want:  `stats count(where (status >= 500)) as errors, count() as total`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q, diags := Parse(tt.input)
			if len(diags) > 0 {
				t.Errorf("Parse(%q): diags:", tt.input)
				for _, d := range diags {
					t.Errorf("  [%s] %s", d.Code, d.Message)
				}
			}
			if q == nil {
				t.Fatal("nil Query")
			}
			got := q.String()
			if got != tt.want {
				t.Errorf("String():\n  got  %q\n  want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Error recovery tests
// ---------------------------------------------------------------------------

func TestErrorRecovery_MultiStageErrors(t *testing.T) {
	// A 4-stage query with errors in stages 2 and 4.
	input := `| where status >= 500 | badstage1 foo bar | sort -count | badstage2 baz`
	q, diags := Parse(input)
	if q == nil {
		t.Fatal("nil Query")
	}

	// Should have exactly 2 error diags (one per bad stage).
	errorCount := 0
	for _, d := range diags {
		if d.Code == CodeUnknownStage || d.Code == CodeKilledSpelling {
			errorCount++
		}
	}
	if errorCount < 2 {
		t.Errorf("expected at least 2 stage-level errors, got %d", errorCount)
		for _, d := range diags {
			t.Logf("  [%s] %s", d.Code, d.Message)
		}
	}

	// Both good stages should still be present.
	if len(q.Pipeline.Stages) < 4 {
		t.Errorf("expected 4 stages (2 good + 2 error), got %d", len(q.Pipeline.Stages))
	}
}

func TestErrorRecovery_DidYouMean(t *testing.T) {
	input := `| wher status >= 500`
	_, diags := Parse(input)
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "did you mean") && strings.Contains(d.Message, "where") {
			found = true
		}
	}
	if !found {
		t.Error("expected did-you-mean suggestion for 'wher'")
		for _, d := range diags {
			t.Logf("  [%s] %s", d.Code, d.Message)
		}
	}
}

func TestErrorRecovery_KilledSpellings(t *testing.T) {
	tests := []struct {
		input      string
		killedName string
		fixSubstr  string
	}{
		{`| eval x = 1`, "eval", "extend"},
		{`| fields level, service`, "fields", "keep or drop"},
		{`| table level, service`, "table", "keep"},
		{`| timechart count span=5m`, "timechart", "every"},
		{`| search "error"`, "search", "where has"},
		{`| rex field=_raw "pattern"`, "rex", "parse regex"},
		{`| fillnull value="N/A" field1`, "fillnull", "extend"},
		{`| take 10`, "take", "head"},
		{`| limit 10`, "limit", "head"},
		{`| omit _raw`, "omit", "drop"},
		{`| enrich avg(x) by service`, "enrich", "eventstats"},
		{`| running window=3 avg(x)`, "running", "streamstats"},
		{`| glimpse`, "glimpse", "describe"},
		{`| slowest 5 duration_ms`, "slowest", "stats"},
		{`| topby 5 duration_ms`, "topby", "stats"},
		{`| bottomby 5 duration_ms`, "bottomby", "stats"},
		{`| append [sub]`, "append", "union"},
		{`| multisearch [sub1] [sub2]`, "multisearch", "union"},
		{`| group by service compute count()`, "group", "stats"},
		{`| select service, level`, "select", "keep"},
		{`| filter status >= 500`, "filter", "where"},
		{`| order -count`, "order", "sort"},
	}
	for _, tt := range tests {
		t.Run(tt.killedName, func(t *testing.T) {
			_, diags := Parse(tt.input)
			found := false
			for _, d := range diags {
				if d.Code == CodeKilledSpelling && strings.Contains(d.Suggestion, tt.fixSubstr) {
					found = true
				}
			}
			if !found {
				t.Errorf("expected killed-spelling fix-it for %q containing %q", tt.killedName, tt.fixSubstr)
				for _, d := range diags {
					t.Logf("  [%s] %s => %s", d.Code, d.Message, d.Suggestion)
				}
			}
		})
	}
}

func TestErrorRecovery_CountNoParens(t *testing.T) {
	// D28: count without parens
	input := `| stats count by service`
	_, diags := Parse(input)
	found := false
	for _, d := range diags {
		if d.Code == CodeCountNoParens {
			found = true
		}
	}
	if !found {
		t.Error("expected D28 count-no-parens diagnostic")
		for _, d := range diags {
			t.Logf("  [%s] %s", d.Code, d.Message)
		}
	}
}

func TestErrorRecovery_SortByDesc(t *testing.T) {
	// D22: sort by x desc
	input := `| sort by x desc`
	_, diags := Parse(input)
	found := false
	for _, d := range diags {
		if d.Code == CodeSortByDesc {
			found = true
		}
	}
	if !found {
		t.Error("expected D22 sort-by-desc diagnostic")
		for _, d := range diags {
			t.Logf("  [%s] %s", d.Code, d.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. Span assertions
// ---------------------------------------------------------------------------

func TestSpan_StageName(t *testing.T) {
	tests := []struct {
		input         string
		stageName     string
		wantNameStart int
		wantNameEnd   int
	}{
		{"| where x > 0", "where", 2, 7},
		{"| stats count()", "stats", 2, 7},
		{"| head 10", "head", 2, 6},
		{"| sort -count", "sort", 2, 6},
		{"| keep a, b", "keep", 2, 6},
	}
	for _, tt := range tests {
		t.Run(tt.stageName, func(t *testing.T) {
			q, _ := Parse(tt.input)
			if q == nil || len(q.Pipeline.Stages) == 0 {
				t.Fatal("no stages parsed")
			}
			s := q.Pipeline.Stages[0]
			if s.Name != tt.stageName {
				t.Errorf("name = %q, want %q", s.Name, tt.stageName)
			}
			if s.NamePos.Start != tt.wantNameStart || s.NamePos.End != tt.wantNameEnd {
				t.Errorf("NamePos = [%d, %d), want [%d, %d)",
					s.NamePos.Start, s.NamePos.End, tt.wantNameStart, tt.wantNameEnd)
			}
		})
	}
}

func TestSpan_OptionValues(t *testing.T) {
	// Test that option value spans are correct.
	q, diags := Parse(`| streamstats window=3 avg(x) as rolling`)
	if len(diags) > 0 {
		t.Errorf("unexpected diags: %v", diagMsgs(diags))
	}
	if q == nil || len(q.Pipeline.Stages) == 0 {
		t.Fatal("no stages")
	}
	s := q.Pipeline.Stages[0]
	if s.Streamstats == nil {
		t.Fatal("expected streamstats payload")
	}
	if s.Streamstats.Window == nil || *s.Streamstats.Window != 3 {
		t.Errorf("window = %v, want 3", s.Streamstats.Window)
	}
}

// ---------------------------------------------------------------------------
// 6. Fuzz test
// ---------------------------------------------------------------------------

func FuzzParse(f *testing.F) {
	// Seed with corpus entries.
	corpus := loadCorpusFuzz(f)
	for _, e := range corpus {
		f.Add(e.LynxFlow)
	}

	// RFC-002 §13 examples
	examples := []string{
		`from app[-1h] error timeout`,
		`from nginx[-1h] timeout status>=500`,
		`| parse json | where status >= 500`,
		`| parse logfmt prefix log.`,
		`| parse regex r"user=(?<user>\w+)" into (ip as string)`,
		`| keep host, status`,
		`| drop _raw`,
		`| keep * except _raw`,
		`| rename duration_ms as latency`,
		`| extend is_err = status >= 500`,
		`| stats count(), avg(dur) by service`,
		`| every 5m by service stats count()`,
		`| latency dur every 5m by endpoint`,
		`| where exists(amount)`,
		`from nginx | parse combined | where status == 500`,
		`let $a = from x; from $a | head 1`,
		`| union [from a | head 1], [from b | head 1]`,
		`| join type=left on user_id with [from users]`,
		`| stats count(where status >= 500) as errors`,
	}
	for _, ex := range examples {
		f.Add(ex)
	}

	f.Fuzz(func(t *testing.T, input string) {
		q, diags := Parse(input)

		// Property 1: never panics (if we got here, it didn't).

		// Property 2: q is never nil.
		if q == nil {
			t.Fatal("Parse returned nil Query")
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

		// Property 4: String() never panics.
		_ = q.String()
	})
}

// loadCorpusFuzz loads the corpus for fuzz test seeding (uses testing.F).
func loadCorpusFuzz(f *testing.F) []corpusEntry {
	f.Helper()
	file, err := os.Open("../testdata/corpus/corpus.jsonl")
	if err != nil {
		f.Fatalf("open corpus: %v", err)
	}
	defer file.Close()

	var entries []corpusEntry
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e corpusEntry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func diagMsgs(diags []Diag) []string {
	msgs := make([]string, len(diags))
	for i, d := range diags {
		msgs[i] = fmt.Sprintf("[%s] %s", d.Code, d.Message)
	}
	return msgs
}

// Unused but matches the test pattern from expr_test.go.
var _ = ast.Span{}
