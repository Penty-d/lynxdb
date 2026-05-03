package pipeline

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

const estimatedTransactionGroupBytes int64 = 96
const transactionSpillPartitions = 64

const (
	transactionSpillOrderField = "__lynxdb_internal_transaction_order"
	transactionSpillKeyField   = "__lynxdb_internal_transaction_key"
)

type transactionGroup struct {
	events     []map[string]event.Value
	order      int64
	rowBytes   int64
	stateBytes int64
}

type transactionOutput struct {
	order int64
	row   map[string]event.Value
}

// TransactionIterator groups events by a field with maxspan/startswith/endswith.
type TransactionIterator struct {
	child           Iterator
	field           string
	maxSpan         time.Duration
	startsWith      string
	endsWith        string
	rows            []map[string]event.Value
	emitted         bool
	offset          int
	batchSize       int
	acct            memgov.MemoryAccount // per-operator memory tracking
	spillMgr        *SpillManager
	spillPaths      []string
	spilledRows     int64
	spillBytesTotal int64
}

// NewTransactionIterator creates a transaction grouping operator.
func NewTransactionIterator(child Iterator, field string, maxSpan time.Duration, startsWith, endsWith string, batchSize int) *TransactionIterator {
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	return &TransactionIterator{
		child:      child,
		field:      field,
		maxSpan:    maxSpan,
		startsWith: startsWith,
		endsWith:   endsWith,
		batchSize:  batchSize,
		acct:       memgov.NopAccount(),
	}
}

// NewTransactionIteratorWithBudget creates a transaction operator with memory budget tracking.
func NewTransactionIteratorWithBudget(child Iterator, field string, maxSpan time.Duration, startsWith, endsWith string, batchSize int, acct memgov.MemoryAccount) *TransactionIterator {
	t := NewTransactionIterator(child, field, maxSpan, startsWith, endsWith, batchSize)
	t.acct = memgov.EnsureAccount(acct)

	return t
}

// NewTransactionIteratorWithSpill creates a transaction operator with budget
// tracking and a hash-partitioned spill fallback when grouped rows exceed memory.
func NewTransactionIteratorWithSpill(child Iterator, field string, maxSpan time.Duration, startsWith, endsWith string, batchSize int, acct memgov.MemoryAccount, mgr *SpillManager) *TransactionIterator {
	t := NewTransactionIteratorWithBudget(child, field, maxSpan, startsWith, endsWith, batchSize, acct)
	t.spillMgr = mgr

	return t
}

func (t *TransactionIterator) Init(ctx context.Context) error {
	return t.child.Init(ctx)
}

func (t *TransactionIterator) Next(ctx context.Context) (*Batch, error) {
	if !t.emitted {
		if err := t.materialize(ctx); err != nil {
			return nil, err
		}
	}
	if t.offset >= len(t.rows) {
		return nil, nil
	}
	end := t.offset + t.batchSize
	if end > len(t.rows) {
		end = len(t.rows)
	}
	batch := BatchFromRows(t.rows[t.offset:end])
	t.offset = end

	return batch, nil
}

func (t *TransactionIterator) Close() error {
	t.acct.Close()
	if t.spillMgr != nil {
		t.spillBytesTotal = sumSpillPathBytes(t.spillPaths)
		for _, path := range t.spillPaths {
			t.spillMgr.Release(path)
		}
	}
	t.spillPaths = nil

	return t.child.Close()
}

// MemoryUsed returns the current tracked memory for this operator.
func (t *TransactionIterator) MemoryUsed() int64 {
	return t.acct.Used()
}

// ResourceStats implements ResourceReporter for per-operator spill metrics.
func (t *TransactionIterator) ResourceStats() OperatorResourceStats {
	spillBytes := t.spillBytesTotal
	if spillBytes == 0 {
		spillBytes = sumSpillPathBytes(t.spillPaths)
	}

	return OperatorResourceStats{
		PeakBytes:   t.acct.MaxUsed(),
		SpilledRows: t.spilledRows,
		SpillBytes:  spillBytes,
	}
}

func (t *TransactionIterator) Schema() []FieldInfo { return nil }

