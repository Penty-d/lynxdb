package event

import (
	"testing"
	"time"
)

var testNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestMemSize_Scalars(t *testing.T) {
	// Scalar types must return a fixed base size (40 bytes).
	cases := []struct {
		name string
		v    Value
	}{
		{"null", NullValue()},
		{"int", IntValue(42)},
		{"float", FloatValue(3.14)},
		{"bool", BoolValue(true)},
		{"timestamp", TimestampValue(testNow)},
		{"duration", DurationValue(5e9)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.v.MemSize()
			if got != 40 {
				t.Errorf("MemSize() = %d, want 40 for scalar %s", got, tc.name)
			}
		})
	}
}

func TestMemSize_String(t *testing.T) {
	v := StringValue("hello") // 5 bytes
	got := v.MemSize()
	// base(40) + len("hello")(5) = 45
	if got != 45 {
		t.Errorf("MemSize() = %d, want 45", got)
	}

	v2 := StringValue("") // empty
	if v2.MemSize() != 40 {
		t.Errorf("MemSize() = %d, want 40 for empty string", v2.MemSize())
	}
}

func TestMemSize_Array(t *testing.T) {
	arr := ArrayValue([]Value{IntValue(1), IntValue(2), IntValue(3)})
	got := arr.MemSize()
	// base(40) + refPayload(32) + 3*base(40) = 40+32+120 = 192
	if got != 192 {
		t.Errorf("MemSize() = %d, want 192 for [1,2,3]", got)
	}

	// Array with strings.
	arrStr := ArrayValue([]Value{StringValue("abc"), StringValue("defgh")})
	gotStr := arrStr.MemSize()
	// base(40) + refPayload(32) + 2*base(40) + extra(3+5) = 40+32+80+8 = 160
	if gotStr != 160 {
		t.Errorf("MemSize() = %d, want 160 for ['abc','defgh']", gotStr)
	}
}

func TestMemSize_Object(t *testing.T) {
	obj := ObjectValue(map[string]Value{
		"a": IntValue(1),
		"b": StringValue("xy"),
	})
	got := obj.MemSize()
	// base(40) + refPayload(32) +
	// entry "a": mapEntryOverhead(56) + len("a")(1) + MemSize(int)(40) = 97
	// entry "b": mapEntryOverhead(56) + len("b")(1) + MemSize("xy")(42) = 99
	// total = 40+32+97+99 = 268
	if got != 268 {
		t.Errorf("MemSize() = %d, want 268", got)
	}
}

func TestMemSize_NestedArrayOfObjects(t *testing.T) {
	v := ArrayValue([]Value{
		ObjectValue(map[string]Value{"x": IntValue(1)}),
		ObjectValue(map[string]Value{"y": StringValue("hello")}),
	})
	got := v.MemSize()
	// Must be substantially larger than a scalar.
	if got <= 100 {
		t.Errorf("MemSize() = %d, too small for nested array-of-objects", got)
	}
}

func TestMemSize_ArrayOfThousandStrings(t *testing.T) {
	elems := make([]Value, 1000)
	for i := range elems {
		elems[i] = StringValue("some_string_value_here")
	}
	arr := ArrayValue(elems)
	got := arr.MemSize()
	scalar := IntValue(0).MemSize()

	// Array of 1000 strings must account for substantially more than a scalar.
	if int64(got) <= int64(scalar)*10 {
		t.Errorf("MemSize() = %d, should be >> %d for 1000 strings", got, scalar*10)
	}
	// Should include the string payload: 1000 * 22 = 22000 bytes of string data.
	if got < 22000 {
		t.Errorf("MemSize() = %d, should be >= 22000 to include string payloads", got)
	}
}
