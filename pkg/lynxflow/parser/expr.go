// Package parser implements a Pratt / precedence-climbing expression parser
// for the LynxFlow v2 query language (RFC-002 Phase 2). It parses the single
// expression grammar defined in RFC-002 §4 and produces an [ast.Expr] tree.
//
// Error recovery: on encountering an unexpected token the parser emits a
// [Diag], inserts an [ast.ErrorExpr] placeholder, and attempts to resume at
// the next recovery point (closing delimiter or comma). This allows reporting
// multiple errors in a single pass.
package parser

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/lexer"
)

// parser holds the mutable state for one expression parse.
type parser struct {
	lex   *lexer.Lexer
	cur   lexer.Token
	prev  lexer.Token // previous consumed token (for span building)
	diags []Diag
	src   string // original source (for span rendering)
}

// ParseExpr parses a single expression from the input string and returns the
// AST along with any diagnostics. On success the diags slice is empty. On
// error the returned Expr is a best-effort partial tree (never nil) and diags
// contains one or more entries.
func ParseExpr(input string) (ast.Expr, []Diag) {
	p := &parser{
		lex: lexer.New(input),
		src: input,
	}
	p.advance() // prime cur

	expr := p.parseExpr()

	// If there are remaining non-EOF tokens, report them.
	if p.cur.Kind != lexer.EOF {
		p.errorf(p.cur, CodeUnexpectedToken, nil, "",
			"unexpected %s after expression", kindName(p.cur.Kind))
	}

	return expr, p.diags
}

// ---------------------------------------------------------------------------
// Token helpers
// ---------------------------------------------------------------------------

// advance consumes the current token and reads the next one.
func (p *parser) advance() {
	p.prev = p.cur
	for {
		p.cur = p.lex.Next()
		if p.cur.Kind == lexer.Error {
			// Surface lexer errors as parser diagnostics.
			p.diags = append(p.diags, Diag{
				Code:    CodeLexerError,
				Message: p.cur.Text,
				Span:    ast.Span{Start: p.cur.Start, End: p.cur.End},
			})
			continue // skip error tokens, try next
		}
		break
	}
}

// expect consumes the current token if it matches kind, otherwise emits a
// diagnostic and returns a zero token.
func (p *parser) expect(kind lexer.Kind) (lexer.Token, bool) {
	if p.cur.Kind == kind {
		tok := p.cur
		p.advance()
		return tok, true
	}
	p.errorf(p.cur, CodeUnexpectedToken, []string{kindName(kind)}, "",
		"expected %s, got %s", kindName(kind), kindName(p.cur.Kind))
	return lexer.Token{Start: p.cur.Start, End: p.cur.End}, false
}

// at reports whether the current token is of the given kind.
func (p *parser) at(kind lexer.Kind) bool {
	return p.cur.Kind == kind
}

// consume advances past the current token if it matches kind and returns true;
// otherwise returns false without advancing.
func (p *parser) consume(kind lexer.Kind) bool {
	if p.cur.Kind == kind {
		p.advance()
		return true
	}
	return false
}

// curSpan returns the span of the current token.
func (p *parser) curSpan() ast.Span {
	return ast.Span{Start: p.cur.Start, End: p.cur.End}
}

// ---------------------------------------------------------------------------
// Soft keyword handling (D29)
// ---------------------------------------------------------------------------

// identLike checks whether the current token can be treated as an identifier
// in an expression position. This includes bare Ident, BacktickIdent, and all
// soft keywords (stage-starting keywords that are not hard keywords). Hard
// keywords are: and, or, not, in, between, true, false, null.
//
// Returns the resolved name and whether the token is ident-like. For soft
// keywords the name is the lowercase-normalized form.
func (p *parser) identLike() (string, bool) {
	switch p.cur.Kind {
	case lexer.Ident:
		return p.cur.Text, true
	case lexer.BacktickIdent:
		// Strip surrounding backticks.
		raw := p.cur.Text
		if len(raw) >= 2 && raw[0] == '`' && raw[len(raw)-1] == '`' {
			return raw[1 : len(raw)-1], true
		}
		return raw, true
	default:
		if isSoftKeyword(p.cur.Kind) {
			return strings.ToLower(p.cur.Text), true
		}
		return "", false
	}
}

