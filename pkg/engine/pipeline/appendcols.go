package pipeline

import (
	"context"
	"errors"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

// AppendcolsIterator appends subsearch fields to current rows by row position.
type AppendcolsIterator struct {
	child    Iterator
	sub      Iterator
	override bool
	maxout   int
	batch    int
	output   Iterator
	acct     memgov.MemoryAccount
}

// NewAppendcolsIterator creates a row-wise appendcols operator.
func NewAppendcolsIterator(child, sub Iterator, override bool, maxout int, batchSize int) *AppendcolsIterator {
	return NewAppendcolsIteratorWithBudget(child, sub, override, maxout, batchSize, memgov.NopAccount())
}

// NewAppendcolsIteratorWithBudget creates an appendcols operator that charges
// both materialized result sets to the given memory account.
func NewAppendcolsIteratorWithBudget(child, sub Iterator, override bool, maxout int, batchSize int, acct memgov.MemoryAccount) *AppendcolsIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	return &AppendcolsIterator{
		child:    child,
		sub:      sub,
		override: override,
		maxout:   maxout,
		batch:    batchSize,
		acct:     memgov.EnsureAccount(acct),
	}
}

func (a *AppendcolsIterator) Init(ctx context.Context) error {
	if err := a.child.Init(ctx); err != nil {
		return err
	}
	return a.sub.Init(ctx)
}

func (a *AppendcolsIterator) Next(ctx context.Context) (*Batch, error) {
	if a.output == nil {
		if err := a.materialize(ctx); err != nil {
			return nil, err
		}
	}
	return a.output.Next(ctx)
}

func (a *AppendcolsIterator) materialize(ctx context.Context) error {
	mainRows, err := CollectAllWithBudget(ctx, a.child, a.acct)
	if err != nil {
		return err
	}
	subRows, err := CollectAllWithBudget(ctx, a.sub, a.acct)
	if err != nil {
		return err
	}
	if a.maxout >= 0 && len(subRows) > a.maxout {
		subRows = subRows[:a.maxout]
	}

	out := cloneAppendcolsRows(mainRows)
	for i := range out {
		if i >= len(subRows) {
			break
		}
		for k, v := range subRows[i] {
			if strings.HasPrefix(k, "_") {
				continue
			}
			if _, exists := out[i][k]; !exists || a.override {
				out[i][k] = v
			}
		}
	}
	// Charge the merged clone set — it coexists with the collected inputs.
	if err := growRowsEstimate(a.acct, out); err != nil {
		return err
	}
	a.output = NewRowScanIterator(out, a.batch)
	return a.output.Init(ctx)
}

func (a *AppendcolsIterator) Close() error {
	var err error
	if a.output != nil {
		err = errors.Join(err, a.output.Close())
	}
	err = errors.Join(err, a.sub.Close())
	err = errors.Join(err, a.child.Close())
	a.acct.Close()
	return err
}

func (a *AppendcolsIterator) Schema() []FieldInfo {
	return nil
}

func cloneAppendcolsRows(rows []map[string]event.Value) []map[string]event.Value {
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
