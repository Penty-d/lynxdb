package pipeline

import (
	"context"

	"github.com/lynxbase/lynxdb/pkg/event"
)

// UntableIterator converts wide rows into name/value rows.
type UntableIterator struct {
	child      Iterator
	xField     string
	yNameField string
	yDataField string
	batchSize  int
	pending    []map[string]event.Value
	offset     int
}

// NewUntableIterator creates an unpivot operator.
func NewUntableIterator(child Iterator, xField, yNameField, yDataField string, batchSize int) *UntableIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	return &UntableIterator{
		child:      child,
		xField:     xField,
		yNameField: yNameField,
		yDataField: yDataField,
		batchSize:  batchSize,
	}
}

func (u *UntableIterator) Init(ctx context.Context) error {
	return u.child.Init(ctx)
}

func (u *UntableIterator) Next(ctx context.Context) (*Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if u.offset < len(u.pending) {
		return u.emitPending(), nil
	}

	for {
		batch, err := u.child.Next(ctx)
		if err != nil {
			return nil, err
		}
		if batch == nil {
			return nil, nil
		}

		u.pending = u.unpivotBatch(batch)
		u.offset = 0
		if len(u.pending) > 0 {
			return u.emitPending(), nil
		}
	}
}

func (u *UntableIterator) Close() error {
	return u.child.Close()
}

func (u *UntableIterator) Schema() []FieldInfo {
	return []FieldInfo{
		{Name: u.xField, Type: "any"},
		{Name: u.yNameField, Type: "string"},
		{Name: u.yDataField, Type: "any"},
	}
}

func (u *UntableIterator) emitPending() *Batch {
	end := u.offset + u.batchSize
	if end > len(u.pending) {
		end = len(u.pending)
	}
	batch := BatchFromRows(u.pending[u.offset:end])
	u.offset = end
	if u.offset >= len(u.pending) {
		u.pending = nil
		u.offset = 0
	}

	return batch
}

func (u *UntableIterator) unpivotBatch(batch *Batch) []map[string]event.Value {
	columns := batch.ColumnNames()
	rows := make([]map[string]event.Value, 0, batch.Len*max(0, len(columns)-1))
	for i := 0; i < batch.Len; i++ {
		xValue := batch.Value(u.xField, i)
		for _, column := range columns {
			if column == u.xField {
				continue
			}
			rows = append(rows, map[string]event.Value{
				u.xField:     xValue,
				u.yNameField: event.StringValue(column),
				u.yDataField: batch.Value(column, i),
			})
		}
	}

	return rows
}
