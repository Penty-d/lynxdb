package pipeline

import (
	"context"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestJoinIteratorSkipsManyNonMatchingBatchesIteratively(t *testing.T) {
	const leftRows = 20000

	left := make([]map[string]event.Value, leftRows)
	for i := range left {
		left[i] = map[string]event.Value{"key": event.IntValue(int64(i))}
	}
	right := []map[string]event.Value{
		{"key": event.IntValue(int64(leftRows + 1))},
	}

	iter := NewJoinIteratorWithBudget(
		NewRowScanIterator(left, 1),
		NewRowScanIterator(right, 1),
		"key",
		"inner",
		memgov.NewTestBudget("join", 1<<30).NewAccount("join"),
	)

	ctx := context.Background()
	if err := iter.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := iter.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()

	batch, err := iter.Next(ctx)
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if batch != nil {
		t.Fatalf("Next() returned %d rows, want EOF", batch.Len)
	}
}
