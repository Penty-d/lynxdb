package index

import (
	"math"
	"sort"
	"testing"
)

func TestUnit_BSIValue_FloatOrderedEncoding_RoundTripsExactBits(t *testing.T) {
	values := []float64{
		0,
		math.Copysign(0, -1),
		math.Inf(1),
		math.Inf(-1),
		math.NaN(),
		math.SmallestNonzeroFloat64,
		-math.SmallestNonzeroFloat64,
		math.MaxFloat64,
		-math.MaxFloat64,
		1.5,
		-1.5,
		1e-300,
		-1e-300,
		1e300,
		-1e300,
	}

	for _, v := range values {
		got := OrderedInt64ToFloat(FloatToOrderedInt64(v))
		if math.IsNaN(v) {
			if !math.IsNaN(got) {
				t.Fatalf("round trip for NaN = %v, want NaN", got)
			}
			continue
		}
		if math.Float64bits(got) != math.Float64bits(v) {
			t.Fatalf("round trip bits for %v = %#x, want %#x",
				v, math.Float64bits(got), math.Float64bits(v))
		}
	}
}

func TestUnit_BSIValue_FloatOrderedEncoding_PreservesSortOrder(t *testing.T) {
	values := []float64{
		math.Inf(-1),
		-math.MaxFloat64,
		-1e300,
		-1.5,
		-1e-300,
		-math.SmallestNonzeroFloat64,
		math.Copysign(0, -1),
		0,
		math.SmallestNonzeroFloat64,
		1e-300,
		1.5,
		1e300,
		math.MaxFloat64,
		math.Inf(1),
	}

	if !sort.Float64sAreSorted(values) {
		t.Fatal("test grid is not sorted")
	}
	for i, a := range values {
		for _, b := range values[i+1:] {
			if !(a < b) {
				continue
			}
			encodedA := FloatToOrderedInt64(a)
			encodedB := FloatToOrderedInt64(b)
			if encodedA >= encodedB {
				t.Fatalf("encoded order for %v < %v = %d >= %d", a, b, encodedA, encodedB)
			}
		}
	}
}

func TestUnit_BSIValue_BoolToInt64_MapsFalseZeroTrueOne(t *testing.T) {
	if got := BoolToInt64(false); got != 0 {
		t.Fatalf("BoolToInt64(false) = %d, want 0", got)
	}
	if got := BoolToInt64(true); got != 1 {
		t.Fatalf("BoolToInt64(true) = %d, want 1", got)
	}
}
