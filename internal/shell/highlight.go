package shell

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/lexer"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/registry"
)

// highlightClassifier holds pre-built lookup tables for classifying identifiers
// as operators, functions, or aggregates. Built once and reused across calls.
type highlightClassifier struct {
	operators  map[string]struct{}
	functions  map[string]struct{}
	aggregates map[string]struct{}
}

// hlClassifier is built once at package load time from the registry.
var hlClassifier = buildHighlightClassifier()

func buildHighlightClassifier() highlightClassifier {
	ops := registry.Operators()
	fns := registry.Functions()
	aggs := registry.Aggregates()

	c := highlightClassifier{
		operators:  make(map[string]struct{}, len(ops)),
		functions:  make(map[string]struct{}, len(fns)),
		aggregates: make(map[string]struct{}, len(aggs)),
	}

	for _, op := range ops {
		c.operators[op.Name] = struct{}{}
	}
	for _, fn := range fns {
		c.functions[fn.Name] = struct{}{}
	}
	for _, ag := range aggs {
		c.aggregates[ag.Name] = struct{}{}
	}

	return c
}

// isOperatorName reports whether the lowercase name is a known stage operator.
func (c *highlightClassifier) isOperatorName(name string) bool {
	_, ok := c.operators[strings.ToLower(name)]
	return ok
}

// isFuncOrAgg reports whether the lowercase name is a known function or aggregate.
func (c *highlightClassifier) isFuncOrAgg(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := c.functions[lower]; ok {
		return true
	}
	_, ok := c.aggregates[lower]
	return ok
}

// tokenClass represents how a token should be styled.
type tokenClass int

const (
	classPlain    tokenClass = iota // unstyled text (whitespace, gaps)
	classPipe                       // pipe operator |
	classCommand                    // stage/operator name
	classKeyword                    // reserved keywords (by, as, and, or, etc.)
	classFunction                   // function/aggregate call name
	classString                     // string literals (double-quoted and raw)
	classNumber                     // numeric literals and durations
	classOperator                   // comparison/arithmetic operators
	classField                      // identifiers and backtick-quoted names
	classError                      // lexer error tokens
	classComment                    // comments (// and /* */)
)

// styledSpan is a contiguous slice of the input with an assigned class.
type styledSpan struct {
	start int
	end   int
	class tokenClass
}

// HighlightSPL2 applies syntax highlighting to a LynxFlow query string.
// It tokenizes with the LynxFlow lexer, classifies each token, and returns
// a styled string using the provided theme. The output preserves the exact
// byte width of the input (no characters added or removed).
//
// The function is safe to call on every keystroke: it never panics even on
// incomplete or invalid input, gracefully degrading to unstyled text.
func HighlightSPL2(input string, theme *ShellTheme) string {
	if input == "" {
		return ""
	}
	if theme == nil {
		return input
	}

	spans := classifyInput(input)
	if len(spans) == 0 {
		return input
	}

	return renderSpans(input, spans, theme)
}

// classifyInput tokenizes the input and produces a sequence of styled spans
// covering every byte of the input. Gaps between tokens (whitespace and
// comments) are emitted as plain or comment spans.
func classifyInput(input string) []styledSpan {
	lex := lexer.New(input)

	// Collect all tokens first so we can do lookahead (e.g. ident followed by "(").
	tokens := make([]lexer.Token, 0, 32)
	for {
		tok := lex.Next()
		tokens = append(tokens, tok)
		if tok.Kind == lexer.EOF {
			break
		}
	}

	spans := make([]styledSpan, 0, len(tokens)*2)
	cursor := 0

	// expectStage tracks whether the next ident/keyword token is in
	// "stage-name position" (first token or first token after a pipe).
	expectStage := true

	for i, tok := range tokens {
		if tok.Kind == lexer.EOF {
			// Emit trailing gap.
			if cursor < len(input) {
				spans = appendGapSpans(spans, input, cursor, len(input))
			}
			break
		}

		// Emit gap between previous token end and this token start.
		if tok.Start > cursor {
			spans = appendGapSpans(spans, input, cursor, tok.Start)
		}

		class := classifyToken(tok, tokens, i, expectStage)
		spans = append(spans, styledSpan{start: tok.Start, end: tok.End, class: class})

		// Update stage expectation state.
		if tok.Kind == lexer.Pipe {
			expectStage = true
		} else if tok.Kind != lexer.EOF {
			// Any non-whitespace token after a pipe consumes the stage slot.
			if expectStage {
				expectStage = false
			}
		}

		cursor = tok.End
	}

	return spans
}

