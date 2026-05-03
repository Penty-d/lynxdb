package pipeline

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestCorrelatePearsonStreaming(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": event.IntValue(1), "y": event.IntValue(2)},
		{"x": event.IntValue(2), "y": event.IntValue(4)},
		{"x": event.IntValue(3), "y": event.IntValue(6)},
		{"x": event.StringValue("skip"), "y": event.IntValue(8)},
	}
	iter := NewCorrelateIterator(NewRowScanIterator(rows, 2), "x", "y", "pearson")

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: got %d, want 1", len(got))
	}
	if got[0]["_n"].AsInt() != 3 {
		t.Fatalf("_n: got %d, want 3", got[0]["_n"].AsInt())
	}
	corr := got[0]["_correlation"].AsFloat()
	if math.Abs(corr-1) > 1e-9 {
		t.Fatalf("correlation: got %f, want 1", corr)
	}
}

func TestCorrelateSpearmanStillWorks(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": event.IntValue(10), "y": event.IntValue(100)},
		{"x": event.IntValue(20), "y": event.IntValue(300)},
		{"x": event.IntValue(30), "y": event.IntValue(200)},
	}
	iter := NewCorrelateIterator(NewRowScanIterator(rows, 2), "x", "y", "spearman")

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rows: got %d, want 1", len(got))
	}
	if got[0]["_method"].String() != "spearman" {
		t.Fatalf("method: got %q, want spearman", got[0]["_method"].String())
	}
	if got[0]["_correlation"].IsNull() {
		t.Fatal("spearman correlation should not be null")
	}
}

func TestCorrelateSpearmanAccountsRankMaterialization(t *testing.T) {
	rows := []map[string]event.Value{
		{"x": event.IntValue(10), "y": event.IntValue(100)},
		{"x": event.IntValue(20), "y": event.IntValue(300)},
	}
	acct := memgov.NewTestBudget("correlate", estimatedCorrelationPairBytes).NewAccount("correlate")
	iter := NewCorrelateIteratorWithBudget(NewRowScanIterator(rows, 2), "x", "y", "spearman", acct)

	_, err := CollectAll(context.Background(), iter)
	if err == nil {
		t.Fatal("expected memory budget error")
	}
	if !strings.Contains(err.Error(), "correlate: memory budget exceeded") {
		t.Fatalf("unexpected error: %v", err)
	}
}
