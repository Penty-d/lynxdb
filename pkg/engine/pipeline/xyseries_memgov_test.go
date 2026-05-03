package pipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestXYSeriesAccountsPivotState(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": event.StringValue("row1"), "y": event.StringValue("col1"), "v": event.IntValue(1)},
		{"x": event.StringValue("row1"), "y": event.StringValue("col2"), "v": event.IntValue(2)},
	}
	acct := memgov.NewTestBudget("xyseries", 0).NewAccount("xyseries")
	iter := NewXYSeriesIteratorWithBudget(NewRowScanIterator(rows, 2), "x", "y", "v", 10, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: got %d, want 1", len(got))
	}
	if got[0]["col2"].AsInt() != 2 {
		t.Fatalf("col2: got %d, want 2", got[0]["col2"].AsInt())
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected xyseries account to track pivot state")
	}
}

func TestXYSeriesBudgetExceeded(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": event.StringValue("row1"), "y": event.StringValue("col1"), "v": event.IntValue(1)},
	}
	acct := memgov.NewTestBudget("xyseries", 64).NewAccount("xyseries")
	iter := NewXYSeriesIteratorWithBudget(NewRowScanIterator(rows, 1), "x", "y", "v", 10, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected xyseries pivot state to exceed tiny budget")
	}
}

func TestXYSeriesSpillsHighCardinalityPivot(t *testing.T) {
	rows := make([]map[string]event.Value, 0, 100)
	for i := 0; i < 50; i++ {
		rows = append(rows,
			map[string]event.Value{"x": event.StringValue(fmt.Sprintf("row-%03d", i)), "y": event.StringValue("col-a"), "v": event.IntValue(int64(i))},
			map[string]event.Value{"x": event.StringValue(fmt.Sprintf("row-%03d", i)), "y": event.StringValue("col-b"), "v": event.IntValue(int64(i + 100))},
		)
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("create spill manager: %v", err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("xyseries", 4*1024).NewAccount("xyseries")
	iter := NewXYSeriesIteratorWithSpill(NewRowScanIterator(rows, 10), "x", "y", "v", 10, acct, mgr)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("rows: got %d, want 50", len(got))
	}
	if got[0]["x"].String() != "row-000" || got[0]["col-a"].AsInt() != 0 || got[0]["col-b"].AsInt() != 100 {
		t.Fatalf("first row mismatch: %+v", got[0])
	}
	if got[49]["x"].String() != "row-049" || got[49]["col-a"].AsInt() != 49 || got[49]["col-b"].AsInt() != 149 {
		t.Fatalf("last row mismatch: %+v", got[49])
	}
	rs := iter.ResourceStats()
	if rs.SpilledRows == 0 {
		t.Fatal("expected xyseries spill path to write rows")
	}
	if rs.SpillBytes == 0 {
		t.Fatal("expected xyseries spill bytes to be reported")
	}
}
