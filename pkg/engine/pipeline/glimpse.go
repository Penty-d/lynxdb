package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/lynxbase/lynxdb/internal/ui"
	"github.com/lynxbase/lynxdb/pkg/event"
)

const glimpseMaxSample = 10000

// GlimpseIterator samples events from the child and outputs a schema table.
// It accumulates per-field stats (type, coverage, cardinality, top values)
// and produces a single-row result batch with the formatted table in _raw.
type GlimpseIterator struct {
	child     Iterator
	maxSample int
	sampled   int
	done      bool
	start     time.Time

	// Per-field accumulators.
	fields map[string]*glimpseField
	total  int
}

type glimpseField struct {
	count     int
	types     map[string]int
	values    map[string]int
	nullCount int
	isBuiltin bool
	// Numeric statistics (populated when field has int/float values).
	numMin   float64
	numMax   float64
	numSum   float64
	numCount int
	numVals  []float64 // sampled for percentile computation (cap 1000)
}

func NewGlimpseIterator(child Iterator, sampleSize int) *GlimpseIterator {
	if sampleSize <= 0 {
		sampleSize = glimpseMaxSample
	}

	return &GlimpseIterator{
		child:     child,
		maxSample: sampleSize,
		fields:    make(map[string]*glimpseField),
	}
}

func (g *GlimpseIterator) Init(ctx context.Context) error {
	g.start = time.Now()

	return g.child.Init(ctx)
}

func (g *GlimpseIterator) Next(ctx context.Context) (*Batch, error) {
	if g.done {
		return nil, nil
	}
	g.done = true

	// Drain the child pipeline, accumulating field stats.
	for g.sampled < g.maxSample {
		batch, err := g.child.Next(ctx)
		if err != nil {
			return nil, err
		}
		if batch == nil {
			break
		}
		g.accumulate(batch)
		g.sampled += batch.Len
		if g.sampled > g.maxSample {
			g.sampled = g.maxSample
		}
	}

	result := g.buildResult()

	// Build the formatted table for TTY/_raw output.
	table := g.formatTable(result)

	// Serialize structured result for API consumers.
	resultJSON, _ := json.Marshal(result)

	// Return as a single-row batch with both structured and raw columns.
	b := NewBatch(1)
	b.Columns["__glimpse_result"] = []event.Value{event.StringValue(string(resultJSON))}
	b.Columns["_raw"] = []event.Value{event.StringValue(table)}
	b.Len = 1

	return b, nil
}

func (g *GlimpseIterator) accumulate(batch *Batch) {
	n := batch.Len
	g.total += n

	for name, col := range batch.Columns {
		f := g.getOrCreate(name)
		for i := 0; i < n && i < len(col); i++ {
			v := col[i]
			if v.IsNull() {
				f.nullCount++
			} else {
				f.count++
				typeName := v.Type().String()
				f.types[typeName]++
				// Track top values (cap at 50 unique per field).
				if len(f.values) < 50 {
					f.values[v.String()]++
				}
				// Track numeric statistics.
				if v.Type() == event.FieldTypeInt || v.Type() == event.FieldTypeFloat {
					var fv float64
					if v.Type() == event.FieldTypeInt {
						fv = float64(v.AsInt())
					} else {
						fv = v.AsFloat()
					}
					f.numCount++
					f.numSum += fv
					if f.numCount == 1 || fv < f.numMin {
						f.numMin = fv
					}
					if f.numCount == 1 || fv > f.numMax {
						f.numMax = fv
					}
					if len(f.numVals) < 1000 {
						f.numVals = append(f.numVals, fv)
					}
				}
			}
		}
	}

	// Account for columns that don't exist in this batch (all null).
	for name := range g.fields {
		if _, ok := batch.Columns[name]; !ok {
			g.fields[name].nullCount += n
		}
	}
}

