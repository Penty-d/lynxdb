package segment

import (
	"testing"
)

func BenchmarkRangeBSI_Query_StatusGE500_BruteScan(b *testing.B) {
	fx := makeRangeBSIPhase6Fixture(b, rangeBSIBenchEventCount())
	c := rangeBSIStatusGE500Case()
	want, _ := measureRangeBSIBruteCount(b, fx.v1, c, 1)
	b.ReportMetric(float64(want), "matches")
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		got, _ := measureRangeBSIBruteCount(b, fx.v1, c, 1)
		if got != want {
			b.Fatalf("matches = %d, want %d", got, want)
		}
	}
}

func BenchmarkRangeBSI_Query_StatusGE500_BSI(b *testing.B) {
	fx := makeRangeBSIPhase6Fixture(b, rangeBSIBenchEventCount())
	c := rangeBSIStatusGE500Case()
	want, _ := measureRangeBSIBruteCount(b, fx.v1, c, 1)
	got, _, stats := measureRangeBSIBitmapCount(b, fx.v2, c, 1)
	if got != want {
		b.Fatalf("BSI matches = %d, want %d", got, want)
	}
	b.ReportMetric(float64(stats.RangeBSIChecks), "bsi_checks")
	b.ReportMetric(float64(stats.RangeBSIMaskBytes), "bsi_mask_bytes")
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		got, _, _ := measureRangeBSIBitmapCount(b, fx.v2, c, 1)
		if got != want {
			b.Fatalf("BSI matches = %d, want %d", got, want)
		}
	}
}

func BenchmarkRangeBSI_Query_DurationBetween_BruteScan(b *testing.B) {
	fx := makeRangeBSIPhase6Fixture(b, rangeBSIBenchEventCount())
	c := rangeBSIDurationBetweenCase()
	want, _ := measureRangeBSIBruteCount(b, fx.v1, c, 1)
	b.ReportMetric(float64(want), "matches")
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		got, _ := measureRangeBSIBruteCount(b, fx.v1, c, 1)
		if got != want {
			b.Fatalf("matches = %d, want %d", got, want)
		}
	}
}

func BenchmarkRangeBSI_Query_DurationBetween_BSI(b *testing.B) {
	fx := makeRangeBSIPhase6Fixture(b, rangeBSIBenchEventCount())
	c := rangeBSIDurationBetweenCase()
	want, _ := measureRangeBSIBruteCount(b, fx.v1, c, 1)
	got, _, stats := measureRangeBSIBitmapCount(b, fx.v2, c, 1)
	if got != want {
		b.Fatalf("BSI matches = %d, want %d", got, want)
	}
	b.ReportMetric(float64(stats.RangeBSIChecks), "bsi_checks")
	b.ReportMetric(float64(stats.RangeBSIMaskBytes), "bsi_mask_bytes")
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		got, _, _ := measureRangeBSIBitmapCount(b, fx.v2, c, 1)
		if got != want {
			b.Fatalf("BSI matches = %d, want %d", got, want)
		}
	}
}

func TestAcceptance_RangeBSIQuerySpeedup_StatusGE500(t *testing.T) {
	if !rangeBSIAcceptanceEnabled() {
		t.Skip("set LYNXDB_RANGE_BSI_ACCEPTANCE=1 to run range BSI acceptance gates")
	}
	assertRangeBSIQuerySpeedup(t, rangeBSIStatusGE500Case())
}

func TestAcceptance_RangeBSIQuerySpeedup_DurationBetween(t *testing.T) {
	if !rangeBSIAcceptanceEnabled() {
		t.Skip("set LYNXDB_RANGE_BSI_ACCEPTANCE=1 to run range BSI acceptance gates")
	}
	assertRangeBSIQuerySpeedup(t, rangeBSIDurationBetweenCase())
}

func assertRangeBSIQuerySpeedup(t *testing.T, c rangeBSIQueryCase) {
	t.Helper()
	fx := makeRangeBSIPhase6Fixture(t, rangeBSIBenchEventCount())
	reps := rangeBSIBenchReps()
	v1Count, v1Dur := measureRangeBSIBruteCount(t, fx.v1, c, reps)
	v2Count, v2Dur, stats := measureRangeBSIBitmapCount(t, fx.v2, c, reps)
	if v2Count != v1Count {
		t.Fatalf("%s BSI count = %d, want brute count %d", c.name, v2Count, v1Count)
	}
	if stats.RangeBSIChecks == 0 {
		t.Fatalf("%s RangeBSIChecks = 0, want BSI consultations", c.name)
	}
	ratio := float64(v1Dur) / float64(v2Dur)
	t.Logf("%s V1=%s V2=%s ratio=%.2fx matches=%d bsi_checks=%d mask_bytes=%d",
		c.name, v1Dur, v2Dur, ratio, v1Count, stats.RangeBSIChecks, stats.RangeBSIMaskBytes)
	if ratio < c.wantRatio {
		t.Fatalf("%s speedup = %.2fx, want >= %.2fx", c.name, ratio, c.wantRatio)
	}
}
