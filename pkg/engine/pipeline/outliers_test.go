package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func outlierRow(v int64, rawSize int) map[string]event.Value {
	return map[string]event.Value{
		"value": event.IntValue(v),
		"_raw":  event.StringValue(strings.Repeat("x", rawSize)),
	}
}

func TestOutliersZScoreMarksLargeOutlier(t *testing.T) {
	rows := []map[string]event.Value{
		outlierRow(10, 0),
		outlierRow(11, 0),
		outlierRow(12, 0),
		outlierRow(1000, 0),
	}
	iter := NewOutliersIterator(NewRowScanIterator(rows, 2), "value", "zscore", 1.0)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("rows: got %d, want 4", len(got))
	}
	if !got[3]["_outlier"].AsBool() {
		t.Fatal("expected largest value to be marked as outlier")
	}
}

func TestOutliersBudgetWithoutSpillFailsOnLargeRows(t *testing.T) {
	rows := []map[string]event.Value{outlierRow(1, 4096)}
	acct := memgov.NewTestBudget("outliers", 2048).NewAccount("outliers")
	iter := NewOutliersIteratorWithBudget(NewRowScanIterator(rows, 1), "value", "iqr", 1.5, acct, nil)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected large outliers row to exceed budget without spill")
	}
}

func TestOutliersSpillsRowsUnderBudget(t *testing.T) {
	rows := make([]map[string]event.Value, 100)
	for i := range rows {
		rows[i] = outlierRow(int64(i), 512)
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("outliers", 16<<10).NewAccount("outliers")
	iter := NewOutliersIteratorWithBudget(NewRowScanIterator(rows, 10), "value", "iqr", 1.5, acct, mgr)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("rows: got %d, want %d", len(got), len(rows))
	}
	if !iter.spilled {
		t.Fatal("expected outliers to spill row buffer")
	}
	if iter.spilledRows == 0 {
		t.Fatal("expected spilled row count to be tracked")
	}
}
