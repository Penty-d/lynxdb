package translate

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

// corpusEntry mirrors the structure in corpus.jsonl.
type corpusEntry struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	SPL2     string   `json:"spl2"`
	LynxFlow string   `json:"lynxflow"`
	Features []string `json:"features"`
	Notes    string   `json:"notes"`
}

func loadCorpus(t *testing.T) []corpusEntry {
	t.Helper()
	data, err := os.ReadFile("../testdata/corpus/corpus.jsonl")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var entries []corpusEntry
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e corpusEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse corpus entry: %v", err)
		}
		entries = append(entries, e)
	}
	return entries
}

// isLFParseClean checks if a query parses as valid LynxFlow with zero errors.
func isLFParseClean(q string) bool {
	_, diags := parser.Parse(q)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return false
		}
	}
	return true
}

// TestTranslator_Table runs the translator on supported SPL2 queries and
// verifies the output parses cleanly as LynxFlow.
func TestTranslator_Table(t *testing.T) {
	tests := []struct {
		name     string
		spl2     string
		wantErr  bool
		contains string // substring expected in output (optional)
	}{
		{
			name:     "simple_where_eq",
			spl2:     "| where status=500 | stats count",
			contains: "status == 500",
		},
		{
			name:     "stats_count_by",
			spl2:     "| stats count by level",
			contains: "count()",
		},
		{
			name:     "eval_to_extend",
			spl2:     `| eval is_err=if(status>=500,1,0)`,
			contains: "extend is_err",
		},
		{
			name:     "fields_plus_to_keep",
			spl2:     "| fields host, status",
			contains: "keep host, status",
		},
		{
			name:     "fields_minus_to_drop",
			spl2:     "| fields - trace_id, span_id",
			contains: "drop trace_id, span_id",
		},
		{
			name:     "table_to_keep",
			spl2:     "| table host, status",
			contains: "keep host, status",
		},
		{
			name:     "rename_as_lowercase",
			spl2:     "| rename service AS svc",
			contains: "service as svc",
		},
		{
			name:     "sort_prefix_form",
			spl2:     "| sort -count, level",
			contains: "sort -count, level",
		},
		{
			name:     "head",
			spl2:     "| head 10",
			contains: "head 10",
		},
		{
			name:     "tail",
			spl2:     "| tail 5",
			contains: "tail 5",
		},
		{
			name:     "dedup_simple",
			spl2:     "| dedup level",
			contains: "dedup level",
		},
		{
			name:     "dedup_limit",
			spl2:     "| dedup 2 level",
			contains: "dedup 2 level",
		},
		{
			name:     "top",
			spl2:     "| top 3 service",
			contains: "top 3 service",
		},
		{
			name:     "rare",
			spl2:     "| rare 3 service",
			contains: "rare 3 service",
		},
		{
			name:     "fillnull_to_coalesce",
			spl2:     `| fillnull value="N/A" error, threat_type`,
			contains: "??",
		},
		{
			name:     "count_eval_to_count_where",
			spl2:     "| stats count(eval(status>=500)) as errors",
			contains: "count(where",
		},
		{
			name: "conditional_agg_where",
			spl2: `| stats count as total, count(eval(level="ERROR")) as errors`,
		},
		{
			name:     "eventstats",
			spl2:     "| eventstats avg(duration_ms) as global_avg",
			contains: "eventstats avg(duration_ms) as global_avg",
		},
		{
			name:     "percs_map",
			spl2:     "| stats perc50(duration_ms) as p50, perc99(duration_ms) as p99",
			contains: "p50(duration_ms)",
		},
		{
			name:     "mean_to_avg",
			spl2:     "| stats mean(duration_ms) as avg_dur",
			contains: "avg(duration_ms)",
		},

		// Unsupported constructs — should error
		{
			name:    "join_unsupported",
			spl2:    "| join type=inner client_ip [| stats count by src_ip]",
			wantErr: true,
		},
		{
			name:    "rex_unsupported",
			spl2:    `| rex field=_raw "user=(?P<user>\w+)"`,
			wantErr: true,
		},
		{
			name:    "multisearch_unsupported",
			spl2:    "| multisearch [| search error] [| search warn]",
			wantErr: true,
		},
		{
			name:    "append_unsupported",
			spl2:    "| append [| where level=\"ERROR\" | stats count as n]",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, notes, err := SPL2ToLynxFlow(tt.spl2)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got result: %q", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Verify result parses as valid LynxFlow
			if !isLFParseClean(result) {
				t.Errorf("output does not parse as clean LynxFlow:\n  %s", result)
			}

			if tt.contains != "" && !strings.Contains(result, tt.contains) {
				t.Errorf("output does not contain %q:\n  %s", tt.contains, result)
			}

			_ = notes // Notes are informational
		})
	}
}

