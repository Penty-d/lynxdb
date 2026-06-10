package physical

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/logical/opt"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
	"github.com/lynxbase/lynxdb/pkg/storage/part"
	"github.com/lynxbase/lynxdb/pkg/storage/segment"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// drainWithStats runs the full pipeline with scan stats collection.
func drainWithStats(t *testing.T, query string, events map[string][]*event.Event) ([]map[string]event.Value, *ScanStats) {
	t.Helper()

	ss := &ScanStats{}
	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Message)
		}
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: "main"})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			t.Fatalf("lower error: %s", d.Message)
		}
	}
	plan, _ = opt.Optimize(plan)

	source := NewStorageSourceFromMapWithStats(events, "main", ss)
	iter, err := Build(plan, BuildOptions{
		Source:    source,
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	return result, ss
}

// drainPartSource runs the full pipeline using a part-backed source.
func drainPartSource(t *testing.T, query string, parts []PartHandle) ([]map[string]event.Value, *ScanStats) {
	t.Helper()

	ss := &ScanStats{}
	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Message)
		}
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: "main"})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			t.Fatalf("lower error: %s", d.Message)
		}
	}
	plan, _ = opt.Optimize(plan)

	source := NewPartSource(parts, "main", ss)
	iter, err := Build(plan, BuildOptions{
		Source:    source,
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	return result, ss
}

// makeTestEvents creates events with controlled content for a given source.
func makeTestEvents(n int, source string, rawTemplate func(i int) string) []*event.Event {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	events := make([]*event.Event, n)
	for i := 0; i < n; i++ {
		raw := rawTemplate(i)
		ev := event.NewEvent(base.Add(time.Duration(i)*time.Millisecond), raw)
		ev.Source = source
		ev.SourceType = "test"
		ev.Index = "main"
		events[i] = ev
	}
	return events
}

// writePartFile writes events as a .lsg part file.
func writePartFile(t *testing.T, dir string, idx string, events []*event.Event) {
	t.Helper()
	layout := part.NewLayout(dir)
	w := part.NewWriter(layout, segment.CompressionLZ4, part.DefaultRowGroupSize)
	_, err := w.Write(context.Background(), idx, events, 0)
	if err != nil {
		t.Fatalf("part.Writer.Write: %v", err)
	}
}

// openPartHandle opens a part file and returns a PartHandle with reader + inverted index.
func openPartHandle(t *testing.T, path string, idx string) PartHandle {
	t.Helper()

	ms, err := segment.OpenSegmentFile(path)
	if err != nil {
		t.Fatalf("OpenSegmentFile(%s): %v", path, err)
	}
	t.Cleanup(func() { ms.Close() })

	reader := ms.Reader()

	// Extract inverted index from the segment via the Reader's InvertedIndex method.
	invIdx, err := reader.InvertedIndex()
	if err != nil {
		t.Logf("warning: InvertedIndex: %v", err)
	}

	return PartHandle{
		Reader:      reader,
		InvertedIdx: invIdx,
		Index:       idx,
	}
}

// findPartFiles finds .lsg files in a directory tree.
func findPartFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".lsg" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("findPartFiles: %v", err)
	}
	return files
}

// ---------------------------------------------------------------------------
// Task 1: Ephemeral pushdown mapping verification
// ---------------------------------------------------------------------------

func TestEphemeralPushdown_RawTerms(t *testing.T) {
	// has(_raw, "needle") should filter to only events containing the token "needle".
	events := map[string][]*event.Event{
		"main": {
			mkEvent("this has a needle in it"),
			mkEvent("this does not have what you seek"),
			mkEvent("another NEEDLE here"), // case-insensitive
		},
	}

	// "from main needle" desugars to: from main | where has(_raw, "needle")
	// The optimizer should push "needle" as a RawTerm.
	results, ss := drainWithStats(t, `from main needle`, events)

	if ss.TotalEvents.Load() != 3 {
		t.Errorf("TotalEvents: got %d, want 3", ss.TotalEvents.Load())
	}
	// Should match 2 events (first and third).
	if len(results) != 2 {
		t.Errorf("results: got %d, want 2", len(results))
	}
}

