package pipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

// makeJoinRows creates n rows with a "key" field (mod numKeys for key diversity)
// and a "side" field identifying which side they belong to.
func makeJoinRows(n, numKeys int, side string) []map[string]event.Value {
	rows := make([]map[string]event.Value, n)
	for i := 0; i < n; i++ {
		rows[i] = map[string]event.Value{
			"key":  event.StringValue(fmt.Sprintf("k%d", i%numKeys)),
			"side": event.StringValue(side),
			"idx":  event.IntValue(int64(i)),
		}
	}

	return rows
}

func TestJoinInMemoryFastPath(t *testing.T) {
	// Right side fits in budget — no grace fallback.
	leftRows := makeJoinRows(100, 10, "left")
	rightRows := makeJoinRows(50, 10, "right")

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	// Large budget — no spill needed.
	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "inner", acct, mgr)

	// 100 left rows, each matching 5 right rows (50 right / 10 keys = 5 per key).
	expected := 100 * 5
	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, expected*10)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != expected {
		t.Fatalf("expected %d joined rows, got %d", expected, len(result))
	}

	// No spill files.
	count, _ := mgr.Stats()
	if count != 0 {
		t.Fatalf("expected 0 spill files, got %d", count)
	}
}

func TestJoinGraceHashJoinFallback(t *testing.T) {
	// Right side exceeds budget — must fall back to grace hash join.
	numKeys := 20
	leftRows := makeJoinRows(200, numKeys, "left")
	rightRows := makeJoinRows(100, numKeys, "right")

	left := NewRowScanIterator(leftRows, 32)
	right := NewRowScanIterator(rightRows, 32)

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	// Budget: 8KB fits ~32 rows at 256 bytes/row, so 100 right rows will
	// trigger grace fallback during build. After partitioning into 64 buckets,
	// each partition has ~1-2 rows which fits within 8KB easily.
	acct := memgov.NewTestBudget("test", 8*1024).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "inner", acct, mgr)

	// Brute-force expected: each left row's key matches 5 right rows (100/20).
	expected := 200 * 5
	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, expected*10)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != expected {
		t.Fatalf("expected %d joined rows, got %d", expected, len(result))
	}

	// Verify that all result rows have both "side" fields merged.
	for i, row := range result {
		if _, ok := row["key"]; !ok {
			t.Fatalf("row %d: missing key field", i)
		}
	}

	// Verify ResourceReporter reports spilled rows.
	rs := iter.ResourceStats()
	if rs.SpilledRows == 0 {
		t.Fatal("expected SpilledRows > 0 for grace join")
	}
	if rs.SpillBytes == 0 {
		t.Fatal("expected SpillBytes > 0 for grace join")
	}
}

func TestJoinGraceHashJoinLeftOuter(t *testing.T) {
	numKeys := 10
	// Left rows have keys 0-9, right rows only have keys 0-4.
	leftRows := makeJoinRows(100, numKeys, "left")
	rightRows := makeJoinRows(25, 5, "right") // only keys k0..k4

	left := NewRowScanIterator(leftRows, 32)
	right := NewRowScanIterator(rightRows, 32)

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	// Budget: small enough to trigger grace on 25 right rows, large enough per partition.
	acct := memgov.NewTestBudget("test", 4*1024).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "left", acct, mgr)

	// Left rows k0..k4 (50 rows) match 5 right rows each = 250.
	// Left rows k5..k9 (50 rows) have no match = 50 passed through.
	expected := 50*5 + 50
	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, expected*10)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != expected {
		t.Fatalf("expected %d joined rows, got %d", expected, len(result))
	}
}

func TestJoinGraceHashJoinInner(t *testing.T) {
	numKeys := 10
	// Right side only has keys k0..k4.
	leftRows := makeJoinRows(100, numKeys, "left")
	rightRows := makeJoinRows(25, 5, "right")

	left := NewRowScanIterator(leftRows, 32)
	right := NewRowScanIterator(rightRows, 32)

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("test", 4*1024).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "inner", acct, mgr)

	// Only k0..k4 match: 50 left rows x 5 right rows each = 250.
	expected := 50 * 5
	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, expected*10)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != expected {
		t.Fatalf("expected %d joined rows, got %d", expected, len(result))
	}
}

