package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
	"github.com/lynxbase/lynxdb/pkg/vm"
)

// OutliersIterator is a blocking operator that identifies outlier rows using
// statistical methods (IQR, Z-score, or MAD). Rows are buffered in memory
// until the budget pushes back; with a SpillManager configured, row buffering
// transitions to columnar spill and replay.
type OutliersIterator struct {
	child     Iterator
	field     string
	method    string
	threshold float64
	acct      memgov.MemoryAccount

	// Accumulation phase.
	done        bool
	rows        []map[string]event.Value
	rowBytesMem int64
	values      []float64

	// Emission phase.
	scores    []float64
	isOutlier []bool
	offset    int

	// Spill state.
	spillMgr        *SpillManager
	spillWriter     *ColumnarSpillWriter
	spillReader     *ColumnarSpillReader
	spillPath       string
	spilled         bool
	spilledRows     int64
	spillBytesTotal int64
}

// NewOutliersIterator creates a new outliers iterator.
func NewOutliersIterator(child Iterator, field, method string, threshold float64) *OutliersIterator {
	return NewOutliersIteratorWithBudget(child, field, method, threshold, memgov.NopAccount(), nil)
}

// NewOutliersIteratorWithBudget creates an outliers iterator with row-buffer
// accounting and optional columnar spill support.
func NewOutliersIteratorWithBudget(child Iterator, field, method string, threshold float64, acct memgov.MemoryAccount, mgr *SpillManager) *OutliersIterator {
	o := &OutliersIterator{
		child:     child,
		field:     field,
		method:    method,
		threshold: threshold,
		acct:      memgov.EnsureAccount(acct),
		spillMgr:  mgr,
	}
	if ca, ok := o.acct.(*CoordinatedAccount); ok && mgr != nil {
		ca.SetOnRevoke(func(target int64) int64 {
			if o.spilled || len(o.rows) == 0 {
				return 0
			}
			before := o.acct.Used()
			if err := o.spillBufferedRows(); err != nil {
				return 0
			}
			freed := before - o.acct.Used()
			if freed < 0 {
				return 0
			}

			return freed
		})
	}

	return o
}

func (o *OutliersIterator) Init(ctx context.Context) error {
	return o.child.Init(ctx)
}

func (o *OutliersIterator) Next(ctx context.Context) (*Batch, error) {
	if !o.done {
		if err := o.materialize(ctx); err != nil {
			return nil, err
		}
		o.done = true
	}

	if o.offset >= len(o.values) {
		return nil, nil
	}

	batch := NewBatch(DefaultBatchSize)
	for batch.Len < DefaultBatchSize && o.offset < len(o.values) {
		row, err := o.rowAtOffset()
		if err != nil {
			return nil, err
		}
		row["_outlier"] = event.BoolValue(o.isOutlier[o.offset])
		row["_score"] = event.FloatValue(o.scores[o.offset])
		batch.AddRow(row)
		o.offset++
	}

	if batch.Len == 0 {
		return nil, nil
	}

	return batch, nil
}

