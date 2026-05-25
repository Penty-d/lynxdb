package pipeline

import (
	"context"
	"errors"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

type appendpipeBuildFunc func(Iterator, []spl2.Command) (Iterator, error)

// AppendpipeIterator appends the result of a subpipe run over the current rows.
type AppendpipeIterator struct {
	child    Iterator
	commands []spl2.Command
	build    appendpipeBuildFunc
	batch    int
	output   Iterator
}

// NewAppendpipeIterator creates an appendpipe operator.
func NewAppendpipeIterator(child Iterator, commands []spl2.Command, batchSize int, build appendpipeBuildFunc) *AppendpipeIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	return &AppendpipeIterator{child: child, commands: commands, build: build, batch: batchSize}
}

func (a *AppendpipeIterator) Init(ctx context.Context) error {
	return a.child.Init(ctx)
}

func (a *AppendpipeIterator) Next(ctx context.Context) (*Batch, error) {
	if a.output == nil {
		if err := a.materialize(ctx); err != nil {
			return nil, err
		}
	}
	return a.output.Next(ctx)
}

func (a *AppendpipeIterator) materialize(ctx context.Context) error {
	rows, err := CollectAll(ctx, a.child)
	if err != nil {
		return err
	}
	original := NewRowScanIterator(cloneAppendpipeRows(rows), a.batch)
	subSource := NewRowScanIterator(cloneAppendpipeRows(rows), a.batch)
	subIter, err := a.build(subSource, a.commands)
	if err != nil {
		return err
	}
	a.output = NewUnionIterator([]Iterator{original, subIter})
	return a.output.Init(ctx)
}

func (a *AppendpipeIterator) Close() error {
	var err error
	if a.output != nil {
		err = errors.Join(err, a.output.Close())
	}
	err = errors.Join(err, a.child.Close())
	return err
}

func (a *AppendpipeIterator) Schema() []FieldInfo {
	return nil
}

func cloneAppendpipeRows(rows []map[string]event.Value) []map[string]event.Value {
	out := make([]map[string]event.Value, len(rows))
	for i, row := range rows {
		cp := make(map[string]event.Value, len(row))
		for k, v := range row {
			cp[k] = v
		}
		out[i] = cp
	}
	return out
}