func TestEphemeralPushdown_BloomTerms(t *testing.T) {
	// contains(_raw, "special_phrase") should be pushed as a bloom term.
	events := map[string][]*event.Event{
		"main": {
			mkEvent("log line with special_phrase inside"),
			mkEvent("another log line without the term"),
			mkEvent("SPECIAL_PHRASE in uppercase"), // CI
		},
	}

	results, _ := drainWithStats(t, `from main | where contains(_raw, "special_phrase")`, events)

	if len(results) != 2 {
		t.Errorf("results: got %d, want 2", len(results))
	}
}

func TestEphemeralPushdown_FieldPredicates(t *testing.T) {
	events := map[string][]*event.Event{
		"main": {
			mkEventWithField("info log", "level", "info"),
			mkEventWithField("error log", "level", "error"),
			mkEventWithField("warn log", "level", "warn"),
		},
	}

	results, ss := drainWithStats(t, `from main | where level == "error"`, events)

	if len(results) != 1 {
		t.Errorf("results: got %d, want 1", len(results))
	}
	if ss.TotalEvents.Load() != 3 {
		t.Errorf("TotalEvents: got %d, want 3", ss.TotalEvents.Load())
	}
}

// ---------------------------------------------------------------------------
// Task 2: Skip-effectiveness tests (Part-backed)
// ---------------------------------------------------------------------------

func TestPartSource_SkipEffectiveness(t *testing.T) {
	// Create multiple parts with controlled content:
	// Part 1: events with "needle" term
	// Part 2: events WITHOUT "needle" term
	// Part 3: events WITHOUT "needle" term
	//
	// A query for "needle" should skip parts 2 and 3 via inverted index.

	dir := t.TempDir()

	// Part 1: 50 events, all containing "needle".
	needleEvents := makeTestEvents(50, "app.log", func(i int) string {
		return fmt.Sprintf("2026-06-01T00:00:00Z level=INFO msg=found needle item=%d", i)
	})
	writePartFile(t, dir, "main", needleEvents)

	// Part 2: 50 events, NO "needle".
	hayEvents := makeTestEvents(50, "app.log", func(i int) string {
		return fmt.Sprintf("2026-06-01T00:01:00Z level=INFO msg=normal haystack item=%d", i)
	})
	writePartFile(t, dir, "main", hayEvents)

	// Part 3: 50 more events, NO "needle".
	moreHayEvents := makeTestEvents(50, "app.log", func(i int) string {
		return fmt.Sprintf("2026-06-01T00:02:00Z level=WARN msg=other haystack item=%d", i)
	})
	writePartFile(t, dir, "main", moreHayEvents)

	// Find the actual .lsg files.
	lsgFiles := findPartFiles(t, dir)
	if len(lsgFiles) != 3 {
		t.Fatalf("expected 3 .lsg files, got %d: %v", len(lsgFiles), lsgFiles)
	}

	// Open all parts.
	var parts []PartHandle
	for _, f := range lsgFiles {
		parts = append(parts, openPartHandle(t, f, "main"))
	}

	// Query: "from main needle | stats count()"
	// The optimizer should push "needle" as RawTerm.
	results, ss := drainPartSource(t, `from main needle | stats count()`, parts)

	// Verify correct result: 50 events match.
	if len(results) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(results))
	}
	countVal, ok := results[0]["count()"]
	if !ok {
		t.Fatal("missing count() in result")
	}
	if countVal.AsInt() != 50 {
		t.Errorf("count(): got %d, want 50", countVal.AsInt())
	}

	// Verify skip effectiveness: 3 parts total, 2 should be skipped.
	partsTotal := ss.PartsTotal.Load()
	partsSkipped := ss.PartsSkipped.Load()
	totalEvents := ss.TotalEvents.Load()
	filteredEvents := ss.FilteredEvents.Load()

	t.Logf("PartsTotal=%d, PartsSkipped=%d, TotalEvents=%d, FilteredEvents=%d",
		partsTotal, partsSkipped, totalEvents, filteredEvents)

	if partsTotal != 3 {
		t.Errorf("PartsTotal: got %d, want 3", partsTotal)
	}
	if partsSkipped < 2 {
		t.Errorf("PartsSkipped: got %d, want >= 2 (inverted index should skip non-matching parts)", partsSkipped)
	}
	// TotalEvents counts events across all parts considered (before skip).
	// 3 parts x 50 events = 150 total.
	if totalEvents != 150 {
		t.Errorf("TotalEvents: got %d, want 150 (all parts' events counted)", totalEvents)
	}
	// FilteredEvents should be only from the 1 non-skipped part: 50.
	if filteredEvents != 50 {
		t.Errorf("FilteredEvents: got %d, want 50 (only matching part's events)", filteredEvents)
	}
}

