package vm

import (
	"encoding/binary"
	"fmt"
)

// Opcode represents a single VM instruction.
type Opcode byte

const (
	// Stack Manipulation.
	OpNop Opcode = 0x00
	OpPop Opcode = 0x01
	OpDup Opcode = 0x02

	// Constants (operand: 2-byte index into constant pool).
	OpConstInt   Opcode = 0x10
	OpConstFloat Opcode = 0x11
	OpConstStr   Opcode = 0x12
	OpConstTrue  Opcode = 0x13
	OpConstFalse Opcode = 0x14
	OpConstNull  Opcode = 0x15

	// Field Access (operand: 2-byte field name index).
	OpLoadField   Opcode = 0x20
	OpStoreField  Opcode = 0x21
	OpFieldExists Opcode = 0x22

	// Integer Arithmetic (pop 2, push 1).
	OpAddInt Opcode = 0x30
	OpSubInt Opcode = 0x31
	OpMulInt Opcode = 0x32
	OpDivInt Opcode = 0x33
	OpModInt Opcode = 0x34
	OpNegInt Opcode = 0x35

	// Float Arithmetic.
	OpAddFloat Opcode = 0x38
	OpSubFloat Opcode = 0x39
	OpMulFloat Opcode = 0x3A
	OpDivFloat Opcode = 0x3B
	OpNegFloat Opcode = 0x3C

	// Mixed Arithmetic (auto-promote int->float).
	OpAdd Opcode = 0x3E
	OpSub Opcode = 0x3F
	OpMul Opcode = 0x40
	OpDiv Opcode = 0x41
	OpMod Opcode = 0x42

	// String Operations.
	OpConcat    Opcode = 0x48
	OpStrLen    Opcode = 0x49
	OpSubstr    Opcode = 0x4A
	OpToLower   Opcode = 0x4B
	OpToUpper   Opcode = 0x4C
	OpStrMatch  Opcode = 0x4D
	OpGlobMatch Opcode = 0x4E
	OpReplace   Opcode = 0x4F
	OpSplit     Opcode = 0x57

	// String Predicates (pop 2, push bool).
	OpStartsWith Opcode = 0x59
	OpEndsWith   Opcode = 0x5A
	OpContains   Opcode = 0x5B
	OpTrim       Opcode = 0x5D
	OpLTrim      Opcode = 0x5E
	OpRTrim      Opcode = 0x5F

	// Comparison (pop 2, push bool).
	OpEq     Opcode = 0x50
	OpNeq    Opcode = 0x51
	OpLt     Opcode = 0x52
	OpLte    Opcode = 0x53
	OpGt     Opcode = 0x54
	OpGte    Opcode = 0x55
	OpInList Opcode = 0x56
	OpLike   Opcode = 0x58
	// OpBSIHandledCompare jumps to a true branch when runtime metadata proves
	// the current row already survived this field's range-BSI mask.
	OpBSIHandledCompare Opcode = 0x5C

	// Logic.
	OpAnd Opcode = 0x60
	OpOr  Opcode = 0x61
	OpNot Opcode = 0x62
	OpXor Opcode = 0x63

	// Control Flow.
	OpJump        Opcode = 0x70
	OpJumpIfFalse Opcode = 0x71
	OpJumpIfTrue  Opcode = 0x72

	// Type Conversion.
	OpToInt    Opcode = 0x80
	OpToFloat  Opcode = 0x81
	OpToString Opcode = 0x82
	OpToBool   Opcode = 0x83

	// Math Functions.
	OpRound      Opcode = 0x90
	OpLn         Opcode = 0x91
	OpAbs        Opcode = 0x92
	OpCeil       Opcode = 0x93
	OpFloor      Opcode = 0x94
	OpSqrt       Opcode = 0x95
	OpExp        Opcode = 0x96
	OpPow        Opcode = 0x97
	OpLog        Opcode = 0x98
	OpMax        Opcode = 0x99
	OpMin        Opcode = 0x9A
	OpMathUnary  Opcode = 0x9B
	OpMathBinary Opcode = 0x9C
	OpRandom     Opcode = 0x9D

	// Multivalue Operations.
	OpMvAppend Opcode = 0xA0
	OpMvJoin   Opcode = 0xA1
	OpMvDedup  Opcode = 0xA2
	OpMvCount  Opcode = 0xA3

	// Null Handling.
	OpCoalesce  Opcode = 0xB0
	OpIsNull    Opcode = 0xB1
	OpIsNotNull Opcode = 0xB2

	// Type Checks (pop 1, push bool).
	OpIsNum    Opcode = 0xB3
	OpIsInt    Opcode = 0xB4
	OpIsBool   Opcode = 0xB5
	OpTypeOf   Opcode = 0xB6
	OpIsArray  Opcode = 0xB7
	OpIsObject Opcode = 0xB8

	// Time Functions.
	OpStrftime    Opcode = 0xC0
	OpURLDecode   Opcode = 0xC1
	OpMD5         Opcode = 0xC2
	OpSHA1        Opcode = 0xC3
	OpSHA256      Opcode = 0xC4
	OpSHA512      Opcode = 0xC5
	OpPrintf      Opcode = 0xC6
	OpIPMask      Opcode = 0xC7
	OpStrptime    Opcode = 0xC8
	OpSearchMatch Opcode = 0xC9

	// Network (operand: 2-byte CIDR pool index).
	OpCIDRMatch Opcode = 0xE0 // net.IPNet.Contains

	// RFC-002 typed-value opcodes (duration, array, object).
	// These are additive plumbing for the LynxFlow frontend; the SPL2
	// compiler does not emit them yet.
	OpArrayBuild    Opcode = 0xE1 // 2-byte operand: element count N; pop N values, push array
	OpObjectBuild   Opcode = 0xE2 // 2-byte operand: entry count N; pop 2N values (key,val pairs), push object
	OpIndex         Opcode = 0xE3 // pop index, pop container; arr[i] / obj["k"]; OOB/missing/null → null
	OpMember        Opcode = 0xE4 // 2-byte operand: constant-pool index of key string; pop object, push obj.key or null
	OpLen           Opcode = 0xE5 // pop value; string → rune count, array → element count; else null
	OpConstDuration Opcode = 0xE6 // 2-byte operand: constant-pool index; pushes duration value

	// RFC-002 LynxFlow strict-comparison opcodes.
	// Same-type: typed comparison. int vs float: cross-promote (both are "number").
	// Null/missing operand → null. Incompatible types → null + warning counter.
	OpEqStrict  Opcode = 0xE7 // pop 2, push bool/null; strict == (CS strings, no coercion)
	OpNeqStrict Opcode = 0xE8 // pop 2, push bool/null; strict !=
	OpLtStrict  Opcode = 0xE9 // pop 2, push bool/null; strict <
	OpLteStrict Opcode = 0xEA // pop 2, push bool/null; strict <=
	OpGtStrict  Opcode = 0xEB // pop 2, push bool/null; strict >
	OpGteStrict Opcode = 0xEC // pop 2, push bool/null; strict >=

	// RFC-002 LynxFlow 3VL logic opcodes.
	OpNot3VL        Opcode = 0xED // pop 1, push bool/null; not(true)=false, not(false)=true, not(null)=null, not(non-bool)=null+warn
	OpJumpIfNull3VL Opcode = 0xEE // 2-byte operand: target; if TOS is null, pop and jump; else leave on stack
	OpAnd3VLNull    Opcode = 0xEF // pop 1 (right); left was null: if right=false→false, else→null
	OpOr3VLNull     Opcode = 0xF0 // pop 1 (right); left was null: if right=true→true, else→null

	// RFC-002 LynxFlow strict arithmetic opcodes.
	// These follow §5.4 exactly: int+int→int, int+float→float, string+string→concat
	// (+ only), division by zero→null, no string-to-number promotion.
	// Timestamp/duration algebra same as existing opcodes.
	OpAddStrict Opcode = 0xF1 // string+string=concat, int+int=int, int+float=float, ts/dur algebra; string+num→null+warn
	OpSubStrict Opcode = 0xF2 // no string sub; int-int=int, int-float=float, ts/dur algebra
	OpMulStrict Opcode = 0xF3 // int*int=int, int*float=float, dur*num, no string
	OpDivStrict Opcode = 0xF4 // int/int=TRUNCATING int, int/float=float, dur/dur=float, dur/num=dur, /0→null
	OpModStrict Opcode = 0xF5 // int%int only; non-int→null+warn; %0→null

	// RFC-002 LynxFlow field-path and search opcodes.
	OpLoadPath     Opcode = 0xF6 // 2-byte operand: constant-pool index of dotted path string; flat column first, then object walk, no _raw fallback
	OpFieldMissing Opcode = 0xF7 // 2-byte operand: field name index; pushes true if field absent from row
	OpNegStrict    Opcode = 0xF8 // pop 1; negate int/float/duration; non-numeric→null+warn
	OpInStrict     Opcode = 0xF9 // 2-byte operand: count N; pop N items + 1 value; strict equality check; null-aware

	// RFC-002 LynxFlow function opcodes.
	OpHasToken     Opcode = 0xFA // pop 2 (field, term); case-insensitive token match per §6.1
	OpContainsCI   Opcode = 0xFB // pop 2 (field, substr); case-insensitive substring
	OpExtract      Opcode = 0xFC // 2-byte operand: regex pool index; pop string, push first capture group or null
	OpExtractAll   Opcode = 0xFD // 2-byte operand: regex pool index; pop string, push array of all matches
	OpSubstr0Based Opcode = 0xFE // pop 3 (str, start, len); 0-based start per RFC-002

	// RFC-002 lambda + array/object function opcodes (b2).
	OpArrayAny        Opcode = 0x03 // 2-byte operand: sub-program index; pop array, push bool (3VL)
	OpArrayAll        Opcode = 0x04 // 2-byte operand: sub-program index; pop array, push bool (3VL)
	OpArrayFilter     Opcode = 0x05 // 2-byte operand: sub-program index; pop array, push filtered array
	OpArrayMap        Opcode = 0x06 // 2-byte operand: sub-program index; pop array, push mapped array
	OpLoadLambdaParam Opcode = 0x07 // 2-byte operand: depth (0 = current lambda param)
	OpSlice           Opcode = 0x08 // pop 2 or 3 (arr, start[, end]); push sliced array
	OpArrayConcat     Opcode = 0x09 // 2-byte operand: count N; pop N arrays, push concatenated
	OpArrayDistinct   Opcode = 0x0A // pop array, push order-preserving deduplicated array
	OpArraySort       Opcode = 0x0B // pop array, push sorted array (CompareValues; nulls last)
	OpFlatten         Opcode = 0x0C // pop array, push one-level-flattened array
	OpKeys            Opcode = 0x23 // pop object, push sorted array of key strings
	OpValues          Opcode = 0x24 // pop object, push array of values in key-sorted order
	OpMerge           Opcode = 0x25 // pop 2 objects (a, b), push merged (b wins)
	OpHasKey          Opcode = 0x26 // pop object + string key, push bool
	OpURLParse        Opcode = 0x27 // pop string, push object {scheme, host, port, path, query, fragment}
	OpIPParseObj      Opcode = 0x28 // pop string, push object {version, private, loopback}
	OpFromJSONNative  Opcode = 0x29 // pop string, push native Value (recursive arrays/objects)
	OpBin             Opcode = 0x2A // pop 2 (ts, dur); snap ts to dur boundary; coercion: string→parse RFC3339, int→unix-nanos; push timestamp

	// JSON Functions.
	OpJsonExtract  Opcode = 0xD0 // pop path, pop field, push extracted value
	OpJsonValid    Opcode = 0xD1 // pop field, push bool
	OpJsonKeys     Opcode = 0xD2 // pop path, pop field, push JSON array of keys
	OpJsonArrayLen Opcode = 0xD3 // pop path, pop field, push int length
	OpJsonObject   Opcode = 0xD4 // 2-byte operand: arg count; pop N values, push JSON object
	OpJsonArray    Opcode = 0xD5 // 2-byte operand: arg count; pop N values, push JSON array
	OpJsonType     Opcode = 0xD6 // pop path, pop field, push type string
	OpJsonSet      Opcode = 0xD7 // pop value, pop path, pop field, push modified JSON
	OpJsonRemove   Opcode = 0xD8 // pop path, pop field, push modified JSON
	OpJsonMerge    Opcode = 0xD9 // pop json2, pop json1, push merged JSON

	OpReturn Opcode = 0xFF
)