// isSoftKeyword returns true for keyword token kinds that are treated as
// ordinary identifiers in expression positions (RFC-002 D29). All stage-name
// keywords, plus as/by/with/on/except, are soft.
func isSoftKeyword(k lexer.Kind) bool {
	switch k {
	case lexer.KwFrom, lexer.KwLet, lexer.KwWhere, lexer.KwParse,
		lexer.KwExtend, lexer.KwKeep, lexer.KwDrop, lexer.KwRename,
		lexer.KwStats, lexer.KwEventstats, lexer.KwStreamstats,
		lexer.KwSort, lexer.KwHead, lexer.KwTail, lexer.KwDedup,
		lexer.KwJoin, lexer.KwUnion, lexer.KwExplode, lexer.KwDescribe,
		lexer.KwTop, lexer.KwRare, lexer.KwEvery, lexer.KwRate,
		lexer.KwLatency, lexer.KwPercentiles, lexer.KwProportion,
		lexer.KwFacets, lexer.KwImpact, lexer.KwBaseline, lexer.KwChanges,
		lexer.KwExemplars, lexer.KwPatterns, lexer.KwCompare,
		lexer.KwOutliers, lexer.KwSessionize, lexer.KwTransaction,
		lexer.KwTrace, lexer.KwTopology, lexer.KwCorrelate,
		lexer.KwRollup, lexer.KwXyseries, lexer.KwMaterialize,
		lexer.KwTee, lexer.KwUse,
		lexer.KwAs, lexer.KwBy, lexer.KwWith, lexer.KwOn, lexer.KwExcept:
		return true
	}
	return false
}