func TestJoinSpillFileCleanup(t *testing.T) {
	leftRows := makeJoinRows(100, 10, "left")
	rightRows := makeJoinRows(100, 10, "right")

	left := NewRowScanIterator(leftRows, 32)
	right := NewRowScanIterator(rightRows, 32)

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	// Budget small enough to force grace, large enough per partition.
	acct := memgov.NewTestBudget("test", 8*1024).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "inner", acct, mgr)

	// 100 left rows x 10 right rows per key = 1000 max.
	ctx := context.Background()
	_, err = collectAllCapped(t, ctx, iter, 10000)
	if err != nil {
		t.Fatal(err)
	}

	// After Close(), all spill files should be released.
	count, _ := mgr.Stats()
	if count != 0 {
		t.Fatalf("expected 0 tracked files after Close(), got %d", count)
	}
}

func TestJoinEmptyRightSide(t *testing.T) {
	leftRows := makeJoinRows(50, 5, "left")
	var rightRows []map[string]event.Value

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "inner", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 500)
	if err != nil {
		t.Fatal(err)
	}

	// Inner join with empty right = 0 results.
	if len(result) != 0 {
		t.Fatalf("expected 0 rows for inner join with empty right, got %d", len(result))
	}
}

func TestJoinEmptyRightSideLeftJoin(t *testing.T) {
	leftRows := makeJoinRows(50, 5, "left")
	var rightRows []map[string]event.Value

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "left", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 500)
	if err != nil {
		t.Fatal(err)
	}

	// Left join with empty right = all left rows returned.
	if len(result) != 50 {
		t.Fatalf("expected 50 rows for left join with empty right, got %d", len(result))
	}
}

func TestJoinEmptyLeftSide(t *testing.T) {
	var leftRows []map[string]event.Value
	rightRows := makeJoinRows(50, 5, "right")

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "inner", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 500)
	if err != nil {
		t.Fatal(err)
	}

	// Inner join with empty left = 0 results.
	if len(result) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Tests: Full outer join
// ---------------------------------------------------------------------------

// hasNonNull reports whether the row carries a non-null value for the field.
// Columnar batches pad missing columns with null values, so "absent" surfaces
// as either a missing key or a present null.
func hasNonNull(row map[string]event.Value, field string) bool {
	v, ok := row[field]

	return ok && !v.IsNull()
}

func TestJoinOuterInMemory(t *testing.T) {
	// Left: keys a, b. Right: keys a, c.
	// Expected: key=a (merged), key=b (left only), key=c (right only).
	leftRows := []map[string]event.Value{
		{"key": event.StringValue("a"), "lval": event.IntValue(1)},
		{"key": event.StringValue("b"), "lval": event.IntValue(2)},
	}
	rightRows := []map[string]event.Value{
		{"key": event.StringValue("a"), "rval": event.IntValue(10)},
		{"key": event.StringValue("c"), "rval": event.IntValue(30)},
	}

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "outer", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 100)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}

	// Verify all keys present.
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

	// Verify per-key field presence. Columnar batches pad missing columns
	// with nulls, so absence is asserted as null-or-missing.
	for _, row := range result {
		v, ok := row["key"]
		if !ok {
			continue
		}
		switch v.String() {
		case "a":
			if !hasNonNull(row, "lval") {
				t.Error("key=a should have 'lval' from left side")
			}
			if !hasNonNull(row, "rval") {
				t.Error("key=a should have 'rval' from right side")
			}
		case "b":
			if !hasNonNull(row, "lval") {
				t.Error("key=b should have 'lval' from left side")
			}
			if hasNonNull(row, "rval") {
				t.Error("key=b should NOT have a non-null 'rval'")
			}
		case "c":
			if hasNonNull(row, "lval") {
				t.Error("key=c should NOT have a non-null 'lval'")
			}
			if !hasNonNull(row, "rval") {
				t.Error("key=c should have 'rval' from right side")
			}
		}
	}
}

func TestJoinOuterEmptyLeft(t *testing.T) {
	// Empty left, right has keys a, b.
	// Full outer join should emit all right rows.
	var leftRows []map[string]event.Value
	rightRows := []map[string]event.Value{
		{"key": event.StringValue("a"), "rval": event.IntValue(10)},
		{"key": event.StringValue("b"), "rval": event.IntValue(20)},
	}

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "outer", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 100)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 rows (all right), got %d", len(result))
	}
}