// classifyToken determines the token class for a single token.
func classifyToken(tok lexer.Token, tokens []lexer.Token, idx int, expectStage bool) tokenClass {
	switch tok.Kind {
	case lexer.Pipe:
		return classPipe

	case lexer.String, lexer.RawString:
		return classString

	case lexer.Int, lexer.Float, lexer.Duration:
		return classNumber

	case lexer.True, lexer.False, lexer.Null:
		return classKeyword

	case lexer.BacktickIdent:
		return classField

	case lexer.Error:
		return classError

	case lexer.Ident:
		return classifyIdent(tok, tokens, idx, expectStage)

	default:
		if tok.Kind.IsKeyword() {
			return classifyKeyword(tok, expectStage)
		}
		if isOperatorKind(tok.Kind) {
			return classOperator
		}
		// Punctuation (parens, brackets, braces, comma, etc.) -- plain.
		return classPlain
	}
}

// isStageKeyword reports whether the keyword kind is a stage-starting keyword
// (as opposed to expression keywords like and/or/not or clause keywords like by/as).
func isStageKeyword(k lexer.Kind) bool {
	switch k {
	case lexer.KwFrom, lexer.KwLet, lexer.KwWhere, lexer.KwParse, lexer.KwExtend,
		lexer.KwKeep, lexer.KwDrop, lexer.KwRename, lexer.KwStats,
		lexer.KwEventstats, lexer.KwStreamstats, lexer.KwSort,
		lexer.KwHead, lexer.KwTail, lexer.KwDedup, lexer.KwJoin,
		lexer.KwUnion, lexer.KwExplode, lexer.KwDescribe, lexer.KwTop,
		lexer.KwRare, lexer.KwEvery, lexer.KwRate, lexer.KwLatency,
		lexer.KwPercentiles, lexer.KwProportion, lexer.KwFacets,
		lexer.KwImpact, lexer.KwBaseline, lexer.KwChanges,
		lexer.KwExemplars, lexer.KwPatterns, lexer.KwCompare,
		lexer.KwOutliers, lexer.KwSessionize, lexer.KwTransaction,
		lexer.KwTrace, lexer.KwTopology, lexer.KwCorrelate,
		lexer.KwRollup, lexer.KwXyseries, lexer.KwMaterialize,
		lexer.KwTee, lexer.KwUse:
		return true
	}
	return false
}

// classifyKeyword maps a keyword token to either command or keyword class,
// depending on whether it is in stage-name position.
func classifyKeyword(tok lexer.Token, expectStage bool) tokenClass {
	if isStageKeyword(tok.Kind) {
		if expectStage {
			return classCommand
		}
		// Stage keywords not in stage position -- still highlight as command
		// because they stand out as recognizable operators.
		return classCommand
	}
	// Expression keywords (and, or, not, in, between) and clause keywords
	// (as, by, with, on, except).
	return classKeyword
}

// classifyIdent classifies a bare identifier by checking the registry.
func classifyIdent(tok lexer.Token, tokens []lexer.Token, idx int, expectStage bool) tokenClass {
	lower := strings.ToLower(tok.Text)

	// In stage-name position, check if this ident is a known operator.
	if expectStage && hlClassifier.isOperatorName(lower) {
		return classCommand
	}

	// Check if this ident is followed by '(' -- if so, it might be a function.
	if nextNonEOF(tokens, idx).Kind == lexer.LParen {
		if hlClassifier.isFuncOrAgg(lower) {
			return classFunction
		}
		// Also treat unknown ident+( as function for visual consistency --
		// users may define custom functions or use pipeline functions.
		return classFunction
	}

	return classField
}

