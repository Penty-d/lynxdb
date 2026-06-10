package vm

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// funcSpec describes a single eval function's compilation behavior.
// Functions whose emission follows a simple pattern (validate arity, compile
// args left-to-right, emit one opcode) use the generic emitSimple helper.
// Functions with control flow (jump patching), regex-pool usage, or other
// bespoke logic provide a custom emit function.
type funcSpec struct {
	// name is the canonical (lowercase) function name. Stored for diagnostics.
	name string

	// minArgs and maxArgs define the valid arity range (inclusive).
	// Use -1 for maxArgs to indicate unbounded (variadic).
	minArgs int
	maxArgs int

	// emit compiles the function call into bytecode.
	// The compiler has already lowercased the name and will emit OpReturn
	// after the top-level expression, so emit should not emit OpReturn.
	emit func(c *compiler, call *spl2.FuncCallExpr) error
}

// funcRegistry maps lowercase function names to their compilation specs.
// Built once at package init time.
var funcRegistry map[string]*funcSpec

// funcRegistryList is the sorted list of registered function names.
// Built once at package init time alongside funcRegistry.
var funcRegistryList []string

func init() {
	specs := buildFuncSpecs()
	funcRegistry = make(map[string]*funcSpec, len(specs))
	for i := range specs {
		s := &specs[i]
		funcRegistry[s.name] = s
	}

	funcRegistryList = make([]string, 0, len(funcRegistry))
	for name := range funcRegistry {
		funcRegistryList = append(funcRegistryList, name)
	}
	sort.Strings(funcRegistryList)
}

// RegisteredFunctions returns a sorted list of all function names known to the
// expression compiler. Useful for diagnostics, documentation generation, and
// autocomplete.
func RegisteredFunctions() []string {
	out := make([]string, len(funcRegistryList))
	copy(out, funcRegistryList)
	return out
}

// lookupFunc returns the funcSpec for a lowercase function name, or nil.
func lookupFunc(name string) *funcSpec {
	return funcRegistry[name]
}

// --- Generic emitter helpers ------------------------------------------------

// emitSimpleUnary validates 1 arg, compiles it, emits one opcode.
func emitSimpleUnary(op Opcode) func(*compiler, *spl2.FuncCallExpr) error {
	return func(c *compiler, call *spl2.FuncCallExpr) error {
		if err := c.compileExpr(call.Args[0]); err != nil {
			return err
		}
		c.prog.EmitOp(op)
		return nil
	}
}

// emitSimpleBinary validates 2 args, compiles them left-to-right, emits one opcode.
func emitSimpleBinary(op Opcode) func(*compiler, *spl2.FuncCallExpr) error {
	return func(c *compiler, call *spl2.FuncCallExpr) error {
		if err := c.compileExpr(call.Args[0]); err != nil {
			return err
		}
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
		c.prog.EmitOp(op)
		return nil
	}
}

// emitUnaryMath validates 1 arg, compiles it, emits OpMathUnary with fn index.
func emitUnaryMath(fn int) func(*compiler, *spl2.FuncCallExpr) error {
	return func(c *compiler, call *spl2.FuncCallExpr) error {
		if err := c.compileExpr(call.Args[0]); err != nil {
			return err
		}
		c.prog.EmitOp(OpMathUnary, fn)
		return nil
	}
}

// emitBinaryMath validates 2 args, compiles them, emits OpMathBinary with fn index.
func emitBinaryMath(fn int) func(*compiler, *spl2.FuncCallExpr) error {
	return func(c *compiler, call *spl2.FuncCallExpr) error {
		if err := c.compileExpr(call.Args[0]); err != nil {
			return err
		}
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
		c.prog.EmitOp(OpMathBinary, fn)
		return nil
	}
}

// emitHash validates 1 arg, compiles it, emits the given hash opcode.
// Alias for emitSimpleUnary but named for clarity in the registry.
func emitHash(op Opcode) func(*compiler, *spl2.FuncCallExpr) error {
	return emitSimpleUnary(op)
}

// --- Bespoke emitter functions ----------------------------------------------

func emitIf(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileIf(call)
}

