package pipeline

import (
	"context"
	"fmt"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func outliersMemgovRows(n int) []map[string]event.Value {
	rows := make([]map[string]event.Value, 0, n+1)
	for i := 0; i < n; i++ {
		rows = append(rows, map[string]event.Value{
			"host":  event.StringValue(fmt.Sprintf("host-%03d", i)),
			"value": event.FloatValue(100 + float64(i%10)),
		})
	}
	// One clear outlier.
	rows = append(rows, map[string]event.Value{
		"host":  event.StringValue("host-outlier"),
		"value": event.FloatValue(100000),
	})

	return rows
}

func TestOutliersAccountsRowBuffer(t *testing.T) {
	acct := memgov.NewTestBudget("outliers", 0).NewAccount("outliers")
	iter := NewOutliersIteratorWithBudget(
		NewRowScanIterator(outliersMemgovRows(50), 16), "value", "iqr", 1.5, acct, nil)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 51 {
		t.Fatalf("rows: got %d, want 51", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected outliers account to track buffered rows")
	}
}

func TestOutliersSpillPreservesAnnotations(t *testing.T) {
	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("create spill manager: %v", err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("outliers", 4*1024).NewAccount("outliers")
	iter := NewOutliersIteratorWithBudget(
		NewRowScanIterator(outliersMemgovRows(500), 32), "value", "iqr", 1.5, acct, mgr)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 501 {
		t.Fatalf("rows: got %d, want 501", len(got))
	}
	flagged := 0
	for _, row := range got {
		if row["_outlier"].AsBool() {
			flagged++
			if row["host"].String() != "host-outlier" {
				t.Fatalf("unexpected row flagged as outlier: %+v", row)
			}
		}
	}
	if flagged != 1 {
		t.Fatalf("flagged outliers: got %d, want 1", flagged)
	}
	rs := iter.ResourceStats()
	if rs.SpilledRows == 0 {
		t.Fatal("expected outliers spill path to write rows")
	}
	if rs.SpillBytes == 0 {
		t.Fatal("expected outliers spill bytes to be reported")
	}
}
