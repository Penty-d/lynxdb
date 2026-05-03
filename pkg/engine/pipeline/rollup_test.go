package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func rollupRow(ts time.Time, host string) map[string]event.Value {
	return map[string]event.Value{
		"_time": event.TimestampValue(ts),
		"host":  event.StringValue(host),
	}
}

func TestRollupAggregatesMultipleSpans(t *testing.T) {
	base := time.Unix(3600, 0).UTC()
	rows := []map[string]event.Value{
		rollupRow(base, "a"),
		rollupRow(base.Add(30*time.Second), "a"),
		rollupRow(base.Add(90*time.Second), "a"),
	}

	iter := NewRollupIterator(NewRowScanIterator(rows, 2), []string{"1m", "1h"}, []string{"host"}, 10)
	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}

	var oneHourCount int64
	minuteBuckets := 0
	for _, row := range got {
		switch row["_resolution"].String() {
		case "1h":
			oneHourCount += row["count"].AsInt()
		case "1m":
			minuteBuckets++
		}
	}
	if oneHourCount != 3 {
		t.Fatalf("1h count: got %d, want 3", oneHourCount)
	}
	if minuteBuckets != 2 {
		t.Fatalf("1m buckets: got %d, want 2", minuteBuckets)
	}
}

func TestRollupBudgetExceededOnHighCardinalityGroups(t *testing.T) {
	base := time.Unix(3600, 0).UTC()
	rows := []map[string]event.Value{
		rollupRow(base, "host-a"),
		rollupRow(base, "host-b"),
	}
	acct := memgov.NewTestBudget("rollup", 128).NewAccount("rollup")
	iter := NewRollupIteratorWithBudget(NewRowScanIterator(rows, 1), []string{"1m"}, []string{"host"}, 10, acct)

	if _, err := CollectAll(context.Background(), iter); err == nil {
		t.Fatal("expected rollup group state to exceed tiny budget")
	}
}