func TestPartSource_SkipEffectiveness_FullScan(t *testing.T) {
	// When no search terms are present, all parts should be scanned.
	dir := t.TempDir()

	events1 := makeTestEvents(30, "app.log", func(i int) string {
		return fmt.Sprintf("2026-06-01T00:00:00Z level=INFO msg=hello item=%d", i)
	})
	writePartFile(t, dir, "main", events1)

	events2 := makeTestEvents(30, "app.log", func(i int) string {
		return fmt.Sprintf("2026-06-01T00:01:00Z level=WARN msg=world item=%d", i)
	})
	writePartFile(t, dir, "main", events2)

	lsgFiles := findPartFiles(t, dir)
	var parts []PartHandle
	for _, f := range lsgFiles {
		parts = append(parts, openPartHandle(t, f, "main"))
	}

	results, ss := drainPartSource(t, `from main | stats count()`, parts)

	if len(results) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(results))
	}
	if results[0]["count()"].AsInt() != 60 {
		t.Errorf("count(): got %d, want 60", results[0]["count()"].AsInt())
	}

	// No parts skipped — no search terms.
	if ss.PartsSkipped.Load() != 0 {
		t.Errorf("PartsSkipped: got %d, want 0", ss.PartsSkipped.Load())
	}
	if ss.PartsTotal.Load() != 2 {
		t.Errorf("PartsTotal: got %d, want 2", ss.PartsTotal.Load())
	}
}

// ---------------------------------------------------------------------------
// Task 3: Case-rule verification (end-to-end)
// ---------------------------------------------------------------------------

func TestCaseRule_HasIsCaseInsensitive(t *testing.T) {
	// has() is CI: searching for "error" should match "ERROR", "Error", "error".
	events := map[string][]*event.Event{
		"main": {
			mkEvent("level=ERROR something failed"),
			mkEvent("level=Error mixed case"),
			mkEvent("level=error lowercase"),
			mkEvent("level=INFO should not match"),
		},
	}

	results, _ := drainWithStats(t, `from main error`, events)

	if len(results) != 3 {
		t.Errorf("has() CI: got %d results, want 3", len(results))
		for i, r := range results {
			t.Logf("  result[%d]: _raw=%s", i, r["_raw"])
		}
	}
}

func TestCaseRule_HasIsCaseInsensitive_DifferentCaseInIndex(t *testing.T) {
	// Prove the full path incl. index candidates: search for "ERROR" (uppercase)
	// should match events indexed as "error" (the tokenizer lowercases).
	events := map[string][]*event.Event{
		"main": {
			mkEvent("found an error in the system"),
			mkEvent("no problems here"),
		},
	}

	// Search for "ERROR" — should match because has() is CI.
	results, _ := drainWithStats(t, `from main ERROR`, events)
	if len(results) != 1 {
		t.Errorf("has() CI uppercase search: got %d results, want 1", len(results))
	}
}

func TestCaseRule_HasIsCaseInsensitive_PartBacked(t *testing.T) {
	// End-to-end through part-backed inverted index: the inverted index stores
	// lowercased tokens. A has() search for "ERROR" should still match because
	// the optimizer lowercases the search term before pushing to RawTerms.
	dir := t.TempDir()

	events := makeTestEvents(10, "app.log", func(i int) string {
		if i < 5 {
			return fmt.Sprintf("2026-06-01T00:00:00Z level=ERROR msg=failure item=%d", i)
		}
		return fmt.Sprintf("2026-06-01T00:00:00Z level=INFO msg=success item=%d", i)
	})
	writePartFile(t, dir, "main", events)

	lsgFiles := findPartFiles(t, dir)
	var parts []PartHandle
	for _, f := range lsgFiles {
		parts = append(parts, openPartHandle(t, f, "main"))
	}

	// Search for "error" (lowercase). The inverted index has "error" from "ERROR".
	results, _ := drainPartSource(t, `from main error | stats count()`, parts)
	if len(results) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(results))
	}
	if results[0]["count()"].AsInt() != 5 {
		t.Errorf("count(): got %d, want 5 (events with ERROR)", results[0]["count()"].AsInt())
	}
}

