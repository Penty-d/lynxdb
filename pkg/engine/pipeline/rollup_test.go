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

func TestRollupSpillsHighCardinalityGroups(t *testing.T) {
	base := time.Unix(3600, 0).UTC()
	rows := make([]map[string]event.Value, 0, 120)
	for i := 0; i < 120; i++ {
		rows = append(rows, rollupRow(base.Add(time.Duration(i)*time.Second), "host"))
	}

	mgr, err := NewSpillManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("create spill manager: %v", err)
	}
	defer mgr.CleanupAll()

	acct := memgov.NewTestBudget("rollup", 4*1024).NewAccount("rollup")
	iter := NewRollupIteratorWithSpill(NewRowScanIterator(rows, 10), []string{"1s", "1m"}, []string{"host"}, 20, acct, mgr)

	got, err := CollectAll(context.Background(), iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 122 {
		t.Fatalf("rows: got %d, want 122", len(got))
	}
	rs := iter.ResourceStats()
	if rs.SpilledRows == 0 {
		t.Fatal("expected rollup spill path to write rows")
	}
	if rs.SpillBytes == 0 {
		t.Fatal("expected rollup spill bytes to be reported")
	}

	var oneMinuteCount int64
	for _, row := range got {
		if row["_resolution"].String() == "1m" {
			oneMinuteCount += row["count"].AsInt()
		}
	}
	if oneMinuteCount != 120 {
		t.Fatalf("1m count: got %d, want 120", oneMinuteCount)
	}
}
