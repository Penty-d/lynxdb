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

const (
	estimatedXYSeriesRowBytes  int64 = 96
	estimatedXYSeriesCellBytes int64 = 64
)

const xySeriesSpillPartitions = 64

const (
	xySeriesSpillX     = "__lynxdb_internal_xyseries_x"
	xySeriesSpillY     = "__lynxdb_internal_xyseries_y"
	xySeriesSpillValue = "__lynxdb_internal_xyseries_value"
	xySeriesSpillOrder = "__lynxdb_internal_xyseries_order"
)

type xySeriesOutput struct {
	order int64
	row   map[string]event.Value
}

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

	spillMgr        *SpillManager
	spillPaths      []string
	spilledRows     int64
	spillBytesTotal int64
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

// NewXYSeriesIteratorWithSpill creates a pivot operator with budget tracking
// and a partitioned columnar spill fallback for high-cardinality pivots.
func NewXYSeriesIteratorWithSpill(child Iterator, xField, yField, valueField string, batchSize int, acct memgov.MemoryAccount, mgr *SpillManager) *XYSeriesIterator {
	x := NewXYSeriesIteratorWithBudget(child, xField, yField, valueField, batchSize, acct)
	x.spillMgr = mgr

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
	var errs []error

	x.acct.Close()
	if x.spillMgr != nil {
		x.spillBytesTotal = sumSpillPathBytes(x.spillPaths)
		for _, path := range x.spillPaths {
			x.spillMgr.Release(path)
		}
	}
	x.spillPaths = nil

	if err := x.child.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// MemoryUsed returns the current tracked memory for this operator.
func (x *XYSeriesIterator) MemoryUsed() int64 {
	return x.acct.Used()
}

// ResourceStats implements ResourceReporter for spill observability.
func (x *XYSeriesIterator) ResourceStats() OperatorResourceStats {
	spillBytes := x.spillBytesTotal
	if spillBytes == 0 {
		spillBytes = sumSpillPathBytes(x.spillPaths)
	}

	return OperatorResourceStats{
		PeakBytes:   x.acct.MaxUsed(),
		SpilledRows: x.spilledRows,
		SpillBytes:  spillBytes,
	}
}

func (x *XYSeriesIterator) Schema() []FieldInfo { return nil }

func (x *XYSeriesIterator) materialize(ctx context.Context) error {
	xOrder := make([]string, 0)
	xSeen := make(map[string]bool)
	xOrderByValue := make(map[string]int64)
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
					if x.spillMgr != nil {
						return x.spillAndMaterialize(ctx, pivot, xOrder, xOrderByValue, batch, i)
					}

					return fmt.Errorf("xyseries.materialize: row state: %w", err)
				}
				xSeen[xv] = true
				xOrderByValue[xv] = int64(len(xOrder))
				xOrder = append(xOrder, xv)
				pivot[xv] = make(map[string]event.Value)
			}
			if _, ok := pivot[xv][yv]; !ok {
				if err := x.acct.Grow(estimateXYSeriesCellBytes(yv, vv)); err != nil {
					if x.spillMgr != nil {
						return x.spillAndMaterialize(ctx, pivot, xOrder, xOrderByValue, batch, i)
					}

					return fmt.Errorf("xyseries.materialize: cell state: %w", err)
				}
			}
			pivot[xv][yv] = vv
		}
	}

	x.materializeRowsFromPivot(pivot, xOrder)
	x.emitted = true

	return nil
}

func (x *XYSeriesIterator) materializeRowsFromPivot(pivot map[string]map[string]event.Value, xOrder []string) {
	for _, xv := range xOrder {
		row := make(map[string]event.Value)
		row[x.xField] = event.StringValue(xv)
		for yv, val := range pivot[xv] {
			row[yv] = val
		}
		x.rows = append(x.rows, row)
	}
}

