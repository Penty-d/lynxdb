package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func largeRawRow(size int) map[string]event.Value {
	return map[string]event.Value{
		"_raw": event.StringValue(strings.Repeat("x", size)),
		"key":  event.StringValue("k"),
		"n":    event.IntValue(1),
	}
}

func TestEstimateRowAndBatchBytesIncludeStringPayload(t *testing.T) {
	row := largeRawRow(4096)

	rowBytes := EstimateRowBytes(row)
	if rowBytes <= 4096 {
		t.Fatalf("EstimateRowBytes() = %d, want it to include string payload", rowBytes)
	}

	batch := BatchFromRows([]map[string]event.Value{row})
	batchBytes := EstimateBatchBytes(batch)
	if batchBytes <= 4096 {
		t.Fatalf("EstimateBatchBytes() = %d, want it to include string payload", batchBytes)
	}
}

func TestLargeRowsTripBlockingOperatorBudgets(t *testing.T) {
	ctx := context.Background()
	row := largeRawRow(4096)

	t.Run("eventstats", func(t *testing.T) {
		acct := memgov.NewTestBudget("eventstats", 2048).NewAccount("eventstats")
		iter := NewEventStatsIteratorWithBudget(
			NewRowScanIterator([]map[string]event.Value{row}, 1),
			[]AggFunc{{Name: "count", Alias: "cnt"}},
			nil,
			1,
			acct,
		)
		if _, err := CollectAll(ctx, iter); err == nil {
			t.Fatal("expected large eventstats row to exceed 2KB budget")
		}
	})

	t.Run("transaction", func(t *testing.T) {
		acct := memgov.NewTestBudget("transaction", 2048).NewAccount("transaction")
		iter := NewTransactionIteratorWithBudget(
			NewRowScanIterator([]map[string]event.Value{row}, 1),
			"key",
			0,
			"",
			"",
			1,
			acct,
		)
		if _, err := CollectAll(ctx, iter); err == nil {
			t.Fatal("expected large transaction row to exceed 2KB budget")
		}
	})

	t.Run("join", func(t *testing.T) {
		left := NewRowScanIterator([]map[string]event.Value{{"key": event.StringValue("k")}}, 1)
		right := NewRowScanIterator([]map[string]event.Value{row}, 1)
		acct := memgov.NewTestBudget("join", 2048).NewAccount("join")
		iter := NewJoinIteratorWithBudget(left, right, "key", "inner", acct)
		if _, err := CollectAll(ctx, iter); err == nil {
			t.Fatal("expected large join build row to exceed 2KB budget")
		}
	})
}

func TestBoundedOperatorsTrackActualLargeRowBytes(t *testing.T) {
	ctx := context.Background()
	row := largeRawRow(4096)

	t.Run("topn", func(t *testing.T) {
		acct := memgov.NewTestBudget("topn", 0).NewAccount("topn")
		iter := NewTopNIteratorWithBudget(
			NewRowScanIterator([]map[string]event.Value{row}, 1),
			[]SortField{{Name: "n"}},
			1,
			1,
			acct,
		)
		if _, err := CollectAll(ctx, iter); err != nil {
			t.Fatal(err)
		}
		if acct.MaxUsed() <= 4096 {
			t.Fatalf("topn MaxUsed() = %d, want large row payload tracked", acct.MaxUsed())
		}
	})

	t.Run("tail", func(t *testing.T) {
		acct := memgov.NewTestBudget("tail", 0).NewAccount("tail")
		iter := NewTailIteratorWithBudget(NewRowScanIterator([]map[string]event.Value{row}, 1), 1, 1, acct)
		if _, err := CollectAll(ctx, iter); err != nil {
			t.Fatal(err)
		}
		if acct.MaxUsed() <= 4096 {
			t.Fatalf("tail MaxUsed() = %d, want large row payload tracked", acct.MaxUsed())
		}
	})

	t.Run("streamstats", func(t *testing.T) {
		acct := memgov.NewTestBudget("streamstats", 0).NewAccount("streamstats")
		iter := NewStreamStatsIteratorWithBudget(
			NewRowScanIterator([]map[string]event.Value{row}, 1),
			[]AggFunc{{Name: "count", Alias: "cnt"}},
			nil,
			1,
			true,
			acct,
		)
		if _, err := CollectAll(ctx, iter); err != nil {
			t.Fatal(err)
		}
		if acct.MaxUsed() <= 4096 {
			t.Fatalf("streamstats MaxUsed() = %d, want large row payload tracked", acct.MaxUsed())
		}
	})
}