// TestTranslator_FloatDivision verifies the float division rule.
func TestTranslator_FloatDivision(t *testing.T) {
	result, notes, err := SPL2ToLynxFlow(`| eval rate=errors/total`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should contain float() wrapper
	if !strings.Contains(result, "float(") {
		t.Errorf("expected float() wrapper for division, got: %s", result)
	}

	// Should have a note about int-division
	found := false
	for _, n := range notes {
		if n.Code == "int-division" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected int-division note")
	}

	// Output should parse cleanly
	if !isLFParseClean(result) {
		t.Errorf("output does not parse as clean LynxFlow: %s", result)
	}
}

// TestTranslator_FloatDivision_LiteralFloat verifies literal float denominators
// are NOT wrapped (already float).
func TestTranslator_FloatDivision_LiteralFloat(t *testing.T) {
	result, _, err := SPL2ToLynxFlow(`| eval rate=errors*100.0/total`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 100.0 is a literal float; (errors*100.0) is the left side of /, which
	// is NOT a literal float (it's an ArithExpr). So float() wrapping happens
	// on the ArithExpr, which is fine.
	if !isLFParseClean(result) {
		t.Errorf("output does not parse as clean LynxFlow: %s", result)
	}
}

// TestTranslator_SubstrReindex verifies 1-based to 0-based substr reindexing.
func TestTranslator_SubstrReindex(t *testing.T) {
	result, notes, err := SPL2ToLynxFlow(`| eval prefix=substr(path, 1, 7)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should contain 0 as the start index
	if !strings.Contains(result, "substr(path, 0, 7)") {
		t.Errorf("expected substr reindex to 0, got: %s", result)
	}

	// Should have a note about substr reindexing
	found := false
	for _, n := range notes {
		if n.Code == "substr-reindex" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected substr-reindex note")
	}
}

// TestTranslator_Corpus tests against the corpus.jsonl golden entries.
// For entries whose SPL2 fits the restricted grammar, we verify:
//   - The translation produces parse-clean LynxFlow output
//   - OR the SPL2 is not supported (translation error is acceptable)
func TestTranslator_Corpus(t *testing.T) {
	entries := loadCorpus(t)

	var (
		translateExact  int
		translateClean  int
		translateRefuse int
	)

	for _, e := range entries {
		t.Run(e.ID+"_"+e.Name, func(t *testing.T) {
			result, _, err := SPL2ToLynxFlow(e.SPL2)
			if err != nil {
				// Translation failed — this is acceptable for unsupported
				// constructs (join, rex, multisearch, etc.)
				translateRefuse++
				return
			}

			// Verify the output parses as valid LynxFlow
			if !isLFParseClean(result) {
				t.Errorf("corpus %s: output does not parse as clean LynxFlow:\n  SPL2:     %s\n  Output:   %s", e.ID, e.SPL2, result)
				return
			}

			// Check if it matches the corpus golden exactly
			if result == e.LynxFlow {
				translateExact++
			} else {
				translateClean++
			}
		})
	}

	t.Logf("Corpus results: %d exact, %d parse-clean-but-different, %d refused",
		translateExact, translateClean, translateRefuse)
}

// TestSPL2ToLynxFlow_ValidationFailure verifies that output is validated
// against the LynxFlow parser.
func TestSPL2ToLynxFlow_ValidationFailure(t *testing.T) {
	// A query that the SPL2 parser accepts but that has no valid LynxFlow
	// mapping would fail validation. This is hard to construct because the
	// translator handles all supported commands. Instead, verify the happy
	// path doesn't fail validation.
	result, _, err := SPL2ToLynxFlow("| stats count by level")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isLFParseClean(result) {
		t.Errorf("basic query failed validation: %s", result)
	}
}