func (t *TransactionIterator) materialize(ctx context.Context) error {
	// Collect all events grouped by field value.
	groups := make(map[string]*transactionGroup)
	groupOrder := make([]string, 0)
	nextOrder := int64(0)

	for {
		batch, err := t.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		batchRows := make([]map[string]event.Value, batch.Len)
		for i := 0; i < batch.Len; i++ {
			batchRows[i] = batch.Row(i)
		}

		for i, row := range batchRows {
			rowBytes := EstimateRowBytes(row)
			if err := t.acct.Grow(rowBytes); err != nil {
				if t.spillMgr != nil {
					return t.spillAndMaterialize(ctx, groups, groupOrder, batchRows[i:])
				}

				return fmt.Errorf("transaction.materialize: %w", err)
			}

			key := ""
			if v, ok := row[t.field]; ok {
				key = v.String()
			}
			g, ok := groups[key]
			if !ok {
				stateBytes := estimatedTransactionGroupBytes + int64(len(key))
				if err := t.acct.Grow(stateBytes); err != nil {
					t.acct.Shrink(rowBytes)
					if t.spillMgr != nil {
						return t.spillAndMaterialize(ctx, groups, groupOrder, batchRows[i:])
					}

					return fmt.Errorf("transaction.materialize: group state: %w", err)
				}
				g = &transactionGroup{order: nextOrder, stateBytes: stateBytes}
				nextOrder++
				groups[key] = g
				groupOrder = append(groupOrder, key)
			}
			g.events = append(g.events, row)
			g.rowBytes += rowBytes
		}
	}

	outputs := make([]transactionOutput, 0, len(groupOrder))
	for _, key := range groupOrder {
		g := groups[key]
		if err := t.appendTransactionOutput(&outputs, key, g); err != nil {
			return err
		}
		t.releaseTransactionGroup(g)
	}
	t.sortAndStoreOutputs(outputs)
	t.emitted = true

	return nil
}

func (t *TransactionIterator) spillAndMaterialize(ctx context.Context, groups map[string]*transactionGroup, groupOrder []string, overflowRows []map[string]event.Value) error {
	writers := make([]*ColumnarSpillWriter, transactionSpillPartitions)
	for i := range writers {
		sw, err := NewColumnarSpillWriter(t.spillMgr, fmt.Sprintf("transaction-%02d", i))
		if err != nil {
			return fmt.Errorf("transaction.spill: create partition: %w", err)
		}
		writers[i] = sw
	}

	orderByKey := make(map[string]int64, len(groups))
	nextOrder := int64(len(groupOrder))
	for _, key := range groupOrder {
		g := groups[key]
		orderByKey[key] = g.order
		for _, row := range g.events {
			if err := t.writeTransactionSpillRow(writers, key, g.order, row); err != nil {
				return err
			}
		}
		t.releaseTransactionGroup(g)
	}
	groups = nil

	writeRows := func(rows []map[string]event.Value) error {
		for _, row := range rows {
			key := ""
			if v, ok := row[t.field]; ok {
				key = v.String()
			}
			order, ok := orderByKey[key]
			if !ok {
				order = nextOrder
				nextOrder++
				orderByKey[key] = order
			}
			if err := t.writeTransactionSpillRow(writers, key, order, row); err != nil {
				return err
			}
		}

		return nil
	}

	if err := writeRows(overflowRows); err != nil {
		return err
	}
	for {
		batch, err := t.child.Next(ctx)
		if err != nil {
			return fmt.Errorf("transaction.spill: read child: %w", err)
		}
		if batch == nil {
			break
		}
		rows := make([]map[string]event.Value, batch.Len)
		for i := 0; i < batch.Len; i++ {
			rows[i] = batch.Row(i)
		}
		if err := writeRows(rows); err != nil {
			return err
		}
	}

	t.spillPaths = make([]string, len(writers))
	for i, sw := range writers {
		t.spillPaths[i] = sw.Path()
		if err := sw.CloseFile(); err != nil {
			return fmt.Errorf("transaction.spill: close partition %d: %w", i, err)
		}
	}

	var outputs []transactionOutput
	for _, path := range t.spillPaths {
		partOutputs, err := t.processTransactionPartition(path)
		if err != nil {
			return err
		}
		outputs = append(outputs, partOutputs...)
	}
	t.sortAndStoreOutputs(outputs)
	t.emitted = true

	return nil
}

func (t *TransactionIterator) writeTransactionSpillRow(writers []*ColumnarSpillWriter, key string, order int64, row map[string]event.Value) error {
	p := hashPartition(key, len(writers))
	spillRow := make(map[string]event.Value, len(row)+2)
	for k, v := range row {
		spillRow[k] = v
	}
	spillRow[transactionSpillKeyField] = event.StringValue(key)
	spillRow[transactionSpillOrderField] = event.IntValue(order)
	if err := writers[p].WriteRow(spillRow); err != nil {
		return fmt.Errorf("transaction.spill: write row: %w", err)
	}
	t.spilledRows++

	return nil
}