func (x *XYSeriesIterator) spillAndMaterialize(ctx context.Context, pivot map[string]map[string]event.Value, xOrder []string, xOrderByValue map[string]int64, overflowBatch *Batch, overflowOffset int) error {
	writers := make([]*ColumnarSpillWriter, xySeriesSpillPartitions)
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		for _, sw := range writers {
			if sw != nil {
				_ = sw.CloseFile()
				if x.spillMgr != nil {
					x.spillMgr.Release(sw.Path())
				}
			}
		}
	}()

	for i := range writers {
		sw, err := NewColumnarSpillWriter(x.spillMgr, fmt.Sprintf("xyseries-%02d", i))
		if err != nil {
			return fmt.Errorf("xyseries.spill: create partition: %w", err)
		}
		writers[i] = sw
	}

	for _, xv := range xOrder {
		for yv, val := range pivot[xv] {
			if err := x.writeXYSeriesSpillCell(writers, xv, yv, val, xOrderByValue[xv]); err != nil {
				return err
			}
		}
	}
	x.acct.Shrink(x.acct.Used())

	writeBatchRows := func(batch *Batch, start int) error {
		for i := start; i < batch.Len; i++ {
			xv, yv, vv := x.xyValues(batch.Row(i))
			order, ok := xOrderByValue[xv]
			if !ok {
				order = int64(len(xOrderByValue))
				xOrderByValue[xv] = order
			}
			if err := x.writeXYSeriesSpillCell(writers, xv, yv, vv, order); err != nil {
				return err
			}
		}

		return nil
	}

	if err := writeBatchRows(overflowBatch, overflowOffset); err != nil {
		return err
	}
	for {
		batch, err := x.child.Next(ctx)
		if err != nil {
			return fmt.Errorf("xyseries.spill: read child: %w", err)
		}
		if batch == nil {
			break
		}
		if err := writeBatchRows(batch, 0); err != nil {
			return err
		}
	}

	x.spillPaths = make([]string, len(writers))
	for i, sw := range writers {
		x.spillPaths[i] = sw.Path()
		if err := sw.CloseFile(); err != nil {
			return fmt.Errorf("xyseries.spill: close partition %d: %w", i, err)
		}
		writers[i] = nil
	}

	var outputs []xySeriesOutput
	for _, path := range x.spillPaths {
		partOutputs, err := x.processXYSeriesPartition(path)
		if err != nil {
			return err
		}
		outputs = append(outputs, partOutputs...)
	}
	sort.SliceStable(outputs, func(i, j int) bool {
		return outputs[i].order < outputs[j].order
	})
	x.rows = make([]map[string]event.Value, 0, len(outputs))
	for _, out := range outputs {
		x.rows = append(x.rows, out.row)
	}
	x.emitted = true
	cleanup = false

	return nil
}

func (x *XYSeriesIterator) xyValues(row map[string]event.Value) (string, string, event.Value) {
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

	return xv, yv, vv
}

func (x *XYSeriesIterator) writeXYSeriesSpillCell(writers []*ColumnarSpillWriter, xv, yv string, vv event.Value, order int64) error {
	p := hashPartition(xv, len(writers))
	row := map[string]event.Value{
		xySeriesSpillX:     event.StringValue(xv),
		xySeriesSpillY:     event.StringValue(yv),
		xySeriesSpillValue: vv,
		xySeriesSpillOrder: event.IntValue(order),
	}
	if err := writers[p].WriteRow(row); err != nil {
		return fmt.Errorf("xyseries.spill: write cell: %w", err)
	}
	x.spilledRows++

	return nil
}

func (x *XYSeriesIterator) processXYSeriesPartition(path string) ([]xySeriesOutput, error) {
	reader, err := NewColumnarSpillReader(path)
	if err != nil {
		return nil, fmt.Errorf("xyseries.spill: open partition: %w", err)
	}
	defer reader.Close()

	xOrder := make([]string, 0)
	xOrderByValue := make(map[string]int64)
	pivot := make(map[string]map[string]event.Value)
	var tracked int64

	for {
		row, readErr := reader.ReadRow()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}

			return nil, fmt.Errorf("xyseries.spill: read partition: %w", readErr)
		}
		xv, yv, vv := "", "", event.NullValue()
		if v, ok := row[xySeriesSpillX]; ok {
			xv = v.String()
		}
		if v, ok := row[xySeriesSpillY]; ok {
			yv = v.String()
		}
		if v, ok := row[xySeriesSpillValue]; ok {
			vv = v
		}
		order := int64(0)
		if v, ok := row[xySeriesSpillOrder]; ok {
			if n, nok := v.TryAsInt(); nok {
				order = n
			}
		}

		if _, ok := pivot[xv]; !ok {
			rowBytes := estimatedXYSeriesRowBytes + int64(len(xv))
			if err := x.acct.Grow(rowBytes); err != nil {
				x.acct.Shrink(tracked)

				return nil, fmt.Errorf("xyseries.spill: partition row state: %w", err)
			}
			tracked += rowBytes
			pivot[xv] = make(map[string]event.Value)
			xOrder = append(xOrder, xv)
			xOrderByValue[xv] = order
		}
		if _, ok := pivot[xv][yv]; !ok {
			cellBytes := estimateXYSeriesCellBytes(yv, vv)
			if err := x.acct.Grow(cellBytes); err != nil {
				x.acct.Shrink(tracked)

				return nil, fmt.Errorf("xyseries.spill: partition cell state: %w", err)
			}
			tracked += cellBytes
		}
		pivot[xv][yv] = vv
		if order < xOrderByValue[xv] {
			xOrderByValue[xv] = order
		}
	}

	outputs := make([]xySeriesOutput, 0, len(xOrder))
	for _, xv := range xOrder {
		row := make(map[string]event.Value)
		row[x.xField] = event.StringValue(xv)
		for yv, val := range pivot[xv] {
			row[yv] = val
		}
		outputs = append(outputs, xySeriesOutput{order: xOrderByValue[xv], row: row})
	}
	x.acct.Shrink(tracked)

	return outputs, nil
}

func estimateXYSeriesCellBytes(yVal string, val event.Value) int64 {
	size := estimatedXYSeriesCellBytes + int64(len(yVal))
	if val.Type() == event.FieldTypeString {
		size += int64(len(val.String()))
	}

	return size
}
