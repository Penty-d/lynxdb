package vm

// RFC-002 LynxFlow strict-semantics opcode runtime implementations.
//
// These functions implement the new opcodes added for the LynxFlow v2 compiler.
// They follow RFC-002 §5.2 (3VL logic), §5.4 (arithmetic/comparison rules),
// and §5.5 (case sensitivity). The old SPL2 opcodes are untouched.

import (
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/lynxbase/lynxdb/pkg/event"
)

// ---------------------------------------------------------------------------
// Strict comparison (RFC-002 §5.4)
// ---------------------------------------------------------------------------

// strictCompare implements RFC-002 strict comparison semantics:
// - null/missing operand → null
// - same type: typed comparison (string is lexical CS, numeric, bool ==/!= only, timestamp, duration)
// - int vs float: cross-promote (both are "number" — NOT a type error)
// - incompatible types → null + warning counter increment
//
// Returns (result int, isNull bool). result is -1/0/1; isNull means the
// comparison could not be performed (null operand or incompatible types).
func strictCompare(a, b event.Value, warnings *WarningCounters) (int, bool) {
	if a.IsNull() || b.IsNull() {
		return 0, true
	}

	at, bt := a.Type(), b.Type()

	// Same-type fast paths
	if at == bt {
		switch at {
		case event.FieldTypeString:
			return strings.Compare(a.AsString(), b.AsString()), false
		case event.FieldTypeInt:
			ai, bi := a.AsInt(), b.AsInt()
			if ai < bi {
				return -1, false
			} else if ai > bi {
				return 1, false
			}
			return 0, false
		case event.FieldTypeFloat:
			af, bf := a.AsFloat(), b.AsFloat()
			if af < bf {
				return -1, false
			} else if af > bf {
				return 1, false
			}
			return 0, false
		case event.FieldTypeBool:
			// Bool supports ==/!= only. For < > we treat as incompatible.
			// But the caller (eq/neq) will use this correctly.
			ab, bb := boolToInt(a.AsBool()), boolToInt(b.AsBool())
			return ab - bb, false
		case event.FieldTypeTimestamp:
			at, bt := a.AsTimestamp(), b.AsTimestamp()
			if at.Before(bt) {
				return -1, false
			} else if at.After(bt) {
				return 1, false
			}
			return 0, false
		case event.FieldTypeDuration:
			ad, bd := a.AsDuration(), b.AsDuration()
			if ad < bd {
				return -1, false
			} else if ad > bd {
				return 1, false
			}
			return 0, false
		case event.FieldTypeArray:
			return compareArrays(a.AsArray(), b.AsArray()), false
		case event.FieldTypeObject:
			if objectsEqual(a.AsObject(), b.AsObject()) {
				return 0, false
			}
			return strings.Compare(valueToString(a), valueToString(b)), false
		}
	}

	// Cross-type numeric: int vs float (RFC-002: both are "number", cross-promote IS allowed)
	if (at == event.FieldTypeInt && bt == event.FieldTypeFloat) ||
		(at == event.FieldTypeFloat && bt == event.FieldTypeInt) {
		af := toFloat64ForCompare(a)
		bf := toFloat64ForCompare(b)
		if af < bf {
			return -1, false
		} else if af > bf {
			return 1, false
		}
		return 0, false
	}

	// Incompatible types → null + warning
	if warnings != nil {
		warnings.Increment(warnIncompatibleTypes)
	}
	return 0, true
}

func toFloat64ForCompare(v event.Value) float64 {
	switch v.Type() {
	case event.FieldTypeInt:
		return float64(v.AsInt())
	case event.FieldTypeFloat:
		return v.AsFloat()
	default:
		return 0
	}
}

// strictEq implements RFC-002 strict equality. Returns (bool, isNull).
func strictEq(a, b event.Value, warnings *WarningCounters) (bool, bool) {
	cmp, isNull := strictCompare(a, b, warnings)
	if isNull {
		return false, true
	}
	return cmp == 0, false
}

// ---------------------------------------------------------------------------
// Strict arithmetic (RFC-002 §5.4)
// ---------------------------------------------------------------------------