func TestJoinOuterEmptyRight(t *testing.T) {
	// Left has keys a, b; right is empty.
	// Full outer join should emit all left rows.
	leftRows := []map[string]event.Value{
		{"key": event.StringValue("a"), "lval": event.IntValue(1)},
		{"key": event.StringValue("b"), "lval": event.IntValue(2)},
	}
	var rightRows []map[string]event.Value

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "outer", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 100)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 rows (all left), got %d", len(result))
	}
}

func TestJoinOuterMultipleRightPerKey(t *testing.T) {
	// Left: key=a. Right: key=a (two rows), key=b.
	// Expected: 2 merged rows for key=a, 1 right-only row for key=b.
	leftRows := []map[string]event.Value{
		{"key": event.StringValue("a"), "lval": event.IntValue(1)},
	}
	rightRows := []map[string]event.Value{
		{"key": event.StringValue("a"), "rval": event.IntValue(10)},
		{"key": event.StringValue("a"), "rval": event.IntValue(11)},
		{"key": event.StringValue("b"), "rval": event.IntValue(20)},
	}

	left := NewRowScanIterator(leftRows, DefaultBatchSize)
	right := NewRowScanIterator(rightRows, DefaultBatchSize)

	acct := memgov.NewTestBudget("test", 1<<30).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "outer", acct, nil)

	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, 100)
	if err != nil {
		t.Fatal(err)
	}

	// 2 merged (key=a x2) + 1 right-only (key=b) = 3.
	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}
}

func TestJoinOuterGraceHashJoin(t *testing.T) {
	numKeys := 10
	// Left rows have keys 0-9, right rows only have keys 0-4 plus keys 10-14.
	leftRows := makeJoinRows(100, numKeys, "left")
	// Right rows: keys k0..k4 (matched) and k10..k14 (unmatched).
	rightRows := make([]map[string]event.Value, 0, 50)
	for i := 0; i < 25; i++ {
		rightRows = append(rightRows, map[string]event.Value{
			"key":  event.StringValue(fmt.Sprintf("k%d", i%5)),
			"side": event.StringValue("right"),
			"idx":  event.IntValue(int64(i)),
		})
	}
	for i := 0; i < 25; i++ {
		rightRows = append(rightRows, map[string]event.Value{
			"key":  event.StringValue(fmt.Sprintf("k%d", 10+i%5)),
			"side": event.StringValue("right"),
			"idx":  event.IntValue(int64(25 + i)),
		})
	}

	left := NewRowScanIterator(leftRows, 32)
	right := NewRowScanIterator(rightRows, 32)

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	// Budget: small enough to trigger grace on 50 right rows.
	acct := memgov.NewTestBudget("test", 4*1024).NewAccount("join")
	iter := NewJoinIteratorWithSpill(left, right, "key", "outer", acct, mgr)

	// Left rows k0..k4 (50 rows) matched by right (5 right rows each) = 250 merged.
	// Left rows k5..k9 (50 rows) have no match = 50 left-only.
	// Right rows k10..k14 (25 rows) have no match = 25 right-only.
	expected := 50*5 + 50 + 25
	ctx := context.Background()
	result, err := collectAllCapped(t, ctx, iter, expected*10)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != expected {
		t.Fatalf("expected %d rows, got %d", expected, len(result))
	}
}

func TestHashPartitionDistribution(t *testing.T) {
	numPartitions := 64
	numKeys := 10000
	buckets := make([]int, numPartitions)

	for i := 0; i < numKeys; i++ {
		key := fmt.Sprintf("key-%d", i)
		p := hashPartition(key, numPartitions)
		if p < 0 || p >= numPartitions {
			t.Fatalf("partition %d out of range [0, %d)", p, numPartitions)
		}
		buckets[p]++
	}

	// Check that no bucket is empty — with 10K keys and 64 buckets,
	// each should have ~156 keys. Allow 10-500 as a generous range.
	for i, count := range buckets {
		if count == 0 {
			t.Fatalf("partition %d is empty with %d keys", i, numKeys)
		}
		if count > 500 {
			t.Fatalf("partition %d has %d keys — too skewed", i, count)
		}
	}
}
