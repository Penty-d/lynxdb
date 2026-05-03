package pipeline

import (
	"context"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestPatternsIteratorAccountsDrainState(t *testing.T) {
	rows := []map[string]event.Value{
		{"_raw": event.StringValue("alpha beta gamma")},
		{"_raw": event.StringValue("alpha beta delta")},
	}
	acct := memgov.NewTestBudget("patterns", 0).NewAccount("patterns")
	iter := NewPatternsIteratorWithBudget(NewRowScanIterator(rows, 1), "_raw", 10, 1.0, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected patterns output")
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected patterns memory account to track drain state")
	}
}

func TestPatternsIteratorBudgetExceeded(t *testing.T) {
	rows := []map[string]event.Value{
		{"_raw": event.StringValue("alpha beta gamma")},
	}
	acct := memgov.NewTestBudget("patterns", 64).NewAccount("patterns")
	iter := NewPatternsIteratorWithBudget(NewRowScanIterator(rows, 1), "_raw", 10, 1.0, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected tiny patterns budget to reject drain state")
	}
}