func (g *GlimpseIterator) getOrCreate(name string) *glimpseField {
	f, ok := g.fields[name]
	if !ok {
		f = &glimpseField{
			types:     make(map[string]int),
			values:    make(map[string]int),
			isBuiltin: isBuiltinGlimpseField(name),
		}
		g.fields[name] = f
	}

	return f
}

func isBuiltinGlimpseField(name string) bool {
	switch name {
	case "_time", "_raw", "_source", "_sourcetype", "host", "index", "source":
		return true
	}

	return false
}

func (g *GlimpseIterator) buildResult() *GlimpseResult {
	names := g.sortedFieldNames()
	fields := make([]GlimpseFieldInfo, 0, len(names))
	elapsed := time.Since(g.start)

	for _, name := range names {
		f := g.fields[name]
		total := f.count + f.nullCount
		if total == 0 {
			continue
		}

		coverage := float64(f.count) / float64(total) * 100
		nullPct := float64(f.nullCount) / float64(total) * 100
		dominantType := g.dominantType(f)

		info := GlimpseFieldInfo{
			Name:        name,
			Type:        dominantType,
			CoveragePct: math.Round(coverage*10) / 10,
			NullPct:     math.Round(nullPct*10) / 10,
		}

		if dominantType == "number" {
			if f.numCount > 0 {
				vals := make([]float64, len(f.numVals))
				copy(vals, f.numVals)
				sort.Float64s(vals)
				info.NumStats = &GlimpseNumStats{
					Min: f.numMin,
					P50: glimpsePercentile(vals, 0.50),
					P99: glimpsePercentile(vals, 0.99),
					Max: f.numMax,
				}
				// Show cardinality for numeric fields with few unique values.
				card := len(f.values)
				if card > 0 && card <= 50 {
					info.Cardinality = card
				}
			}
		} else {
			info.Cardinality = len(f.values)
			info.TopValues = g.buildTopValues(f)
		}

		fields = append(fields, info)
	}

	return &GlimpseResult{
		Fields:    fields,
		Sampled:   g.sampled,
		ElapsedMs: elapsed.Milliseconds(),
	}
}

func (g *GlimpseIterator) buildTopValues(f *glimpseField) []GlimpseValue {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(f.values))
	for k, v := range f.values {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].v > pairs[j].v
	})

	total := f.count
	if total == 0 {
		return nil
	}

	result := make([]GlimpseValue, 0, len(pairs))
	for _, p := range pairs {
		pct := float64(p.v) / float64(total) * 100
		result = append(result, GlimpseValue{
			Value: p.k,
			Count: p.v,
			Pct:   math.Round(pct*10) / 10,
		})
	}

	return result
}

func (g *GlimpseIterator) formatTable(result *GlimpseResult) string {
	var sb strings.Builder

	termWidth := ui.TerminalWidth()

	// Fixed columns: FIELD(18) + TYPE(9) + COVERAGE(7) + NULL%(6) + CARDINALITY(13) + spaces(8)
	fixedWidth := 61
	topValuesWidth := termWidth - fixedWidth
	if topValuesWidth < 20 {
		topValuesWidth = 20
	}

	// Header.
	header := "  FIELD              TYPE      COVERAGE   NULL%   CARDINALITY   TOP VALUES"
	if len(header) > termWidth {
		header = header[:termWidth]
	}
	sb.WriteString(header + "\n")
	sepLen := len(header) - 2
	if sepLen > 95 {
		sepLen = 95
	}
	if sepLen > termWidth-2 {
		sepLen = termWidth - 2
	}
	sb.WriteString("  " + strings.Repeat("─", sepLen) + "\n")

	for _, f := range result.Fields {
		fieldDisplay := f.Name
		if len(fieldDisplay) > 18 {
			fieldDisplay = fieldDisplay[:15] + "..."
		}

		cardStr := ""
		if f.Type == "number" {
			if f.Cardinality > 0 {
				cardStr = fmt.Sprintf("%d", f.Cardinality)
			} else {
				cardStr = "—"
			}
		} else {
			if f.Cardinality >= 50 {
				cardStr = fmt.Sprintf("%d+", f.Cardinality)
			} else {
				cardStr = fmt.Sprintf("%d", f.Cardinality)
			}
		}

		topValues := g.formatFieldDisplay(&f, topValuesWidth)

		fmt.Fprintf(&sb, "  %-18s %-9s %5.1f%%    %4.1f%%   %-13s %s\n",
			fieldDisplay,
			f.Type,
			f.CoveragePct,
			f.NullPct,
			cardStr,
			topValues,
		)
	}

	// Footer.
	footer := fmt.Sprintf("\n  ✔ %s events sampled · %d fields · %s",
		formatGlimpseNumber(result.Sampled),
		len(result.Fields),
		time.Duration(result.ElapsedMs)*time.Millisecond,
	)
	sb.WriteString(footer + "\n")

	return sb.String()
}

