package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

const topSpillPartitions = 64

const (
	topSpillFieldValue = "__lynxdb_internal_top_field"
	topSpillByValue    = "__lynxdb_internal_top_by"
	topSpillCount      = "__lynxdb_internal_top_count"
)

type topCounterKey struct {
	fieldVal string
	byVal    string
}

type topCountEntry struct {
	fieldVal string
	byVal    string
	count    int64
}

// TopIterator implements the top/rare command.
// It aggregates count by field, sorts descending (top) or ascending (rare),
// and returns the top/bottom N values.
type TopIterator struct {
	child     Iterator
	field     string
	byField   string
	n         int
	ascending bool // true for rare, false for top
	batchSize int
	rows      []map[string]event.Value
	emitted   bool
	offset    int
	acct      memgov.MemoryAccount // per-operator memory tracking

	spillMgr        *SpillManager
	spillPaths      []string
	spilledRows     int64
	spillBytesTotal int64
}

// NewTopIterator creates a top/rare iterator.
func NewTopIterator(child Iterator, field, byField string, n int, ascending bool, batchSize int) *TopIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	return &TopIterator{
		child:     child,
		field:     field,
		byField:   byField,
		n:         n,
		ascending: ascending,
		batchSize: batchSize,
		acct:      memgov.NopAccount(),
	}
}

// NewTopIteratorWithBudget creates a top/rare iterator with memory budget tracking.
func NewTopIteratorWithBudget(child Iterator, field, byField string, n int, ascending bool, batchSize int, acct memgov.MemoryAccount) *TopIterator {
	t := NewTopIterator(child, field, byField, n, ascending, batchSize)
	t.acct = memgov.EnsureAccount(acct)

	return t
}

// NewTopIteratorWithSpill creates a top/rare iterator with memory budget
// tracking and a hash-partitioned spill fallback for high-cardinality keys.
func NewTopIteratorWithSpill(child Iterator, field, byField string, n int, ascending bool, batchSize int, acct memgov.MemoryAccount, mgr *SpillManager) *TopIterator {
	t := NewTopIteratorWithBudget(child, field, byField, n, ascending, batchSize, acct)
	t.spillMgr = mgr

	return t
}

func (t *TopIterator) Init(ctx context.Context) error {
	return t.child.Init(ctx)
}

func (t *TopIterator) Next(ctx context.Context) (*Batch, error) {
	if !t.emitted {
		if err := t.materialize(ctx); err != nil {
			return nil, err
		}
		t.emitted = true
	}

	if t.offset >= len(t.rows) {
		return nil, nil
	}

	end := t.offset + t.batchSize
	if end > len(t.rows) {
		end = len(t.rows)
	}

	batch := NewBatch(end - t.offset)
	for _, row := range t.rows[t.offset:end] {
		batch.AddRow(row)
	}
	t.offset = end

	return batch, nil
}

func (t *TopIterator) Close() error {
	t.acct.Close()
	if t.spillMgr != nil {
		for _, path := range t.spillPaths {
			t.spillMgr.Release(path)
		}
	}
	t.spillPaths = nil

	return t.child.Close()
}

// MemoryUsed returns the current tracked memory for this operator.
func (t *TopIterator) MemoryUsed() int64 {
	return t.acct.Used()
}

// ResourceStats implements ResourceReporter for per-operator spill metrics.
func (t *TopIterator) ResourceStats() OperatorResourceStats {
	return OperatorResourceStats{
		PeakBytes:   t.acct.MaxUsed(),
		SpilledRows: t.spilledRows,
		SpillBytes:  t.spillBytesTotal,
	}
}

func (t *TopIterator) Schema() []FieldInfo {
	fields := []FieldInfo{
		{Name: t.field},
		{Name: "count"},
		{Name: "percent"},
	}
	if t.byField != "" {
		fields = append(fields, FieldInfo{Name: t.byField})
	}

	return fields
}

func (t *TopIterator) materialize(ctx context.Context) error {
	counts := make(map[topCounterKey]int64)
	total := int64(0)

	for {
		batch, err := t.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}

		for i := 0; i < batch.Len; i++ {
			row := batch.Row(i)
			fv := ""
			if v, ok := row[t.field]; ok {
				fv = v.String()
			}

			bv := ""
			if t.byField != "" {
				if v, ok := row[t.byField]; ok {
					bv = v.String()
				}
			}

			ck := topCounterKey{fv, bv}
			if counts[ck] == 0 {
				// New counter key — track memory.
				if err := t.acct.Grow(estimateTopCounterBytes(fv, bv)); err != nil {
					if t.spillMgr != nil {
						return t.spillAndMaterialize(ctx, counts, total, batch, i)
					}

					return fmt.Errorf("top.materialize: %w", err)
				}
			}
			counts[ck]++
			total++
		}
	}

	entries := make([]topCountEntry, 0, len(counts))
	for k, c := range counts {
		entries = append(entries, topCountEntry{k.fieldVal, k.byVal, c})
	}
	t.materializeRowsFromEntries(entries, total)

	return nil
}

func (t *TopIterator) materializeRowsFromEntries(entries []topCountEntry, total int64) {
	sort.Slice(entries, func(i, j int) bool {
		return t.topEntryLess(entries[i], entries[j])
	})

	if len(entries) > t.n {
		entries = entries[:t.n]
	}

	t.rows = make([]map[string]event.Value, len(entries))
	for i, e := range entries {
		row := map[string]event.Value{
			t.field: event.StringValue(e.fieldVal),
			"count": event.IntValue(e.count),
		}
		if total > 0 {
			pct := float64(e.count) / float64(total) * 100
			row["percent"] = event.FloatValue(pct)
		}
		if t.byField != "" {
			row[t.byField] = event.StringValue(e.byVal)
		}
		t.rows[i] = row
	}
}

