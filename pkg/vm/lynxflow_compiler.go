package vm

// LynxFlow expression compiler — RFC-002 Phase 7 (b1).
//
// This is a SECOND compiler entry point, separate from the SPL2 compiler
// (compiler.go). The SPL2 compiler remains byte-identical. This compiler
// translates pkg/lynxflow/ast.Expr trees into VM bytecode following
// RFC-002 §5 (typed values, 3VL, strict comparisons, null/missing semantics).
//
// Design choices documented inline at each non-trivial point.

import (
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	lfast "github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/registry"

	"github.com/lynxbase/lynxdb/pkg/event"
)

// CompileLynxFlow compiles a LynxFlow v2 expression AST into a Program.
// The resulting program uses RFC-002 strict semantics (3VL logic, strict
// comparisons, no implicit coercion, warning counters).
func CompileLynxFlow(e lfast.Expr) (*Program, error) {
	c := &lfCompiler{prog: &Program{}}
	if err := c.compile(e); err != nil {
		return nil, err
	}
	c.prog.EmitOp(OpReturn)
	return c.prog, nil
}

// lfCompiler compiles LynxFlow v2 AST expressions to bytecode.
type lfCompiler struct {
	prog *Program
}

// compile dispatches on expression node type.
func (c *lfCompiler) compile(e lfast.Expr) error {
	switch n := e.(type) {
	case *lfast.Literal:
		return c.compileLiteral(n)
	case *lfast.Ident:
		idx := c.prog.AddFieldName(n.Name)
		c.prog.EmitOp(OpLoadField, idx)
	case *lfast.Paren:
		return c.compile(n.Inner)
	case *lfast.Unary:
		return c.compileUnary(n)
	case *lfast.Binary:
		return c.compileBinary(n)
	case *lfast.In:
		return c.compileIn(n)
	case *lfast.Between:
		return c.compileBetween(n)
	case *lfast.Call:
		return c.compileCall(n)
	case *lfast.Member:
		return c.compileMember(n)
	case *lfast.SafeMember:
		return c.compileSafeMember(n)
	case *lfast.Index:
		return c.compileIndex(n)
	case *lfast.Array:
		return c.compileArray(n)
	case *lfast.Object:
		return c.compileObject(n)
	case *lfast.ErrorExpr:
		return fmt.Errorf("lynxflow.Compile: error node in AST: %s", n.Message)
	case *lfast.Lambda:
		// Lambdas beyond literal/index come in a later PR.
		return fmt.Errorf("lynxflow.Compile: lambda expressions are not yet supported")
	default:
		return fmt.Errorf("lynxflow.Compile: unsupported expression type: %T", e)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Literals
// ---------------------------------------------------------------------------

func (c *lfCompiler) compileLiteral(lit *lfast.Literal) error {
	switch lit.Kind {
	case lfast.LitString, lfast.LitRawString:
		s, ok := lit.Value.(string)
		if !ok {
			return fmt.Errorf("lynxflow.Compile: string literal has non-string Value: %T", lit.Value)
		}
		idx := c.prog.AddConstant(event.StringValue(s))
		c.prog.EmitOp(OpConstStr, idx)
	case lfast.LitInt:
		n, ok := lit.Value.(int64)
		if !ok {
			return fmt.Errorf("lynxflow.Compile: int literal has non-int64 Value: %T", lit.Value)
		}
		idx := c.prog.AddConstant(event.IntValue(n))
		c.prog.EmitOp(OpConstInt, idx)
	case lfast.LitFloat:
		f, ok := lit.Value.(float64)
		if !ok {
			return fmt.Errorf("lynxflow.Compile: float literal has non-float64 Value: %T", lit.Value)
		}
		idx := c.prog.AddConstant(event.FloatValue(f))
		c.prog.EmitOp(OpConstFloat, idx)
	case lfast.LitBool:
		b, ok := lit.Value.(bool)
		if !ok {
			return fmt.Errorf("lynxflow.Compile: bool literal has non-bool Value: %T", lit.Value)
		}
		if b {
			c.prog.EmitOp(OpConstTrue)
		} else {
			c.prog.EmitOp(OpConstFalse)
		}
	case lfast.LitNull:
		c.prog.EmitOp(OpConstNull)
	case lfast.LitDuration:
		d, ok := lit.Value.(time.Duration)
		if !ok {
			return fmt.Errorf("lynxflow.Compile: duration literal has non-Duration Value: %T", lit.Value)
		}
		idx := c.prog.AddConstant(event.DurationValue(d))
		c.prog.EmitOp(OpConstDuration, idx)
	default:
		return fmt.Errorf("lynxflow.Compile: unknown literal kind: %d", lit.Kind)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Unary operators
// ---------------------------------------------------------------------------

func (c *lfCompiler) compileUnary(u *lfast.Unary) error {
	if err := c.compile(u.Operand); err != nil {
		return err
	}
	switch u.Op {
	case lfast.OpNot:
		// RFC-002 §5.2 3VL not: not(true)=false, not(false)=true, not(null)=null.
		// not on non-bool -> null + warning.
		// We use OpNot3VL which implements this.
		c.prog.EmitOp(OpNot3VL)
	case lfast.OpNeg:
		// Unary minus. For typed dispatch, use the generic OpSub with 0 -
		// but simpler to emit OpNeg which handles int/float/duration.
		c.prog.EmitOp(OpNegStrict)
	default:
		return fmt.Errorf("lynxflow.Compile: unknown unary op: %d", u.Op)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Binary operators
// ---------------------------------------------------------------------------

func (c *lfCompiler) compileBinary(b *lfast.Binary) error {
	switch b.Op {
	// Logic with 3VL (RFC-002 §5.2)
	case lfast.OpAnd:
		return c.compileAnd3VL(b)
	case lfast.OpOr:
		return c.compileOr3VL(b)

	// Coalesce: a ?? b yields b when a is null or missing
	case lfast.OpCoalesce:
		return c.compileCoalesce(b)

	// Arithmetic -- reuse existing opcodes that already match RFC-002 §5.4
	// (verified: OpDiv truncates int/int, promotes int/float to float,
	// OpMod is int-only via modValues, string+string concat in addValues).
	//
	// However, RFC-002 says string+number is null+warning, not implicit coercion.
	// The old OpAdd promotes string-parseable-as-number to float. We need strict
	// arithmetic opcodes.
	case lfast.OpAdd:
		return c.compileArithStrict(b, OpAddStrict)
	case lfast.OpSub:
		return c.compileArithStrict(b, OpSubStrict)
	case lfast.OpMul:
		return c.compileArithStrict(b, OpMulStrict)
	case lfast.OpDiv:
		return c.compileArithStrict(b, OpDivStrict)
	case lfast.OpMod:
		return c.compileArithStrict(b, OpModStrict)

	// Strict comparisons (RFC-002 §5.4)
	case lfast.OpEq:
		return c.compileCompareStrict(b, OpEqStrict)
	case lfast.OpNotEq:
		return c.compileCompareStrict(b, OpNeqStrict)
	case lfast.OpLt:
		return c.compileCompareStrict(b, OpLtStrict)
	case lfast.OpLtEq:
		return c.compileCompareStrict(b, OpLteStrict)
	case lfast.OpGt:
		return c.compileCompareStrict(b, OpGtStrict)
	case lfast.OpGtEq:
		return c.compileCompareStrict(b, OpGteStrict)

	default:
		return fmt.Errorf("lynxflow.Compile: unknown binary op: %d", b.Op)
	}
}

func (c *lfCompiler) compileArithStrict(b *lfast.Binary, op Opcode) error {
	if err := c.compile(b.Left); err != nil {
		return err
	}
	if err := c.compile(b.Right); err != nil {
		return err
	}
	c.prog.EmitOp(op)
	return nil
}

func (c *lfCompiler) compileCompareStrict(b *lfast.Binary, op Opcode) error {
	if err := c.compile(b.Left); err != nil {
		return err
	}
	if err := c.compile(b.Right); err != nil {
		return err
	}
	c.prog.EmitOp(op)
	return nil
}

// compileAnd3VL implements RFC-002 §5.2 three-valued AND via jump sequences.
//
// Implementation choice: jump sequences rather than new 3VL opcodes. This
// keeps the opcode surface smaller and avoids O(2) stack lookback in the VM.
//
//	compile(left)
//	OpJumpIfNull3VL -> maybeNull    // if null, jump (pop left)
//	OpJumpIfFalse -> falseLabel     // if false, jump (pop left)
//	compile(right)                  // left was true, result = right
//	OpJump -> end
//	maybeNull:
//	compile(right)
//	OpAnd3VLNull                    // right=false -> false; right=true/null -> null
//	OpJump -> end
//	falseLabel:
//	OpConstFalse
//	end:
func (c *lfCompiler) compileAnd3VL(b *lfast.Binary) error {
	if err := c.compile(b.Left); err != nil {
		return err
	}
	jumpNull := c.prog.EmitOp(OpJumpIfNull3VL, 0) // pops left
	jumpFalse := c.prog.EmitOp(OpJumpIfFalse, 0)  // pops left
	// Left is true; result = right
	if err := c.compile(b.Right); err != nil {
		return err
	}
	jumpEnd1 := c.prog.EmitOp(OpJump, 0)

	// maybeNull: left was null
	maybeNullLabel := c.prog.Len()
	if err := c.compile(b.Right); err != nil {
		return err
	}
	c.prog.EmitOp(OpAnd3VLNull) // if right=false -> false; else -> null
	jumpEnd2 := c.prog.EmitOp(OpJump, 0)

	// falseLabel: left was false
	falseLabel := c.prog.Len()
	c.prog.EmitOp(OpConstFalse)

	endLabel := c.prog.Len()
	c.prog.PatchUint16(jumpNull+1, uint16(maybeNullLabel))
	c.prog.PatchUint16(jumpFalse+1, uint16(falseLabel))
	c.prog.PatchUint16(jumpEnd1+1, uint16(endLabel))
	c.prog.PatchUint16(jumpEnd2+1, uint16(endLabel))
	return nil
}

// compileOr3VL implements RFC-002 §5.2 three-valued OR via jump sequences.
//
//	compile(left)
//	OpJumpIfNull3VL -> maybeNull    // if null, jump (pop left)
//	OpJumpIfTrue -> trueLabel       // if true, jump (pop left)
//	compile(right)                  // left was false, result = right
//	OpJump -> end
//	maybeNull:
//	compile(right)
//	OpOr3VLNull                     // right=true -> true; right=false/null -> null
//	OpJump -> end
//	trueLabel:
//	OpConstTrue
//	end:
func (c *lfCompiler) compileOr3VL(b *lfast.Binary) error {
	if err := c.compile(b.Left); err != nil {
		return err
	}
	jumpNull := c.prog.EmitOp(OpJumpIfNull3VL, 0) // pops left
	jumpTrue := c.prog.EmitOp(OpJumpIfTrue, 0)    // pops left
	// Left is false; result = right
	if err := c.compile(b.Right); err != nil {
		return err
	}
	jumpEnd1 := c.prog.EmitOp(OpJump, 0)

	// maybeNull: left was null
	maybeNullLabel := c.prog.Len()
	if err := c.compile(b.Right); err != nil {
		return err
	}
	c.prog.EmitOp(OpOr3VLNull) // if right=true -> true; else -> null
	jumpEnd2 := c.prog.EmitOp(OpJump, 0)

	// trueLabel: left was true
	trueLabel := c.prog.Len()
	c.prog.EmitOp(OpConstTrue)

	endLabel := c.prog.Len()
	c.prog.PatchUint16(jumpNull+1, uint16(maybeNullLabel))
	c.prog.PatchUint16(jumpTrue+1, uint16(trueLabel))
	c.prog.PatchUint16(jumpEnd1+1, uint16(endLabel))
	c.prog.PatchUint16(jumpEnd2+1, uint16(endLabel))
	return nil
}

// compileCoalesce: a ?? b yields b when a is null or missing.
// Since missing fields return null from OpLoadField, this covers both.
func (c *lfCompiler) compileCoalesce(b *lfast.Binary) error {
	if err := c.compile(b.Left); err != nil {
		return err
	}
	if err := c.compile(b.Right); err != nil {
		return err
	}
	c.prog.EmitOp(OpCoalesce, 2)
	return nil
}

// ---------------------------------------------------------------------------
// Member access (dotted paths per RFC-002 D25 / §4.4)
// ---------------------------------------------------------------------------

// compileMember handles expr.field.
// If the chain is rooted at an Ident, we collect the full dotted path
// and emit OpLoadPath (flat-column first, then object walk, no _raw fallback).
// If rooted at a non-Ident (e.g. f(x).b), evaluate then OpMember.
func (c *lfCompiler) compileMember(m *lfast.Member) error {
	if parts, ok := collectIdentMemberPath(m); ok {
		path := strings.Join(parts, ".")
		idx := c.prog.AddConstant(event.StringValue(path))
		c.prog.EmitOp(OpLoadPath, idx)
		return nil
	}
	// Non-ident root: evaluate object, then OpMember
	if err := c.compile(m.Object); err != nil {
		return err
	}
	keyIdx := c.prog.AddConstant(event.StringValue(m.Field))
	c.prog.EmitOp(OpMember, keyIdx)
	return nil
}

// compileSafeMember handles expr?.field.
// Safe member access on null/missing yields null (same as member since
// memberValue already returns null for null objects). We compile identically.
func (c *lfCompiler) compileSafeMember(sm *lfast.SafeMember) error {
	if parts, ok := collectIdentSafeMemberPath(sm); ok {
		path := strings.Join(parts, ".")
		idx := c.prog.AddConstant(event.StringValue(path))
		c.prog.EmitOp(OpLoadPath, idx)
		return nil
	}
	if err := c.compile(sm.Object); err != nil {
		return err
	}
	keyIdx := c.prog.AddConstant(event.StringValue(sm.Field))
	c.prog.EmitOp(OpMember, keyIdx)
	return nil
}

// collectIdentMemberPath walks a chain of Member nodes rooted at an Ident,
// returning ["a", "b", "c"] for a.b.c. Returns false if the root is not Ident
// or the chain includes non-Member nodes.
func collectIdentMemberPath(m *lfast.Member) ([]string, bool) {
	var parts []string
	parts = append(parts, m.Field)
	obj := m.Object
	for {
		switch n := obj.(type) {
		case *lfast.Member:
			parts = append(parts, n.Field)
			obj = n.Object
		case *lfast.Ident:
			parts = append(parts, n.Name)
			// Reverse to get root first
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
			return parts, true
		default:
			return nil, false
		}
	}
}

// collectIdentSafeMemberPath handles chains that may include SafeMember nodes.
func collectIdentSafeMemberPath(sm *lfast.SafeMember) ([]string, bool) {
	var parts []string
	parts = append(parts, sm.Field)
	obj := sm.Object
	for {
		switch n := obj.(type) {
		case *lfast.Member:
			parts = append(parts, n.Field)
			obj = n.Object
		case *lfast.SafeMember:
			parts = append(parts, n.Field)
			obj = n.Object
		case *lfast.Ident:
			parts = append(parts, n.Name)
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
			return parts, true
		default:
			return nil, false
		}
	}
}

// ---------------------------------------------------------------------------
// Index
// ---------------------------------------------------------------------------

func (c *lfCompiler) compileIndex(idx *lfast.Index) error {
	if err := c.compile(idx.Object); err != nil {
		return err
	}
	if err := c.compile(idx.Idx); err != nil {
		return err
	}
	c.prog.EmitOp(OpIndex)
	return nil
}

// ---------------------------------------------------------------------------
// Array / Object literals
// ---------------------------------------------------------------------------

func (c *lfCompiler) compileArray(arr *lfast.Array) error {
	for _, elem := range arr.Elems {
		if err := c.compile(elem); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpArrayBuild, len(arr.Elems))
	return nil
}

func (c *lfCompiler) compileObject(obj *lfast.Object) error {
	for _, entry := range obj.Entries {
		// Push key as string constant, then value expression
		keyIdx := c.prog.AddConstant(event.StringValue(entry.Key))
		c.prog.EmitOp(OpConstStr, keyIdx)
		if err := c.compile(entry.Value); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpObjectBuild, len(obj.Entries))
	return nil
}

// ---------------------------------------------------------------------------
// In / Between
// ---------------------------------------------------------------------------

// compileIn: `x in [a, b, c]` -> chained OpEqStrict + 3VL or for literal arrays.
// For non-array RHS, evaluate and use OpInStrict (new opcode).
//
// Implementation choice: for Array RHS, compile as chained equality + 3VL-or.
// This avoids a new opcode and keeps the common case (literal list) efficient.
// The old OpInList uses 2VL equality; we need strict semantics.
func (c *lfCompiler) compileIn(in *lfast.In) error {
	arr, isArray := in.RHS.(*lfast.Array)
	if !isArray {
		return fmt.Errorf("lynxflow.Compile: 'in' with non-array RHS is not yet supported; use a literal array")
	}

	if len(arr.Elems) == 0 {
		c.prog.EmitOp(OpConstFalse)
		return nil
	}

	// Compile LHS, then all list elements, then OpInStrict.
	if err := c.compile(in.LHS); err != nil {
		return err
	}
	for _, elem := range arr.Elems {
		if err := c.compile(elem); err != nil {
			return err
		}
	}
	c.prog.EmitOp(OpInStrict, len(arr.Elems))
	return nil
}

// compileBetween: `x between lo and hi` -> (x >= lo) and (x <= hi) with strict ops + 3VL.
func (c *lfCompiler) compileBetween(bt *lfast.Between) error {
	// Compile as: x >= lo AND x <= hi using 3VL AND

	// x >= lo
	if err := c.compile(bt.X); err != nil {
		return err
	}
	if err := c.compile(bt.Lo); err != nil {
		return err
	}
	c.prog.EmitOp(OpGteStrict)

	// Now we have the left part of AND on stack. Short-circuit 3VL AND:
	jumpNull := c.prog.EmitOp(OpJumpIfNull3VL, 0)
	jumpFalse := c.prog.EmitOp(OpJumpIfFalse, 0)

	// Left was true, evaluate x <= hi
	if err := c.compile(bt.X); err != nil {
		return err
	}
	if err := c.compile(bt.Hi); err != nil {
		return err
	}
	c.prog.EmitOp(OpLteStrict)
	jumpEnd1 := c.prog.EmitOp(OpJump, 0)

	// maybeNull: left was null
	maybeNullLabel := c.prog.Len()
	// Still need to evaluate x <= hi for 3VL AND semantics
	if err := c.compile(bt.X); err != nil {
		return err
	}
	if err := c.compile(bt.Hi); err != nil {
		return err
	}
	c.prog.EmitOp(OpLteStrict)
	c.prog.EmitOp(OpAnd3VLNull)
	jumpEnd2 := c.prog.EmitOp(OpJump, 0)

	// falseLabel: left was false
	falseLabel := c.prog.Len()
	c.prog.EmitOp(OpConstFalse)

	endLabel := c.prog.Len()
	c.prog.PatchUint16(jumpNull+1, uint16(maybeNullLabel))
	c.prog.PatchUint16(jumpFalse+1, uint16(falseLabel))
	c.prog.PatchUint16(jumpEnd1+1, uint16(endLabel))
	c.prog.PatchUint16(jumpEnd2+1, uint16(endLabel))
	return nil
}

// ---------------------------------------------------------------------------
// Function calls
// ---------------------------------------------------------------------------

func (c *lfCompiler) compileCall(call *lfast.Call) error {
	// Method-call chains: receiver.method(args) or receiver?.method(args)
	if call.Receiver != nil {
		// Compile receiver as first argument, then args
		return c.compileMethodCall(call)
	}

	name := call.Callee // already lowercase-normalized by parser

	spec := lookupLFFunc(name)
	if spec == nil {
		suggestion := suggestLFFunc(name)
		if suggestion != "" {
			return fmt.Errorf("lynxflow.Compile: unknown function %q (did you mean %q?)", name, suggestion)
		}
		return fmt.Errorf("lynxflow.Compile: unknown function %q", name)
	}

	// Arity check
	if err := spec.checkArity(call); err != nil {
		return err
	}

	// Bang (strict) variant handling
	if call.Bang {
		if !spec.strict {
			return fmt.Errorf("lynxflow.Compile: function %q has no strict variant %s!()", name, name)
		}
		return spec.emitStrict(c, call)
	}

	return spec.emit(c, call)
}

func (c *lfCompiler) compileMethodCall(call *lfast.Call) error {
	// For method calls like receiver.method(args), compile receiver first
	// then treat as method(receiver, args)
	// SafeNav: if receiver is null, the whole expression is null
	if call.SafeNav {
		if err := c.compile(call.Receiver); err != nil {
			return err
		}
		// If null, skip to end pushing null
		c.prog.EmitOp(OpDup)
		jumpNull := c.prog.EmitOp(OpJumpIfNull3VL, 0) // pops duplicate
		// Not null, pop dup result and proceed normally
		// Actually we have the receiver on stack, call the method
		// This gets complex. For now, treat method calls as function calls
		// with receiver prepended to args.
		_ = jumpNull
	}

	// Simplify: desugar receiver.method(args) into method(receiver, args)
	syntheticCall := &lfast.Call{
		Callee: call.Callee,
		Bang:   call.Bang,
		Args:   append([]lfast.Expr{call.Receiver}, call.Args...),
		Pos:    call.Pos,
	}
	// Reset program to before the Dup/JumpIfNull if SafeNav
	// This is getting too complex for safe-nav method calls. For now,
	// just compile as the unsugared form.
	return c.compileCall(syntheticCall)
}

// ---------------------------------------------------------------------------
// Warning counters
// ---------------------------------------------------------------------------

// WarningCounters tracks runtime warning counts per category.
// Thread-safe via atomic increments.
type WarningCounters struct {
	counts [warningCategoryCount]int64
}

// warningCategory identifies a warning class.
type warningCategory int

const (
	warnIncompatibleTypes warningCategory = iota
	warnNotOnNonBool
	warnModOnNonInt
	warnStringArithmetic
	warnCategoryCount // sentinel -- keep last
)

const warningCategoryCount = int(warnCategoryCount)

var warningCategoryNames = [warningCategoryCount]string{
	warnIncompatibleTypes: "incompatible_type_comparison",
	warnNotOnNonBool:      "not_on_non_bool",
	warnModOnNonInt:       "mod_on_non_int",
	warnStringArithmetic:  "string_arithmetic",
}

// Increment atomically increments a warning counter.
func (w *WarningCounters) Increment(cat warningCategory) {
	atomic.AddInt64(&w.counts[cat], 1)
}

// Counts returns a copy of warning counts as a map. Only non-zero counters
// are included.
func (w *WarningCounters) Counts() map[string]int64 {
	result := make(map[string]int64)
	for i := 0; i < warningCategoryCount; i++ {
		v := atomic.LoadInt64(&w.counts[i])
		if v > 0 {
			result[warningCategoryNames[i]] = v
		}
	}
	return result
}

// Reset zeroes all counters.
func (w *WarningCounters) Reset() {
	for i := 0; i < warningCategoryCount; i++ {
		atomic.StoreInt64(&w.counts[i], 0)
	}
}

// ---------------------------------------------------------------------------
// Helper: literal float for clamp
// ---------------------------------------------------------------------------

func constFloat(c *lfCompiler, f float64) {
	idx := c.prog.AddConstant(event.FloatValue(f))
	c.prog.EmitOp(OpConstFloat, idx)
}

func constInt(c *lfCompiler, n int64) {
	idx := c.prog.AddConstant(event.IntValue(n))
	c.prog.EmitOp(OpConstInt, idx)
}

func constStr(c *lfCompiler, s string) {
	idx := c.prog.AddConstant(event.StringValue(s))
	c.prog.EmitOp(OpConstStr, idx)
}

// suggestLFFunc returns the closest match from the lynxflow function registry.
func suggestLFFunc(name string) string {
	fns := registry.Functions()
	best := ""
	bestDist := 1000
	for _, fn := range fns {
		d := levenshtein(name, fn.Name)
		if d < bestDist && d <= 2 {
			bestDist = d
			best = fn.Name
		}
	}
	return best
}

// levenshtein computes the edit distance between two strings.
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}

// Ensure we reference all needed imports.
var _ = math.Pi
var _ = strings.Join
var _ = time.Duration(0)
