package segment

import (
	"bytes"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
)

func BenchmarkRangeBSI_Writer_Disabled(b *testing.B) {
	events := makeRangeBSIPhase6Events(rangeBSIBenchEventCount())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writeRangeBSIPhase6WriterBench(b, events, false)
	}
}

func BenchmarkRangeBSI_Writer_Enabled(b *testing.B) {
	events := makeRangeBSIPhase6Events(rangeBSIBenchEventCount())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writeRangeBSIPhase6WriterBench(b, events, true)
	}
}

func TestAcceptance_RangeBSIWriterOverhead(t *testing.T) {
	if !rangeBSIAcceptanceEnabled() {
		t.Skip("set LYNXDB_RANGE_BSI_ACCEPTANCE=1 to run range BSI acceptance gates")
	}
	events := makeRangeBSIPhase6Events(rangeBSIBenchEventCount())
	reps := rangeBSIBenchReps()
	off := measureRangeBSIWriteCost(t, events, false, reps)
	on := measureRangeBSIWriteCost(t, events, true, reps)
	ratio := float64(on) / float64(off)
	t.Logf("writer BSI off=%s on=%s ratio=%.2fx events=%d reps=%d", off, on, ratio, len(events), reps)
	if ratio > 1.10 {
		t.Fatalf("writer BSI overhead = %.2fx, want <= 1.10x", ratio)
	}
}

func measureRangeBSIWriteCost(t testing.TB, events []*event.Event, enableBSI bool, reps int) time.Duration {
	t.Helper()
	start := time.Now()
	for i := 0; i < reps; i++ {
		writeRangeBSIPhase6WriterBench(t, events, enableBSI)
	}
	return time.Since(start)
}

func writeRangeBSIPhase6WriterBench(t testing.TB, events []*event.Event, enableBSI bool) {
	t.Helper()
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
		t.Fatalf("Write(BSI=%v): %v", enableBSI, err)
	}
}