func (t *TopIterator) spillAndMaterialize(ctx context.Context, counts map[topCounterKey]int64, total int64, overflowBatch *Batch, overflowOffset int) error {
	writers := make([]*ColumnarSpillWriter, topSpillPartitions)
	var paths []string
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		for _, sw := range writers {
			if sw != nil {
				_ = sw.CloseFile()
				if t.spillMgr != nil {
					t.spillMgr.Release(sw.Path())
				}
			}
		}
		for _, path := range paths {
			if t.spillMgr != nil {
				t.spillMgr.Release(path)
			}
		}
	}()

	for i := range writers {
		sw, err := NewColumnarSpillWriter(t.spillMgr, fmt.Sprintf("top-%02d", i))
		if err != nil {
			return fmt.Errorf("top.spill: create partition: %w", err)
		}
		writers[i] = sw
	}

	for key, count := range counts {
		if err := t.writeTopSpillCount(writers, key.fieldVal, key.byVal, count); err != nil {
			return err
		}
	}
	t.acct.Shrink(t.acct.Used())
	counts = nil

	writeBatchRows := func(batch *Batch, start int) error {
		for i := start; i < batch.Len; i++ {
			fv, bv := t.topValues(batch.Row(i))
			if err := t.writeTopSpillCount(writers, fv, bv, 1); err != nil {
				return err
			}
			total++
		}

		return nil
	}

	if err := writeBatchRows(overflowBatch, overflowOffset); err != nil {
		return err
	}
	for {
		batch, err := t.child.Next(ctx)
		if err != nil {
			return fmt.Errorf("top.spill: read child: %w", err)
		}
		if batch == nil {
			break
		}
		if err := writeBatchRows(batch, 0); err != nil {
			return err
		}
	}

	t.spillPaths = make([]string, len(writers))
	for i, sw := range writers {
		t.spillPaths[i] = sw.Path()
		if err := sw.CloseFile(); err != nil {
			return fmt.Errorf("top.spill: close partition %d: %w", i, err)
		}
		paths = append(paths, sw.Path())
		writers[i] = nil
	}

	candidates := make([]topCountEntry, 0, t.n)
	for _, path := range t.spillPaths {
		entries, err := t.processTopSpillPartition(path)
		if err != nil {
			return err
		}
		candidates = append(candidates, entries...)
		candidates = t.trimTopEntries(candidates)
	}
	t.materializeRowsFromEntries(candidates, total)
	cleanup = false

	return nil
}

func (t *TopIterator) topValues(row map[string]event.Value) (string, string) {
	fv := ""
	if v, ok := row[t.field]; ok {
		fv = v.String()
	}

	bv := ""
	if t.byField != "" {
		if v, ok := row[t.byField]; ok {
			bv = v.String()
		}
	}

	return fv, bv
}

func (t *TopIterator) writeTopSpillCount(writers []*ColumnarSpillWriter, fieldVal, byVal string, count int64) error {
	p := hashPartition(fieldVal+"\x00"+byVal, len(writers))
	row := map[string]event.Value{
		topSpillFieldValue: event.StringValue(fieldVal),
		topSpillByValue:    event.StringValue(byVal),
		topSpillCount:      event.IntValue(count),
	}
	if err := writers[p].WriteRow(row); err != nil {
		return fmt.Errorf("top.spill: write count: %w", err)
	}
	t.spilledRows += count

	return nil
}

func (t *TopIterator) processTopSpillPartition(path string) ([]topCountEntry, error) {
	reader, err := NewColumnarSpillReader(path)
	if err != nil {
		return nil, fmt.Errorf("top.spill: open partition: %w", err)
	}
	defer reader.Close()

	counts := make(map[topCounterKey]int64)
	var tracked int64
	for {
		row, readErr := reader.ReadRow()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			return nil, fmt.Errorf("top.spill: read partition: %w", readErr)
		}
		fv := ""
		if v, ok := row[topSpillFieldValue]; ok {
			fv = v.String()
		}
		bv := ""
		if v, ok := row[topSpillByValue]; ok {
			bv = v.String()
		}
		count := int64(0)
		if v, ok := row[topSpillCount]; ok {
			if n, nok := v.TryAsInt(); nok {
				count = n
			}
		}
		key := topCounterKey{fieldVal: fv, byVal: bv}
		if counts[key] == 0 {
			keyBytes := estimateTopCounterBytes(fv, bv)
			if err := t.acct.Grow(keyBytes); err != nil {
				t.acct.Shrink(tracked)

				return nil, fmt.Errorf("top.spill: partition counters: %w", err)
			}
			tracked += keyBytes
		}
		counts[key] += count
	}

	entries := make([]topCountEntry, 0, len(counts))
	for key, count := range counts {
		entries = append(entries, topCountEntry{fieldVal: key.fieldVal, byVal: key.byVal, count: count})
	}
	t.acct.Shrink(tracked)

	return entries, nil
}

func (t *TopIterator) trimTopEntries(entries []topCountEntry) []topCountEntry {
	sort.Slice(entries, func(i, j int) bool {
		return t.topEntryLess(entries[i], entries[j])
	})
	if len(entries) > t.n {
		entries = entries[:t.n]
	}

	return entries
}

func (t *TopIterator) topEntryLess(a, b topCountEntry) bool {
	if a.count != b.count {
		if t.ascending {
			return a.count < b.count
		}

		return a.count > b.count
	}
	if a.fieldVal != b.fieldVal {
		return a.fieldVal < b.fieldVal
	}

	return a.byVal < b.byVal
}

func estimateTopCounterBytes(fieldVal, byVal string) int64 {
	return 96 + int64(len(fieldVal)+len(byVal))
}
