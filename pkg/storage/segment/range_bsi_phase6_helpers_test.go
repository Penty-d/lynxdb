package segment

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring"

	"github.com/lynxbase/lynxdb/pkg/event"
)

const (
	rangeBSIDefaultBenchEvents = 200_000
	rangeBSIDefaultBenchReps   = 3
)

type rangeBSIQueryCase struct {
	name      string
	preds     []Predicate
	filter    *RGFilterNode
	wantRatio float64
}

type rangeBSIBenchFixture struct {
	events []*event.Event
	v1Data []byte
	v2Data []byte
	v1     *Reader
	v2     *Reader
}

func rangeBSIAcceptanceEnabled() bool {
	v := os.Getenv("LYNXDB_RANGE_BSI_ACCEPTANCE")
	return v == "1" || v == "true" || v == "TRUE"
}

func rangeBSIBenchEventCount() int {
	if raw := os.Getenv("LYNXDB_RANGE_BSI_BENCH_EVENTS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			return n
		}
	}
	return rangeBSIDefaultBenchEvents
}

func rangeBSIBenchReps() int {
	if raw := os.Getenv("LYNXDB_RANGE_BSI_BENCH_REPS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			return n
		}
	}
	return rangeBSIDefaultBenchReps
}

func makeRangeBSIPhase6Fixture(t testing.TB, n int) rangeBSIBenchFixture {
	t.Helper()
	events := makeRangeBSIPhase6Events(n)
	v1Data := writeRangeBSIPhase6Segment(t, events, LSG_FORMAT_MAJOR_V1, false)
	v2Data := writeRangeBSIPhase6Segment(t, events, LSG_FORMAT_MAJOR_V2, true)

	v1, err := OpenSegment(v1Data)
	if err != nil {
		t.Fatalf("OpenSegment(v1): %v", err)
	}
	v2, err := OpenSegment(v2Data)
	if err != nil {
		t.Fatalf("OpenSegment(v2): %v", err)
	}
	if v2.HasRangeBSI() == false {
		t.Fatal("V2 fixture has no RangeBSI")
	}

	return rangeBSIBenchFixture{
		events: events,
		v1Data: v1Data,
		v2Data: v2Data,
		v1:     v1,
		v2:     v2,
	}
}

func makeRangeBSIPhase6Events(n int) []*event.Event {
	base := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)
	events := make([]*event.Event, n)
	for i := 0; i < n; i++ {
		status := int64(200)
		switch {
		case i%100 >= 97:
			status = int64(500 + (i % 100))
		case i%100 >= 90:
			status = 404
		}

		duration := int64(2_000 + ((i * 7919) % 28_000))
		if i%10 == 0 {
			duration = int64(100 + ((i / 10) % 901))
		}
		bytes := int64(512 + ((i * 104729) % 2_000_000))
		latency := math.Log1p(float64(i+1)) * 37.25
		ts := base.Add(time.Duration(i) * time.Millisecond)

		raw := fmt.Sprintf("%s host=web-%02d status=%d duration_ms=%d bytes=%d latency=%.6f msg=request",
			ts.Format(time.RFC3339Nano), i%32, status, duration, bytes, latency)
		e := event.NewEvent(ts, raw)
		e.Host = fmt.Sprintf("web-%02d", i%32)
		e.Source = "/var/log/range-bsi-phase6.log"
		e.SourceType = "bench"
		e.Index = "main"
		e.SetField("status", event.IntValue(status))
		e.SetField("duration_ms", event.IntValue(duration))
		e.SetField("bytes", event.IntValue(bytes))
		e.SetField("latency", event.FloatValue(latency))
		events[i] = e
	}

	return events
}

func writeRangeBSIPhase6Segment(t testing.TB, events []*event.Event, formatMajor uint16, enableBSI bool) []byte {
	t.Helper()
	restore := defaultFormatMajor
	defaultFormatMajor = formatMajor
	defer func() { defaultFormatMajor = restore }()

	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetRowGroupSize(8192)
	cfg := IndexConfig{DisableBSI: !enableBSI}
	if enableBSI {
		cfg.ProfileOverrides = map[string]IndexProfile{
			"status":      IndexProfileRangeBSI,
			"duration_ms": IndexProfileRangeBSI,
			"bytes":       IndexProfileRangeBSI,
		}
		cfg.BSIMaxBitCount = 64
	}
	w.SetIndexConfig(cfg)
	if _, err := w.Write(events); err != nil {
		t.Fatalf("Write(format=%d, bsi=%v): %v", formatMajor, enableBSI, err)
	}

	return append([]byte(nil), buf.Bytes()...)
}

