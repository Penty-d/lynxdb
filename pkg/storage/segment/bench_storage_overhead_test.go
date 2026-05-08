package segment

import "testing"

type rangeBSIOverheadReport struct {
	v1Bytes       int
	v2Bytes       int
	bsiBytes      int64
	indexedCols   int
	maxColumnFrac float64
	meanColFrac   float64
	totalFrac     float64
}

func BenchmarkRangeBSI_StorageOverhead(b *testing.B) {
	fx := makeRangeBSIPhase6Fixture(b, rangeBSIBenchEventCount())
	report := measureRangeBSIStorageOverhead(b, fx.v2Data)
	b.ReportMetric(float64(len(fx.v1Data)), "v1_bytes")
	b.ReportMetric(float64(len(fx.v2Data)), "v2_bytes")
	b.ReportMetric(float64(report.bsiBytes), "bsi_bytes")
	b.ReportMetric(report.totalFrac*100, "bsi_total_pct")
	b.ReportMetric(report.maxColumnFrac*100, "bsi_max_col_pct")
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = measureRangeBSIStorageOverhead(b, fx.v2Data)
	}
}

func TestAcceptance_RangeBSIStorageOverhead(t *testing.T) {
	if !rangeBSIAcceptanceEnabled() {
		t.Skip("set LYNXDB_RANGE_BSI_ACCEPTANCE=1 to run range BSI acceptance gates")
	}
	fx := makeRangeBSIPhase6Fixture(t, rangeBSIBenchEventCount())
	report := measureRangeBSIStorageOverhead(t, fx.v2Data)
	report.v1Bytes = len(fx.v1Data)
	report.v2Bytes = len(fx.v2Data)
	t.Logf("storage v1=%dB v2=%dB bsi=%dB total=%.2f%% mean_col=%.2f%% max_col=%.2f%% cols=%d",
		report.v1Bytes, report.v2Bytes, report.bsiBytes, report.totalFrac*100,
		report.meanColFrac*100, report.maxColumnFrac*100, report.indexedCols)
	if report.totalFrac > 0.20 {
		t.Fatalf("total BSI fraction = %.2f%%, want <= 20.00%%", report.totalFrac*100)
	}
	if report.meanColFrac > 0.06 {
		t.Fatalf("mean per-column BSI fraction = %.2f%%, want <= 6.00%%", report.meanColFrac*100)
	}
	if report.maxColumnFrac > 0.06 {
		t.Fatalf("max per-column BSI fraction = %.2f%%, want <= 6.00%%", report.maxColumnFrac*100)
	}
}

func measureRangeBSIStorageOverhead(t testing.TB, data []byte) rangeBSIOverheadReport {
	t.Helper()
	footer, sections := rangeSectionsFromFooter(t, data)
	bsiCols, bsiBytes := footer.RangeBSIStats()
	if bsiCols == 0 {
		t.Fatal("RangeBSIStats columns = 0, want indexed columns")
	}
	if bsiBytes == 0 {
		t.Fatal("RangeBSIStats sectionBytes = 0, want non-empty BSI sections")
	}

	perColumn := make(map[string]int64)
	for _, raw := range sections {
		if len(raw) == 0 {
			continue
		}
		section := parseRangeSectionSegmentTest(t, raw)
		for _, entry := range section.Entries {
			perColumn[entry.Name] += int64(len(entry.crcPayload) + 4)
		}
	}
	if len(perColumn) == 0 {
		t.Fatal("decoded no per-column BSI entries")
	}

	var sum int64
	var maxBytes int64
	for _, n := range perColumn {
		sum += n
		if n > maxBytes {
			maxBytes = n
		}
	}
	v2Bytes := len(data)
	return rangeBSIOverheadReport{
		v2Bytes:       v2Bytes,
		bsiBytes:      bsiBytes,
		indexedCols:   len(perColumn),
		maxColumnFrac: float64(maxBytes) / float64(v2Bytes),
		meanColFrac:   float64(sum) / float64(len(perColumn)) / float64(v2Bytes),
		totalFrac:     float64(bsiBytes) / float64(v2Bytes),
	}
}