func emitCase(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileCase(call)
}

func emitValidate(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileValidate(call)
}

func emitCoalesce(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileCoalesce(call)
}

func emitNull(c *compiler, _ *spl2.FuncCallExpr) error {
	c.prog.EmitOp(OpConstNull)
	return nil
}

func emitNullIf(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileNullIf(call.Args[0], call.Args[1])
}

func emitSearchMatch(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	c.prog.EmitOp(OpSearchMatch)
	return nil
}

func emitIn(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	for _, arg := range call.Args[1:] {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpInList, len(call.Args)-1)
	return nil
}

func emitPrintf(c *compiler, call *spl2.FuncCallExpr) error {
	for _, arg := range call.Args {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpPrintf, len(call.Args))
	return nil
}

func emitRound(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	if len(call.Args) == 2 {
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
	} else {
		idx := c.prog.AddConstant(event.IntValue(0))
		c.prog.EmitOp(OpConstInt, idx)
	}
	c.prog.EmitOp(OpRound)
	return nil
}

func emitLog(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	if len(call.Args) == 1 {
		idx := c.prog.AddConstant(event.FloatValue(10))
		c.prog.EmitOp(OpConstFloat, idx)
		c.prog.EmitOp(OpLog)
	} else {
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
		c.prog.EmitOp(OpLog)
	}
	return nil
}

func emitPi(c *compiler, _ *spl2.FuncCallExpr) error {
	idx := c.prog.AddConstant(event.FloatValue(math.Pi))
	c.prog.EmitOp(OpConstFloat, idx)
	return nil
}

func emitRandom(c *compiler, _ *spl2.FuncCallExpr) error {
	c.prog.EmitOp(OpRandom)
	return nil
}