func rangeBSIStatusGE500Case() rangeBSIQueryCase {
	return rangeBSIQueryCase{
		name: "status_ge_500",
		preds: []Predicate{{
			Field: "status",
			Op:    ">=",
			Value: "500",
		}},
		filter: &RGFilterNode{
			Op:       RGFilterFieldRange,
			Field:    "status",
			RangeOp:  ">=",
			RangeVal: "500",
		},
		wantRatio: 5.0,
	}
}

func rangeBSIDurationBetweenCase() rangeBSIQueryCase {
	return rangeBSIQueryCase{
		name: "duration_between_100_1000",
		preds: []Predicate{
			{Field: "duration_ms", Op: ">=", Value: "100"},
			{Field: "duration_ms", Op: "<=", Value: "1000"},
		},
		filter: &RGFilterNode{
			Op: RGFilterAnd,
			Children: []RGFilterNode{
				{Op: RGFilterFieldRange, Field: "duration_ms", RangeOp: ">=", RangeVal: "100"},
				{Op: RGFilterFieldRange, Field: "duration_ms", RangeOp: "<=", RangeVal: "1000"},
			},
		},
		wantRatio: 5.0,
	}
}

func measureRangeBSIBruteCount(t testing.TB, reader *Reader, c rangeBSIQueryCase, reps int) (int, time.Duration) {
	t.Helper()
	var count int
	start := time.Now()
	for i := 0; i < reps; i++ {
		events, err := reader.ReadEventsFiltered(c.preds, nil, predicateColumns(c.preds))
		if err != nil {
			t.Fatalf("ReadEventsFiltered(%s): %v", c.name, err)
		}
		count = len(events)
	}
	return count, time.Since(start)
}

func measureRangeBSIBitmapCount(t testing.TB, reader *Reader, c rangeBSIQueryCase, reps int) (int, time.Duration, RGFilterStats) {
	t.Helper()
	var count int
	var stats RGFilterStats
	start := time.Now()
	for i := 0; i < reps; i++ {
		next, st := countRangeBSIByRowMask(t, reader, c.filter)
		count = next
		stats = st
	}
	return count, time.Since(start), stats
}

func countRangeBSIByRowMask(t testing.TB, reader *Reader, filter *RGFilterNode) (int, RGFilterStats) {
	t.Helper()
	eval := NewRGFilterEvaluator(filter, reader)
	var stats RGFilterStats
	count := 0
	for rgIdx := 0; rgIdx < reader.RowGroupCount(); rgIdx++ {
		if eval.EvaluateRowGroup(rgIdx, &stats) == RGSkip {
			continue
		}
		mask := eval.RowMaskFor(rgIdx)
		if mask == nil {
			events, err := reader.ReadRowGroupFiltered(rgIdx, nil, filterPredicates(filter), predicateColumns(filterPredicates(filter)))
			if err != nil {
				t.Fatalf("ReadRowGroupFiltered(%d): %v", rgIdx, err)
			}
			count += len(events)
			continue
		}
		count += int(mask.GetCardinality())
	}
	return count, stats
}

func predicateColumns(preds []Predicate) []string {
	cols := make([]string, 0, len(preds))
	seen := make(map[string]struct{}, len(preds))
	for _, pred := range preds {
		if _, ok := seen[pred.Field]; ok {
			continue
		}
		seen[pred.Field] = struct{}{}
		cols = append(cols, pred.Field)
	}
	return cols
}

func filterPredicates(node *RGFilterNode) []Predicate {
	if node == nil {
		return nil
	}
	switch node.Op {
	case RGFilterFieldRange:
		return []Predicate{{Field: node.Field, Op: node.RangeOp, Value: node.RangeVal}}
	case RGFilterAnd:
		var out []Predicate
		for i := range node.Children {
			out = append(out, filterPredicates(&node.Children[i])...)
		}
		return out
	default:
		return nil
	}
}

func shiftedRangeBSIMask(mask *roaring.Bitmap, offset uint32) *roaring.Bitmap {
	if mask == nil {
		return nil
	}
	out := roaring.New()
	it := mask.Iterator()
	for it.HasNext() {
		out.Add(it.Next() + offset)
	}
	return out
}
