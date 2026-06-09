package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

func memgovTestRows(n int) []map[string]event.Value {
	rows := make([]map[string]event.Value, n)
	for i := range rows {
		rows[i] = map[string]event.Value{
			"host":   event.StringValue("host-with-a-reasonably-long-name"),
			"status": event.IntValue(int64(200 + i)),
		}
	}

	return rows
}

func TestAppendcolsAccountsMaterialization(t *testing.T) {
	acct := memgov.NewTestBudget("appendcols", 0).NewAccount("appendcols")
	iter := NewAppendcolsIteratorWithBudget(
		NewRowScanIterator(memgovTestRows(10), 4),
		NewRowScanIterator(memgovTestRows(10), 4),
		false, -1, 4, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("rows: got %d, want 10", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected appendcols account to track materialized rows")
	}
}

func TestAppendcolsBudgetExceeded(t *testing.T) {
	acct := memgov.NewTestBudget("appendcols", 64).NewAccount("appendcols")
	iter := NewAppendcolsIteratorWithBudget(
		NewRowScanIterator(memgovTestRows(100), 16),
		NewRowScanIterator(memgovTestRows(100), 16),
		false, -1, 16, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected appendcols materialization to exceed tiny budget")
	}
}

func TestAppendpipeAccountsMaterialization(t *testing.T) {
	identity := func(src Iterator, _ []spl2.Command) (Iterator, error) { return src, nil }
	acct := memgov.NewTestBudget("appendpipe", 0).NewAccount("appendpipe")
	iter := NewAppendpipeIteratorWithBudget(
		NewRowScanIterator(memgovTestRows(10), 4), nil, 4, identity, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 { // original + identity subpipe
		t.Fatalf("rows: got %d, want 20", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected appendpipe account to track materialized rows")
	}
}

func TestAppendpipeBudgetExceeded(t *testing.T) {
	identity := func(src Iterator, _ []spl2.Command) (Iterator, error) { return src, nil }
	acct := memgov.NewTestBudget("appendpipe", 64).NewAccount("appendpipe")
	iter := NewAppendpipeIteratorWithBudget(
		NewRowScanIterator(memgovTestRows(100), 16), nil, 16, identity, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected appendpipe materialization to exceed tiny budget")
	}
}

func TestCompareAccountsMaterialization(t *testing.T) {
	reExec := func(ctx context.Context) (Iterator, error) {
		return NewRowScanIterator(memgovTestRows(10), 4), nil
	}
	acct := memgov.NewTestBudget("compare", 0).NewAccount("compare")
	iter := NewCompareIteratorWithBudget(
		NewRowScanIterator(memgovTestRows(10), 4), time.Hour, reExec, 4, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 {
		t.Fatalf("rows: got %d, want 10", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected compare account to track materialized rows")
	}
}

func TestCompareBudgetExceeded(t *testing.T) {
	acct := memgov.NewTestBudget("compare", 64).NewAccount("compare")
	iter := NewCompareIteratorWithBudget(
		NewRowScanIterator(memgovTestRows(100), 16), time.Hour, nil, 16, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected compare materialization to exceed tiny budget")
	}
}

func TestMvcombineAccountsGroupState(t *testing.T) {
	rows := []map[string]event.Value{
		{"host": event.StringValue("a"), "ip": event.StringValue("10.0.0.1")},
		{"host": event.StringValue("a"), "ip": event.StringValue("10.0.0.2")},
		{"host": event.StringValue("b"), "ip": event.StringValue("10.0.0.3")},
	}
	acct := memgov.NewTestBudget("mvcombine", 0).NewAccount("mvcombine")
	iter := NewMvcombineIteratorWithBudget(NewRowScanIterator(rows, 2), "ip", 2, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows: got %d, want 2", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected mvcombine account to track group state")
	}
}

func TestMvcombineBudgetExceeded(t *testing.T) {
	acct := memgov.NewTestBudget("mvcombine", 64).NewAccount("mvcombine")
	iter := NewMvcombineIteratorWithBudget(NewRowScanIterator(memgovTestRows(100), 16), "status", 16, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected mvcombine group state to exceed tiny budget")
	}
}