// nextNonEOF returns the next token after idx, or a zero-value EOF token.
func nextNonEOF(tokens []lexer.Token, idx int) lexer.Token {
	if idx+1 < len(tokens) {
		return tokens[idx+1]
	}
	return lexer.Token{Kind: lexer.EOF}
}

// isOperatorKind reports whether the token kind is a comparison or arithmetic
// operator that should receive operator styling.
func isOperatorKind(k lexer.Kind) bool {
	switch k {
	case lexer.Eq, lexer.EqEq, lexer.BangEq,
		lexer.Lt, lexer.LtEq, lexer.Gt, lexer.GtEq,
		lexer.Plus, lexer.Minus, lexer.Star, lexer.Slash, lexer.Percent,
		lexer.Coalesce, lexer.Bang, lexer.Arrow, lexer.DotDot:
		return true
	}
	return false
}

// appendGapSpans scans the gap text (whitespace and comments between tokens)
// and emits plain spans for whitespace and comment spans for comment content.
func appendGapSpans(spans []styledSpan, input string, start, end int) []styledSpan {
	pos := start
	for pos < end {
		ch := input[pos]

		// Look for line comment start.
		if ch == '/' && pos+1 < end && input[pos+1] == '/' {
			commentStart := pos
			pos += 2
			for pos < end && input[pos] != '\n' {
				pos++
			}
			spans = append(spans, styledSpan{start: commentStart, end: pos, class: classComment})
			continue
		}

		// Look for block comment start.
		if ch == '/' && pos+1 < end && input[pos+1] == '*' {
			commentStart := pos
			pos += 2
			depth := 1
			for pos < end && depth > 0 {
				if pos+1 < end && input[pos] == '/' && input[pos+1] == '*' {
					depth++
					pos += 2
					continue
				}
				if pos+1 < end && input[pos] == '*' && input[pos+1] == '/' {
					depth--
					pos += 2
					continue
				}
				pos++
			}
			spans = append(spans, styledSpan{start: commentStart, end: pos, class: classComment})
			continue
		}

		// Non-comment text (whitespace, or a bare '/' that the lexer
		// somehow placed in a gap -- defensive). Consume all characters
		// until we hit a potential comment start.
		wsStart := pos
		pos++
		for pos < end {
			if input[pos] == '/' && pos+1 < end && (input[pos+1] == '/' || input[pos+1] == '*') {
				break
			}
			pos++
		}
		spans = append(spans, styledSpan{start: wsStart, end: pos, class: classPlain})
	}

	return spans
}

// renderSpans builds the final styled string from spans and the theme.
func renderSpans(input string, spans []styledSpan, theme *ShellTheme) string {
	var b strings.Builder
	b.Grow(len(input) + len(spans)*16) // rough estimate for ANSI escape overhead

	for _, sp := range spans {
		text := input[sp.start:sp.end]
		if sp.class == classPlain {
			// Pass through whitespace/gap text verbatim to preserve tabs
			// and avoid any lipgloss normalization.
			b.WriteString(text)
			continue
		}
		style := spanStyle(sp.class, theme)
		b.WriteString(style.Render(text))
	}

	return b.String()
}

// spanStyle returns the lipgloss style for a given token class.
func spanStyle(class tokenClass, theme *ShellTheme) lipgloss.Style {
	switch class {
	case classPipe:
		return theme.Pipe
	case classCommand:
		return theme.Command
	case classKeyword:
		return theme.Keyword
	case classFunction:
		return theme.Function
	case classString:
		return theme.String
	case classNumber:
		return theme.Number
	case classOperator:
		return theme.Operator
	case classField:
		return theme.Field
	case classError:
		return theme.Error
	case classComment:
		return theme.Comment
	default:
		return lipgloss.NewStyle()
	}
}
