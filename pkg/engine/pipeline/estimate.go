package pipeline

import "github.com/lynxbase/lynxdb/pkg/event"

const (
	// estimateValueBytes is the approximate in-slice size of event.Value on
	// current 64-bit Go targets: type tag plus string/int/float payload fields.
	estimateValueBytes int64 = 40

	estimateMapOverhead   int64 = 64
	estimateEntryOverhead int64 = 56
)

// EstimateRowBytes estimates the heap size of a materialized row map.
// It intentionally includes string payload lengths so large _raw fields are
// charged against operator budgets instead of disappearing behind a fixed row
// constant.
func EstimateRowBytes(row map[string]event.Value) int64 {
	size := estimateMapOverhead
	for k, v := range row {
		size += estimateEntryOverhead + int64(len(k))
		if v.Type() == event.FieldTypeString {
			size += int64(len(v.String()))
		}
	}

	return size
}

// EstimateBatchBytes estimates the heap size of a columnar Batch.
func EstimateBatchBytes(batch *Batch) int64 {
	if batch == nil {
		return 0
	}

	var total int64
	for name, col := range batch.Columns {
		// Map key payload, slice header, and in-slice Value storage.
		total += int64(len(name)) + 24 + int64(len(col))*estimateValueBytes
		for _, v := range col {
			if v.Type() == event.FieldTypeString {
				total += int64(len(v.String()))
			}
		}
	}

	return total
}

// estimateRowMapBytes preserves the old unexported helper for package-local
// callers and tests while routing all accounting through the canonical helper.
func estimateRowMapBytes(row map[string]event.Value) int64 {
	return EstimateRowBytes(row)
}
