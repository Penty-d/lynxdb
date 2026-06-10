package main

import (
	"os"
	"strings"
	"testing"
)

// Test that --language flag is validated.
func TestQueryFile_LanguageFlag_InvalidValue(t *testing.T) {
	_, _, err := runCmd(t, "query", "--file", testdataPath("logs/access.log"), "--language", "invalid", "| stats count()")
	if err == nil {
		t.Fatal("expected error for invalid language")
	}
	if !strings.Contains(err.Error(), "invalid language") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Test that --language lynxflow routes to the LynxFlow engine for file-mode queries.
func TestQueryFile_LynxFlow_StatsCount(t *testing.T) {
	stdout, _, err := runCmd(t, "query", "--file", testdataPath("logs/access.log"),
		"--language", "lynxflow", "--format", "json",
		"from main | stats count()")
	if err != nil {
		t.Fatalf("lynxflow query failed: %v", err)
	}

	// Parse JSON output to verify we got a count.
	rows := mustParseJSON(t, stdout)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	// LynxFlow uses "count()" as the field name (includes parens).
	count, ok := rows[0]["count()"]
	if !ok {
		// Fallback: check "count" without parens.
		count, ok = rows[0]["count"]
	}
	if !ok {
		t.Fatalf("missing 'count' or 'count()' field in result: %v", rows[0])
	}
	// The count should be 1000 (test data has 1000 lines).
	if c, ok := count.(float64); !ok || c != 1000 {
		t.Errorf("count = %v, want 1000", count)
	}
}

// Test that --language spl2 routes to the SPL2 engine.
func TestQueryFile_SPL2_Explicit(t *testing.T) {
	stdout, _, err := runCmd(t, "query", "--file", testdataPath("logs/access.log"),
		"--language", "spl2", "--format", "json",
		"| stats count")
	if err != nil {
		t.Fatalf("spl2 query failed: %v", err)
	}

	got := jsonCount(t, stdout)
	if got != 1000 {
		t.Errorf("expected count=1000, got %d", got)
	}
}

// Test LynxFlow file mode with NDJSON input via temp file.
func TestQueryFile_LynxFlow_NDJSONTempFile(t *testing.T) {
	// Create a temp NDJSON file.
	tmpFile := t.TempDir() + "/events.ndjson"
	lines := []string{
		`{"level":"error","service":"api","duration_ms":150}`,
		`{"level":"info","service":"web","duration_ms":50}`,
		`{"level":"error","service":"api","duration_ms":250}`,
		`{"level":"info","service":"api","duration_ms":30}`,
	}
	if err := os.WriteFile(tmpFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, "query", "--file", tmpFile,
		"--language", "lynxflow", "--format", "json", "--raw",
		`from main | where level == "error" | stats count()`)
	if err != nil {
		t.Fatalf("lynxflow NDJSON query failed: %v", err)
	}

	rows := mustParseJSON(t, stdout)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d: %s", len(rows), stdout)
	}
	// LynxFlow uses "count()" as the field name.
	count, ok := rows[0]["count()"]
	if !ok {
		count, ok = rows[0]["count"]
	}
	if !ok {
		t.Fatalf("missing 'count' or 'count()' field: %v", rows[0])
	}
	if c, ok := count.(float64); !ok || c != 2 {
		t.Errorf("count = %v, want 2", count)
	}
}

// Test --show-rewritten for LynxFlow sugar queries.
func TestQueryFile_LynxFlow_ShowRewritten(t *testing.T) {
	tmpFile := t.TempDir() + "/events.ndjson"
	lines := []string{
		`{"service":"api","status":200}`,
		`{"service":"web","status":500}`,
		`{"service":"api","status":404}`,
	}
	if err := os.WriteFile(tmpFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, err := runCmd(t, "query", "--file", tmpFile,
		"--language", "lynxflow", "--format", "json", "--raw",
		"--show-rewritten",
		"from main | top 2 service")
	if err != nil {
		t.Fatalf("lynxflow top query failed: %v", err)
	}

	// The stderr should contain rewrite info about sugar:top desugaring.
	if !strings.Contains(stderr, "rewrite") {
		t.Errorf("expected rewrite output in stderr, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "sugar:top") {
		t.Errorf("expected sugar:top in rewrite, got:\n%s", stderr)
	}
}

// Test that auto-detection defaults to spl2 for ambiguous queries in file mode
// (preserving golden transcript compatibility).
func TestQueryFile_AutoDetect_DefaultsSPL2(t *testing.T) {
	// "| stats count" is valid in both languages. File mode should default to spl2.
	stdout, _, err := runCmd(t, "query", "--file", testdataPath("logs/access.log"),
		"--format", "json",
		"| stats count")
	if err != nil {
		t.Fatalf("auto-detect query failed: %v", err)
	}

	got := jsonCount(t, stdout)
	if got != 1000 {
		t.Errorf("expected count=1000, got %d", got)
	}
}

// Test LynxFlow explain output.
func TestExplain_LynxFlow(t *testing.T) {
	stdout, _, err := runCmd(t, "query", "--explain",
		"--language", "lynxflow",
		"from main | where status >= 500 | stats count()")
	if err != nil {
		t.Fatalf("lynxflow explain failed: %v", err)
	}

	// Should contain plan tree elements.
	if !strings.Contains(stdout, "Aggregate") && !strings.Contains(stdout, "Scan") && !strings.Contains(stdout, "count") {
		t.Errorf("expected plan output, got:\n%s", stdout)
	}
}

// jsonCount and mustParseJSON are defined in cli_test.go; reused here.