func TestCaseRule_EqualityIsCaseSensitive(t *testing.T) {
	// == on a field is case-sensitive.
	events := map[string][]*event.Event{
		"main": {
			mkEventWithField("log1", "level", "ERROR"),
			mkEventWithField("log2", "level", "error"),
			mkEventWithField("log3", "level", "Error"),
		},
	}

	results, _ := drainWithStats(t, `from main | where level == "ERROR"`, events)

	if len(results) != 1 {
		t.Errorf("== CS: got %d results, want 1", len(results))
		for i, r := range results {
			t.Logf("  result[%d]: level=%s", i, r["level"])
		}
	}
}

func TestCaseRule_ContainsIsCaseInsensitive(t *testing.T) {
	// contains() is CI (OpContainsCI).
	events := map[string][]*event.Event{
		"main": {
			mkEvent("this has ERROR in it"),
			mkEvent("this has error in it"),
			mkEvent("this has nothing special"),
		},
	}

	results, _ := drainWithStats(t, `from main | where contains(_raw, "error")`, events)

	if len(results) != 2 {
		t.Errorf("contains() CI: got %d results, want 2", len(results))
	}
}

func TestCaseRule_GlobIsCaseSensitive(t *testing.T) {
	// glob() is CS (uses filepath.Match which is CS).
	events := map[string][]*event.Event{
		"main": {
			mkEventWithField("log1", "host", "web-01"),
			mkEventWithField("log2", "host", "Web-01"),
			mkEventWithField("log3", "host", "WEB-01"),
			mkEventWithField("log4", "host", "db-01"),
		},
	}

	results, _ := drainWithStats(t, `from main | where glob(host, "web-*")`, events)

	if len(results) != 1 {
		t.Errorf("glob() CS: got %d results, want 1 (only 'web-01')", len(results))
		for i, r := range results {
			t.Logf("  result[%d]: host=%s", i, r["host"])
		}
	}
}

// ---------------------------------------------------------------------------
// Task 4: Verify-stays contract test
// ---------------------------------------------------------------------------

func TestVerifyStays_IndexCandidateNotInRow(t *testing.T) {
	// A term that appears in a segment but NOT in every row of that segment.
	// The inverted index gives candidates at the segment level (the segment
	// contains the term), but the row-level filter must verify that only
	// truly-matching rows are returned.
	//
	// In the ephemeral path, the RawTerms filter does this verification.
	// In the part path, the inverted index bitmap is row-level, so it also
	// provides row-level precision.

	events := map[string][]*event.Event{
		"main": {
			mkEvent("this line has needle in it"),       // matches
			mkEvent("this line has different content"),  // does NOT match
			mkEvent("another line with needle present"), // matches
			mkEvent("completely unrelated log entry"),   // does NOT match
		},
	}

	results, ss := drainWithStats(t, `from main needle`, events)

	if len(results) != 2 {
		t.Errorf("verify-stays: got %d results, want 2 (only rows with 'needle')", len(results))
		for i, r := range results {
			t.Logf("  result[%d]: _raw=%s", i, r["_raw"])
		}
	}

	// All 4 events were scanned, but only 2 survived filtering.
	if ss.TotalEvents.Load() != 4 {
		t.Errorf("TotalEvents: got %d, want 4", ss.TotalEvents.Load())
	}
	if ss.FilteredEvents.Load() != 2 {
		t.Errorf("FilteredEvents: got %d, want 2", ss.FilteredEvents.Load())
	}
}

