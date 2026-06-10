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
// constant. For array and object values, uses Value.MemSize() for deep
// recursive size estimation.
func EstimateRowBytes(row map[string]event.Value) int64 {
	size := estimateMapOverhead
	for k, v := range row {
		size += estimateEntryOverhead + int64(len(k))
		switch v.Type() {
		case event.FieldTypeString:
			size += int64(len(v.String()))
		case event.FieldTypeArray, event.FieldTypeObject:
			// MemSize includes the base valueBase (40 bytes) which is already
			// counted in the estimateValueBytes constant embedded in
			// estimateEntryOverhead. Subtract it to avoid double-counting.
			extra := int64(v.MemSize()) - estimateValueBytes
			if extra > 0 {
				size += extra
			}
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
			switch v.Type() {
			case event.FieldTypeString:
				total += int64(len(v.String()))
			case event.FieldTypeArray, event.FieldTypeObject:
				extra := int64(v.MemSize()) - estimateValueBytes
				if extra > 0 {
					total += extra
				}
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
