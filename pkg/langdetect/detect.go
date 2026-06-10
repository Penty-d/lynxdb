// Package langdetect provides language detection for query strings, determining
// whether a query should be routed to the LynxFlow v2 parser or the legacy SPL2
// parser. This package is shared between the REST API layer and the CLI to
// ensure identical detection behavior across all query submission surfaces.
package langdetect

import (
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/registry"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// Language identifies which parser/execution path to use.
type Language string

const (
	// LangLynxFlow selects the LynxFlow v2 parser and execution path.
	LangLynxFlow Language = "lynxflow"
	// LangSPL2 selects the legacy SPL2 parser and execution path.
	LangSPL2 Language = "spl2"
)

// Result holds the outcome of language detection.
type Result struct {
	// Language is the resolved language ("lynxflow" or "spl2").
	Language Language
	// Explicit is true when the caller specified the language explicitly.
	Explicit bool
	// DetectNotice is non-empty when detection was used (not explicit) and
	// provides a human-readable notice about the detection result.
	DetectNotice string
}

// Detect resolves the language for a query.
//
// Detection heuristic (applied when language is empty/absent):
//  1. Try lynxflow parse; if it produces zero error-severity diagnostics
//     and all callees are registered, choose lynxflow.
//  2. Try spl2 parse; if it succeeds, choose spl2.
//  3. If both clean, choose lynxflow (default going forward).
//  4. If both fail, return lynxflow (the default) with the lynxflow
//     diagnostics surfaced as the error.
//
// When language is explicit ("lynxflow" or "spl2"), no detection runs.
func Detect(query string, explicitLang string) Result {
	// Explicit language -- no detection.
	switch Language(strings.ToLower(strings.TrimSpace(explicitLang))) {
	case LangLynxFlow:
		return Result{Language: LangLynxFlow, Explicit: true}
	case LangSPL2:
		return Result{Language: LangSPL2, Explicit: true}
	}

	// Try lynxflow parse.
	lfAST, diags := parser.Parse(query)
	lfClean := !HasErrorDiag(diags)

	// Semantic validation: check that every aggregate and function call used
	// by the query is registered in the LynxFlow registry.
	if lfClean && lfAST != nil {
		desugared, _ := desugar.Desugar(lfAST, desugar.Options{DefaultSource: "main"})
		if !LFSemanticClean(desugared) {
			lfClean = false
		}
	}

	// Try spl2 parse.
	_, spl2Err := spl2.ParseProgram(spl2.NormalizeQuery(query))
	spl2Clean := spl2Err == nil

	switch {
	case lfClean && !spl2Clean:
		return Result{
			Language: LangLynxFlow,
			Explicit: false,
			DetectNotice: "language detected as lynxflow (spl2 parse failed); " +
				"set language=lynxflow to suppress this notice",
		}

	case !lfClean && spl2Clean:
		return Result{
			Language: LangSPL2,
			Explicit: false,
			DetectNotice: "language detected as spl2; " +
				"set language=spl2 or language=lynxflow to suppress this notice",
		}

	case lfClean && spl2Clean:
		return Result{
			Language: LangLynxFlow,
			Explicit: false,
			DetectNotice: "query parses as both lynxflow and spl2; " +
				"using lynxflow; " +
				"set language=spl2 to force the legacy path",
		}

	default:
		return Result{
			Language: LangLynxFlow,
			Explicit: false,
			DetectNotice: "language defaulted to lynxflow (neither parser succeeded); " +
				"set language explicitly to control behavior",
		}
	}
}

// DetectStrict is a stricter variant of Detect intended for CLI file/pipe mode
// where the 371 SPL2 golden transcripts must continue to pass. When auto-detecting
// (no explicit language), ambiguous queries that parse as both lynxflow and spl2
// are routed to spl2 (not lynxflow). Only queries that parse as lynxflow-only
// are auto-routed to lynxflow.
func DetectStrict(query string, explicitLang string) Result {
	// Explicit language -- no detection, identical to Detect.
	switch Language(strings.ToLower(strings.TrimSpace(explicitLang))) {
	case LangLynxFlow:
		return Result{Language: LangLynxFlow, Explicit: true}
	case LangSPL2:
		return Result{Language: LangSPL2, Explicit: true}
	}

	// Try lynxflow parse.
	lfAST, diags := parser.Parse(query)
	lfClean := !HasErrorDiag(diags)

	if lfClean && lfAST != nil {
		desugared, _ := desugar.Desugar(lfAST, desugar.Options{DefaultSource: "main"})
		if !LFSemanticClean(desugared) {
			lfClean = false
		}
	}

	// Try spl2 parse.
	_, spl2Err := spl2.ParseProgram(spl2.NormalizeQuery(query))
	spl2Clean := spl2Err == nil

	switch {
	case lfClean && !spl2Clean:
		// Only lynxflow understands this query.
		return Result{
			Language: LangLynxFlow,
			Explicit: false,
			DetectNotice: "language detected as lynxflow (spl2 parse failed); " +
				"set language=lynxflow to suppress this notice",
		}

	default:
		// Both clean, both fail, or only spl2 clean: default to spl2.
		return Result{
			Language: LangSPL2,
			Explicit: false,
		}
	}
}

// ValidateExplicitLanguage returns an error message if the language value is
// invalid. Returns "" for valid or absent values.
func ValidateExplicitLanguage(lang string) string {
	if lang == "" {
		return ""
	}
	switch Language(strings.ToLower(strings.TrimSpace(lang))) {
	case LangLynxFlow, LangSPL2:
		return ""
	}
	return "invalid language: must be \"lynxflow\" or \"spl2\""
}

// HasErrorDiag reports whether any diagnostic has error severity.
func HasErrorDiag(diags []parser.Diag) bool {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
}

// LFSemanticClean checks that every aggregate-position callee and every
// expression-position function call in the desugared AST is a registered
// LynxFlow aggregate or function.
func LFSemanticClean(q *ast.Query) bool {
	if q == nil {
		return true
	}
	for _, let := range q.Lets {
		if !lfPipelineClean(let.Pipeline) {
			return false
		}
	}
	return lfPipelineClean(q.Pipeline)
}

func lfPipelineClean(p ast.Pipeline) bool {
	for _, s := range p.Stages {
		if !lfStageClean(s) {
			return false
		}
	}
	return true
}

func lfStageClean(s ast.Stage) bool {
	if sp := s.Stats; sp != nil {
		if !lfAggListClean(sp) {
			return false
		}
	}
	if sp := s.Eventstats; sp != nil {
		if !lfAggListClean(sp) {
			return false
		}
	}
	if sp := s.Streamstats; sp != nil {
		if !lfAggListClean(&sp.StatsPayload) {
			return false
		}
	}

	if s.Where != nil && !lfExprClean(s.Where.Expr) {
		return false
	}
	if s.Extend != nil {
		for _, a := range s.Extend.Assignments {
			if !lfExprClean(a.Value) {
				return false
			}
		}
	}

	if s.Join != nil && s.Join.Right != nil {
		if s.Join.Right.Pipeline != nil {
			if !lfPipelineClean(*s.Join.Right.Pipeline) {
				return false
			}
		}
	}
	if s.Union != nil {
		for _, src := range s.Union.Sources {
			if src.Pipeline != nil {
				if !lfPipelineClean(*src.Pipeline) {
					return false
				}
			}
		}
	}

	return true
}

func lfAggListClean(sp *ast.StatsPayload) bool {
	for _, agg := range sp.Aggs {
		call, ok := agg.Func.(*ast.Call)
		if !ok {
			continue
		}
		name := strings.ToLower(call.Callee)
		if _, found := registry.LookupAggregate(name); !found {
			return false
		}
		for _, arg := range call.Args {
			if !lfExprClean(arg) {
				return false
			}
		}
		if agg.WhereCond != nil && !lfExprClean(agg.WhereCond) {
			return false
		}
	}
	for _, expr := range sp.By {
		if !lfExprClean(expr) {
			return false
		}
	}
	return true
}

func lfExprClean(e ast.Expr) bool {
	if e == nil {
		return true
	}
	switch x := e.(type) {
	case *ast.Call:
		name := strings.ToLower(x.Callee)
		_, isFunc := registry.LookupFunction(name)
		_, isAgg := registry.LookupAggregate(name)
		if !isFunc && !isAgg {
			return false
		}
		for _, arg := range x.Args {
			if !lfExprClean(arg) {
				return false
			}
		}
		if x.Receiver != nil && !lfExprClean(x.Receiver) {
			return false
		}
	case *ast.Binary:
		if !lfExprClean(x.Left) || !lfExprClean(x.Right) {
			return false
		}
	case *ast.Unary:
		if !lfExprClean(x.Operand) {
			return false
		}
	case *ast.In:
		if !lfExprClean(x.LHS) || !lfExprClean(x.RHS) {
			return false
		}
	case *ast.Between:
		if !lfExprClean(x.X) || !lfExprClean(x.Lo) || !lfExprClean(x.Hi) {
			return false
		}
	case *ast.Member:
		if !lfExprClean(x.Object) {
			return false
		}
	case *ast.SafeMember:
		if !lfExprClean(x.Object) {
			return false
		}
	case *ast.Index:
		if !lfExprClean(x.Object) || !lfExprClean(x.Idx) {
			return false
		}
	case *ast.Array:
		for _, elem := range x.Elems {
			if !lfExprClean(elem) {
				return false
			}
		}
	case *ast.Object:
		for _, p := range x.Entries {
			if !lfExprClean(p.Value) {
				return false
			}
		}
	case *ast.Lambda:
		if !lfExprClean(x.Body) {
			return false
		}
	case *ast.Paren:
		if !lfExprClean(x.Inner) {
			return false
		}
	}
	return true
}
