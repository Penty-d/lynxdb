package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestTransactionAccountsGroupAndOutputState(t *testing.T) {
	rows := []map[string]event.Value{
		{"session": event.StringValue("s1"), "_raw": event.StringValue("a"), "_time": event.TimestampValue(time.Unix(1, 0))},
		{"session": event.StringValue("s1"), "_raw": event.StringValue("b"), "_time": event.TimestampValue(time.Unix(2, 0))},
	}
	acct := memgov.NewTestBudget("transaction", 0).NewAccount("transaction")
	iter := NewTransactionIteratorWithBudget(NewRowScanIterator(rows, 1), "session", 0, "", "", 10, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: got %d, want 1", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected transaction account to track state")
	}
}

func TestTransactionBudgetExceededOnOutputState(t *testing.T) {
	rows := []map[string]event.Value{
		{"session": event.StringValue("s1"), "_raw": event.StringValue("a"), "_time": event.TimestampValue(time.Unix(1, 0))},
	}
	acct := memgov.NewTestBudget("transaction", 128).NewAccount("transaction")
	iter := NewTransactionIteratorWithBudget(NewRowScanIterator(rows, 1), "session", 0, "", "", 10, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected tiny transaction budget to fail")
	}
}

func TestTransactionSpillsPartitionedGroups(t *testing.T) {
	rows := make([]map[string]event.Value, 0, 240)
	for i := 0; i < 120; i++ {
		session := fmt.Sprintf("s-%03d", i)
		rows = append(rows,
			map[string]event.Value{
				"session": event.StringValue(session),
				"_raw":    event.StringValue("start-" + session),
				"_time":   event.TimestampValue(time.Unix(0, 0)),
			},
			map[string]event.Value{
				"session": event.StringValue(session),
				"_raw":    event.StringValue("end-" + session),
				"_time":   event.TimestampValue(time.Unix(100, 0)),
			},
		)
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("create spill manager: %v", err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("transaction", 12*1024).NewAccount("transaction")
	iter := NewTransactionIteratorWithSpill(
		NewRowScanIterator(rows, 16),
		"session",
		time.Second,
		"",
		"",
		10,
		acct,
		mgr,
	)
	defer iter.Close()

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("rows: got %d, want 0 because every group exceeds maxspan", len(got))
	}
	rs := iter.ResourceStats()
	if rs.SpilledRows == 0 {
		t.Fatal("expected transaction spill path to write rows")
	}
	if rs.SpillBytes == 0 {
		t.Fatal("expected transaction spill bytes to be reported")
	}
}