func (g *GlimpseIterator) formatFieldDisplay(f *GlimpseFieldInfo, maxWidth int) string {
	if f.NumStats != nil {
		ns := f.NumStats
		if math.Abs(ns.Min-ns.Max) < 1e-12 {
			return fmt.Sprintf("const %g", ns.Min)
		}

		return fmt.Sprintf("min=%g p50=%g p99=%g max=%g", ns.Min, ns.P50, ns.P99, ns.Max)
	}

	remain := maxWidth
	parts := make([]string, 0, 4)
	for _, tv := range f.TopValues {
		if remain <= 0 {
			break
		}
		val := tv.Value
		if len(val) > 20 {
			val = val[:17] + "..."
		}
		part := fmt.Sprintf("%s(%.0f%%)", val, tv.Pct)
		parts = append(parts, part)
		remain -= len(part) + 2
	}

	return strings.Join(parts, ", ")
}

func (g *GlimpseIterator) sortedFieldNames() []string {
	builtinOrder := []string{"_time", "_raw", "index", "_source", "_sourcetype", "source", "sourcetype", "host"}
	builtinRank := make(map[string]int, len(builtinOrder))
	for i, n := range builtinOrder {
		builtinRank[n] = i
	}

	names := make([]string, 0, len(g.fields))
	for name := range g.fields {
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		ri, oki := builtinRank[names[i]]
		rj, okj := builtinRank[names[j]]
		if oki && okj {
			return ri < rj
		}
		if oki {
			return true
		}
		if okj {
			return false
		}
		// User fields: sort by coverage descending, then name.
		fi := g.fields[names[i]]
		fj := g.fields[names[j]]
		ci := float64(fi.count) / float64(fi.count+fi.nullCount+1)
		cj := float64(fj.count) / float64(fj.count+fj.nullCount+1)
		if ci != cj {
			return ci > cj
		}

		return names[i] < names[j]
	})

	return names
}

func (g *GlimpseIterator) dominantType(f *glimpseField) string {
	best := "string"
	bestCount := 0
	for t, c := range f.types {
		if c > bestCount {
			bestCount = c
			best = t
		}
	}

	// Display-friendly: merge int/float as "number".
	if best == "int" || best == "float" {
		return "number"
	}

	return best
}

// glimpsePercentile returns the value at the given quantile (0.0-1.0) from a sorted slice.
func glimpsePercentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := q * float64(len(sorted)-1)
	lower := int(idx)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)

	return sorted[lower] + frac*(sorted[upper]-sorted[lower])
}

func formatGlimpseNumber(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	// Add comma separators.
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)

	return strings.Join(parts, ",")
}

func (g *GlimpseIterator) Close() error {
	return g.child.Close()
}

func (g *GlimpseIterator) Schema() []FieldInfo {
	return []FieldInfo{
		{Name: "__glimpse_result", Type: "glimpse"},
		{Name: "_raw", Type: "string"},
	}
}