// addStrict implements RFC-002 §5.4 addition:
// - null propagation
// - string + string = concat ONLY
// - string + non-string = null + warning (sema catches known cases; runtime is lenient-null)
// - int + int = int
// - int + float or float + float = float
// - timestamp/duration algebra
func addStrict(a, b event.Value, warnings *WarningCounters) event.Value {
	if a.IsNull() || b.IsNull() {
		return event.NullValue()
	}
	at, bt := a.Type(), b.Type()

	// String + string = concat
	if at == event.FieldTypeString && bt == event.FieldTypeString {
		return event.StringValue(a.AsString() + b.AsString())
	}
	// String + non-string or non-string + string = null + warning
	if at == event.FieldTypeString || bt == event.FieldTypeString {
		if warnings != nil {
			warnings.Increment(warnStringArithmetic)
		}
		return event.NullValue()
	}
	// int + int
	if at == event.FieldTypeInt && bt == event.FieldTypeInt {
		return event.IntValue(a.AsInt() + b.AsInt())
	}
	// float/float, int/float, float/int → float
	if isNumericType(at) && isNumericType(bt) {
		return event.FloatValue(toFloat64ForCompare(a) + toFloat64ForCompare(b))
	}
	// Timestamp/duration algebra
	if at == event.FieldTypeTimestamp && bt == event.FieldTypeDuration {
		return event.TimestampValue(a.AsTimestamp().Add(b.AsDuration()))
	}
	if at == event.FieldTypeDuration && bt == event.FieldTypeTimestamp {
		return event.TimestampValue(b.AsTimestamp().Add(a.AsDuration()))
	}
	if at == event.FieldTypeDuration && bt == event.FieldTypeDuration {
		return event.DurationValue(a.AsDuration() + b.AsDuration())
	}
	// Incompatible
	if warnings != nil {
		warnings.Increment(warnIncompatibleTypes)
	}
	return event.NullValue()
}

func subStrict(a, b event.Value, warnings *WarningCounters) event.Value {
	if a.IsNull() || b.IsNull() {
		return event.NullValue()
	}
	at, bt := a.Type(), b.Type()

	if at == event.FieldTypeInt && bt == event.FieldTypeInt {
		return event.IntValue(a.AsInt() - b.AsInt())
	}
	if isNumericType(at) && isNumericType(bt) {
		return event.FloatValue(toFloat64ForCompare(a) - toFloat64ForCompare(b))
	}
	// ts - ts → dur
	if at == event.FieldTypeTimestamp && bt == event.FieldTypeTimestamp {
		return event.DurationValue(a.AsTimestamp().Sub(b.AsTimestamp()))
	}
	// ts - dur → ts
	if at == event.FieldTypeTimestamp && bt == event.FieldTypeDuration {
		return event.TimestampValue(a.AsTimestamp().Add(-b.AsDuration()))
	}
	// dur - dur → dur
	if at == event.FieldTypeDuration && bt == event.FieldTypeDuration {
		return event.DurationValue(a.AsDuration() - b.AsDuration())
	}
	if warnings != nil {
		warnings.Increment(warnIncompatibleTypes)
	}
	return event.NullValue()
}

func mulStrict(a, b event.Value, warnings *WarningCounters) event.Value {
	if a.IsNull() || b.IsNull() {
		return event.NullValue()
	}
	at, bt := a.Type(), b.Type()

	if at == event.FieldTypeInt && bt == event.FieldTypeInt {
		return event.IntValue(a.AsInt() * b.AsInt())
	}
	if isNumericType(at) && isNumericType(bt) {
		return event.FloatValue(toFloat64ForCompare(a) * toFloat64ForCompare(b))
	}
	// dur * number → dur (commutative)
	if at == event.FieldTypeDuration && isNumericType(bt) {
		return event.DurationValue(time.Duration(float64(a.AsDuration()) * toFloat64ForCompare(b)))
	}
	if isNumericType(at) && bt == event.FieldTypeDuration {
		return event.DurationValue(time.Duration(toFloat64ForCompare(a) * float64(b.AsDuration())))
	}
	if warnings != nil {
		warnings.Increment(warnIncompatibleTypes)
	}
	return event.NullValue()
}

