package segment

import (
	"bytes"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
)

func BenchmarkReader_LoadRangeBSI_Cold_8192Rows32Bit(b *testing.B) {
	events := makeRangeBSIEventsForReaderBench(8192)
	data := writeRangeBSISegmentForBench(b, events)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		r, err := OpenSegment(data)
		if err != nil {
			b.Fatalf("OpenSegment: %v", err)
		}
		idx, err := r.LoadRangeBSI(0, "duration_ms")
		if err != nil {
			b.Fatalf("LoadRangeBSI: %v", err)
		}
		if idx == nil {
			b.Fatal("LoadRangeBSI = nil, want BSI")
		}
	}
}

func BenchmarkReader_LoadRangeBSI_Warm_8192Rows32Bit(b *testing.B) {
	events := makeRangeBSIEventsForReaderBench(8192)
	data := writeRangeBSISegmentForBench(b, events)
	r, err := OpenSegment(data)
	if err != nil {
		b.Fatalf("OpenSegment: %v", err)
	}
	if idx, err := r.LoadRangeBSI(0, "duration_ms"); err != nil {
		b.Fatalf("LoadRangeBSI preload: %v", err)
	} else if idx == nil {
		b.Fatal("LoadRangeBSI preload = nil, want BSI")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx, err := r.LoadRangeBSI(0, "duration_ms")
		if err != nil {
			b.Fatalf("LoadRangeBSI: %v", err)
		}
		if idx == nil {
			b.Fatal("LoadRangeBSI = nil, want BSI")
		}
	}
}

func makeRangeBSIEventsForReaderBench(n int) []*event.Event {
	base := time.Date(2026, 5, 8, 14, 0, 0, 0, time.UTC)
	events := make([]*event.Event, n)
	for i := range events {
		duration := int64((i*7919)%200000 + i/7)
		latency := math.Log1p(float64(i+1)) * 37.25
		ts := base.Add(time.Duration(i*91+17) * time.Millisecond)
		e := event.NewEvent(ts, fmt.Sprintf("duration_ms=%d latency=%.6f row=%d", duration, latency, i))
		e.Host = fmt.Sprintf("host-%02d", i%17)
		e.Source = "/var/log/range-bsi-bench.log"
		e.SourceType = "json"
		e.Index = "main"
		e.SetField("duration_ms", event.IntValue(duration))
		e.SetField("latency", event.FloatValue(latency))
		e.SetField("level", event.StringValue(fmt.Sprintf("level-%d", i%5)))
		events[i] = e
	}
	return events
}

func writeRangeBSISegmentForBench(b *testing.B, events []*event.Event) []byte {
	b.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	w.SetRowGroupSize(8192)
	if _, err := w.Write(events); err != nil {
		b.Fatalf("Write: %v", err)
	}
	return append([]byte(nil), buf.Bytes()...)
}
