package pipeline

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
	"github.com/lynxbase/lynxdb/pkg/vm"
)

const estimatedCorrelationPairBytes int64 = 16

// CorrelateIterator computes correlation between two numeric fields and emits
// a single result row. Pearson correlation is streaming; Spearman still needs
// rank materialization.
type CorrelateIterator struct {
	child  Iterator
	field1 string
	field2 string
	method string // "pearson" (default) or "spearman"

	done   bool
	output *Batch
	offset int
	acct   memgov.MemoryAccount
}

// NewCorrelateIterator creates a new correlate iterator.
func NewCorrelateIterator(child Iterator, field1, field2, method string) *CorrelateIterator {
	return NewCorrelateIteratorWithBudget(child, field1, field2, method, memgov.NopAccount())
}

// NewCorrelateIteratorWithBudget creates a correlate iterator with memory
// accounting for rank materialization used by Spearman correlation.
func NewCorrelateIteratorWithBudget(child Iterator, field1, field2, method string, acct memgov.MemoryAccount) *CorrelateIterator {
	return &CorrelateIterator{
		child:  child,
		field1: field1,
		field2: field2,
		method: method,
		acct:   memgov.EnsureAccount(acct),
	}
}

func (c *CorrelateIterator) Init(ctx context.Context) error {
	return c.child.Init(ctx)
}

func (c *CorrelateIterator) Next(ctx context.Context) (*Batch, error) {
	if !c.done {
		if err := c.materialize(ctx); err != nil {
			return nil, err
		}
		c.done = true
	}
	if c.output == nil || c.offset >= c.output.Len {
		return nil, nil
	}

	end := c.offset + 1
	batch := c.output.Slice(c.offset, end)
	c.offset = end

	return batch, nil
}

func (c *CorrelateIterator) materialize(ctx context.Context) error {
	method := c.method
	if method == "" {
		method = "pearson"
	}
	if method != "spearman" {
		return c.materializePearson(ctx, method)
	}

	var x, y []float64
	for {
		batch, err := c.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			v1, ok1 := getNumeric(batch.Columns[c.field1], i)
			v2, ok2 := getNumeric(batch.Columns[c.field2], i)
			if ok1 && ok2 {
				if err := c.acct.Grow(estimatedCorrelationPairBytes); err != nil {
					return fmt.Errorf("correlate: memory budget exceeded for spearman ranks: %w", err)
				}
				x = append(x, v1)
				y = append(y, v2)
			}
		}
	}

	n := len(x)
	corrValue := event.NullValue()
	if n >= 2 {
		var r float64
		if method == "spearman" {
			r = spearmanCorrelation(x, y)
		} else {
			r = pearsonCorrelation(x, y)
		}
		if !math.IsNaN(r) && !math.IsInf(r, 0) {
			corrValue = event.FloatValue(r)
		}
	}

	c.output = BatchFromRows([]map[string]event.Value{
		{
			"_correlation": corrValue,
			"_method":      event.StringValue(method),
			"_n":           event.IntValue(int64(n)),
			"_field1":      event.StringValue(c.field1),
			"_field2":      event.StringValue(c.field2),
		},
	})
	return nil
}

func (c *CorrelateIterator) materializePearson(ctx context.Context, method string) error {
	var n int64
	var sumX, sumY, sumXY, sumX2, sumY2 float64
	for {
		batch, err := c.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			x, ok1 := getNumeric(batch.Columns[c.field1], i)
			y, ok2 := getNumeric(batch.Columns[c.field2], i)
			if !ok1 || !ok2 {
				continue
			}
			n++
			sumX += x
			sumY += y
			sumXY += x * y
			sumX2 += x * x
			sumY2 += y * y
		}
	}

	corrValue := event.NullValue()
	if n >= 2 {
		r := pearsonFromSums(n, sumX, sumY, sumXY, sumX2, sumY2)
		if !math.IsNaN(r) && !math.IsInf(r, 0) {
			corrValue = event.FloatValue(r)
		}
	}

	c.output = BatchFromRows([]map[string]event.Value{
		{
			"_correlation": corrValue,
			"_method":      event.StringValue(method),
			"_n":           event.IntValue(n),
			"_field1":      event.StringValue(c.field1),
			"_field2":      event.StringValue(c.field2),
		},
	})

	return nil
}

func pearsonCorrelation(x, y []float64) float64 {
	n := int64(len(x))
	if n == 0 {
		return 0
	}

	var sumX, sumY, sumXY, sumX2, sumY2 float64
	for i := range x {
		sumX += x[i]
		sumY += y[i]
		sumXY += x[i] * y[i]
		sumX2 += x[i] * x[i]
		sumY2 += y[i] * y[i]
	}

	return pearsonFromSums(n, sumX, sumY, sumXY, sumX2, sumY2)
}

func pearsonFromSums(n int64, sumX, sumY, sumXY, sumX2, sumY2 float64) float64 {
	nf := float64(n)
	num := nf*sumXY - sumX*sumY
	den := math.Sqrt((nf*sumX2 - sumX*sumX) * (nf*sumY2 - sumY*sumY))
	if den == 0 {
		return 0
	}

	return num / den
}

func spearmanCorrelation(x, y []float64) float64 {
	rx := rank(x)
	ry := rank(y)

	return pearsonCorrelation(rx, ry)
}

func rank(vals []float64) []float64 {
	n := len(vals)
	type pair struct {
		val float64
		idx int
	}
	pairs := make([]pair, n)
	for i, v := range vals {
		pairs[i] = pair{v, i}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].val < pairs[j].val })

	ranks := make([]float64, n)
	for i, p := range pairs {
		ranks[p.idx] = float64(i + 1)
	}

	return ranks
}

func getNumeric(col []event.Value, i int) (float64, bool) {
	if col == nil || i >= len(col) {
		return 0, false
	}
	v := col[i]
	if v.IsNull() {
		return 0, false
	}
	// ValueToFloat handles int, float, and schema-on-read string columns
	// that contain numeric values (common with JSON ingest).
	return vm.ValueToFloat(v)
}

func (c *CorrelateIterator) Close() error {
	c.acct.Close()

	return c.child.Close()
}

func (c *CorrelateIterator) Schema() []FieldInfo {
	return []FieldInfo{
		{Name: "_correlation", Type: "float"},
		{Name: "_method", Type: "string"},
		{Name: "_n", Type: "int"},
		{Name: "_field1", Type: "string"},
		{Name: "_field2", Type: "string"},
	}
}