func divStrict(a, b event.Value, warnings *WarningCounters) event.Value {
	if a.IsNull() || b.IsNull() {
		return event.NullValue()
	}
	at, bt := a.Type(), b.Type()

	// int / int → TRUNCATING int (5/2 == 2 per §5.4)
	if at == event.FieldTypeInt && bt == event.FieldTypeInt {
		bv := b.AsInt()
		if bv == 0 {
			return event.NullValue()
		}
		return event.IntValue(a.AsInt() / bv)
	}
	// numeric / numeric → float
	if isNumericType(at) && isNumericType(bt) {
		bf := toFloat64ForCompare(b)
		if bf == 0 {
			return event.NullValue()
		}
		return event.FloatValue(toFloat64ForCompare(a) / bf)
	}
	// dur / dur → float
	if at == event.FieldTypeDuration && bt == event.FieldTypeDuration {
		bd := b.AsDuration()
		if bd == 0 {
			return event.NullValue()
		}
		return event.FloatValue(float64(a.AsDuration()) / float64(bd))
	}
	// dur / number → dur
	if at == event.FieldTypeDuration && isNumericType(bt) {
		bf := toFloat64ForCompare(b)
		if bf == 0 {
			return event.NullValue()
		}
		return event.DurationValue(time.Duration(float64(a.AsDuration()) / bf))
	}
	if warnings != nil {
		warnings.Increment(warnIncompatibleTypes)
	}
	return event.NullValue()
}

func modStrict(a, b event.Value, warnings *WarningCounters) event.Value {
	if a.IsNull() || b.IsNull() {
		return event.NullValue()
	}
	// RFC-002 §5.4: % is int-only (other types → null + warning)
	if a.Type() == event.FieldTypeInt && b.Type() == event.FieldTypeInt {
		bv := b.AsInt()
		if bv == 0 {
			return event.NullValue()
		}
		return event.IntValue(a.AsInt() % bv)
	}
	// dur % dur → dur (also valid per §5.4)
	if a.Type() == event.FieldTypeDuration && b.Type() == event.FieldTypeDuration {
		bd := b.AsDuration()
		if bd == 0 {
			return event.NullValue()
		}
		return event.DurationValue(a.AsDuration() % bd)
	}
	if warnings != nil {
		warnings.Increment(warnModOnNonInt)
	}
	return event.NullValue()
}

func negStrict(a event.Value, warnings *WarningCounters) event.Value {
	if a.IsNull() {
		return event.NullValue()
	}
	switch a.Type() {
	case event.FieldTypeInt:
		return event.IntValue(-a.AsInt())
	case event.FieldTypeFloat:
		return event.FloatValue(-a.AsFloat())
	case event.FieldTypeDuration:
		return event.DurationValue(-a.AsDuration())
	default:
		if warnings != nil {
			warnings.Increment(warnIncompatibleTypes)
		}
		return event.NullValue()
	}
}

func isNumericType(t event.FieldType) bool {
	return t == event.FieldTypeInt || t == event.FieldTypeFloat
}

// ---------------------------------------------------------------------------
// OpLoadPath: flat-column first, then object walk, no _raw fallback (D25)
// ---------------------------------------------------------------------------

func execLoadPath(fields map[string]event.Value, path string) event.Value {
	// 1. Flat column lookup (the full dotted path as a literal column name)
	if val, ok := fields[path]; ok {
		return val
	}
	// 2. Object walk: split on '.', load root, walk members
	dot := strings.IndexByte(path, '.')
	if dot <= 0 {
		// No dot or leading dot — field simply missing
		return event.NullValue()
	}
	root := path[:dot]
	rest := path[dot+1:]
	rootVal, ok := fields[root]
	if !ok || rootVal.IsNull() {
		return event.NullValue()
	}
	// Walk the chain
	val := rootVal
	for _, part := range strings.Split(rest, ".") {
		if val.IsNull() {
			return event.NullValue()
		}
		if val.Type() != event.FieldTypeObject {
			return event.NullValue()
		}
		m := val.AsObject()
		next, found := m[part]
		if !found {
			return event.NullValue()
		}
		val = next
	}
	return val
}

// ---------------------------------------------------------------------------
// OpHasToken: case-insensitive whole-token match per §6.1/6.2
// ---------------------------------------------------------------------------

// hasToken checks if the field string contains the search term as a whole token,
// case-insensitively. A token is a run of ASCII alphanumerics and Unicode
// letters/digits; everything else delimits. Multi-word terms are treated as
// conjunction: all tokens must be present.
func hasToken(field, term string) bool {
	if field == "" || term == "" {
		return false
	}
	fieldLower := strings.ToLower(field)
	termLower := strings.ToLower(term)

	// Tokenize the term to get the tokens we need to find
	termTokens := tokenize(termLower)
	if len(termTokens) == 0 {
		return false
	}

	// Tokenize the field
	fieldTokens := tokenize(fieldLower)
	fieldSet := make(map[string]struct{}, len(fieldTokens))
	for _, t := range fieldTokens {
		fieldSet[t] = struct{}{}
	}

	// All term tokens must be present
	for _, tt := range termTokens {
		if _, ok := fieldSet[tt]; !ok {
			return false
		}
	}
	return true
}

