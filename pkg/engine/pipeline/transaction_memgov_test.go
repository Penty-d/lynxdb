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

	acct := memgov.NewTestBudget("transaction", 64*1024).NewAccount("transaction")
	iter := NewTransactionIteratorWithSpill(
		NewRowScanIterator(rows, 16),
		"session",
		0,
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
	if len(got) != 120 {
		t.Fatalf("rows: got %d, want 120", len(got))
	}
	rs := iter.ResourceStats()
	if rs.SpilledRows == 0 {
		t.Fatal("expected transaction spill path to write rows")
	}
	if rs.SpillBytes == 0 {
		t.Fatal("expected transaction spill bytes to be reported")
	}
}

func TestTransactionMaxSpanFallsBackToPartitionSpill(t *testing.T) {
	rows := make([]map[string]event.Value, 0, 240)
	for i := 0; i < 120; i++ {
		session := fmt.Sprintf("s-%03d", i)
		rows = append(rows, map[string]event.Value{
			"session": event.StringValue(session),
			"_raw":    event.StringValue("start-" + session),
			"_time":   event.TimestampValue(time.Unix(0, 0)),
		})
	}
	for i := 0; i < 120; i++ {
		session := fmt.Sprintf("s-%03d", i)
		rows = append(rows, map[string]event.Value{
			"session": event.StringValue(session),
			"_raw":    event.StringValue("end-" + session),
			"_time":   event.TimestampValue(time.Unix(1, 0)),
		})
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("create spill manager: %v", err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("transaction", 64*1024).NewAccount("transaction")
	iter := NewTransactionIteratorWithSpill(
		NewRowScanIterator(rows, 16),
		"session",
		time.Minute,
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
	if len(got) != 120 {
		t.Fatalf("rows: got %d, want 120", len(got))
	}
	if rs := iter.ResourceStats(); rs.SpilledRows == 0 || rs.SpillBytes == 0 {
		t.Fatalf("expected partition spill stats, got rows=%d bytes=%d", rs.SpilledRows, rs.SpillBytes)
	}
}

func TestTransactionMaxSpanStreamsSortedWindows(t *testing.T) {
	rows := []map[string]event.Value{
		{"session": event.StringValue("s1"), "_raw": event.StringValue("a"), "_time": event.TimestampValue(time.Unix(0, 0))},
		{"session": event.StringValue("s2"), "_raw": event.StringValue("b"), "_time": event.TimestampValue(time.Unix(1, 0))},
		{"session": event.StringValue("s1"), "_raw": event.StringValue("c"), "_time": event.TimestampValue(time.Unix(2, 0))},
		{"session": event.StringValue("s3"), "_raw": event.StringValue("d"), "_time": event.TimestampValue(time.Unix(20, 0))},
	}

	acct := memgov.NewTestBudget("transaction", 4*1024).NewAccount("transaction")
	iter := NewTransactionIteratorWithBudget(NewRowScanIterator(rows, 2), "session", 5*time.Second, "", "", 10, acct)
	defer iter.Close()

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("rows: got %d, want 3", len(got))
	}
	if got[0]["session"].String() != "s1" || got[0]["eventcount"].String() != "2" {
		t.Fatalf("first transaction: got session=%s eventcount=%s, want s1/2", got[0]["session"].String(), got[0]["eventcount"].String())
	}
	if got[1]["session"].String() != "s2" || got[2]["session"].String() != "s3" {
		t.Fatalf("unexpected transaction order: %s, %s, %s", got[0]["session"].String(), got[1]["session"].String(), got[2]["session"].String())
	}
}