func emitSubstr(c *compiler, call *spl2.FuncCallExpr) error {
	for _, arg := range call.Args {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	if len(call.Args) == 2 {
		idx := c.prog.AddConstant(event.IntValue(math.MaxInt32))
		c.prog.EmitOp(OpConstInt, idx)
	}
	c.prog.EmitOp(OpSubstr)
	return nil
}

func emitMatch(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	pattern := exprToString(call.Args[1])
	regIdx := c.prog.AddRegex(pattern)
	c.prog.EmitOp(OpStrMatch, regIdx)
	return nil
}

func emitMvAppend(c *compiler, call *spl2.FuncCallExpr) error {
	for _, arg := range call.Args {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpMvAppend, len(call.Args))
	return nil
}

func emitReplace(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	pattern := exprToString(call.Args[1])
	regIdx := c.prog.AddRegex(pattern)
	if err := c.compileExpr(call.Args[2]); err != nil {
		return err
	}
	c.prog.EmitOp(OpReplace, regIdx)
	return nil
}

func emitTrim(op Opcode) func(*compiler, *spl2.FuncCallExpr) error {
	return func(c *compiler, call *spl2.FuncCallExpr) error {
		return c.compileTrim(call, op)
	}
}

func emitMax(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileMaxMin(call.Args, true)
}

func emitMin(c *compiler, call *spl2.FuncCallExpr) error {
	return c.compileMaxMin(call.Args, false)
}

func emitJsonExtract(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	if err := c.compileExpr(call.Args[1]); err != nil {
		return err
	}
	c.prog.EmitOp(OpJsonExtract)
	return nil
}

func emitJsonKeys(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	if len(call.Args) == 2 {
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
	} else {
		idx := c.prog.AddConstant(event.StringValue(""))
		c.prog.EmitOp(OpConstStr, idx)
	}
	c.prog.EmitOp(OpJsonKeys)
	return nil
}

func emitJsonArrayLength(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	if len(call.Args) == 2 {
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
	} else {
		idx := c.prog.AddConstant(event.StringValue(""))
		c.prog.EmitOp(OpConstStr, idx)
	}
	c.prog.EmitOp(OpJsonArrayLen)
	return nil
}

func emitJsonObject(c *compiler, call *spl2.FuncCallExpr) error {
	for _, arg := range call.Args {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpJsonObject, len(call.Args))
	return nil
}

func emitJsonArray(c *compiler, call *spl2.FuncCallExpr) error {
	for _, arg := range call.Args {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpJsonArray, len(call.Args))
	return nil
}

func emitJsonType(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	if len(call.Args) == 2 {
		if err := c.compileExpr(call.Args[1]); err != nil {
			return err
		}
	} else {
		idx := c.prog.AddConstant(event.StringValue(""))
		c.prog.EmitOp(OpConstStr, idx)
	}
	c.prog.EmitOp(OpJsonType)
	return nil
}

func emitJsonSet(c *compiler, call *spl2.FuncCallExpr) error {
	for _, arg := range call.Args {
		if err := c.compileExpr(arg); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpJsonSet)
	return nil
}

func emitCIDRMatch(c *compiler, call *spl2.FuncCallExpr) error {
	cidrStr := exprToString(call.Args[0])
	idx, err := c.prog.AddCIDR(cidrStr)
	if err != nil {
		return fmt.Errorf("cidrmatch: %w", err)
	}
	if err := c.compileExpr(call.Args[1]); err != nil {
		return err
	}
	c.prog.EmitOp(OpCIDRMatch, idx)
	return nil
}

// isstr is special: it maps to OpIsNotNull (schema-on-read: all non-null values
// are strings).
func emitIsStr(c *compiler, call *spl2.FuncCallExpr) error {
	if err := c.compileExpr(call.Args[0]); err != nil {
		return err
	}
	c.prog.EmitOp(OpIsNotNull)
	return nil
}

// --- Build the spec table ---------------------------------------------------

// buildFuncSpecs returns the full list of function specs. This is called once
// at init time. The order does not matter; funcRegistry is a map.
//
// Functions are categorized:
//   - generic: use emitSimpleUnary, emitSimpleBinary, emitUnaryMath, etc.
//   - bespoke: use named emitter functions (jump patching, regex pool, etc.)
func buildFuncSpecs() []funcSpec {
	return []funcSpec{
		// --- Control flow (bespoke: jump patching) ---
		{name: "if", minArgs: 3, maxArgs: 3, emit: emitIf},
		{name: "case", minArgs: 0, maxArgs: -1, emit: emitCase},
		{name: "validate", minArgs: 2, maxArgs: -1, emit: emitValidateWithArityCheck},
		{name: "coalesce", minArgs: 0, maxArgs: -1, emit: emitCoalesce},

		// --- Null handling (bespoke) ---
		{name: "null", minArgs: 0, maxArgs: 0, emit: emitNull},
		{name: "nullif", minArgs: 2, maxArgs: 2, emit: emitNullIf},

		// --- Search / predicate (bespoke: OpSearchMatch, InList) ---
		{name: "searchmatch", minArgs: 1, maxArgs: 1, emit: emitSearchMatch},
		{name: "in", minArgs: 2, maxArgs: -1, emit: emitIn},

		// --- Null checks (generic unary) ---
		{name: "isnull", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsNull)},
		{name: "isnotnull", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsNotNull)},

		// --- Type conversions (generic unary) ---
		{name: "tonumber", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToFloat)},
		{name: "todouble", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToFloat)},
		{name: "toint", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToInt)},
		{name: "tostring", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToString)},
		{name: "tobool", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToBool)},

		// --- String functions ---
		{name: "len", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpStrLen)},
		{name: "lower", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToLower)},
		{name: "upper", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpToUpper)},
		{name: "substr", minArgs: 2, maxArgs: 3, emit: emitSubstr},
		{name: "match", minArgs: 2, maxArgs: 2, emit: emitMatch},         // regex pool
		{name: "replace", minArgs: 3, maxArgs: 3, emit: emitReplace},     // regex pool
		{name: "trim", minArgs: 1, maxArgs: 2, emit: emitTrim(OpTrim)},   // default chars
		{name: "ltrim", minArgs: 1, maxArgs: 2, emit: emitTrim(OpLTrim)}, // default chars
		{name: "rtrim", minArgs: 1, maxArgs: 2, emit: emitTrim(OpRTrim)}, // default chars
		{name: "split", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpSplit)},
		{name: "urldecode", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpURLDecode)},

		// --- String predicates ---
		{name: "startswith", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpStartsWith)},
		{name: "endswith", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpEndsWith)},
		{name: "contains", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpContains)},

		// --- LIKE ---
		{name: "like", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpLike)},
		{name: "ilike", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpLike)},

		// --- Math functions ---
		{name: "round", minArgs: 1, maxArgs: 2, emit: emitRound},
		{name: "ln", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpLn)},
		{name: "log", minArgs: 1, maxArgs: 2, emit: emitLog},
		{name: "exp", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpExp)},
		{name: "pow", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpPow)},
		{name: "abs", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpAbs)},
		{name: "ceil", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpCeil)},
		{name: "ceiling", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpCeil)},
		{name: "floor", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpFloor)},
		{name: "sqrt", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpSqrt)},
		{name: "pi", minArgs: 0, maxArgs: 0, emit: emitPi},
		{name: "random", minArgs: 0, maxArgs: 0, emit: emitRandom},

		// --- Trig / unary math (OpMathUnary) ---
		{name: "acos", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnAcos)},
		{name: "acosh", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnAcosh)},
		{name: "asin", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnAsin)},
		{name: "asinh", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnAsinh)},
		{name: "atan", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnAtan)},
		{name: "atanh", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnAtanh)},
		{name: "cos", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnCos)},
		{name: "cosh", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnCosh)},
		{name: "sin", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnSin)},
		{name: "sinh", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnSinh)},
		{name: "tan", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnTan)},
		{name: "tanh", minArgs: 1, maxArgs: 1, emit: emitUnaryMath(mathFnTanh)},

		// --- Binary math (OpMathBinary) ---
		{name: "atan2", minArgs: 2, maxArgs: 2, emit: emitBinaryMath(mathFnAtan2)},
		{name: "hypot", minArgs: 2, maxArgs: 2, emit: emitBinaryMath(mathFnHypot)},

		// --- Variadic math ---
		{name: "max", minArgs: 2, maxArgs: -1, emit: emitMax},
		{name: "min", minArgs: 2, maxArgs: -1, emit: emitMin},

		// --- Multivalue ---
		{name: "mvappend", minArgs: 0, maxArgs: -1, emit: emitMvAppend},
		{name: "mvjoin", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpMvJoin)},
		{name: "mvdedup", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpMvDedup)},
		{name: "mvcount", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpMvCount)},

		// --- Printf (bespoke: variadic, operand=argc) ---
		{name: "printf", minArgs: 1, maxArgs: -1, emit: emitPrintf},

		// --- IP functions ---
		{name: "ipmask", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpIPMask)},
		{name: "cidrmatch", minArgs: 2, maxArgs: 2, emit: emitCIDRMatch},

		// --- Time functions ---
		{name: "strftime", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpStrftime)},
		{name: "strptime", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpStrptime)},

		// --- Hash functions ---
		{name: "md5", minArgs: 1, maxArgs: 1, emit: emitHash(OpMD5)},
		{name: "sha1", minArgs: 1, maxArgs: 1, emit: emitHash(OpSHA1)},
		{name: "sha256", minArgs: 1, maxArgs: 1, emit: emitHash(OpSHA256)},
		{name: "sha512", minArgs: 1, maxArgs: 1, emit: emitHash(OpSHA512)},

		// --- JSON functions ---
		{name: "json_extract", minArgs: 2, maxArgs: 2, emit: emitJsonExtract},
		{name: "spath", minArgs: 2, maxArgs: 2, emit: emitJsonExtract},
		{name: "json_valid", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpJsonValid)},
		{name: "json_keys", minArgs: 1, maxArgs: 2, emit: emitJsonKeys},
		{name: "json_array_length", minArgs: 1, maxArgs: 2, emit: emitJsonArrayLength},
		{name: "json_object", minArgs: 0, maxArgs: -1, emit: emitJsonObjectWithArityCheck},
		{name: "json_array", minArgs: 0, maxArgs: -1, emit: emitJsonArray},
		{name: "json_type", minArgs: 1, maxArgs: 2, emit: emitJsonType},
		{name: "json_set", minArgs: 3, maxArgs: 3, emit: emitJsonSet},
		{name: "json_remove", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpJsonRemove)},
		{name: "json_merge", minArgs: 2, maxArgs: 2, emit: emitSimpleBinary(OpJsonMerge)},

		// --- Type checks ---
		{name: "isnum", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsNum)},
		{name: "isnumeric", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsNum)},
		{name: "isint", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsInt)},
		{name: "isbool", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsBool)},
		{name: "isarray", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsArray)},
		{name: "isobject", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpIsObject)},
		{name: "isstr", minArgs: 1, maxArgs: 1, emit: emitIsStr},
		{name: "typeof", minArgs: 1, maxArgs: 1, emit: emitSimpleUnary(OpTypeOf)},
	}
}