func (t *TransactionIterator) processTransactionPartition(path string) ([]transactionOutput, error) {
	reader, err := NewColumnarSpillReader(path)
	if err != nil {
		return nil, fmt.Errorf("transaction.spill: open partition: %w", err)
	}
	defer reader.Close()

	groups := make(map[string]*transactionGroup)
	for {
		row, readErr := reader.ReadRow()
		if readErr != nil {
			if io.EOF == readErr {
				break
			}

			return nil, fmt.Errorf("transaction.spill: read partition: %w", readErr)
		}
		key := ""
		if v, ok := row[transactionSpillKeyField]; ok {
			key = v.String()
		}
		order := int64(0)
		if v, ok := row[transactionSpillOrderField]; ok {
			if n, nok := v.TryAsInt(); nok {
				order = n
			}
		}
		delete(row, transactionSpillKeyField)
		delete(row, transactionSpillOrderField)

		rowBytes := EstimateRowBytes(row)
		if err := t.acct.Grow(rowBytes); err != nil {
			return nil, fmt.Errorf("transaction.spill: partition row memory: %w", err)
		}
		g, ok := groups[key]
		if !ok {
			stateBytes := estimatedTransactionGroupBytes + int64(len(key))
			if err := t.acct.Grow(stateBytes); err != nil {
				t.acct.Shrink(rowBytes)

				return nil, fmt.Errorf("transaction.spill: partition group memory: %w", err)
			}
			g = &transactionGroup{order: order, stateBytes: stateBytes}
			groups[key] = g
		}
		g.events = append(g.events, row)
		g.rowBytes += rowBytes
		if order < g.order {
			g.order = order
		}
	}

	outputs := make([]transactionOutput, 0, len(groups))
	for key, g := range groups {
		if err := t.appendTransactionOutput(&outputs, key, g); err != nil {
			return nil, err
		}
		t.releaseTransactionGroup(g)
	}

	return outputs, nil
}

func (t *TransactionIterator) appendTransactionOutput(outputs *[]transactionOutput, key string, g *transactionGroup) error {
	txRow := t.buildTransactionRow(key, g.events)
	if txRow == nil {
		return nil
	}
	if err := t.acct.Grow(EstimateRowBytes(txRow)); err != nil {
		return fmt.Errorf("transaction.materialize: output row: %w", err)
	}
	*outputs = append(*outputs, transactionOutput{order: g.order, row: txRow})

	return nil
}

func (t *TransactionIterator) buildTransactionRow(key string, events []map[string]event.Value) map[string]event.Value {
	if len(events) == 0 {
		return nil
	}

	// Apply maxspan filter.
	if t.maxSpan > 0 && len(events) >= 2 {
		firstTime := getTime(events[0])
		lastTime := getTime(events[len(events)-1])
		if lastTime.Sub(firstTime) > t.maxSpan {
			return nil
		}
	}

	txRow := make(map[string]event.Value)
	txRow[t.field] = event.StringValue(key)
	txRow["eventcount"] = event.IntValue(int64(len(events)))

	// Duration.
	if len(events) >= 2 {
		firstTime := getTime(events[0])
		lastTime := getTime(events[len(events)-1])
		dur := lastTime.Sub(firstTime).Seconds()
		txRow["duration"] = event.FloatValue(dur)
	} else {
		txRow["duration"] = event.FloatValue(0)
	}

	// Merge _raw.
	var raws []string
	for _, e := range events {
		if r, ok := e["_raw"]; ok && !r.IsNull() {
			raws = append(raws, r.String())
		}
	}
	txRow["_raw"] = event.StringValue(strings.Join(raws, "\n"))

	// Copy first event's _time.
	if ts, ok := events[0]["_time"]; ok {
		txRow["_time"] = ts
	}

	// Copy all other fields from first event.
	for k, v := range events[0] {
		if k == transactionSpillKeyField || k == transactionSpillOrderField {
			continue
		}
		if _, exists := txRow[k]; !exists {
			txRow[k] = v
		}
	}

	return txRow
}

func (t *TransactionIterator) releaseTransactionGroup(g *transactionGroup) {
	if g == nil {
		return
	}
	t.acct.Shrink(g.rowBytes + g.stateBytes)
}

func (t *TransactionIterator) sortAndStoreOutputs(outputs []transactionOutput) {
	sort.SliceStable(outputs, func(i, j int) bool {
		return outputs[i].order < outputs[j].order
	})
	t.rows = make([]map[string]event.Value, 0, len(outputs))
	for _, out := range outputs {
		t.rows = append(t.rows, out.row)
	}
}

func getTime(row map[string]event.Value) time.Time {
	if v, ok := row["_time"]; ok {
		if t, tok := v.TryAsTimestamp(); tok {
			return t
		}
	}

	return time.Time{}
}
