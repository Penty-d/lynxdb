package index

import (
	"math/bits"
	"math/rand"
	"testing"
)

func TestUnit_BSIBuilder_Set_NormalizesByMinValue(t *testing.T) {
	builder := NewBSIBuilder(RangeBSIValueInt, 100, 200)
	builder.Set(0, 100)
	builder.Set(1, 150)
	builder.Set(2, 200)

	idx := builder.Build()
	for _, tc := range []struct {
		rowID uint64
		want  int64
	}{
		{rowID: 0, want: 0},
		{rowID: 1, want: 50},
		{rowID: 2, want: 100},
	} {
		got, ok := idx.GetValue(tc.rowID)
		if !ok {
			t.Fatalf("GetValue(%d) missing", tc.rowID)
		}
		if got != tc.want {
			t.Fatalf("GetValue(%d) = %d, want normalized %d", tc.rowID, got, tc.want)
		}
	}
}

func TestUnit_BSIBuilder_Set_DistinctRowsMatchExistenceCardinality(t *testing.T) {
	const (
		minValue = int64(100)
		maxValue = int64(200)
		pairs    = 1024
	)
	rng := rand.New(rand.NewSource(42))
	builder := NewBSIBuilder(RangeBSIValueInt, minValue, maxValue)
	lastByRow := make(map[uint32]int64)

	for i := 0; i < pairs; i++ {
		rowID := uint32(rng.Intn(1500))
		raw := minValue + int64(rng.Intn(int(maxValue-minValue+1)))
		builder.Set(rowID, raw)
		lastByRow[rowID] = raw
	}

	idx := builder.Build()
	if got, want := idx.GetExistenceBitmap().GetCardinality(), uint64(len(lastByRow)); got != want {
		t.Fatalf("existence cardinality = %d, want %d", got, want)
	}

	checked := 0
	for rowID, raw := range lastByRow {
		got, ok := idx.GetValue(uint64(rowID))
		if !ok {
			t.Fatalf("GetValue(%d) missing", rowID)
		}
		if want := raw - minValue; got != want {
			t.Fatalf("GetValue(%d) = %d, want normalized %d", rowID, got, want)
		}
		checked++
		if checked == 16 {
			break
		}
	}
}

func TestUnit_BSIBuilder_BitCount_MatchesValueDomainWidth(t *testing.T) {
	for _, tc := range []struct {
		name string
		min  int64
		max  int64
	}{
		{name: "single bit", min: 100, max: 101},
		{name: "seven bits", min: 100, max: 200},
		{name: "forty bits", min: -500, max: -500 + (1 << 40) - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := NewBSIBuilder(RangeBSIValueInt, tc.min, tc.max)
			spread := uint64(tc.max) - uint64(tc.min)
			want := bits.Len64(spread)
			if got := builder.BitCount(); got != want {
				t.Fatalf("BitCount() = %d, want %d", got, want)
			}
		})
	}
}