const (
	mathFnAcos = iota
	mathFnAcosh
	mathFnAsin
	mathFnAsinh
	mathFnAtan
	mathFnAtanh
	mathFnCos
	mathFnCosh
	mathFnSin
	mathFnSinh
	mathFnTan
	mathFnTanh
	mathFnAtan2
	mathFnHypot
)

// Definition describes an opcode's name and operand widths.
type Definition struct {
	Name          string
	OperandWidths []int // each entry is 1 or 2 bytes
}

var definitions = map[Opcode]*Definition{
	OpNop: {"OpNop", nil},
	OpPop: {"OpPop", nil},
	OpDup: {"OpDup", nil},

	OpConstInt:   {"OpConstInt", []int{2}},
	OpConstFloat: {"OpConstFloat", []int{2}},
	OpConstStr:   {"OpConstStr", []int{2}},
	OpConstTrue:  {"OpConstTrue", nil},
	OpConstFalse: {"OpConstFalse", nil},
	OpConstNull:  {"OpConstNull", nil},

	OpLoadField:   {"OpLoadField", []int{2}},
	OpStoreField:  {"OpStoreField", []int{2}},
	OpFieldExists: {"OpFieldExists", []int{2}},

	OpAddInt: {"OpAddInt", nil},
	OpSubInt: {"OpSubInt", nil},
	OpMulInt: {"OpMulInt", nil},
	OpDivInt: {"OpDivInt", nil},
	OpModInt: {"OpModInt", nil},
	OpNegInt: {"OpNegInt", nil},

	OpAddFloat: {"OpAddFloat", nil},
	OpSubFloat: {"OpSubFloat", nil},
	OpMulFloat: {"OpMulFloat", nil},
	OpDivFloat: {"OpDivFloat", nil},
	OpNegFloat: {"OpNegFloat", nil},

	OpAdd: {"OpAdd", nil},
	OpSub: {"OpSub", nil},
	OpMul: {"OpMul", nil},
	OpDiv: {"OpDiv", nil},
	OpMod: {"OpMod", nil},

	OpConcat:    {"OpConcat", nil},
	OpStrLen:    {"OpStrLen", nil},
	OpSubstr:    {"OpSubstr", nil},
	OpToLower:   {"OpToLower", nil},
	OpToUpper:   {"OpToUpper", nil},
	OpStrMatch:  {"OpStrMatch", []int{2}},
	OpGlobMatch: {"OpGlobMatch", []int{2}},
	OpReplace:   {"OpReplace", []int{2}},
	OpSplit:     {"OpSplit", nil},

	OpStartsWith: {"OpStartsWith", nil},
	OpEndsWith:   {"OpEndsWith", nil},
	OpContains:   {"OpContains", nil},
	OpTrim:       {"OpTrim", nil},
	OpLTrim:      {"OpLTrim", nil},
	OpRTrim:      {"OpRTrim", nil},

	OpEq:                {"OpEq", nil},
	OpNeq:               {"OpNeq", nil},
	OpLt:                {"OpLt", nil},
	OpLte:               {"OpLte", nil},
	OpGt:                {"OpGt", nil},
	OpGte:               {"OpGte", nil},
	OpInList:            {"OpInList", []int{2}},
	OpLike:              {"OpLike", nil},
	OpBSIHandledCompare: {"OpBSIHandledCompare", []int{2, 2}},

	OpAnd: {"OpAnd", nil},
	OpOr:  {"OpOr", nil},
	OpNot: {"OpNot", nil},
	OpXor: {"OpXor", nil},

	OpJump:        {"OpJump", []int{2}},
	OpJumpIfFalse: {"OpJumpIfFalse", []int{2}},
	OpJumpIfTrue:  {"OpJumpIfTrue", []int{2}},

	OpToInt:    {"OpToInt", nil},
	OpToFloat:  {"OpToFloat", nil},
	OpToString: {"OpToString", nil},
	OpToBool:   {"OpToBool", nil},

	OpRound:      {"OpRound", nil},
	OpLn:         {"OpLn", nil},
	OpAbs:        {"OpAbs", nil},
	OpCeil:       {"OpCeil", nil},
	OpFloor:      {"OpFloor", nil},
	OpSqrt:       {"OpSqrt", nil},
	OpExp:        {"OpExp", nil},
	OpPow:        {"OpPow", nil},
	OpLog:        {"OpLog", nil},
	OpMax:        {"OpMax", []int{2}},
	OpMin:        {"OpMin", []int{2}},
	OpMathUnary:  {"OpMathUnary", []int{2}},
	OpMathBinary: {"OpMathBinary", []int{2}},
	OpRandom:     {"OpRandom", nil},

	OpMvAppend: {"OpMvAppend", []int{2}},
	OpMvJoin:   {"OpMvJoin", nil},
	OpMvDedup:  {"OpMvDedup", nil},
	OpMvCount:  {"OpMvCount", nil},

	OpCoalesce:  {"OpCoalesce", []int{2}},
	OpIsNull:    {"OpIsNull", nil},
	OpIsNotNull: {"OpIsNotNull", nil},
	OpIsNum:     {"OpIsNum", nil},
	OpIsInt:     {"OpIsInt", nil},
	OpIsBool:    {"OpIsBool", nil},
	OpTypeOf:    {"OpTypeOf", nil},
	OpIsArray:   {"OpIsArray", nil},
	OpIsObject:  {"OpIsObject", nil},

	OpStrftime:    {"OpStrftime", nil},
	OpURLDecode:   {"OpURLDecode", nil},
	OpMD5:         {"OpMD5", nil},
	OpSHA1:        {"OpSHA1", nil},
	OpSHA256:      {"OpSHA256", nil},
	OpSHA512:      {"OpSHA512", nil},
	OpPrintf:      {"OpPrintf", []int{2}},
	OpIPMask:      {"OpIPMask", nil},
	OpStrptime:    {"OpStrptime", nil},
	OpSearchMatch: {"OpSearchMatch", nil},

	OpCIDRMatch: {"OpCIDRMatch", []int{2}},

	OpArrayBuild:    {"OpArrayBuild", []int{2}},
	OpObjectBuild:   {"OpObjectBuild", []int{2}},
	OpIndex:         {"OpIndex", nil},
	OpMember:        {"OpMember", []int{2}},
	OpLen:           {"OpLen", nil},
	OpConstDuration: {"OpConstDuration", []int{2}},

	// RFC-002 strict comparison
	OpEqStrict:  {"OpEqStrict", nil},
	OpNeqStrict: {"OpNeqStrict", nil},
	OpLtStrict:  {"OpLtStrict", nil},
	OpLteStrict: {"OpLteStrict", nil},
	OpGtStrict:  {"OpGtStrict", nil},
	OpGteStrict: {"OpGteStrict", nil},

	// RFC-002 3VL logic
	OpNot3VL:        {"OpNot3VL", nil},
	OpJumpIfNull3VL: {"OpJumpIfNull3VL", []int{2}},
	OpAnd3VLNull:    {"OpAnd3VLNull", nil},
	OpOr3VLNull:     {"OpOr3VLNull", nil},

	// RFC-002 strict arithmetic
	OpAddStrict: {"OpAddStrict", nil},
	OpSubStrict: {"OpSubStrict", nil},
	OpMulStrict: {"OpMulStrict", nil},
	OpDivStrict: {"OpDivStrict", nil},
	OpModStrict: {"OpModStrict", nil},

	// RFC-002 field/search
	OpLoadPath:     {"OpLoadPath", []int{2}},
	OpFieldMissing: {"OpFieldMissing", []int{2}},
	OpNegStrict:    {"OpNegStrict", nil},
	OpInStrict:     {"OpInStrict", []int{2}},

	// RFC-002 function opcodes
	OpHasToken:     {"OpHasToken", nil},
	OpContainsCI:   {"OpContainsCI", nil},
	OpExtract:      {"OpExtract", []int{2}},
	OpExtractAll:   {"OpExtractAll", []int{2}},
	OpSubstr0Based: {"OpSubstr0Based", nil},

	OpJsonExtract:  {"OpJsonExtract", nil},
	OpJsonValid:    {"OpJsonValid", nil},
	OpJsonKeys:     {"OpJsonKeys", nil},
	OpJsonArrayLen: {"OpJsonArrayLen", nil},
	OpJsonObject:   {"OpJsonObject", []int{2}},
	OpJsonArray:    {"OpJsonArray", []int{2}},
	OpJsonType:     {"OpJsonType", nil},
	OpJsonSet:      {"OpJsonSet", nil},
	OpJsonRemove:   {"OpJsonRemove", nil},
	OpJsonMerge:    {"OpJsonMerge", nil},

	// RFC-002 b2 lambda + array/object
	OpArrayAny:        {"OpArrayAny", []int{2}},
	OpArrayAll:        {"OpArrayAll", []int{2}},
	OpArrayFilter:     {"OpArrayFilter", []int{2}},
	OpArrayMap:        {"OpArrayMap", []int{2}},
	OpLoadLambdaParam: {"OpLoadLambdaParam", []int{2}},
	OpSlice:           {"OpSlice", nil},
	OpArrayConcat:     {"OpArrayConcat", []int{2}},
	OpArrayDistinct:   {"OpArrayDistinct", nil},
	OpArraySort:       {"OpArraySort", nil},
	OpFlatten:         {"OpFlatten", nil},
	OpKeys:            {"OpKeys", nil},
	OpValues:          {"OpValues", nil},
	OpMerge:           {"OpMerge", nil},
	OpHasKey:          {"OpHasKey", nil},
	OpURLParse:        {"OpURLParse", nil},
	OpIPParseObj:      {"OpIPParseObj", nil},
	OpFromJSONNative:  {"OpFromJSONNative", nil},
	OpBin:             {"OpBin", nil},

	OpReturn: {"OpReturn", nil},
}

// Make creates a single encoded instruction from an opcode and operands.
func Make(op Opcode, operands ...int) []byte {
	def, ok := definitions[op]
	if !ok {
		return []byte{byte(op)}
	}
	instructionLen := 1
	for _, w := range def.OperandWidths {
		instructionLen += w
	}
	instruction := make([]byte, instructionLen)
	instruction[0] = byte(op)
	offset := 1
	for i, o := range operands {
		if i >= len(def.OperandWidths) {
			break
		}
		width := def.OperandWidths[i]
		switch width {
		case 1:
			instruction[offset] = byte(o)
		case 2:
			binary.BigEndian.PutUint16(instruction[offset:], uint16(o))
		}
		offset += width
	}

	return instruction
}

// ReadUint16 reads a big-endian uint16 from a byte slice.
func ReadUint16(ins []byte) uint16 {
	return binary.BigEndian.Uint16(ins)
}

func (op Opcode) String() string {
	if def, ok := definitions[op]; ok {
		return def.Name
	}

	return fmt.Sprintf("Unknown(0x%02x)", byte(op))
}