// isHardKeyword returns true for tokens that are never valid as identifiers
// in expressions. These are: and, or, not, in, between, true, false, null.
func isHardKeyword(k lexer.Kind) bool {
	switch k {
	case lexer.KwAnd, lexer.KwOr, lexer.KwNot, lexer.KwIn, lexer.KwBetween,
		lexer.True, lexer.False, lexer.Null:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Expression parser — precedence climbing
// ---------------------------------------------------------------------------

func (p *parser) parseExpr() ast.Expr {
	return p.parseOr()
}

// parseOr: or_expr ::= and_expr ('or' and_expr)*
func (p *parser) parseOr() ast.Expr {
	left := p.parseAnd()
	for p.at(lexer.KwOr) {
		p.advance()
		right := p.parseAnd()
		left = &ast.Binary{
			Op:    ast.OpOr,
			Left:  left,
			Right: right,
			Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
		}
	}
	return left
}

// parseAnd: and_expr ::= not_expr ('and' not_expr)*
func (p *parser) parseAnd() ast.Expr {
	left := p.parseNot()
	for p.at(lexer.KwAnd) {
		p.advance()
		right := p.parseNot()
		left = &ast.Binary{
			Op:    ast.OpAnd,
			Left:  left,
			Right: right,
			Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
		}
	}
	return left
}

// parseNot: not_expr ::= 'not' not_expr | cmp_expr
func (p *parser) parseNot() ast.Expr {
	if p.at(lexer.KwNot) {
		start := p.cur.Start
		p.advance()
		operand := p.parseNot()
		return &ast.Unary{
			Op:      ast.OpNot,
			Operand: operand,
			Pos:     ast.Span{Start: start, End: operand.ExprSpan().End},
		}
	}
	return p.parseComparison()
}

// parseComparison: cmp_expr ::= coal_expr (cmp_op coal_expr | 'in' coal_expr | 'between' coal_expr 'and' coal_expr)?
// Non-associative: a<b<c is a parse error with fix-it (D24).
func (p *parser) parseComparison() ast.Expr {
	left := p.parseCoalesce()

	op, isCmp := comparisonOp(p.cur.Kind)
	if !isCmp {
		// Check for `in` and `between` as comparison-level operators.
		if p.at(lexer.KwIn) {
			p.advance()
			rhs := p.parseCoalesce()
			result := &ast.In{
				LHS: left,
				RHS: rhs,
				Pos: ast.Span{Start: left.ExprSpan().Start, End: rhs.ExprSpan().End},
			}
			// Non-associative: check for chaining.
			p.checkChainedComparison()
			return result
		}
		if p.at(lexer.KwBetween) {
			p.advance()
			lo := p.parseCoalesce()
			if !p.consume(lexer.KwAnd) {
				p.errorf(p.cur, CodeUnexpectedToken, []string{"and"}, "",
					"expected 'and' in 'between ... and ...', got %s", kindName(p.cur.Kind))
			}
			hi := p.parseCoalesce()
			result := &ast.Between{
				X:   left,
				Lo:  lo,
				Hi:  hi,
				Pos: ast.Span{Start: left.ExprSpan().Start, End: hi.ExprSpan().End},
			}
			p.checkChainedComparison()
			return result
		}

		// Check for single = in expression position (D8).
		if p.at(lexer.Eq) {
			p.diags = append(p.diags, Diag{
				Code:       CodeSingleEquals,
				Message:    "'=' is assignment; use '==' for comparison",
				Span:       p.curSpan(),
				Expected:   []string{"=="},
				Suggestion: "replace = with ==",
			})
			// Recover by treating it as ==.
			p.advance()
			right := p.parseCoalesce()
			return &ast.Binary{
				Op:    ast.OpEq,
				Left:  left,
				Right: right,
				Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
			}
		}

		return left
	}

	p.advance()
	right := p.parseCoalesce()
	result := &ast.Binary{
		Op:    op,
		Left:  left,
		Right: right,
		Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
	}

	// Non-associative: detect chaining (D24).
	p.checkChainedComparison()

	return result
}

// checkChainedComparison emits a D24 diagnostic if another comparison
// operator immediately follows.
func (p *parser) checkChainedComparison() {
	_, isCmp := comparisonOp(p.cur.Kind)
	if isCmp || p.at(lexer.KwIn) || p.at(lexer.KwBetween) {
		p.diags = append(p.diags, Diag{
			Code:       CodeChainedComparison,
			Message:    "chained comparisons are not allowed",
			Span:       p.curSpan(),
			Suggestion: "use 'and' to combine: a < b and b < c",
		})
		// Do NOT consume — the caller's loop will not iterate either, since
		// comparisons are non-associative. The leftover token will trigger
		// another error at the top-level or be consumed by a parent rule.
	}
}

// comparisonOp maps a token kind to a BinaryOp for comparison operators.
func comparisonOp(k lexer.Kind) (ast.BinaryOp, bool) {
	switch k {
	case lexer.EqEq:
		return ast.OpEq, true
	case lexer.BangEq:
		return ast.OpNotEq, true
	case lexer.Lt:
		return ast.OpLt, true
	case lexer.LtEq:
		return ast.OpLtEq, true
	case lexer.Gt:
		return ast.OpGt, true
	case lexer.GtEq:
		return ast.OpGtEq, true
	}
	return 0, false
}

// parseCoalesce: coal_expr ::= add_expr ('??' add_expr)*
func (p *parser) parseCoalesce() ast.Expr {
	left := p.parseAdditive()
	for p.at(lexer.Coalesce) {
		p.advance()
		right := p.parseAdditive()
		left = &ast.Binary{
			Op:    ast.OpCoalesce,
			Left:  left,
			Right: right,
			Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
		}
	}
	return left
}

// parseAdditive: add_expr ::= mul_expr (('+' | '-') mul_expr)*
func (p *parser) parseAdditive() ast.Expr {
	left := p.parseMultiplicative()
	for p.at(lexer.Plus) || p.at(lexer.Minus) {
		op := ast.OpAdd
		if p.cur.Kind == lexer.Minus {
			op = ast.OpSub
		}
		p.advance()
		right := p.parseMultiplicative()
		left = &ast.Binary{
			Op:    op,
			Left:  left,
			Right: right,
			Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
		}
	}
	return left
}

// parseMultiplicative: mul_expr ::= unary_expr (('*' | '/' | '%') unary_expr)*
func (p *parser) parseMultiplicative() ast.Expr {
	left := p.parseUnary()
	for p.at(lexer.Star) || p.at(lexer.Slash) || p.at(lexer.Percent) {
		var op ast.BinaryOp
		switch p.cur.Kind {
		case lexer.Star:
			op = ast.OpMul
		case lexer.Slash:
			op = ast.OpDiv
		case lexer.Percent:
			op = ast.OpMod
		}
		p.advance()
		right := p.parseUnary()
		left = &ast.Binary{
			Op:    op,
			Left:  left,
			Right: right,
			Pos:   ast.Span{Start: left.ExprSpan().Start, End: right.ExprSpan().End},
		}
	}
	return left
}

// parseUnary: unary_expr ::= '-' unary_expr | postfix
func (p *parser) parseUnary() ast.Expr {
	if p.at(lexer.Minus) {
		start := p.cur.Start
		p.advance()
		operand := p.parseUnary()
		return &ast.Unary{
			Op:      ast.OpNeg,
			Operand: operand,
			Pos:     ast.Span{Start: start, End: operand.ExprSpan().End},
		}
	}
	return p.parsePostfix()
}

// parsePostfix: postfix ::= primary (call_args | '.' ident | '?.' ident | '[' expr ']')*
func (p *parser) parsePostfix() ast.Expr {
	left := p.parsePrimary()

	for {
		switch {
		case p.at(lexer.LParen):
			// Call: the EBNF allows call_args after any postfix result.
			// For simple function calls (left is Ident), extract the callee name.
			// For method-call-style chains (left is Member/SafeMember), extract
			// the method name and build a Call with a Receiver.
			callee, calleeOK := calleeFromExpr(left)
			if calleeOK {
				left = p.parseCallArgsNode(callee, false, nil, false, left.ExprSpan().Start)
			} else if m, ok := left.(*ast.Member); ok {
				left = p.parseCallArgsNode(m.Field, false, m.Object, false, left.ExprSpan().Start)
			} else if sm, ok := left.(*ast.SafeMember); ok {
				left = p.parseCallArgsNode(sm.Field, false, sm.Object, true, left.ExprSpan().Start)
			} else {
				// Expression like (a+b)(...) — not a valid call form.
				return left
			}

		case p.at(lexer.Bang):
			// Strict-cast bang: ident! immediately followed by (
			// We need the bang to be immediately adjacent (no space) to the
			// ident AND immediately followed by (.
			callee, calleeOK := calleeFromExpr(left)
			if !calleeOK {
				return left
			}
			// Check if bang is immediately adjacent to the identifier
			// (the lexer guarantees Bang is a single byte at a specific offset).
			if p.cur.Start != left.ExprSpan().End {
				// Space between ident and ! — not a strict cast.
				return left
			}
			p.advance() // consume !
			if !p.at(lexer.LParen) {
				// ! without ( is not a strict cast — emit error.
				p.errorf(p.cur, CodeUnexpectedToken, []string{"("}, "",
					"expected '(' after '!' in strict cast")
				return left
			}
			left = p.parseCallArgsNode(callee, true, nil, false, left.ExprSpan().Start)

		case p.at(lexer.Dot):
			p.advance()
			name, ok := p.identLike()
			if !ok {
				p.errorf(p.cur, CodeUnexpectedToken, []string{"identifier"}, "",
					"expected field name after '.', got %s", kindName(p.cur.Kind))
				return left
			}
			end := p.cur.End
			p.advance()
			left = &ast.Member{
				Object: left,
				Field:  name,
				Pos:    ast.Span{Start: left.ExprSpan().Start, End: end},
			}

		case p.at(lexer.SafeNav):
			p.advance()
			name, ok := p.identLike()
			if !ok {
				p.errorf(p.cur, CodeUnexpectedToken, []string{"identifier"}, "",
					"expected field name after '?.', got %s", kindName(p.cur.Kind))
				return left
			}
			end := p.cur.End
			p.advance()
			left = &ast.SafeMember{
				Object: left,
				Field:  name,
				Pos:    ast.Span{Start: left.ExprSpan().Start, End: end},
			}

		case p.at(lexer.LBracket):
			p.advance()
			idx := p.parseExpr()
			end := p.cur.End
			if _, ok := p.expect(lexer.RBracket); !ok {
				// Try recovery: skip to ].
				p.recoverTo(lexer.RBracket)
				end = p.prev.End
			}
			left = &ast.Index{
				Object: left,
				Idx:    idx,
				Pos:    ast.Span{Start: left.ExprSpan().Start, End: end},
			}

		default:
			return left
		}
	}
}

// calleeFromExpr extracts the function name from an expression that is an
// identifier (bare or soft-keyword). Returns the lowercase name and true,
// or empty string and false.
func calleeFromExpr(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		return strings.ToLower(x.Name), true
	}
	return "", false
}

// parseCallArgsNode parses the argument list for a function call, starting from
// the opening '(' (which must be the current token). receiver is non-nil for
// method-call chains (a.b(x) or a?.b(x)); safeNav indicates ?. vs .
func (p *parser) parseCallArgsNode(callee string, bang bool, receiver ast.Expr, safeNav bool, start int) *ast.Call {
	p.advance() // consume (

	var args []Expr
	if !p.at(lexer.RParen) {
		args = p.parseArgList()
	}

	end := p.cur.End
	if _, ok := p.expect(lexer.RParen); !ok {
		p.recoverTo(lexer.RParen)
		end = p.prev.End
	}

	// Convert from internal Expr slice.
	astArgs := make([]ast.Expr, len(args))
	for i, a := range args {
		astArgs[i] = a
	}

	return &ast.Call{
		Receiver: receiver,
		SafeNav:  safeNav,
		Callee:   callee,
		Bang:     bang,
		Args:     astArgs,
		Pos:      ast.Span{Start: start, End: end},
	}
}

// Expr alias for internal use (it is the same as ast.Expr but avoids stuttering).
type Expr = ast.Expr

// parseArgList parses a comma-separated list of expressions.
func (p *parser) parseArgList() []Expr {
	var args []Expr

	// First argument — check for lambda.
	args = append(args, p.parseArgExpr())

	for p.consume(lexer.Comma) {
		args = append(args, p.parseArgExpr())
	}

	return args
}

// parseArgExpr parses a single argument expression. If the current position
// is an identifier followed by ->, it parses a lambda. If it starts with
// 'where', it parses a conditional aggregate argument (RFC-002 §4.2: arg ::= expr | 'where' expr).
func (p *parser) parseArgExpr() ast.Expr {
	// Conditional aggregate: where <predicate>
	// count(where p) or sum(x, where p)
	if p.at(lexer.KwWhere) {
		// Produce a special marker node: an Ident with Name="where" followed
		// by the predicate as the next arg. We use a synthetic approach:
		// emit Ident{where}, then the caller will handle it.
		// Actually, to keep the AST clean, we wrap the where keyword and
		// predicate into a single WhereArg node. But since we want minimal
		// AST changes, let's use an Ident("where") as the arg, and the
		// predicate as the next comma-separated arg.
		// BUT: the grammar says `where expr` is a single arg, not two.
		// count(where status >= 500) — 'where' and 'status >= 500' form ONE arg.
		//
		// Solution: parse 'where' as a marker, then parse the predicate, and
		// wrap them into a Call-like structure. We'll use a synthetic approach:
		// return an Ident("where") and then the aggregate parser will look for it.
		// Actually, let's just produce an Ident("where") and then return the
		// predicate. The arg list will have two entries: Ident{where}, expr.
		// The aggregate parser in parseAggExpr/extractWhereFromCall will handle it.
		span := p.curSpan()
		p.advance() // consume 'where'
		pred := p.parseExpr()
		// Return a synthetic node that wraps both. We'll use a convention:
		// return the Ident{where} as the first arg, and then... no, parseArgExpr
		// returns a single expression.
		// Best approach: return the predicate, but tag it via a wrapper that
		// the aggregate parser can detect. Since we can't add new Expr types
		// without AST changes, use a simple approach: return a Call node
		// with callee="where" and the predicate as arg. This is NOT a real
		// function call — the aggregate parser will extract it.
		return &ast.Call{
			Callee: "where",
			Args:   []ast.Expr{pred},
			Pos:    ast.Span{Start: span.Start, End: pred.ExprSpan().End},
		}
	}
	// Lambda lookahead: ident -> expr
	if name, ok := p.identLike(); ok && p.peekIsArrow() {
		return p.parseLambda(name)
	}
	return p.parseExpr()
}

// peekIsArrow reports whether the token after the current one is ->.
// It uses a one-token lookahead by saving and restoring state.
func (p *parser) peekIsArrow() bool {
	// We peek by looking at what the lexer would produce next.
	// Save the lexer state (we cannot, as it is a pull lexer).
	// Instead, we use a simple heuristic: if cur is ident-like and the
	// source bytes after cur.End (skipping whitespace) start with "->".
	pos := p.cur.End
	for pos < len(p.src) && (p.src[pos] == ' ' || p.src[pos] == '\t' || p.src[pos] == '\n' || p.src[pos] == '\r') {
		pos++
	}
	if pos+1 < len(p.src) && p.src[pos] == '-' && p.src[pos+1] == '>' {
		return true
	}
	return false
}

// parseLambda parses `ident -> expr`. The caller has already verified the
// current token is an ident-like token and the next is ->.
func (p *parser) parseLambda(param string) *ast.Lambda {
	start := p.cur.Start
	p.advance() // consume the ident
	p.advance() // consume ->
	body := p.parseExpr()
	return &ast.Lambda{
		Param: param,
		Body:  body,
		Pos:   ast.Span{Start: start, End: body.ExprSpan().End},
	}
}

// ---------------------------------------------------------------------------
// Primary
// ---------------------------------------------------------------------------

func (p *parser) parsePrimary() ast.Expr {
	switch {
	// Lambda check: ident -> expr (when not inside a call arg; the call arg
	// path handles it via parseArgExpr). At the top-level primary we also
	// check for standalone lambdas in parenthesized contexts.

	// Identifiers (bare and backtick-quoted) and soft keywords.
	case p.cur.Kind == lexer.Ident || p.cur.Kind == lexer.BacktickIdent || isSoftKeyword(p.cur.Kind):
		name, _ := p.identLike()
		quoted := p.cur.Kind == lexer.BacktickIdent
		tok := p.cur
		p.advance()
		return &ast.Ident{
			Name:   name,
			Quoted: quoted,
			Pos:    ast.Span{Start: tok.Start, End: tok.End},
		}

	// String literal.
	case p.cur.Kind == lexer.String:
		return p.parseStringLiteral()

	// Raw string literal.
	case p.cur.Kind == lexer.RawString:
		return p.parseRawStringLiteral()

	// Integer literal.
	case p.cur.Kind == lexer.Int:
		return p.parseIntLiteral()

	// Float literal.
	case p.cur.Kind == lexer.Float:
		return p.parseFloatLiteral()

	// Duration literal.
	case p.cur.Kind == lexer.Duration:
		return p.parseDurationLiteral()

	// Boolean literals.
	case p.cur.Kind == lexer.True:
		tok := p.cur
		p.advance()
		return &ast.Literal{Kind: ast.LitBool, Raw: tok.Text, Value: true,
			Pos: ast.Span{Start: tok.Start, End: tok.End}}

	case p.cur.Kind == lexer.False:
		tok := p.cur
		p.advance()
		return &ast.Literal{Kind: ast.LitBool, Raw: tok.Text, Value: false,
			Pos: ast.Span{Start: tok.Start, End: tok.End}}

	// Null literal.
	case p.cur.Kind == lexer.Null:
		tok := p.cur
		p.advance()
		return &ast.Literal{Kind: ast.LitNull, Raw: tok.Text, Value: nil,
			Pos: ast.Span{Start: tok.Start, End: tok.End}}

	// not (unary prefix — but this is handled in parseNot; if we reach here
	// it means 'not' appeared in primary position, which is fine — delegate).
	case p.cur.Kind == lexer.KwNot:
		return p.parseNot()

	// Parenthesized expression.
	case p.at(lexer.LParen):
		return p.parseParen()

	// Array literal.
	case p.at(lexer.LBracket):
		return p.parseArrayLiteral()

	// Object literal.
	case p.at(lexer.LBrace):
		return p.parseObjectLiteral()

	// Hard keywords in wrong position.
	case isHardKeyword(p.cur.Kind):
		p.errorf(p.cur, CodeUnexpectedToken, []string{"expression"}, "",
			"unexpected keyword '%s'", p.cur.Text)
		err := &ast.ErrorExpr{Message: "unexpected keyword", Pos: p.curSpan()}
		p.advance()
		return err

	default:
		span := p.curSpan()
		p.errorf(p.cur, CodeUnexpectedToken, []string{"expression"}, "",
			"expected expression, got %s", kindName(p.cur.Kind))
		err := &ast.ErrorExpr{Message: "expected expression", Pos: span}
		// Skip the bad token to avoid infinite loops.
		if p.cur.Kind != lexer.EOF {
			p.advance()
		}
		return err
	}
}

// parseParen parses a parenthesized expression or lambda.
func (p *parser) parseParen() ast.Expr {
	start := p.cur.Start
	p.advance() // consume (

	inner := p.parseExpr()

	end := p.cur.End
	if _, ok := p.expect(lexer.RParen); !ok {
		p.recoverTo(lexer.RParen)
		end = p.prev.End
	}

	return &ast.Paren{
		Inner: inner,
		Pos:   ast.Span{Start: start, End: end},
	}
}

// ---------------------------------------------------------------------------
// Literal parsers
// ---------------------------------------------------------------------------

func (p *parser) parseStringLiteral() *ast.Literal {
	tok := p.cur
	p.advance()
	// Interpret escape sequences.
	val := interpretString(tok.Text)
	return &ast.Literal{
		Kind:  ast.LitString,
		Raw:   tok.Text,
		Value: val,
		Pos:   ast.Span{Start: tok.Start, End: tok.End},
	}
}

func (p *parser) parseRawStringLiteral() *ast.Literal {
	tok := p.cur
	p.advance()
	// Raw string: r"..." — strip r" and closing ".
	val := tok.Text
	if len(val) >= 3 && val[0] == 'r' && val[1] == '"' && val[len(val)-1] == '"' {
		val = val[2 : len(val)-1]
	}
	return &ast.Literal{
		Kind:  ast.LitRawString,
		Raw:   tok.Text,
		Value: val,
		Pos:   ast.Span{Start: tok.Start, End: tok.End},
	}
}

func (p *parser) parseIntLiteral() *ast.Literal {
	tok := p.cur
	p.advance()
	val, err := parseInt(tok.Text)
	if err != nil {
		p.errorf(tok, CodeUnexpectedToken, nil, "",
			"invalid integer literal: %v", err)
		return &ast.Literal{Kind: ast.LitInt, Raw: tok.Text, Value: int64(0),
			Pos: ast.Span{Start: tok.Start, End: tok.End}}
	}
	return &ast.Literal{
		Kind:  ast.LitInt,
		Raw:   tok.Text,
		Value: val,
		Pos:   ast.Span{Start: tok.Start, End: tok.End},
	}
}

func (p *parser) parseFloatLiteral() *ast.Literal {
	tok := p.cur
	p.advance()
	val, err := strconv.ParseFloat(tok.Text, 64)
	if err != nil {
		p.errorf(tok, CodeUnexpectedToken, nil, "",
			"invalid float literal: %v", err)
		val = math.NaN()
	}
	return &ast.Literal{
		Kind:  ast.LitFloat,
		Raw:   tok.Text,
		Value: val,
		Pos:   ast.Span{Start: tok.Start, End: tok.End},
	}
}

func (p *parser) parseDurationLiteral() *ast.Literal {
	tok := p.cur
	p.advance()
	val, err := parseDuration(tok.Text)
	if err != nil {
		p.errorf(tok, CodeUnexpectedToken, nil, "",
			"invalid duration literal: %v", err)
		val = 0
	}
	return &ast.Literal{
		Kind:  ast.LitDuration,
		Raw:   tok.Text,
		Value: val,
		Pos:   ast.Span{Start: tok.Start, End: tok.End},
	}
}

// parseArrayLiteral: '[' [expr (',' expr)*] ']'
func (p *parser) parseArrayLiteral() ast.Expr {
	start := p.cur.Start
	p.advance() // consume [

	var elems []ast.Expr
	if !p.at(lexer.RBracket) {
		elems = append(elems, p.parseExpr())
		for p.consume(lexer.Comma) {
			if p.at(lexer.RBracket) {
				break // trailing comma OK
			}
			elems = append(elems, p.parseExpr())
		}
	}

	end := p.cur.End
	if _, ok := p.expect(lexer.RBracket); !ok {
		p.recoverTo(lexer.RBracket)
		end = p.prev.End
	}

	return &ast.Array{
		Elems: elems,
		Pos:   ast.Span{Start: start, End: end},
	}
}

// parseObjectLiteral: '{' [obj_entry (',' obj_entry)*] '}'
func (p *parser) parseObjectLiteral() ast.Expr {
	start := p.cur.Start
	p.advance() // consume {

	var entries []ast.ObjectEntry
	if !p.at(lexer.RBrace) {
		entries = append(entries, p.parseObjectEntry())
		for p.consume(lexer.Comma) {
			if p.at(lexer.RBrace) {
				break // trailing comma OK
			}
			entries = append(entries, p.parseObjectEntry())
		}
	}

	end := p.cur.End
	if _, ok := p.expect(lexer.RBrace); !ok {
		p.recoverTo(lexer.RBrace)
		end = p.prev.End
	}

	return &ast.Object{
		Entries: entries,
		Pos:     ast.Span{Start: start, End: end},
	}
}

// parseObjectEntry parses a single key: value pair.
func (p *parser) parseObjectEntry() ast.ObjectEntry {
	// Key can be an identifier (bare or soft keyword) or a string.
	var key string
	keySpan := p.curSpan()

	switch {
	case p.cur.Kind == lexer.String:
		key = interpretString(p.cur.Text)
		p.advance()
	case p.cur.Kind == lexer.Ident || p.cur.Kind == lexer.BacktickIdent || isSoftKeyword(p.cur.Kind):
		key, _ = p.identLike()
		p.advance()
	default:
		p.errorf(p.cur, CodeUnexpectedToken, []string{"key"}, "",
			"expected object key (identifier or string), got %s", kindName(p.cur.Kind))
		key = "<error>"
		if p.cur.Kind != lexer.EOF {
			p.advance()
		}
	}

	if !p.consume(lexer.Colon) {
		p.errorf(p.cur, CodeUnexpectedToken, []string{":"}, "",
			"expected ':' after object key, got %s", kindName(p.cur.Kind))
	}

	value := p.parseExpr()

	return ast.ObjectEntry{
		Key:     key,
		KeySpan: keySpan,
		Value:   value,
	}
}

// ---------------------------------------------------------------------------
// Error helpers
// ---------------------------------------------------------------------------

func (p *parser) errorf(tok lexer.Token, code DiagCode, expected []string, suggestion, format string, args ...interface{}) {
	p.diags = append(p.diags, Diag{
		Code:       code,
		Message:    fmt.Sprintf(format, args...),
		Span:       ast.Span{Start: tok.Start, End: tok.End},
		Expected:   expected,
		Suggestion: suggestion,
	})
}

// recoverTo skips tokens until it finds kind or EOF, then consumes it.
func (p *parser) recoverTo(kind lexer.Kind) {
	for p.cur.Kind != kind && p.cur.Kind != lexer.EOF {
		p.advance()
	}
	if p.cur.Kind == kind {
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// Value parsers
// ---------------------------------------------------------------------------

// parseInt parses an integer literal (decimal or hex).
func parseInt(s string) (int64, error) {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return strconv.ParseInt(s[2:], 16, 64)
	}
	return strconv.ParseInt(s, 10, 64)
}

// parseDuration parses a duration literal like "30s", "1.5h", "100ms".
func parseDuration(s string) (time.Duration, error) {
	// Find the boundary between number and unit.
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == 'e' || s[i] == 'E' || s[i] == '+' || s[i] == '-') {
		// Be careful: the '-' might be part of the exponent (1e-6), or it
		// might be before the number. For duration we expect number+unit.
		// We just scan until we hit a letter that starts the unit.
		if i > 0 && (s[i] == '+' || s[i] == '-') && s[i-1] != 'e' && s[i-1] != 'E' {
			break
		}
		i++
	}
	numStr := s[:i]
	unit := s[i:]

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number in duration %q: %w", s, err)
	}

	var mult time.Duration
	switch unit {
	case "ns":
		mult = time.Nanosecond
	case "us":
		mult = time.Microsecond
	case "ms":
		mult = time.Millisecond
	case "s":
		mult = time.Second
	case "m":
		mult = time.Minute
	case "h":
		mult = time.Hour
	case "d":
		mult = 24 * time.Hour
	case "w":
		mult = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown duration unit %q in %q", unit, s)
	}

	return time.Duration(num * float64(mult)), nil
}

