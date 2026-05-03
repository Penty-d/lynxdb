package pipeline

import (
	"context"
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