// emitValidateWithArityCheck wraps the validate emitter with its custom arity
// check: even number of args, at least 2.
func emitValidateWithArityCheck(c *compiler, call *spl2.FuncCallExpr) error {
	if len(call.Args) == 0 || len(call.Args)%2 != 0 {
		return fmt.Errorf("validate expects condition/value pairs, got %d arguments", len(call.Args))
	}
	return c.compileValidate(call)
}

// emitJsonObjectWithArityCheck wraps json_object with even-args check.
func emitJsonObjectWithArityCheck(c *compiler, call *spl2.FuncCallExpr) error {
	if len(call.Args)%2 != 0 {
		return fmt.Errorf("json_object expects an even number of arguments, got %d", len(call.Args))
	}
	return emitJsonObject(c, call)
}

// arityErrorName returns the name to use in arity error messages.
// Most functions use the lowercase name, but some use a display name
// from the original code (e.g., "IF" uses uppercase, "ceil" for "ceiling").
func arityErrorName(spec *funcSpec, call *spl2.FuncCallExpr) string {
	name := strings.ToLower(call.Name)

	// The original code uses specific display names in error messages.
	// Preserve these exactly.
	switch name {
	case "if":
		return "IF"
	case "ceil", "ceiling":
		return "ceil"
	case "tonumber", "todouble":
		return name // uses the actual alias name
	case "isnum", "isnumeric":
		return "isnum"
	case "json_extract", "spath":
		return name
	case "like", "ilike":
		return name
	default:
		return spec.name
	}
}

