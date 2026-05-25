package segment

import (
	"math"
	"strconv"
)

type intPredicateMode uint8

const (
	intPredicateCompare intPredicateMode = iota
	intPredicateAlways
	intPredicateNever
)

func coerceIntPredicateValue(value, op string) (int64, intPredicateMode, bool) {
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n, intPredicateCompare, true
	}

	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, intPredicateNever, false
	}

	switch op {
	case "=", "==":
		if math.Trunc(f) != f {
			return 0, intPredicateNever, true
		}
	case "!=":
		if math.Trunc(f) != f {
			return 0, intPredicateAlways, true
		}
	case ">":
		f = math.Floor(f)
	case ">=":
		f = math.Ceil(f)
	case "<":
		f = math.Ceil(f)
	case "<=":
		f = math.Floor(f)
	default:
		return 0, intPredicateNever, false
	}

	const (
		minInt64Float = -9223372036854775808.0
		maxInt64Float = 9223372036854775808.0
	)
	if f < minInt64Float || f >= maxInt64Float {
		return 0, intPredicateNever, false
	}

	return int64(f), intPredicateCompare, true
}
