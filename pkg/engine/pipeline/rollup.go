package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

const estimatedRollupGroupBytes int64 = 128
const rollupSpillPartitions = 64

// RollupIterator provides multi-resolution time bucketing.
// It aggregates all configured span resolutions in one pass over the child.
type RollupIterator struct {
	child     Iterator
	spans     []string
	groupBy   []string
	batchSize int
	acct      memgov.MemoryAccount

	// Blocking state
	done   bool
	output []map[string]event.Value
	offset int

	spillMgr        *SpillManager
	spillPaths      []string
	spilledRows     int64
	spillBytesTotal int64
}

type rollupBucketInfo struct {
	groupFields map[string]event.Value
	count       int64
	bucketTime  time.Time
}

type rollupSpanState struct {
	name   string
	dur    time.Duration
	groups map[string]*rollupBucketInfo
}

// NewRollupIterator creates a rollup operator.
func NewRollupIterator(child Iterator, spans []string, groupBy []string, batchSize int) *RollupIterator {
	return NewRollupIteratorWithBudget(child, spans, groupBy, batchSize, memgov.NopAccount())
}

// NewRollupIteratorWithBudget creates a rollup operator with group-state
// accounting.
func NewRollupIteratorWithBudget(child Iterator, spans []string, groupBy []string, batchSize int, acct memgov.MemoryAccount) *RollupIterator {
	return &RollupIterator{
		child:     child,
		spans:     spans,
		groupBy:   groupBy,
		batchSize: batchSize,
		acct:      memgov.EnsureAccount(acct),
	}
}

// NewRollupIteratorWithSpill creates a rollup operator with group-state
// accounting and a partitioned columnar spill fallback for high-cardinality
// rollup buckets.
func NewRollupIteratorWithSpill(child Iterator, spans []string, groupBy []string, batchSize int, acct memgov.MemoryAccount, mgr *SpillManager) *RollupIterator {
	r := NewRollupIteratorWithBudget(child, spans, groupBy, batchSize, acct)
	r.spillMgr = mgr

	return r
}

func (r *RollupIterator) Init(ctx context.Context) error {
	return r.child.Init(ctx)
}

func (r *RollupIterator) Next(ctx context.Context) (*Batch, error) {
	if !r.done {
		if err := r.materialize(ctx); err != nil {
			return nil, err
		}
		r.done = true
	}
	if r.offset >= len(r.output) {
		return nil, nil
	}
	end := r.offset + r.batchSize
	if end > len(r.output) {
		end = len(r.output)
	}
	batch := BatchFromRows(r.output[r.offset:end])
	r.offset = end

	return batch, nil
}

func (r *RollupIterator) materialize(ctx context.Context) error {
	states := make([]rollupSpanState, 0, len(r.spans))
	for _, span := range r.spans {
		dur := parseDuration(span)
		if dur == 0 {
			continue
		}
		states = append(states, rollupSpanState{
			name:   span,
			dur:    dur,
			groups: make(map[string]*rollupBucketInfo),
		})
	}
	if len(states) == 0 {
		return nil
	}

	for {
		batch, err := r.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			row := batch.Row(i)
			for si := range states {
				if err := r.addToRollupState(&states[si], row); err != nil {
					if r.spillMgr != nil {
						return r.spillAndMaterialize(ctx, states, batch, i)
					}

					return err
				}
			}
		}
	}

	for _, state := range states {
		r.output = append(r.output, r.rollupRows(state.name, state.groups)...)
	}

	return nil
}

func (r *RollupIterator) addToRollupState(state *rollupSpanState, row map[string]event.Value) error {
	ts := rollupEventTime(row)
	bucket := ts.Truncate(state.dur)

	key := state.name + "|" + fmt.Sprintf("%d", bucket.UnixNano())
	keyBytes := int64(len(key))
	for _, f := range r.groupBy {
		key += "|"
		if v, ok := row[f]; ok {
			vk := rollupValueKey(v)
			key += vk
			keyBytes += int64(len(vk))
		}
	}

	info, ok := state.groups[key]
	if !ok {
		if err := r.acct.Grow(estimatedRollupGroupBytes + keyBytes); err != nil {
			return fmt.Errorf("rollup: memory budget exceeded for group state: %w", err)
		}
		info = &rollupBucketInfo{
			groupFields: make(map[string]event.Value, len(r.groupBy)),
			bucketTime:  bucket,
		}
		for _, f := range r.groupBy {
			if v, ok := row[f]; ok {
				info.groupFields[f] = v
			}
		}
		state.groups[key] = info
	}
	info.count++
	return nil
}