// checkArity validates argument count against the spec's minArgs/maxArgs.
// Returns an error with the EXACT same format as the original switch cases.
func checkArity(spec *funcSpec, call *spl2.FuncCallExpr) error {
	argc := len(call.Args)
	displayName := arityErrorName(spec, call)

	// Special cases that have custom arity validation in the original code.
	// The validate and json_object functions handle arity inside their emitters.
	switch spec.name {
	case "validate", "json_object", "case", "coalesce", "mvappend", "json_array":
		// These have custom arity checks or accept any count.
		return nil
	}

	if spec.maxArgs == -1 {
		// Variadic: only check minimum.
		if argc < spec.minArgs {
			if spec.minArgs == 1 {
				return fmt.Errorf("%s expects at least %d argument, got %d", displayName, spec.minArgs, argc)
			}
			return fmt.Errorf("%s expects at least %d arguments, got %d", displayName, spec.minArgs, argc)
		}
		return nil
	}

	if spec.minArgs == spec.maxArgs {
		// Fixed arity.
		if argc != spec.minArgs {
			if spec.minArgs == 0 {
				return fmt.Errorf("%s expects %d arguments, got %d", displayName, spec.minArgs, argc)
			}
			if spec.minArgs == 1 {
				return fmt.Errorf("%s expects %d argument, got %d", displayName, spec.minArgs, argc)
			}
			return fmt.Errorf("%s expects %d arguments, got %d", displayName, spec.minArgs, argc)
		}
		return nil
	}

	// Range arity.
	if argc < spec.minArgs || argc > spec.maxArgs {
		return fmt.Errorf("%s expects %d-%d arguments, got %d", displayName, spec.minArgs, spec.maxArgs, argc)
	}
	return nil
}
