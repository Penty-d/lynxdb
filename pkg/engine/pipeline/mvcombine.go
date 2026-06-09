package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

// MvcombineIterator merges rows that differ only by one field.
type MvcombineIterator struct {
	child     Iterator
	field     string
	batchSize int
	rows      []map[string]event.Value
	offset    int
	built     bool
	acct      memgov.MemoryAccount
}

// NewMvcombineIterator creates a blocking mvcombine operator.
func NewMvcombineIterator(child Iterator, field string, batchSize int) *MvcombineIterator {
	return NewMvcombineIteratorWithBudget(child, field, batchSize, memgov.NopAccount())
}

// NewMvcombineIteratorWithBudget creates an mvcombine operator that charges
// its group state to the given memory account.
func NewMvcombineIteratorWithBudget(child Iterator, field string, batchSize int, acct memgov.MemoryAccount) *MvcombineIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	return &MvcombineIterator{child: child, field: field, batchSize: batchSize, acct: memgov.EnsureAccount(acct)}
}

func (m *MvcombineIterator) Init(ctx context.Context) error {
	return m.child.Init(ctx)
}

func (m *MvcombineIterator) Next(ctx context.Context) (*Batch, error) {
	if !m.built {
		if err := m.materialize(ctx); err != nil {
			return nil, err
		}
	}
	if m.offset >= len(m.rows) {
		return nil, nil
	}
	end := m.offset + m.batchSize
	if end > len(m.rows) {
		end = len(m.rows)
	}
	batch := BatchFromRows(m.rows[m.offset:end])
	m.offset = end

	return batch, nil
}

func (m *MvcombineIterator) Close() error {
	m.acct.Close()

	return m.child.Close()
}

func (m *MvcombineIterator) Schema() []FieldInfo {
	return m.child.Schema()
}

func (m *MvcombineIterator) materialize(ctx context.Context) error {
	type group struct {
		row    map[string]event.Value
		values []string
	}
	groups := make(map[string]*group)
	order := make([]string, 0)

	for {
		batch, err := m.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			row := batch.Row(i)
			key := mvcombineGroupKey(row, m.field)
			g, ok := groups[key]
			if !ok {
				if err := m.acct.Grow(EstimateRowBytes(row) + int64(len(key))); err != nil {
					return fmt.Errorf("mvcombine: memory budget exceeded: %w", err)
				}
				g = &group{row: cloneRow(row)}
				groups[key] = g
				order = append(order, key)
			}
			if value, ok := row[m.field]; ok && !value.IsNull() {
				vals := splitInternalMultivalue(value.String())
				var valBytes int64
				for _, v := range vals {
					valBytes += int64(len(v))
				}
				if err := m.acct.Grow(valBytes); err != nil {
					return fmt.Errorf("mvcombine: memory budget exceeded: %w", err)
				}
				g.values = append(g.values, vals...)
			}
		}
	}

	m.rows = make([]map[string]event.Value, 0, len(order))
	for _, key := range order {
		g := groups[key]
		g.row[m.field] = event.StringValue(strings.Join(g.values, "|||"))
		m.rows = append(m.rows, g.row)
	}
	m.built = true

	return nil
}

func mvcombineGroupKey(row map[string]event.Value, field string) string {
	names := make([]string, 0, len(row))
	for name := range row {
		if name != field {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		value := row[name]
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(value.Type().String())
		b.WriteByte(':')
		b.WriteString(value.String())
		b.WriteByte('\x00')
	}

	return b.String()
}

func splitInternalMultivalue(raw string) []string {
	if strings.Contains(raw, "|||") {
		return strings.Split(raw, "|||")
	}

	return []string{raw}
}