func (r *RollupIterator) rollupRows(span string, groups map[string]*rollupBucketInfo) []map[string]event.Value {
	result := make([]map[string]event.Value, 0, len(groups))
	for _, info := range groups {
		out := make(map[string]event.Value, 2+len(r.groupBy))
		out["_resolution"] = event.StringValue(span)
		out["_bucket"] = event.TimestampValue(info.bucketTime)
		for _, f := range r.groupBy {
			if v, ok := info.groupFields[f]; ok {
				out[f] = v
			}
		}
		out["count"] = event.IntValue(info.count)
		result = append(result, out)
	}

	// Sort by bucket then by group fields for deterministic output.
	sort.Slice(result, func(i, j int) bool {
		ti := rollupTimestampValue(result[i]["_bucket"])
		tj := rollupTimestampValue(result[j]["_bucket"])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		// Tiebreak on group fields.
		for _, f := range r.groupBy {
			vi := result[i][f]
			vj := result[j][f]
			if rollupValueKey(vi) != rollupValueKey(vj) {
				return rollupValueKey(vi) < rollupValueKey(vj)
			}
		}
		return false
	})

	return result
}

func (r *RollupIterator) Close() error {
	var errs []error

	r.acct.Close()
	if r.spillMgr != nil {
		r.spillBytesTotal = sumSpillPathBytes(r.spillPaths)
		for _, path := range r.spillPaths {
			r.spillMgr.Release(path)
		}
	}
	r.spillPaths = nil

	if err := r.child.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// MemoryUsed returns the current tracked memory for this operator.
func (r *RollupIterator) MemoryUsed() int64 { return r.acct.Used() }

// ResourceStats implements ResourceReporter for spill observability.
func (r *RollupIterator) ResourceStats() OperatorResourceStats {
	spillBytes := r.spillBytesTotal
	if spillBytes == 0 {
		spillBytes = sumSpillPathBytes(r.spillPaths)
	}

	return OperatorResourceStats{
		PeakBytes:   r.acct.MaxUsed(),
		SpilledRows: r.spilledRows,
		SpillBytes:  spillBytes,
	}
}

func (r *RollupIterator) Schema() []FieldInfo {
	schema := []FieldInfo{
		{Name: "_resolution", Type: "string"},
		{Name: "_bucket", Type: "timestamp"},
		{Name: "count", Type: "int"},
	}
	for _, f := range r.groupBy {
		schema = append(schema, FieldInfo{Name: f, Type: "any"})
	}

	return schema
}

func (r *RollupIterator) spillAndMaterialize(ctx context.Context, states []rollupSpanState, overflowBatch *Batch, overflowOffset int) error {
	writers := make([]*ColumnarSpillWriter, rollupSpillPartitions)
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		for _, sw := range writers {
			if sw != nil {
				_ = sw.CloseFile()
				if r.spillMgr != nil {
					r.spillMgr.Release(sw.Path())
				}
			}
		}
	}()

	for i := range writers {
		sw, err := NewColumnarSpillWriter(r.spillMgr, fmt.Sprintf("rollup-%02d", i))
		if err != nil {
			return fmt.Errorf("rollup.spill: create partition: %w", err)
		}
		writers[i] = sw
	}

	for _, state := range states {
		for _, info := range state.groups {
			if err := r.writeRollupSpillRow(writers, state.name, info.bucketTime, info.groupFields, info.count); err != nil {
				return err
			}
		}
	}
	r.acct.Shrink(r.acct.Used())

	writeInputRows := func(batch *Batch, start int) error {
		for i := start; i < batch.Len; i++ {
			row := batch.Row(i)
			ts := rollupEventTime(row)
			for _, state := range states {
				groupFields := make(map[string]event.Value, len(r.groupBy))
				for _, f := range r.groupBy {
					if v, ok := row[f]; ok {
						groupFields[f] = v
					}
				}
				if err := r.writeRollupSpillRow(writers, state.name, ts.Truncate(state.dur), groupFields, 1); err != nil {
					return err
				}
			}
		}

		return nil
	}

	if err := writeInputRows(overflowBatch, overflowOffset); err != nil {
		return err
	}
	for {
		batch, err := r.child.Next(ctx)
		if err != nil {
			return fmt.Errorf("rollup.spill: read child: %w", err)
		}
		if batch == nil {
			break
		}
		if err := writeInputRows(batch, 0); err != nil {
			return err
		}
	}

	r.spillPaths = make([]string, len(writers))
	for i, sw := range writers {
		r.spillPaths[i] = sw.Path()
		if err := sw.CloseFile(); err != nil {
			return fmt.Errorf("rollup.spill: close partition %d: %w", i, err)
		}
		writers[i] = nil
	}

	mergedBySpan := make(map[string]map[string]*rollupBucketInfo, len(states))
	for _, state := range states {
		mergedBySpan[state.name] = make(map[string]*rollupBucketInfo)
	}
	for _, path := range r.spillPaths {
		if err := r.mergeRollupPartition(path, mergedBySpan); err != nil {
			return err
		}
	}
	for _, state := range states {
		r.output = append(r.output, r.rollupRows(state.name, mergedBySpan[state.name])...)
	}
	cleanup = false

	return nil
}

