package parser

import "github.com/lynxbase/lynxdb/pkg/lynxflow/ast"

// DiagCode identifies a class of parse diagnostic.
type DiagCode string

const (
	// CodeUnexpectedToken is emitted when the parser encounters a token it
	// cannot incorporate into the current grammar production.
	CodeUnexpectedToken DiagCode = "E001"

	// CodeChainedComparison is emitted for non-associative comparison chains
	// like a < b < c (RFC-002 D24).
	CodeChainedComparison DiagCode = "E002"

	// CodeSingleEquals is emitted when = appears in an expression position
	// where == was intended (RFC-002 D8).
	CodeSingleEquals DiagCode = "E003"

	// CodeLexerError is emitted when the lexer produces an Error token.
	CodeLexerError DiagCode = "E004"

	// CodeUnterminatedGroup is emitted for unclosed parentheses, brackets,
	// or braces.
	CodeUnterminatedGroup DiagCode = "E005"

	// CodeTrailingOperator is emitted when a binary operator has no right
	// operand.
	CodeTrailingOperator DiagCode = "E006"
)

// Diag is a structured diagnostic produced by the parser.
type Diag struct {
	Code       DiagCode
	Message    string
	Span       ast.Span
	Expected   []string // human-readable set of expected tokens/productions
	Suggestion string   // fix-it suggestion (may be empty)
}
