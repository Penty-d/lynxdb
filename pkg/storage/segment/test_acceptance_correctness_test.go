//go:build acceptance

package segment

import (
	"math/rand"
	"strconv"
	"testing"
)

func TestAcceptance_RangeBSICorrectness_RandomPredicatesMatchBruteScan(t *testing.T) {
	fx := makeRangeBSIPhase6Fixture(t, 50_000)
	rng := rand.New(rand.NewSource(20260508))
	fields := []string{"status", "duration_ms", "bytes"}
	ops := []string{"<", "<=", "=", ">=", ">"}

	for i := 0; i < 200; i++ {
		field := fields[rng.Intn(len(fields))]
		if rng.Intn(6) == 0 {
			lo, hi := rangeBSIRandomBounds(field, rng)
			if lo > hi {
				lo, hi = hi, lo
			}
			c := rangeBSIQueryCase{
				name: field + "_between_" + strconv.FormatInt(lo, 10) + "_" + strconv.FormatInt(hi, 10),
				preds: []Predicate{
					{Field: field, Op: ">=", Value: strconv.FormatInt(lo, 10)},
					{Field: field, Op: "<=", Value: strconv.FormatInt(hi, 10)},
				},
				filter: &RGFilterNode{
					Op: RGFilterAnd,
					Children: []RGFilterNode{
						{Op: RGFilterFieldRange, Field: field, RangeOp: ">=", RangeVal: strconv.FormatInt(lo, 10)},
						{Op: RGFilterFieldRange, Field: field, RangeOp: "<=", RangeVal: strconv.FormatInt(hi, 10)},
					},
				},
			}
			assertRangeBSICountMatchesBrute(t, fx, c)
			continue
		}

		op := ops[rng.Intn(len(ops))]
		value, _ := rangeBSIRandomBounds(field, rng)
		valueText := strconv.FormatInt(value, 10)
		c := rangeBSIQueryCase{
			name: field + "_" + op + "_" + valueText,
			preds: []Predicate{{
				Field: field,
				Op:    op,
				Value: valueText,
			}},
			filter: &RGFilterNode{
				Op:       RGFilterFieldRange,
				Field:    field,
				RangeOp:  op,
				RangeVal: valueText,
			},
		}
		assertRangeBSICountMatchesBrute(t, fx, c)
	}
}

func assertRangeBSICountMatchesBrute(t *testing.T, fx rangeBSIBenchFixture, c rangeBSIQueryCase) {
	t.Helper()
	want, _ := measureRangeBSIBruteCount(t, fx.v1, c, 1)
	got, _, _ := measureRangeBSIBitmapCount(t, fx.v2, c, 1)
	if got != want {
		t.Fatalf("%s BSI count = %d, brute count = %d", c.name, got, want)
	}
}

func rangeBSIRandomBounds(field string, rng *rand.Rand) (int64, int64) {
	switch field {
	case "status":
		return int64(150 + rng.Intn(500)), int64(150 + rng.Intn(500))
	case "duration_ms":
		return int64(rng.Intn(30_500)), int64(rng.Intn(30_500))
	case "bytes":
		return int64(rng.Intn(2_100_000)), int64(rng.Intn(2_100_000))
	default:
		return 0, 0
	}
}
