package pipeline

import (
	"context"
	"fmt"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

const (
	estimatedXYSeriesRowBytes  int64 = 96
	estimatedXYSeriesCellBytes int64 = 64
)

// XYSeriesIterator implements pivot/crosstab transformation.
type XYSeriesIterator struct {
	child      Iterator
	xField     string
	yField     string
	valueField string
	rows       []map[string]event.Value
	emitted    bool
	offset     int
	batchSize  int
	acct       memgov.MemoryAccount // per-operator memory tracking
}

// NewXYSeriesIterator creates a pivot operator.
func NewXYSeriesIterator(child Iterator, xField, yField, valueField string, batchSize int) *XYSeriesIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	return &XYSeriesIterator{
		child:      child,
		xField:     xField,
		yField:     yField,
		valueField: valueField,
		batchSize:  batchSize,
		acct:       memgov.NopAccount(),
	}
}

// NewXYSeriesIteratorWithBudget creates a pivot operator with memory budget tracking.
func NewXYSeriesIteratorWithBudget(child Iterator, xField, yField, valueField string, batchSize int, acct memgov.MemoryAccount) *XYSeriesIterator {
	x := NewXYSeriesIterator(child, xField, yField, valueField, batchSize)
	x.acct = memgov.EnsureAccount(acct)

	return x
}

func (x *XYSeriesIterator) Init(ctx context.Context) error {
	return x.child.Init(ctx)
}

func (x *XYSeriesIterator) Next(ctx context.Context) (*Batch, error) {
	if !x.emitted {
		if err := x.materialize(ctx); err != nil {
			return nil, err
		}
	}
	if x.offset >= len(x.rows) {
		return nil, nil
	}
	end := x.offset + x.batchSize
	if end > len(x.rows) {
		end = len(x.rows)
	}
	batch := BatchFromRows(x.rows[x.offset:end])
	x.offset = end

	return batch, nil
}

func (x *XYSeriesIterator) Close() error {
	x.acct.Close()

	return x.child.Close()
}

// MemoryUsed returns the current tracked memory for this operator.
func (x *XYSeriesIterator) MemoryUsed() int64 {
	return x.acct.Used()
}

func (x *XYSeriesIterator) Schema() []FieldInfo { return nil }

func (x *XYSeriesIterator) materialize(ctx context.Context) error {
	xOrder := make([]string, 0)
	xSeen := make(map[string]bool)
	pivot := make(map[string]map[string]event.Value) // xVal -> {yVal -> value}

	for {
		batch, err := x.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			row := batch.Row(i)
			xv, yv, vv := "", "", event.NullValue()
			if v, ok := row[x.xField]; ok && !v.IsNull() {
				xv = v.String()
			}
			if v, ok := row[x.yField]; ok && !v.IsNull() {
				yv = v.String()
			}
			if v, ok := row[x.valueField]; ok {
				vv = v
			}
			if !xSeen[xv] {
				if err := x.acct.Grow(estimatedXYSeriesRowBytes + int64(len(xv))); err != nil {
					return fmt.Errorf("xyseries.materialize: row state: %w", err)
				}
				xSeen[xv] = true
				xOrder = append(xOrder, xv)
				pivot[xv] = make(map[string]event.Value)
			}
			if _, ok := pivot[xv][yv]; !ok {
				if err := x.acct.Grow(estimateXYSeriesCellBytes(yv, vv)); err != nil {
					return fmt.Errorf("xyseries.materialize: cell state: %w", err)
				}
			}
			pivot[xv][yv] = vv
		}
	}

	// Pivot: group by xField, spread yField values as columns.
	for _, xv := range xOrder {
		row := make(map[string]event.Value)
		row[x.xField] = event.StringValue(xv)
		for yv, val := range pivot[xv] {
			row[yv] = val
		}
		x.rows = append(x.rows, row)
	}
	x.emitted = true

	return nil
}

func estimateXYSeriesCellBytes(yVal string, val event.Value) int64 {
	size := estimatedXYSeriesCellBytes + int64(len(yVal))
	if val.Type() == event.FieldTypeString {
		size += int64(len(val.String()))
	}

	return size
}
