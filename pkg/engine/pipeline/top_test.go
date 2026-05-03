package pipeline

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestTopIterator_SortStability(t *testing.T) {
	// Create rows where multiple values have the same count.
	// "banana", "apple", "cherry" each appear twice.
	// With deterministic sort, equal-count entries should be ordered alphabetically.
	rows := []map[string]event.Value{
		{"fruit": event.StringValue("banana")},
		{"fruit": event.StringValue("cherry")},
		{"fruit": event.StringValue("apple")},
		{"fruit": event.StringValue("banana")},
		{"fruit": event.StringValue("cherry")},
		{"fruit": event.StringValue("apple")},
	}

	child := NewRowScanIterator(rows, 64)
	top := NewTopIterator(child, "fruit", "", 3, false, 64)

	ctx := context.Background()
	if err := top.Init(ctx); err != nil {
		t.Fatal(err)
	}

	result, err := CollectAll(ctx, top)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}

	// All have count=2, so should be sorted alphabetically: apple, banana, cherry.
	expected := []string{"apple", "banana", "cherry"}
	for i, row := range result {
		got := row["fruit"].String()
		if got != expected[i] {
			t.Errorf("row %d: expected %q, got %q", i, expected[i], got)
		}
	}
}

func TestTopIterator_SortStabilityRare(t *testing.T) {
	// Same as above but with ascending=true (rare command).
	rows := []map[string]event.Value{
		{"fruit": event.StringValue("banana")},
		{"fruit": event.StringValue("cherry")},
		{"fruit": event.StringValue("apple")},
		{"fruit": event.StringValue("banana")},
		{"fruit": event.StringValue("cherry")},
		{"fruit": event.StringValue("apple")},
	}

	child := NewRowScanIterator(rows, 64)
	top := NewTopIterator(child, "fruit", "", 3, true, 64)

	ctx := context.Background()
	if err := top.Init(ctx); err != nil {
		t.Fatal(err)
	}

	result, err := CollectAll(ctx, top)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}

	// All have count=2, ascending order — still alphabetical for ties.
	expected := []string{"apple", "banana", "cherry"}
	for i, row := range result {
		got := row["fruit"].String()
		if got != expected[i] {
			t.Errorf("row %d: expected %q, got %q", i, expected[i], got)
		}
	}
}

func TestTopIterator_SchemaWithoutByField(t *testing.T) {
	child := NewRowScanIterator(nil, 64)
	top := NewTopIterator(child, "status", "", 10, false, 64)

	schema := top.Schema()
	if len(schema) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(schema))
	}

	expected := []string{"status", "count", "percent"}
	for i, f := range schema {
		if f.Name != expected[i] {
			t.Errorf("schema[%d] = %q, want %q", i, f.Name, expected[i])
		}
	}
}

func TestTopIterator_SchemaWithByField(t *testing.T) {
	child := NewRowScanIterator(nil, 64)
	top := NewTopIterator(child, "status", "host", 10, false, 64)

	schema := top.Schema()
	if len(schema) != 4 {
		t.Fatalf("expected 4 fields (including byField), got %d", len(schema))
	}

	expected := []string{"status", "count", "percent", "host"}
	for i, f := range schema {
		if f.Name != expected[i] {
			t.Errorf("schema[%d] = %q, want %q", i, f.Name, expected[i])
		}
	}
}

func TestTopIterator_DifferentCounts(t *testing.T) {
	// Verify primary sort by count still works with secondary alphabetical.
	rows := []map[string]event.Value{
		{"method": event.StringValue("GET")},
		{"method": event.StringValue("GET")},
		{"method": event.StringValue("GET")},
		{"method": event.StringValue("POST")},
		{"method": event.StringValue("POST")},
		{"method": event.StringValue("DELETE")},
	}

	child := NewRowScanIterator(rows, 64)
	top := NewTopIterator(child, "method", "", 3, false, 64)

	ctx := context.Background()
	if err := top.Init(ctx); err != nil {
		t.Fatal(err)
	}

	result, err := CollectAll(ctx, top)
	if err != nil {
		t.Fatal(err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(result))
	}

	// GET=3, POST=2, DELETE=1 — sorted by count desc.
	expected := []string{"GET", "POST", "DELETE"}
	for i, row := range result {
		got := row["method"].String()
		if got != expected[i] {
			t.Errorf("row %d: expected %q, got %q", i, expected[i], got)
		}
	}
}

func TestTopIterator_AccountsLargeCounterKeys(t *testing.T) {
	rows := []map[string]event.Value{
		{"method": event.StringValue(strings.Repeat("x", 4096))},
	}
	acct := memgov.NewTestBudget("top", 2048).NewAccount("top")
	top := NewTopIteratorWithBudget(NewRowScanIterator(rows, 1), "method", "", 10, false, 10, acct)

	if _, err := CollectAll(context.Background(), top); err == nil {
		t.Fatal("expected large top counter key to exceed budget")
	}
}

func TestTopIterator_SpillsHighCardinalityCounters(t *testing.T) {
	rows := make([]map[string]event.Value, 0, 105)
	for i := 0; i < 5; i++ {
		rows = append(rows, map[string]event.Value{"method": event.StringValue("hot")})
	}
	for i := 0; i < 100; i++ {
		rows = append(rows, map[string]event.Value{"method": event.StringValue(fmt.Sprintf("cold-%03d", i))})
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("create spill manager: %v", err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("top", 4*1024).NewAccount("top")
	top := NewTopIteratorWithSpill(NewRowScanIterator(rows, 16), "method", "", 3, false, 10, acct, mgr)

	got, err := CollectAll(context.Background(), top)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: got %d, want 3", len(got))
	}
	if got[0]["method"].String() != "hot" {
		t.Fatalf("first method = %q, want hot", got[0]["method"])
	}
	if got[0]["count"].AsInt() != 5 {
		t.Fatalf("hot count = %d, want 5", got[0]["count"].AsInt())
	}
	wantPercent := float64(5) / float64(len(rows)) * 100
	if math.Abs(got[0]["percent"].AsFloat()-wantPercent) > 0.0001 {
		t.Fatalf("hot percent = %f, want %f", got[0]["percent"].AsFloat(), wantPercent)
	}
	if top.ResourceStats().SpilledRows == 0 {
		t.Fatal("expected top spill path to write rows")
	}
}