func TestVerifyStays_PartBacked(t *testing.T) {
	// Same verify-stays test but with real part files. The inverted index
	// bitmap is row-level, so rows that don't contain "needle" should be
	// excluded by the bitmap even though the part contains the term.
	dir := t.TempDir()

	events := makeTestEvents(20, "app.log", func(i int) string {
		if i%4 == 0 {
			return fmt.Sprintf("2026-06-01T00:00:00Z needle found item=%d", i)
		}
		return fmt.Sprintf("2026-06-01T00:00:00Z normal log line item=%d", i)
	})
	writePartFile(t, dir, "main", events)

	lsgFiles := findPartFiles(t, dir)
	var parts []PartHandle
	for _, f := range lsgFiles {
		parts = append(parts, openPartHandle(t, f, "main"))
	}

	// Query: "from main needle"
	results, ss := drainPartSource(t, `from main needle | stats count()`, parts)

	if len(results) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(results))
	}

	// 5 out of 20 events have "needle" (i=0,4,8,12,16).
	count := results[0]["count()"].AsInt()
	if count != 5 {
		t.Errorf("count(): got %d, want 5", count)
	}

	// The part was NOT skipped (it contains needle), but the bitmap should
	// have filtered to only 5 rows.
	if ss.PartsSkipped.Load() != 0 {
		t.Errorf("PartsSkipped: got %d, want 0 (part contains needle)", ss.PartsSkipped.Load())
	}
	if ss.FilteredEvents.Load() != 5 {
		t.Errorf("FilteredEvents: got %d, want 5", ss.FilteredEvents.Load())
	}
}

// ---------------------------------------------------------------------------
// Benchmark: Task 5 — Search-tier benchmark
// ---------------------------------------------------------------------------

func BenchmarkSearchTier_Has(b *testing.B) {
	events := generateBenchEvents(100_000)
	evMap := map[string][]*event.Event{"main": events}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ss := &ScanStats{}
		source := NewStorageSourceFromMapWithStats(evMap, "main", ss)
		runBenchQuery(b, `from main error`, source)
	}
}

func BenchmarkSearchTier_Contains(b *testing.B) {
	events := generateBenchEvents(100_000)
	evMap := map[string][]*event.Event{"main": events}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ss := &ScanStats{}
		source := NewStorageSourceFromMapWithStats(evMap, "main", ss)
		runBenchQuery(b, `from main | where contains(_raw, "error")`, source)
	}
}

func BenchmarkSearchTier_Matches(b *testing.B) {
	events := generateBenchEvents(100_000)
	evMap := map[string][]*event.Event{"main": events}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ss := &ScanStats{}
		source := NewStorageSourceFromMapWithStats(evMap, "main", ss)
		runBenchQuery(b, `from main | where matches(_raw, r"err\w+")`, source)
	}
}

func generateBenchEvents(n int) []*event.Event {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	hosts := []string{"web-01", "web-02", "api-01", "db-01"}
	events := make([]*event.Event, n)

	for i := 0; i < n; i++ {
		level := levels[i%len(levels)]
		host := hosts[i%len(hosts)]
		raw := fmt.Sprintf("%s host=%s level=%s status=%d msg=\"request processed\"",
			base.Add(time.Duration(i)*time.Millisecond).Format(time.RFC3339Nano),
			host, level, 200+(i%5)*100)
		ev := event.NewEvent(base.Add(time.Duration(i)*time.Millisecond), raw)
		ev.Source = "app.log"
		ev.SourceType = "test"
		ev.Index = "main"
		ev.SetField("level", event.StringValue(level))
		ev.SetField("host", event.StringValue(host))
		ev.SetField("status", event.IntValue(int64(200+(i%5)*100)))
		events[i] = ev
	}
	return events
}

func runBenchQuery(b *testing.B, query string, source func(*logical.Scan) (pipeline.Iterator, error)) {
	b.Helper()
	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			b.Fatalf("parse error: %s", d.Message)
		}
	}
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: "main"})
	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: "main"})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			b.Fatalf("lower error: %s", d.Message)
		}
	}
	plan, _ = opt.Optimize(plan)

	iter, err := Build(plan, BuildOptions{
		Source:    source,
		BatchSize: 1024,
	})
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	_, err = pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		b.Fatalf("CollectAll: %v", err)
	}
}

// ---------------------------------------------------------------------------
// mkEvent / mkEventWithField helpers
// ---------------------------------------------------------------------------

func mkEvent(raw string) *event.Event {
	ev := event.NewEvent(time.Now(), raw)
	ev.Source = "test"
	ev.Index = "main"
	return ev
}

func mkEventWithField(raw, field, value string) *event.Event {
	ev := mkEvent(raw)
	ev.SetField(field, event.StringValue(value))
	return ev
}