// tokenize splits a string into tokens per the tokenizer contract (§6.1):
// runs of ASCII alphanumerics and Unicode letters/digits.
func tokenize(s string) []string {
	var tokens []string
	start := -1
	for i, r := range s {
		isToken := unicode.IsLetter(r) || unicode.IsDigit(r)
		if isToken {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 {
				tokens = append(tokens, s[start:i])
				start = -1
			}
		}
	}
	if start >= 0 {
		tokens = append(tokens, s[start:])
	}
	return tokens
}

// ---------------------------------------------------------------------------
// OpSubstr0Based: 0-based start per RFC-002
// ---------------------------------------------------------------------------

func substr0Based(str, start, length event.Value) event.Value {
	if str.IsNull() || start.IsNull() {
		return event.NullValue()
	}
	s := valueToString(str)
	runes := []rune(s)
	n := int64(len(runes))

	si, ok := valueToInt64(start)
	if !ok {
		return event.NullValue()
	}
	// Negative counts from end
	if si < 0 {
		si += n
	}
	if si < 0 {
		si = 0
	}
	if si >= n {
		return event.StringValue("")
	}

	var li int64
	if length.IsNull() {
		li = n - si // to end
	} else {
		l, lok := valueToInt64(length)
		if !lok {
			return event.NullValue()
		}
		li = l
	}
	if li < 0 {
		return event.StringValue("")
	}
	end := si + li
	if end > n {
		end = n
	}
	return event.StringValue(string(runes[si:end]))
}

// ---------------------------------------------------------------------------
// OpExtract / OpExtractAll
// ---------------------------------------------------------------------------

func extractFirst(re *regexp.Regexp, s string) event.Value {
	if re.NumSubexp() == 0 {
		// No capture groups, return the full match
		m := re.FindString(s)
		if m == "" {
			return event.NullValue()
		}
		return event.StringValue(m)
	}
	matches := re.FindStringSubmatch(s)
	if matches == nil {
		return event.NullValue()
	}
	if len(matches) > 1 {
		return event.StringValue(matches[1])
	}
	return event.NullValue()
}

func extractAllMatches(re *regexp.Regexp, s string) event.Value {
	if re.NumSubexp() == 0 {
		matches := re.FindAllString(s, -1)
		if matches == nil {
			return event.NullValue()
		}
		elems := make([]event.Value, len(matches))
		for i, m := range matches {
			elems[i] = event.StringValue(m)
		}
		return event.ArrayValue(elems)
	}
	all := re.FindAllStringSubmatch(s, -1)
	if all == nil {
		return event.NullValue()
	}
	elems := make([]event.Value, len(all))
	for i, m := range all {
		if len(m) > 1 {
			elems[i] = event.StringValue(m[1])
		} else {
			elems[i] = event.NullValue()
		}
	}
	return event.ArrayValue(elems)
}

// ---------------------------------------------------------------------------
// OpInStrict: strict equality in-list check with null awareness
// ---------------------------------------------------------------------------

// inStrict checks if val is in the list using strict equality.
// If val is null → result is null.
// If any item comparison is null (incompatible types), accumulate null.
// If any item matches → true.
// If no match and no null comparison → false.
// If no match and some null comparison → null (3VL).
func inStrict(val event.Value, items []event.Value, warnings *WarningCounters) event.Value {
	if val.IsNull() {
		return event.NullValue()
	}
	hasNullComparison := false
	for _, item := range items {
		eq, isNull := strictEq(val, item, warnings)
		if isNull {
			hasNullComparison = true
			continue
		}
		if eq {
			return event.BoolValue(true)
		}
	}
	if hasNullComparison {
		return event.NullValue()
	}
	return event.BoolValue(false)
}

// ---------------------------------------------------------------------------
// Strict-cast bang error
// ---------------------------------------------------------------------------

// ErrStrictCast is returned when a strict-cast function (e.g. int!()) fails.
// The error includes the function name and the value that could not be cast,
// providing row context for debugging. The VM returns this error, halting the
// query — this is the designed behavior for bang variants per RFC-002 §5.4.
type ErrStrictCast struct {
	Func  string // e.g. "int!"
	Value string // string representation of the offending value
	Type  string // type of the offending value
}

func (e *ErrStrictCast) Error() string {
	return fmt.Sprintf("strict cast %s failed: cannot convert %s (type %s)", e.Func, e.Value, e.Type)
}

// Ensure unused imports are referenced.
var _ = binary.BigEndian
var _ = math.Pi
