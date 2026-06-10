package physical

import (
	"context"
	"fmt"
	"sort"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
)

// DescribeSummaryIterator drains its input and emits one row per field with
// field name, type, coverage, estimated distinct count, and top values.
// This implements the RFC-002 §7.4 describe semantics.
type DescribeSummaryIterator struct {
	child     pipeline.Iterator
	batchSize int
	done      bool
}

// NewDescribeSummaryIterator creates a describe operator that produces summary rows.
func NewDescribeSummaryIterator(child pipeline.Iterator, batchSize int) *DescribeSummaryIterator {
	if batchSize <= 0 {
		batchSize = pipeline.DefaultBatchSize
	}
	return &DescribeSummaryIterator{child: child, batchSize: batchSize}
}

func (d *DescribeSummaryIterator) Init(ctx context.Context) error {
	return d.child.Init(ctx)
}

// maxDistinctCap is the maximum number of distinct values tracked per field.
const maxDistinctCap = 10000

// topValuesN is the number of top values to include in output.
const topValuesN = 5

type descFieldState struct {
	nonNull int
	total   int
	types   map[string]int
	values  map[string]int
}

func (d *DescribeSummaryIterator) Next(ctx context.Context) (*pipeline.Batch, error) {
	if d.done {
		return nil, nil
	}
	d.done = true

	fields := make(map[string]*descFieldState)
	totalRows := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := d.child.Next(ctx)
		if err != nil {
			return nil, err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			totalRows++
			for name, col := range batch.Columns {
				fs, ok := fields[name]
				if !ok {
					fs = &descFieldState{
						types:  make(map[string]int),
						values: make(map[string]int),
					}
					fields[name] = fs
				}
				fs.total++
				if i < len(col) && !col[i].IsNull() {
					fs.nonNull++
					fs.types[col[i].Type().String()]++
					if len(fs.values) < maxDistinctCap {
						fs.values[col[i].String()]++
					}
				}
			}
		}
	}

	if len(fields) == 0 {
		return nil, nil
	}

	// Sort field names for deterministic output.
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)

	result := pipeline.NewBatch(len(names))
	for _, name := range names {
		fs := fields[name]

		// Type: most frequent type.
		typ := "unknown"
		maxCount := 0
		for t, c := range fs.types {
			if c > maxCount {
				maxCount = c
				typ = t
			}
		}

		// Coverage.
		coverage := 0.0
		if totalRows > 0 {
			coverage = float64(fs.nonNull) / float64(totalRows)
		}

		// Distinct estimate.
		distinctEst := int64(len(fs.values))

		// Top values: up to topValuesN most frequent.
		type valCount struct {
			val   string
			count int
		}
		sorted := make([]valCount, 0, len(fs.values))
		for v, c := range fs.values {
			sorted = append(sorted, valCount{v, c})
		}
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].count != sorted[j].count {
				return sorted[i].count > sorted[j].count
			}
			return sorted[i].val < sorted[j].val
		})
		topN := topValuesN
		if topN > len(sorted) {
			topN = len(sorted)
		}
		topVals := make([]event.Value, topN)
		for i := 0; i < topN; i++ {
			topVals[i] = event.StringValue(fmt.Sprintf("%s(%d)", sorted[i].val, sorted[i].count))
		}

		row := map[string]event.Value{
			"field":        event.StringValue(name),
			"type":         event.StringValue(typ),
			"coverage":     event.FloatValue(coverage),
			"distinct_est": event.IntValue(distinctEst),
			"top_values":   event.ArrayValue(topVals),
		}
		result.AddRow(row)
	}

	return result, nil
}

func (d *DescribeSummaryIterator) Close() error {
	return d.child.Close()
}

func (d *DescribeSummaryIterator) Schema() []pipeline.FieldInfo {
	return []pipeline.FieldInfo{
		{Name: "field", Type: "string"},
		{Name: "type", Type: "string"},
		{Name: "coverage", Type: "float"},
		{Name: "distinct_est", Type: "int"},
		{Name: "top_values", Type: "array"},
	}
}