// interpretString interprets a double-quoted string literal with escape
// processing. The input includes the surrounding quotes.
func interpretString(raw string) string {
	if len(raw) < 2 {
		return raw
	}
	inner := raw[1 : len(raw)-1] // strip quotes

	var b strings.Builder
	b.Grow(len(inner))
	for i := 0; i < len(inner); i++ {
		if inner[i] != '\\' {
			b.WriteByte(inner[i])
			continue
		}
		i++
		if i >= len(inner) {
			b.WriteByte('\\')
			break
		}
		switch inner[i] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'u':
			// \u{NNNN}
			if i+1 < len(inner) && inner[i+1] == '{' {
				j := i + 2
				for j < len(inner) && inner[j] != '}' {
					j++
				}
				hex := inner[i+2 : j]
				n, err := strconv.ParseUint(hex, 16, 32)
				if err == nil {
					b.WriteRune(rune(n))
				}
				if j < len(inner) {
					i = j // skip past }
				}
			}
		default:
			b.WriteByte('\\')
			b.WriteByte(inner[i])
		}
	}
	return b.String()
}

// kindName returns a human-readable name for a token kind.
func kindName(k lexer.Kind) string {
	switch k {
	case lexer.EOF:
		return "end of input"
	case lexer.Ident:
		return "identifier"
	case lexer.BacktickIdent:
		return "backtick identifier"
	case lexer.String:
		return "string"
	case lexer.RawString:
		return "raw string"
	case lexer.Int:
		return "integer"
	case lexer.Float:
		return "float"
	case lexer.Duration:
		return "duration"
	case lexer.True, lexer.False:
		return "boolean"
	case lexer.Null:
		return "null"
	case lexer.LParen:
		return "'('"
	case lexer.RParen:
		return "')'"
	case lexer.LBracket:
		return "'['"
	case lexer.RBracket:
		return "']'"
	case lexer.LBrace:
		return "'{'"
	case lexer.RBrace:
		return "'}'"
	case lexer.Eq:
		return "'='"
	case lexer.EqEq:
		return "'=='"
	case lexer.BangEq:
		return "'!='"
	case lexer.Comma:
		return "','"
	case lexer.Dot:
		return "'.'"
	case lexer.SafeNav:
		return "'?.'"
	case lexer.Colon:
		return "':'"
	case lexer.Arrow:
		return "'->'"
	case lexer.Bang:
		return "'!'"
	case lexer.Pipe:
		return "'|'"
	case lexer.Plus:
		return "'+'"
	case lexer.Minus:
		return "'-'"
	case lexer.Star:
		return "'*'"
	case lexer.Slash:
		return "'/'"
	case lexer.Percent:
		return "'%'"
	case lexer.Coalesce:
		return "'??'"
	case lexer.Error:
		return "error"
	}
	// For keywords, use the keyword text.
	return k.String()
}
