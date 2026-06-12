package physical

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	lfast "github.com/lynxbase/lynxdb/pkg/lynxflow/ast"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/logical/opt"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

// ---------------------------------------------------------------------------
// sliceSource helper
// ---------------------------------------------------------------------------

// sliceSource builds a RowScanIterator from row maps.
func sliceSource(rows []map[string]event.Value, batchSize int) pipeline.Iterator {
	if batchSize <= 0 {
		batchSize = pipeline.DefaultBatchSize
	}
	return pipeline.NewRowScanIterator(rows, batchSize)
}

// sourceFromRows returns a BuildOptions.Source that ignores the scan node and
// returns a fixed set of rows.
func sourceFromRows(rows []map[string]event.Value) func(*logical.Scan) (pipeline.Iterator, error) {
	return func(_ *logical.Scan) (pipeline.Iterator, error) {
		return sliceSource(rows, 1024), nil
	}
}

// drain runs the full pipeline: parse -> desugar -> lower -> optimize -> build -> collect.
func drain(t *testing.T, query string, rows []map[string]event.Value) []map[string]event.Value {
	t.Helper()
	return drainWithBatchSize(t, query, rows, 1024)
}

func drainWithBatchSize(t *testing.T, query string, rows []map[string]event.Value, batchSize int) []map[string]event.Value {
	t.Helper()
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

	iter, err := Build(plan, BuildOptions{
		Source:    sourceFromRows(rows),
		BatchSize: batchSize,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	return result
}

// ---------------------------------------------------------------------------
// Test data helpers
// ---------------------------------------------------------------------------

func intV(n int64) event.Value              { return event.IntValue(n) }
func strV(s string) event.Value             { return event.StringValue(s) }
func floatV(f float64) event.Value          { return event.FloatValue(f) }
func boolV(b bool) event.Value              { return event.BoolValue(b) }
func nullV() event.Value                    { return event.NullValue() }
func tsV(t time.Time) event.Value           { return event.TimestampValue(t) }
func arrV(elems ...event.Value) event.Value { return event.ArrayValue(elems) }

func sampleRows() []map[string]event.Value {
	return []map[string]event.Value{
		{"level": strV("info"), "status": intV(200), "duration": floatV(10.5), "host": strV("web-01")},
		{"level": strV("error"), "status": intV(500), "duration": floatV(100.2), "host": strV("web-01")},
		{"level": strV("warn"), "status": intV(404), "duration": floatV(5.1), "host": strV("web-02")},
		{"level": strV("error"), "status": intV(503), "duration": floatV(200.3), "host": strV("web-02")},
		{"level": strV("info"), "status": intV(200), "duration": floatV(8.7), "host": strV("web-01")},
	}
}

func timedRows() []map[string]event.Value {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return []map[string]event.Value{
		{"_time": tsV(base), "level": strV("info"), "val": intV(10)},
		{"_time": tsV(base.Add(1 * time.Minute)), "level": strV("error"), "val": intV(20)},
		{"_time": tsV(base.Add(5 * time.Minute)), "level": strV("info"), "val": intV(30)},
		{"_time": tsV(base.Add(6 * time.Minute)), "level": strV("error"), "val": intV(40)},
		{"_time": tsV(base.Add(11 * time.Minute)), "level": strV("warn"), "val": intV(50)},
		{"_time": tsV(base.Add(12 * time.Minute)), "level": strV("info"), "val": intV(60)},
	}
}

// ---------------------------------------------------------------------------
// Tests: Filter (where)
// ---------------------------------------------------------------------------

func TestBuild_Filter_Simple(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | where status >= 500`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
	for _, r := range result {
		s, ok := r["status"]
		if !ok {
			t.Fatal("missing status field")
		}
		n, _ := s.TryAsInt()
		if n < 500 {
			t.Errorf("expected status >= 500, got %d", n)
		}
	}
}

func TestBuild_Filter_StringEquality(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | where level == "error"`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
}

func TestBuild_Filter_And(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | where level == "error" and status >= 503`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: Extend (eval)
// ---------------------------------------------------------------------------

func TestBuild_Extend_Simple(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | extend doubled = status * 2`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	for i, r := range result {
		d, ok := r["doubled"]
		if !ok {
			t.Fatalf("row %d: missing 'doubled' field", i)
		}
		s := rows[i]["status"]
		sn, _ := s.TryAsInt()
		dn, _ := d.TryAsInt()
		if dn != sn*2 {
			t.Errorf("row %d: expected doubled=%d, got %d", i, sn*2, dn)
		}
	}
}

func TestBuild_Extend_LaterSeesEarlier(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": intV(10)},
	}
	result := drain(t, `from * | extend a = x + 1, b = a + 1`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	a, _ := result[0]["a"].TryAsInt()
	b, _ := result[0]["b"].TryAsInt()
	if a != 11 {
		t.Errorf("expected a=11, got %d", a)
	}
	if b != 12 {
		t.Errorf("expected b=12, got %d", b)
	}
}

func TestBuild_Extend_NullPropagation(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": intV(10)},
		{"x": nullV()},
	}
	result := drain(t, `from * | extend y = x + 1`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
	// First row: y = 11
	y0, _ := result[0]["y"].TryAsInt()
	if y0 != 11 {
		t.Errorf("row 0: expected y=11, got %d", y0)
	}
	// Second row: y = null (null propagation)
	if !result[1]["y"].IsNull() {
		t.Errorf("row 1: expected y=null, got %v", result[1]["y"])
	}
}

// ---------------------------------------------------------------------------
// Tests: Extend + Filter chain
// ---------------------------------------------------------------------------

func TestBuild_ExtendThenFilter(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | extend slow = duration > 50 | where slow == true`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: Aggregate (stats)
// ---------------------------------------------------------------------------

func TestBuild_Stats_Count(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | stats count()`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	c, _ := result[0]["count()"].TryAsInt()
	if c != 5 {
		t.Errorf("expected count=5, got %d", c)
	}
}

func TestBuild_Stats_SumAvgByKey(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | stats sum(duration) as total, avg(duration) as mean by level`, rows)
	// 3 levels: info, error, warn
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}
	found := make(map[string]bool)
	for _, r := range result {
		lv, _ := r["level"].TryAsString()
		found[lv] = true
	}
	for _, lv := range []string{"info", "error", "warn"} {
		if !found[lv] {
			t.Errorf("missing level %q in results", lv)
		}
	}
}

func TestBuild_Stats_DC(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | stats dc(host) as hosts`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	h, _ := result[0]["hosts"].TryAsInt()
	if h != 2 {
		t.Errorf("expected dc(host)=2, got %d", h)
	}
}

func TestBuild_Stats_P95(t *testing.T) {
	rows := make([]map[string]event.Value, 100)
	for i := range rows {
		rows[i] = map[string]event.Value{"val": floatV(float64(i + 1))}
	}
	result := drain(t, `from * | stats p95(val) as p95_val`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	p95, _ := result[0]["p95_val"].TryAsFloat()
	// For 1..100, p95 should be ~95.
	if p95 < 90 || p95 > 100 {
		t.Errorf("expected p95 near 95, got %f", p95)
	}
}

func TestBuild_Stats_ConditionalCount(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | stats count(status, where status >= 500) as errors`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	c, _ := result[0]["errors"].TryAsInt()
	if c != 2 {
		t.Errorf("expected errors=2, got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Tests: TimeBin in stats
// ---------------------------------------------------------------------------

func TestBuild_Stats_TimeBin(t *testing.T) {
	rows := timedRows()
	result := drain(t, `from * | stats count() by bin(_time, 5m)`, rows)
	// 0-5m: 2 rows, 5-10m: 2 rows, 10-15m: 2 rows
	if len(result) < 2 {
		t.Fatalf("expected at least 2 time buckets, got %d", len(result))
	}
	for _, r := range result {
		_, ok := r["_time"]
		if !ok {
			t.Error("missing _time in time-bucketed stats result")
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: EventStats / StreamStats
// ---------------------------------------------------------------------------

func TestBuild_EventStats(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | eventstats count() as total by level`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows (all input preserved), got %d", len(result))
	}
	for _, r := range result {
		_, ok := r["total"]
		if !ok {
			t.Error("missing 'total' field from eventstats")
		}
	}
}

func TestBuild_StreamStats(t *testing.T) {
	rows := []map[string]event.Value{
		{"val": intV(1)},
		{"val": intV(2)},
		{"val": intV(3)},
		{"val": intV(4)},
		{"val": intV(5)},
	}
	result := drain(t, `from * | streamstats sum(val) as running_sum`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	// running_sum should be: 1, 3, 6, 10, 15
	expected := []float64{1, 3, 6, 10, 15}
	for i, r := range result {
		rs, _ := r["running_sum"].TryAsFloat()
		if math.Abs(rs-expected[i]) > 0.01 {
			t.Errorf("row %d: expected running_sum=%f, got %f", i, expected[i], rs)
		}
	}
}

func TestBuild_StreamStats_Window(t *testing.T) {
	rows := []map[string]event.Value{
		{"val": intV(1)},
		{"val": intV(2)},
		{"val": intV(3)},
		{"val": intV(4)},
		{"val": intV(5)},
	}
	result := drain(t, `from * | streamstats window=3 sum(val) as running_sum`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	// Window=3: sum of current + 2 previous (or fewer for first rows).
	// Row 0: 1, Row 1: 1+2=3, Row 2: 1+2+3=6, Row 3: 2+3+4=9, Row 4: 3+4+5=12
	expected := []float64{1, 3, 6, 9, 12}
	for i, r := range result {
		rs, _ := r["running_sum"].TryAsFloat()
		if math.Abs(rs-expected[i]) > 0.01 {
			t.Errorf("row %d: expected running_sum=%f, got %f", i, expected[i], rs)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Sort / Head / Tail / Dedup
// ---------------------------------------------------------------------------

func TestBuild_Sort(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | sort duration`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	prev := 0.0
	for _, r := range result {
		d, _ := r["duration"].TryAsFloat()
		if d < prev {
			t.Errorf("not sorted: %f < %f", d, prev)
		}
		prev = d
	}
}

func TestBuild_SortDesc(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | sort -duration`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	prev := math.MaxFloat64
	for _, r := range result {
		d, _ := r["duration"].TryAsFloat()
		if d > prev {
			t.Errorf("not sorted descending: %f > %f", d, prev)
		}
		prev = d
	}
}

func TestBuild_Head(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | head 2`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
}

func TestBuild_Tail(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | tail 2`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
}

func TestBuild_Dedup(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | dedup level`, rows)
	// 3 unique levels: info, error, warn
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}
	seen := make(map[string]bool)
	for _, r := range result {
		lv, _ := r["level"].TryAsString()
		if seen[lv] {
			t.Errorf("duplicate level %q after dedup", lv)
		}
		seen[lv] = true
	}
}

// ---------------------------------------------------------------------------
// Tests: TopK (sort + head fused)
// ---------------------------------------------------------------------------

func TestBuild_TopK(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | sort -duration | head 2`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
	// Should be the 2 highest durations.
	d0, _ := result[0]["duration"].TryAsFloat()
	d1, _ := result[1]["duration"].TryAsFloat()
	if d0 < d1 {
		t.Errorf("topk not sorted descending: %f < %f", d0, d1)
	}
}

// ---------------------------------------------------------------------------
// Tests: Union
// ---------------------------------------------------------------------------

func TestBuild_Union(t *testing.T) {
	// Union is created via the "union" stage in LynxFlow.
	// Build manually since the parser syntax is complex.
	scan := &logical.Scan{OutputSchema: nil}
	union := &logical.Union{
		Inputs: []logical.Node{scan, scan},
	}
	rows := []map[string]event.Value{
		{"x": intV(1)},
		{"x": intV(2)},
	}
	iter, err := Build(&logical.Plan{Root: union}, BuildOptions{
		Source:    sourceFromRows(rows),
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	// Each branch produces 2 rows -> 4 total.
	if len(result) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: Join (inner/left)
// ---------------------------------------------------------------------------

func TestBuild_Join_Inner(t *testing.T) {
	left := &logical.Scan{OutputSchema: nil}
	right := &logical.Scan{OutputSchema: nil}
	join := &logical.Join{
		Type:  "inner",
		On:    []string{"key"},
		Right: right,
	}
	join.SetChildren([]logical.Node{left})

	leftRows := []map[string]event.Value{
		{"key": strV("a"), "val": intV(1)},
		{"key": strV("b"), "val": intV(2)},
		{"key": strV("c"), "val": intV(3)},
	}
	rightRows := []map[string]event.Value{
		{"key": strV("a"), "extra": strV("x")},
		{"key": strV("c"), "extra": strV("z")},
	}

	callCount := 0
	sourceFunc := func(scan *logical.Scan) (pipeline.Iterator, error) {
		callCount++
		if callCount == 1 {
			return sliceSource(leftRows, 1024), nil
		}
		return sliceSource(rightRows, 1024), nil
	}

	iter, err := Build(&logical.Plan{Root: join}, BuildOptions{
		Source:    sourceFunc,
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	// Inner join on key: a and c match -> 2 rows.
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
}

func TestBuild_Join_Left(t *testing.T) {
	left := &logical.Scan{OutputSchema: nil}
	right := &logical.Scan{OutputSchema: nil}
	join := &logical.Join{
		Type:  "left",
		On:    []string{"key"},
		Right: right,
	}
	join.SetChildren([]logical.Node{left})

	leftRows := []map[string]event.Value{
		{"key": strV("a"), "val": intV(1)},
		{"key": strV("b"), "val": intV(2)},
	}
	rightRows := []map[string]event.Value{
		{"key": strV("a"), "extra": strV("x")},
	}

	callCount := 0
	sourceFunc := func(scan *logical.Scan) (pipeline.Iterator, error) {
		callCount++
		if callCount == 1 {
			return sliceSource(leftRows, 1024), nil
		}
		return sliceSource(rightRows, 1024), nil
	}

	iter, err := Build(&logical.Plan{Root: join}, BuildOptions{
		Source:    sourceFunc,
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	// Left join: all left rows (2), key=a enriched, key=b not.
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
}

func TestBuild_Join_Outer(t *testing.T) {
	left := &logical.Scan{OutputSchema: nil}
	right := &logical.Scan{OutputSchema: nil}
	join := &logical.Join{
		Type:  "outer",
		On:    []string{"key"},
		Right: right,
	}
	join.SetChildren([]logical.Node{left})

	leftRows := []map[string]event.Value{
		{"key": strV("a"), "val": intV(1)},
		{"key": strV("b"), "val": intV(2)},
	}
	rightRows := []map[string]event.Value{
		{"key": strV("a"), "extra": strV("x")},
		{"key": strV("c"), "extra": strV("z")},
	}

	callCount := 0
	sourceFunc := func(scan *logical.Scan) (pipeline.Iterator, error) {
		callCount++
		if callCount == 1 {
			return sliceSource(leftRows, 1024), nil
		}
		return sliceSource(rightRows, 1024), nil
	}

	iter, err := Build(&logical.Plan{Root: join}, BuildOptions{
		Source:    sourceFunc,
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Full outer join: key=a (merged), key=b (left only), key=c (right only) -> 3 rows.
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}

	// Verify all expected keys are present.
	keys := make(map[string]bool, len(result))
	for _, row := range result {
		if v, ok := row["key"]; ok {
			keys[v.String()] = true
		}
	}
	for _, k := range []string{"a", "b", "c"} {
		if !keys[k] {
			t.Errorf("expected key %q in result, not found", k)
		}
	}

	// Verify key=a has both val and extra (non-null: columnar batches pad
	// missing columns with nulls).
	for _, row := range result {
		if v, ok := row["key"]; ok && v.String() == "a" {
			if vv, hasVal := row["val"]; !hasVal || vv.IsNull() {
				t.Error("key=a should have 'val' from left side")
			}
			if ev, hasExtra := row["extra"]; !hasExtra || ev.IsNull() {
				t.Error("key=a should have 'extra' from right side")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Explode
// ---------------------------------------------------------------------------

func TestBuild_Explode(t *testing.T) {
	rows := []map[string]event.Value{
		{"tags": arrV(strV("a"), strV("b"), strV("c")), "id": intV(1)},
		{"tags": arrV(strV("x")), "id": intV(2)},
	}
	result := drain(t, `from * | explode tags`, rows)
	// Row 1 expands to 3, Row 2 expands to 1 -> 4 total.
	if len(result) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: Project (keep/drop/rename)
// ---------------------------------------------------------------------------

func TestBuild_Keep(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | keep level, status`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	for i, r := range result {
		if _, ok := r["level"]; !ok {
			t.Errorf("row %d: missing 'level'", i)
		}
		if _, ok := r["status"]; !ok {
			t.Errorf("row %d: missing 'status'", i)
		}
		// duration and host should be dropped.
		if _, ok := r["duration"]; ok {
			t.Errorf("row %d: unexpected 'duration' field", i)
		}
	}
}

func TestBuild_Drop(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | drop duration`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	for i, r := range result {
		if _, ok := r["duration"]; ok {
			t.Errorf("row %d: unexpected 'duration' field", i)
		}
		if _, ok := r["level"]; !ok {
			t.Errorf("row %d: missing 'level'", i)
		}
	}
}

func TestBuild_Rename(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | rename level as severity`, rows)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(result))
	}
	for i, r := range result {
		if _, ok := r["severity"]; !ok {
			t.Errorf("row %d: missing 'severity'", i)
		}
		if _, ok := r["level"]; ok {
			t.Errorf("row %d: unexpected 'level' after rename", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Describe
// ---------------------------------------------------------------------------

func TestBuild_Describe(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | describe`, rows)
	if len(result) == 0 {
		t.Fatal("expected at least 1 describe output row")
	}
	// Each row should have field, type, coverage, distinct_est, top_values.
	for i, r := range result {
		if _, ok := r["field"]; !ok {
			t.Errorf("row %d: missing 'field'", i)
		}
		if _, ok := r["type"]; !ok {
			t.Errorf("row %d: missing 'type'", i)
		}
		if _, ok := r["coverage"]; !ok {
			t.Errorf("row %d: missing 'coverage'", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Parse (json from raw text)
// ---------------------------------------------------------------------------

func TestBuild_Parse_JSON(t *testing.T) {
	rows := []map[string]event.Value{
		{"_raw": strV(`{"name":"alice","age":30}`)},
		{"_raw": strV(`{"name":"bob","age":25}`)},
	}
	result := drain(t, `from * | parse json`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(result))
	}
	for i, r := range result {
		if _, ok := r["name"]; !ok {
			t.Errorf("row %d: missing 'name' after parse json", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Empty
// ---------------------------------------------------------------------------

func TestBuild_Empty(t *testing.T) {
	plan := &logical.Plan{Root: &logical.Empty{}}
	iter, err := Build(plan, BuildOptions{BatchSize: 1024})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(result))
	}
}

func TestBuild_NilPlan(t *testing.T) {
	iter, err := Build(nil, BuildOptions{BatchSize: 1024})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: Error cases
// ---------------------------------------------------------------------------

func TestBuild_Materialize_NotYetImplemented(t *testing.T) {
	plan := &logical.Plan{
		Root: &logical.Materialize{
			Name: "test_mv",
		},
	}
	plan.Root.(*logical.Materialize).SetChildren([]logical.Node{&logical.Scan{}})
	_, err := Build(plan, BuildOptions{
		Source:    sourceFromRows(nil),
		BatchSize: 1024,
	})
	if err == nil {
		t.Fatal("expected error for Materialize, got nil")
	}
	if _, ok := err.(*NotYetImplementedError); !ok {
		t.Fatalf("expected NotYetImplementedError, got %T: %v", err, err)
	}
}

func TestBuild_Tee_Disabled(t *testing.T) {
	sinkPath := filepath.Join(t.TempDir(), "tee.out")
	plan := &logical.Plan{
		Root: &logical.Tee{Sink: sinkPath},
	}
	plan.Root.(*logical.Tee).SetChildren([]logical.Node{&logical.Scan{}})
	_, err := Build(plan, BuildOptions{
		Source:     sourceFromRows(nil),
		BatchSize:  1024,
		TeeEnabled: false,
	})
	if err == nil {
		t.Fatal("expected error for Tee with TeeEnabled=false, got nil")
	}
	if !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("expected 'not enabled' error, got: %v", err)
	}
}

func TestBuild_Tee_RelativePath(t *testing.T) {
	plan := &logical.Plan{
		Root: &logical.Tee{Sink: "relative/path.out"},
	}
	plan.Root.(*logical.Tee).SetChildren([]logical.Node{&logical.Scan{}})
	_, err := Build(plan, BuildOptions{
		Source:     sourceFromRows(nil),
		BatchSize:  1024,
		TeeEnabled: true,
	})
	if err == nil {
		t.Fatal("expected error for relative tee path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-path error, got: %v", err)
	}
}

func TestBuild_Tee_Enabled(t *testing.T) {
	sinkPath := filepath.Join(t.TempDir(), "tee.out")
	rows := sampleRows()

	scan := &logical.Scan{OutputSchema: nil}
	tee := &logical.Tee{Sink: sinkPath}
	tee.SetChildren([]logical.Node{scan})

	iter, err := Build(&logical.Plan{Root: tee}, BuildOptions{
		Source:     sourceFromRows(rows),
		BatchSize:  1024,
		TeeEnabled: true,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Tee is passthrough — all rows must appear in the output.
	if len(result) != len(rows) {
		t.Fatalf("expected %d rows, got %d", len(rows), len(result))
	}

	// The sink file must contain NDJSON with one line per row.
	data, err := os.ReadFile(sinkPath)
	if err != nil {
		t.Fatalf("read sink file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != len(rows) {
		t.Fatalf("expected %d lines in sink file, got %d", len(rows), len(lines))
	}
}

// ---------------------------------------------------------------------------
// Tests: Full end-to-end via parser
// ---------------------------------------------------------------------------

func TestBuild_E2E_WhereExtendStats(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | where status >= 200 | extend is_error = status >= 500 | stats count() as total`, rows)
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	total, _ := result[0]["total"].TryAsInt()
	if total != 5 {
		t.Errorf("expected total=5, got %d", total)
	}
}

func TestBuild_E2E_SortHeadTopK(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | sort -status | head 3`, rows)
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}
}

func TestBuild_E2E_StatsCountByLevel(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | stats count() as cnt by level | sort -cnt`, rows)
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}
	// info: 2, error: 2, warn: 1 -> sorted desc: info/error first, then warn.
	lastCnt, _ := result[len(result)-1]["cnt"].TryAsInt()
	if lastCnt != 1 {
		t.Errorf("expected last cnt=1, got %d", lastCnt)
	}
}

func TestBuild_E2E_DedupSortHead(t *testing.T) {
	rows := sampleRows()
	result := drain(t, `from * | dedup host | sort host | head 10`, rows)
	if len(result) != 2 {
		t.Fatalf("expected 2 rows (unique hosts), got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: CondProgram wiring on AggFunc (direct build, not via parser)
// ---------------------------------------------------------------------------

func TestBuild_CondProgram_AggFunc(t *testing.T) {
	// Build plan manually to test CondProgram.
	scan := &logical.Scan{OutputSchema: nil}
	agg := &logical.Aggregate{
		Aggs: []logical.Agg{
			{
				Func: &lfast.Call{
					Callee: "count",
					Args:   []lfast.Expr{&lfast.Ident{Name: "status"}},
				},
				WhereCond: &lfast.Binary{
					Op:   lfast.OpGtEq,
					Left: &lfast.Ident{Name: "status"},
					Right: &lfast.Literal{
						Kind:  lfast.LitInt,
						Value: int64(500),
						Raw:   "500",
					},
				},
				Alias: "error_count",
			},
		},
	}
	agg.SetChildren([]logical.Node{scan})

	rows := sampleRows()
	iter, err := Build(&logical.Plan{Root: agg}, BuildOptions{
		Source:    sourceFromRows(rows),
		BatchSize: 1024,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	result, err := pipeline.CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 row, got %d", len(result))
	}
	c, _ := result[0]["error_count"].TryAsInt()
	if c != 2 {
		t.Errorf("expected error_count=2 (status 500 and 503), got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Tests: Agg name mapping table coverage
// ---------------------------------------------------------------------------

func TestAggNameMapping(t *testing.T) {
	expected := map[string]string{
		"count":  "count",
		"sum":    "sum",
		"avg":    "avg",
		"min":    "min",
		"max":    "max",
		"dc":     "dc",
		"estdc":  "dc",
		"p50":    "perc50",
		"p95":    "perc95",
		"p99":    "perc99",
		"stdev":  "stdev",
		"values": "values",
		"first":  "first",
		"last":   "last",
		"mode":   "mode",
		"rate":   "rate",
	}
	for input, want := range expected {
		got, ok := aggNameMapping[input]
		if !ok {
			t.Errorf("missing mapping for %q", input)
			continue
		}
		if got != want {
			t.Errorf("aggNameMapping[%q] = %q, want %q", input, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: exprToDuration
// ---------------------------------------------------------------------------

func TestExprToDuration(t *testing.T) {
	tests := []struct {
		expr lfast.Expr
		want time.Duration
	}{
		{
			expr: &lfast.Literal{Kind: lfast.LitDuration, Value: 5 * time.Minute, Raw: "5m"},
			want: 5 * time.Minute,
		},
	}
	for _, tt := range tests {
		got, err := exprToDuration(tt.expr)
		if err != nil {
			t.Errorf("exprToDuration(%v): %v", tt.expr, err)
			continue
		}
		if got != tt.want {
			t.Errorf("exprToDuration(%v) = %v, want %v", tt.expr, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests: Small batch sizes
// ---------------------------------------------------------------------------

func TestBuild_SmallBatchSize(t *testing.T) {
	rows := sampleRows()
	result := drainWithBatchSize(t, `from * | where status >= 200`, rows, 2)
	if len(result) != 5 {
		t.Fatalf("expected 5 rows with batch size 2, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: DescribeSummaryIterator directly
// ---------------------------------------------------------------------------

func TestDescribeSummaryIterator(t *testing.T) {
	rows := sampleRows()
	source := sliceSource(rows, 1024)
	iter := NewDescribeSummaryIterator(source, 1024)

	if err := iter.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	batch, err := iter.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if batch == nil {
		t.Fatal("expected non-nil batch from describe")
	}
	if batch.Len == 0 {
		t.Fatal("expected at least 1 row")
	}

	// Verify expected columns.
	for _, col := range []string{"field", "type", "coverage", "distinct_est", "top_values"} {
		if _, ok := batch.Columns[col]; !ok {
			t.Errorf("missing column %q in describe output", col)
		}
	}

	// Second call should return nil (exhausted).
	batch2, err := iter.Next(context.Background())
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if batch2 != nil {
		t.Errorf("expected nil from second Next, got %d rows", batch2.Len)
	}

	if err := iter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tests: Schema on DescribeSummaryIterator
// ---------------------------------------------------------------------------

func TestDescribeSummaryIterator_Schema(t *testing.T) {
	source := sliceSource(nil, 1024)
	iter := NewDescribeSummaryIterator(source, 1024)
	schema := iter.Schema()
	if len(schema) != 5 {
		t.Fatalf("expected 5 schema fields, got %d", len(schema))
	}
	names := make(map[string]bool)
	for _, f := range schema {
		names[f.Name] = true
	}
	for _, want := range []string{"field", "type", "coverage", "distinct_est", "top_values"} {
		if !names[want] {
			t.Errorf("missing schema field %q", want)
		}
	}
}
