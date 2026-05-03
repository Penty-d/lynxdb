package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func sessionRow(host string, ts time.Time, raw string) map[string]event.Value {
	return map[string]event.Value{
		"host":  event.StringValue(host),
		"_time": event.TimestampValue(ts),
		"_raw":  event.StringValue(raw),
	}
}

func TestSessionizeSortsAndAssignsSessions(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	rows := []map[string]event.Value{
		sessionRow("b", base.Add(2*time.Minute), "b2"),
		sessionRow("a", base.Add(20*time.Minute), "a3"),
		sessionRow("a", base, "a1"),
		sessionRow("a", base.Add(5*time.Minute), "a2"),
	}

	iter := NewSessionizeIterator(NewRowScanIterator(rows, 2), "10m", []string{"host"}, 2)
	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("rows: got %d, want 4", len(got))
	}

	if got[0]["host"].String() != "a" || got[0]["_raw"].String() != "a1" {
		t.Fatalf("row 0 = host %q raw %q, want a/a1", got[0]["host"].String(), got[0]["_raw"].String())
	}
	if got[1]["_session_id"].AsInt() != got[0]["_session_id"].AsInt() {
		t.Fatal("a1 and a2 should be in the same session")
	}
	if got[2]["_session_id"].AsInt() == got[1]["_session_id"].AsInt() {
		t.Fatal("a3 should start a new session after maxpause")
	}
	if got[3]["host"].String() != "b" {
		t.Fatalf("last row host = %q, want b", got[3]["host"].String())
	}
	if !got[0]["_session_end"].AsTimestamp().Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("first session end = %s, want %s", got[0]["_session_end"].AsTimestamp(), base.Add(5*time.Minute))
	}
}

func TestSessionizeAccountsActiveSessionRows(t *testing.T) {
	row := sessionRow("a", time.Unix(1000, 0).UTC(), strings.Repeat("x", 4096))
	sortAcct := memgov.NewTestBudget("sessionize-sort", 0).NewAccount("sessionize-sort")
	sessionAcct := memgov.NewTestBudget("sessionize", 2048).NewAccount("sessionize")
	iter := NewSessionizeIteratorWithBudget(
		NewRowScanIterator([]map[string]event.Value{row}, 1),
		"10m",
		[]string{"host"},
		1,
		sortAcct,
		sessionAcct,
		nil,
	)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected large active session row to exceed sessionize budget")
	}
}

func TestSessionizeCanUseSpillingSort(t *testing.T) {
	base := time.Unix(1000, 0).UTC()
	rows := make([]map[string]event.Value, 200)
	for i := range rows {
		rows[i] = sessionRow("a", base.Add(time.Duration(200-i)*time.Minute), strings.Repeat("x", 512))
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CleanupAll()

	sortAcct := memgov.NewTestBudget("sessionize-sort", 8<<10).NewAccount("sessionize-sort")
	sessionAcct := memgov.NewTestBudget("sessionize", 0).NewAccount("sessionize")
	iter := NewSessionizeIteratorWithBudget(
		NewRowScanIterator(rows, 16),
		"1s",
		[]string{"host"},
		16,
		sortAcct,
		sessionAcct,
		mgr,
	)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("rows: got %d, want %d", len(got), len(rows))
	}
}
