// Package lynxflow will hold the v2 language frontend (RFC-002). Until the
// parser lands (Phase 2), this package only validates the Phase 0 acceptance
// corpus shape.
package lynxflow

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

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
	f, err := os.Open("testdata/corpus/corpus.jsonl")
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

func TestCorpusShape(t *testing.T) {
	entries := loadCorpus(t)
	if len(entries) < 50 {
		t.Fatalf("corpus has %d entries, want at least 50 (PLAN.md §18.1 Phase 0)", len(entries))
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, e := range entries {
		if e.ID == "" || e.Name == "" || e.Source == "" || e.SPL2 == "" || e.LynxFlow == "" {
			t.Errorf("entry %q (%s): id, name, source, spl2, and lynxflow are all required", e.ID, e.Name)
		}
		if ids[e.ID] {
			t.Errorf("entry %q: duplicate id", e.ID)
		}
		ids[e.ID] = true
		if names[e.Name] {
			t.Errorf("entry %q: duplicate name %q", e.ID, e.Name)
		}
		names[e.Name] = true
		if len(e.Features) == 0 {
			t.Errorf("entry %q: at least one feature tag required", e.ID)
		}
	}
}

func TestCorpusBansOldSyntax(t *testing.T) {
	// Spellings that RFC-002 kills outright must not appear in translated
	// queries (word-boundary-ish checks on lowercased text).
	banned := []string{
		"timechart", "fillnull", " eval ", "| eval", " fields ", "| fields",
		"| table", " omit ", "| take", "order by", "=~", "!~", " like ",
		"unpack_", "| rex", "f\"", "glimpse", "enrich ", "| running",
		"count(eval", "perc50(", "perc90(", "perc95(", "perc99(",
		"| search", "| select", "group by", "compute ",
	}
	for _, e := range loadCorpus(t) {
		q := " " + strings.ToLower(e.LynxFlow) + " "
		for _, b := range banned {
			if strings.Contains(q, b) {
				t.Errorf("entry %q: translated query contains killed spelling %q: %s", e.ID, strings.TrimSpace(b), e.LynxFlow)
			}
		}
	}
}