func (o *OutliersIterator) Close() error {
	var errs []error
	if o.spillWriter != nil {
		if err := o.spillWriter.CloseFile(); err != nil {
			errs = append(errs, fmt.Errorf("outliers: close spill writer: %w", err))
		}
		o.spillWriter = nil
	}
	if o.spillReader != nil {
		if err := o.spillReader.Close(); err != nil {
			errs = append(errs, err)
		}
		o.spillReader = nil
	}
	if o.spillPath != "" {
		if info, err := os.Stat(o.spillPath); err == nil {
			o.spillBytesTotal = info.Size()
		}
		if o.spillMgr != nil {
			o.spillMgr.Release(o.spillPath)
		} else {
			os.Remove(o.spillPath)
		}
		o.spillPath = ""
	}
	o.acct.Close()
	if err := o.child.Close(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func (o *OutliersIterator) Schema() []FieldInfo {
	schema := o.child.Schema()
	// Add _outlier and _score columns.
	return append(schema,
		FieldInfo{Name: "_outlier", Type: "bool"},
		FieldInfo{Name: "_score", Type: "float"},
	)
}

// MemoryUsed returns the current tracked memory for this operator.
func (o *OutliersIterator) MemoryUsed() int64 { return o.acct.Used() }

// ResourceStats implements ResourceReporter for spill observability.
func (o *OutliersIterator) ResourceStats() OperatorResourceStats {
	spillBytes := o.spillBytesTotal
	if o.spillPath != "" {
		if info, err := os.Stat(o.spillPath); err == nil {
			spillBytes = info.Size()
		}
	}

	return OperatorResourceStats{
		PeakBytes:   o.acct.MaxUsed(),
		SpilledRows: o.spilledRows,
		SpillBytes:  spillBytes,
	}
}

func (o *OutliersIterator) materialize(ctx context.Context) error {
	for {
		batch, err := o.child.Next(ctx)
		if err != nil {
			return err
		}
		if batch == nil {
			break
		}
		for i := 0; i < batch.Len; i++ {
			row := batch.Row(i)
			if err := o.acct.Grow(8); err != nil {
				return fmt.Errorf("outliers: memory budget exceeded for values: %w", err)
			}
			o.values = append(o.values, outlierNumericValue(row, o.field))
			if o.spilled {
				if err := o.spillWriter.WriteRow(row); err != nil {
					return fmt.Errorf("outliers: write spill: %w", err)
				}
				o.spilledRows++
				continue
			}
			rowBytes := EstimateRowBytes(row)
			if err := o.acct.Grow(rowBytes); err != nil {
				if o.spillMgr == nil {
					return fmt.Errorf("outliers: memory budget exceeded for rows: %w", err)
				}
				if err := o.transitionToSpill(row); err != nil {
					return err
				}
				continue
			}
			o.rows = append(o.rows, row)
			o.rowBytesMem += rowBytes
		}
	}

	if o.spilled && o.spillWriter != nil {
		if err := o.spillWriter.CloseFile(); err != nil {
			return fmt.Errorf("outliers: close spill: %w", err)
		}
		o.spillWriter = nil
	}

	switch o.method {
	case "iqr":
		o.scores, o.isOutlier = o.computeIQR(o.values)
	case "zscore":
		o.scores, o.isOutlier = o.computeZScore(o.values)
	case "mad":
		o.scores, o.isOutlier = o.computeMAD(o.values)
	default:
		o.scores, o.isOutlier = o.computeIQR(o.values)
	}

	return nil
}

func (o *OutliersIterator) transitionToSpill(currentRow map[string]event.Value) error {
	if err := o.spillBufferedRows(); err != nil {
		return err
	}
	if err := o.spillWriter.WriteRow(currentRow); err != nil {
		return fmt.Errorf("outliers: write current row: %w", err)
	}
	o.spilledRows++

	return nil
}

func (o *OutliersIterator) spillBufferedRows() error {
	sw, err := NewColumnarSpillWriter(o.spillMgr, "outliers")
	if err != nil {
		return fmt.Errorf("outliers: create spill file: %w", err)
	}
	o.spillWriter = sw
	o.spillPath = sw.Path()
	o.spilled = true

	for _, row := range o.rows {
		if err := sw.WriteRow(row); err != nil {
			return fmt.Errorf("outliers: write buffered row: %w", err)
		}
	}
	o.spilledRows = int64(len(o.rows))

	o.acct.Shrink(o.rowBytesMem)
	o.rows = nil
	o.rowBytesMem = 0
	if sn, ok := o.acct.(SpillNotifier); ok {
		sn.NotifySpilled()
	}

	return nil
}

func (o *OutliersIterator) rowAtOffset() (map[string]event.Value, error) {
	if !o.spilled {
		return o.rows[o.offset], nil
	}
	if o.spillReader == nil {
		reader, err := NewColumnarSpillReader(o.spillPath)
		if err != nil {
			return nil, fmt.Errorf("outliers: open spill reader: %w", err)
		}
		o.spillReader = reader
	}
	row, err := o.spillReader.ReadRow()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("outliers: spill ended before row %d", o.offset)
		}
		return nil, fmt.Errorf("outliers: read spill: %w", err)
	}

	return row, nil
}