func (r *RollupIterator) writeRollupSpillRow(writers []*ColumnarSpillWriter, span string, bucket time.Time, groupFields map[string]event.Value, count int64) error {
	row := make(map[string]event.Value, 3+len(groupFields))
	row["_resolution"] = event.StringValue(span)
	row["_bucket"] = event.TimestampValue(bucket)
	row["count"] = event.IntValue(count)
	key := span + "|" + fmt.Sprintf("%d", bucket.UnixNano())
	for _, f := range r.groupBy {
		if v, ok := groupFields[f]; ok {
			row[f] = v
			key += "|" + rollupValueKey(v)
		} else {
			key += "|"
		}
	}
	p := hashPartition(key, len(writers))
	if err := writers[p].WriteRow(row); err != nil {
		return fmt.Errorf("rollup.spill: write row: %w", err)
	}
	r.spilledRows += count

	return nil
}

func (r *RollupIterator) mergeRollupPartition(path string, mergedBySpan map[string]map[string]*rollupBucketInfo) error {
	reader, err := NewColumnarSpillReader(path)
	if err != nil {
		return fmt.Errorf("rollup.spill: open partition: %w", err)
	}
	defer reader.Close()

	localBySpan := make(map[string]map[string]*rollupBucketInfo)
	var tracked int64
	for {
		row, readErr := reader.ReadRow()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			return fmt.Errorf("rollup.spill: read partition: %w", readErr)
		}
		span := ""
		if v, ok := row["_resolution"]; ok {
			span = v.String()
		}
		bucket := rollupTimestampValue(row["_bucket"])
		count := int64(0)
		if v, ok := row["count"]; ok {
			if n, nok := v.TryAsInt(); nok {
				count = n
			}
		}
		groups := localBySpan[span]
		if groups == nil {
			groups = make(map[string]*rollupBucketInfo)
			localBySpan[span] = groups
		}
		key, keyBytes, groupFields := r.rollupSpillKey(span, bucket, row)
		info, ok := groups[key]
		if !ok {
			if err := r.acct.Grow(estimatedRollupGroupBytes + keyBytes); err != nil {
				r.acct.Shrink(tracked)

				return fmt.Errorf("rollup.spill: partition group state: %w", err)
			}
			tracked += estimatedRollupGroupBytes + keyBytes
			info = &rollupBucketInfo{
				groupFields: groupFields,
				bucketTime:  bucket,
			}
			groups[key] = info
		}
		info.count += count
	}

	for span, groups := range localBySpan {
		merged := mergedBySpan[span]
		if merged == nil {
			merged = make(map[string]*rollupBucketInfo)
			mergedBySpan[span] = merged
		}
		for key, info := range groups {
			merged[key] = info
		}
	}
	r.acct.Shrink(tracked)

	return nil
}

func (r *RollupIterator) rollupSpillKey(span string, bucket time.Time, row map[string]event.Value) (string, int64, map[string]event.Value) {
	key := span + "|" + fmt.Sprintf("%d", bucket.UnixNano())
	keyBytes := int64(len(key))
	groupFields := make(map[string]event.Value, len(r.groupBy))
	for _, f := range r.groupBy {
		key += "|"
		if v, ok := row[f]; ok {
			groupFields[f] = v
			vk := rollupValueKey(v)
			key += vk
			keyBytes += int64(len(vk))
		}
	}

	return key, keyBytes, groupFields
}

// rollupEventTime extracts the timestamp from a row.
func rollupEventTime(row map[string]event.Value) time.Time {
	for _, field := range []string{"_time", "timestamp", "@timestamp", "time"} {
		if v, ok := row[field]; ok {
			if v.Type() == event.FieldTypeTimestamp {
				if ts, ok := v.TryAsTimestamp(); ok {
					return ts
				}
			}
			// Try parsing string values.
			if v.Type() == event.FieldTypeString {
				s := v.AsString()
				for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05"} {
					if t, err := time.Parse(layout, s); err == nil {
						return t
					}
				}
			}
		}
	}

	return time.Time{}
}

func rollupTimestampValue(v event.Value) time.Time {
	if ts, ok := v.TryAsTimestamp(); ok {
		return ts
	}

	return time.Time{}
}

// rollupValueKey returns a string key for a Value suitable for map/grouping.
func rollupValueKey(v event.Value) string {
	switch v.Type() {
	case event.FieldTypeString:
		return "s:" + v.AsString()
	case event.FieldTypeInt:
		return fmt.Sprintf("i:%d", v.AsInt())
	case event.FieldTypeFloat:
		return fmt.Sprintf("f:%f", v.AsFloat())
	case event.FieldTypeBool:
		return fmt.Sprintf("b:%t", v.AsBool())
	case event.FieldTypeTimestamp:
		return fmt.Sprintf("t:%d", v.AsInt())
	default:
		return "n:"
	}
}
