package pipeline

import (
	"context"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestTopologyIteratorBudgetExceeded(t *testing.T) {
	rows := []map[string]event.Value{
		{"src": event.StringValue("node-a"), "dst": event.StringValue("node-b")},
		{"src": event.StringValue("node-c"), "dst": event.StringValue("node-d")},
	}
	acct := memgov.NewTestBudget("topology", 128).NewAccount("topology")
	iter := NewTopologyIteratorWithBudget(NewRowScanIterator(rows, 1), "src", "dst", "", 0, 1, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected topology graph state to exceed tiny budget")
	}
}

func TestTopologyIteratorAccountsGraphState(t *testing.T) {
	rows := []map[string]event.Value{
		{"src": event.StringValue("node-a"), "dst": event.StringValue("node-b")},
		{"src": event.StringValue("node-a"), "dst": event.StringValue("node-c")},
	}
	acct := memgov.NewTestBudget("topology", 0).NewAccount("topology")
	iter := NewTopologyIteratorWithBudget(NewRowScanIterator(rows, 2), "src", "dst", "", 0, 10, acct)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rows: got %d, want 2", len(got))
	}
	if acct.MaxUsed() == 0 {
		t.Fatal("expected topology account to track graph state")
	}
}