func outlierNumericValue(row map[string]event.Value, field string) float64 {
	v, ok := row[field]
	if !ok {
		return math.NaN()
	}
	if f, ok := vm.ValueToFloat(v); ok {
		return f
	}

	return math.NaN()
}

// computeIQR computes outlier scores using the Interquartile Range method.
func (o *OutliersIterator) computeIQR(values []float64) ([]float64, []bool) {
	valid := filterNaN(values)
	scores := make([]float64, len(values))
	isOutlier := make([]bool, len(values))

	if len(valid) < 4 {
		return scores, isOutlier
	}

	sort.Float64s(valid)
	q1 := percentileFloat64(valid, 25)
	q3 := percentileFloat64(valid, 75)
	iqr := q3 - q1

	if iqr == 0 {
		return scores, isOutlier
	}

	lower := q1 - o.threshold*iqr
	upper := q3 + o.threshold*iqr

	for i, v := range values {
		if math.IsNaN(v) {
			continue
		}
		if v < lower {
			scores[i] = (lower - v) / iqr
			isOutlier[i] = true
		} else if v > upper {
			scores[i] = (v - upper) / iqr
			isOutlier[i] = true
		}
	}

	return scores, isOutlier
}

// computeZScore computes outlier scores using Z-score method.
func (o *OutliersIterator) computeZScore(values []float64) ([]float64, []bool) {
	scores := make([]float64, len(values))
	isOutlier := make([]bool, len(values))

	// Welford's algorithm for mean and stdev.
	var count int
	var mean, m2 float64
	for _, v := range values {
		if math.IsNaN(v) {
			continue
		}
		count++
		delta := v - mean
		mean += delta / float64(count)
		delta2 := v - mean
		m2 += delta * delta2
	}

	if count < 2 {
		return scores, isOutlier
	}

	stdev := math.Sqrt(m2 / float64(count-1))
	if stdev == 0 {
		return scores, isOutlier
	}

	for i, v := range values {
		if math.IsNaN(v) {
			continue
		}
		z := math.Abs((v - mean) / stdev)
		scores[i] = z
		isOutlier[i] = z > o.threshold
	}

	return scores, isOutlier
}

// computeMAD computes outlier scores using Median Absolute Deviation.
func (o *OutliersIterator) computeMAD(values []float64) ([]float64, []bool) {
	valid := filterNaN(values)
	scores := make([]float64, len(values))
	isOutlier := make([]bool, len(values))

	if len(valid) < 3 {
		return scores, isOutlier
	}

	sorted := make([]float64, len(valid))
	copy(sorted, valid)
	sort.Float64s(sorted)
	median := percentileFloat64(sorted, 50)

	deviations := make([]float64, len(valid))
	for i, v := range valid {
		deviations[i] = math.Abs(v - median)
	}
	sort.Float64s(deviations)
	mad := percentileFloat64(deviations, 50)

	if mad == 0 {
		return scores, isOutlier
	}

	const consistencyConstant = 0.6745
	for i, v := range values {
		if math.IsNaN(v) {
			continue
		}
		score := consistencyConstant * math.Abs(v-median) / mad
		scores[i] = score
		isOutlier[i] = score > o.threshold
	}

	return scores, isOutlier
}

// filterNaN returns only non-NaN values from the input.
func filterNaN(values []float64) []float64 {
	valid := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) {
			valid = append(valid, v)
		}
	}

	return valid
}

// percentileFloat64 computes the p-th percentile of a sorted slice.
func percentileFloat64(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}

	rank := p / 100 * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))

	if lower == upper {
		return sorted[lower]
	}

	frac := rank - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}
