package pipeline

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

const estimatedRollupGroupBytes int64 = 128

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
	var keyBytes int64 = int64(len(key))
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
	r.acct.Close()

	return r.child.Close()
}

// MemoryUsed returns the current tracked memory for this operator.
func (r *RollupIterator) MemoryUsed() int64 { return r.acct.Used() }

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
