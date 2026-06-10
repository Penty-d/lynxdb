package parser

import (
	"fmt"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/lexer"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/registry"
)

// ---------------------------------------------------------------------------
// New diag codes for query-level parsing
// ---------------------------------------------------------------------------

const (
	// CodeUnknownStage is emitted for an unrecognized stage name.
	CodeUnknownStage DiagCode = "E010"

	// CodeKilledSpelling is emitted for a killed RFC-001/SPL1 spelling
	// with a specific fix-it.
	CodeKilledSpelling DiagCode = "E011"

	// CodeSortByDesc is emitted when sort uses 'by ... desc' form (D22).
	CodeSortByDesc DiagCode = "E012"

	// CodeCountNoParens is emitted when count is used without parens (D28).
	CodeCountNoParens DiagCode = "E013"

	// CodeStageError is emitted for a generic parse error inside a stage.
	CodeStageError DiagCode = "E014"
)

// ---------------------------------------------------------------------------
// Parse entry point
// ---------------------------------------------------------------------------

// Parse parses a complete LynxFlow v2 query and returns the AST along with
// any diagnostics. On success diags is empty. On error the returned Query
// is a best-effort partial tree (never nil) and diags contains entries.
//
// Error recovery follows RFC-002 principle 9: errors inside a stage are
// recorded, parsing skips to the next | (or ;/EOF), and continues so that
// one parse reports ALL broken stages.
func Parse(input string) (*ast.Query, []Diag) {
	p := &parser{
		lex: lexer.New(input),
		src: input,
	}
	p.advance() // prime cur

	q := p.parseQuery()
	return q, p.diags
}

// ---------------------------------------------------------------------------
// Query-level parsing
// ---------------------------------------------------------------------------

func (p *parser) parseQuery() *ast.Query {
	start := p.cur.Start
	q := &ast.Query{}

	// Parse let bindings: let $name = <pipeline> ;
	for p.at(lexer.KwLet) {
		l := p.parseLet()
		q.Lets = append(q.Lets, l)
	}

	q.Pipeline = p.parsePipeline()
	q.Pos = ast.Span{Start: start, End: p.prev.End}

	// If there are remaining tokens, report them.
	if p.cur.Kind != lexer.EOF {
		p.errorf(p.cur, CodeUnexpectedToken, nil, "",
			"unexpected %s after query", kindName(p.cur.Kind))
	}

	return q
}

func (p *parser) parseLet() ast.Let {
	start := p.cur.Start
	p.advance() // consume 'let'

	// Expect $ followed by ident.
	if !p.at(lexer.Dollar) {
		p.errorf(p.cur, CodeUnexpectedToken, []string{"$"}, "",
			"expected '$' after 'let', got %s", kindName(p.cur.Kind))
	} else {
		p.advance() // consume $
	}

	name := ""
	nameSpan := p.curSpan()
	if n, ok := p.identLike(); ok {
		name = n
		nameSpan = p.curSpan()
		p.advance()
	} else {
		p.errorf(p.cur, CodeUnexpectedToken, []string{"identifier"}, "",
			"expected CTE name after '$', got %s", kindName(p.cur.Kind))
	}

	// Expect =
	if !p.consume(lexer.Eq) {
		p.errorf(p.cur, CodeUnexpectedToken, []string{"="}, "",
			"expected '=' after CTE name, got %s", kindName(p.cur.Kind))
	}

	pipeline := p.parsePipeline()

	end := p.prev.End
	// Expect ;
	if !p.consume(lexer.Semicolon) {
		p.errorf(p.cur, CodeUnexpectedToken, []string{";"}, "",
			"expected ';' after CTE pipeline, got %s", kindName(p.cur.Kind))
	} else {
		end = p.prev.End
	}

	return ast.Let{
		Name:     name,
		NameSpan: nameSpan,
		Pipeline: pipeline,
		Pos:      ast.Span{Start: start, End: end},
	}
}

func (p *parser) parsePipeline() ast.Pipeline {
	start := p.cur.Start
	pip := ast.Pipeline{}

	// Check for from stage or implicit source.
	if p.at(lexer.KwFrom) {
		from := p.parseFromStage()
		pip.Source = &from
	} else if !p.at(lexer.Pipe) && !p.at(lexer.EOF) && !p.at(lexer.Semicolon) && !p.at(lexer.RBracket) {
		// If we're at a stage keyword (not from), this is an implicit source pipeline.
		// If we're at something that looks like search sugar at the top level,
		// it's also implicit from.
		if p.isStageStart() {
			// Implicit source: from <default>
		} else {
			// Could be freehand search: treat as from <default> with search sugar.
			// Actually, for corpus compatibility let's check: the query starts
			// with something that is not a stage keyword or |. This is unusual
			// and we leave Source=nil for the desugarer.
		}
	}

	// Parse pipe-delimited stages.
	for {
		if p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		if p.at(lexer.Pipe) {
			p.advance() // consume |
		}
		if p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		stage := p.parseStage()
		pip.Stages = append(pip.Stages, stage)
	}

	end := p.prev.End
	if end < start {
		end = start
	}
	pip.Pos = ast.Span{Start: start, End: end}
	return pip
}

// ---------------------------------------------------------------------------
// From stage
// ---------------------------------------------------------------------------

func (p *parser) parseFromStage() ast.FromStage {
	start := p.cur.Start
	p.advance() // consume 'from'

	from := ast.FromStage{Pos: ast.Span{Start: start}}

	// Parse source atoms: names, globs, !-excludes, *, $cte refs.
	from.Sources = p.parseSourceList()

	// Parse optional bracket time ranges.
	for p.at(lexer.LBracket) {
		tr := p.parseTimeRange()
		from.TimeRanges = append(from.TimeRanges, tr)
	}

	// Parse search sugar terms (everything until | or EOF or ; or ]).
	if p.isSearchSugarStart() {
		from.SugarTerms = p.parseSearchSugar()
	}

	from.Pos.End = p.prev.End
	return from
}

func (p *parser) parseSourceList() []ast.SourceAtom {
	var sources []ast.SourceAtom

	for {
		atom := p.parseSourceAtom()
		if atom.Kind == SourceAtomEmpty {
			break
		}
		sources = append(sources, atom)
		if !p.consume(lexer.Comma) {
			break
		}
	}

	return sources
}

// SourceAtomEmpty is a sentinel value for an empty/unparseable source atom.
const SourceAtomEmpty ast.SourceAtomKind = 255

func (p *parser) parseSourceAtom() ast.SourceAtom {
	start := p.cur.Start

	// Star: all sources
	if p.at(lexer.Star) {
		end := p.cur.End
		p.advance()
		return ast.SourceAtom{Kind: ast.SourceStar, Pos: ast.Span{Start: start, End: end}}
	}

	// $cte reference
	if p.at(lexer.Dollar) {
		p.advance() // consume $
		if n, ok := p.identLike(); ok {
			end := p.cur.End
			p.advance()
			return ast.SourceAtom{Kind: ast.SourceCTE, Name: n, Pos: ast.Span{Start: start, End: end}}
		}
		return ast.SourceAtom{Kind: SourceAtomEmpty}
	}

	// !-prefixed exclude
	if p.at(lexer.Bang) {
		p.advance()
		name, pattern := p.parseSourceName()
		end := p.prev.End
		if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
			return ast.SourceAtom{Kind: ast.SourceNegated, Name: name, Pattern: pattern, Pos: ast.Span{Start: start, End: end}}
		}
		return ast.SourceAtom{Kind: ast.SourceNegated, Name: pattern, Pos: ast.Span{Start: start, End: end}}
	}

	// Backtick-quoted source
	if p.at(lexer.BacktickIdent) {
		raw := p.cur.Text
		name := raw
		if len(raw) >= 2 && raw[0] == '`' && raw[len(raw)-1] == '`' {
			name = raw[1 : len(raw)-1]
		}
		end := p.cur.End
		p.advance()
		return ast.SourceAtom{Kind: ast.SourceName, Name: name, Quoted: true, Pos: ast.Span{Start: start, End: end}}
	}

	// Bare name, possibly with glob chars or dashes (span-adjacent run)
	if n, ok := p.identLike(); ok {
		nameEnd := p.cur.End
		p.advance()
		pattern, nameEnd, isGlob := p.readAdjacentRun(n, nameEnd)
		if isGlob {
			return ast.SourceAtom{Kind: ast.SourceGlob, Name: n, Pattern: pattern, Pos: ast.Span{Start: start, End: nameEnd}}
		}
		return ast.SourceAtom{Kind: ast.SourceName, Name: pattern, Pos: ast.Span{Start: start, End: nameEnd}}
	}

	return ast.SourceAtom{Kind: SourceAtomEmpty}
}

func (p *parser) parseSourceName() (name, pattern string) {
	if n, ok := p.identLike(); ok {
		nameEnd := p.cur.End
		p.advance()
		pattern, nameEnd, _ = p.readAdjacentRun(n, nameEnd)
		return n, pattern
	}
	return "", ""
}

// runTokenOK reports whether the current token may extend a bare-word/glob
// run: glob metacharacters, dashes, digit groups, or further ident chunks.
// Source names and search-sugar values commonly contain dashes
// (`logs-debug*`, `host=web-01`), which the lexer splits into separate
// tokens; the parser reassembles span-adjacent runs.
func (p *parser) runTokenOK() bool {
	switch p.cur.Kind {
	case lexer.Star, lexer.Minus, lexer.Question, lexer.Int, lexer.Float, lexer.Duration:
		return true
	}
	_, ok := p.identLike()
	return ok
}

// readAdjacentRun extends pattern with span-adjacent run tokens starting at
// byte offset end. It returns the assembled text, the new end offset, and
// whether the run contains glob metacharacters.
func (p *parser) readAdjacentRun(pattern string, end int) (string, int, bool) {
	isGlob := strings.ContainsAny(pattern, "*?")
	for p.cur.Start == end && p.runTokenOK() {
		if p.cur.Kind == lexer.Star || p.cur.Kind == lexer.Question {
			isGlob = true
		}
		pattern += p.cur.Text
		end = p.cur.End
		p.advance()
	}
	return pattern, end, isGlob
}

// ---------------------------------------------------------------------------
// Time range parsing
// ---------------------------------------------------------------------------

func (p *parser) parseTimeRange() ast.TimeRange {
	start := p.cur.Start
	p.advance() // consume [

	tr := ast.TimeRange{Pos: ast.Span{Start: start}}

	// Check for snap-only: [@d]
	if p.at(lexer.At) {
		snapStart := p.cur.Start
		p.advance() // consume @
		if n, ok := p.identLike(); ok {
			tr.Snap = "@" + n
			tr.SnapSpan = ast.Span{Start: snapStart, End: p.cur.End}
			p.advance()
		} else if p.at(lexer.Duration) {
			// Duration token as snap unit
			tr.Snap = "@" + p.cur.Text
			tr.SnapSpan = ast.Span{Start: snapStart, End: p.cur.End}
			p.advance()
		}
		tr.Pos.End = p.cur.End
		if _, ok := p.expect(lexer.RBracket); ok {
			tr.Pos.End = p.prev.End
		}
		return tr
	}

	// Parse start expression (may be negative duration like -1h, absolute timestamp, etc.)
	tr.Start = p.parseExpr()

	// Check for range operator ..
	if p.at(lexer.DotDot) {
		p.advance()
		tr.End = p.parseExpr()
	}

	tr.Pos.End = p.cur.End
	if _, ok := p.expect(lexer.RBracket); ok {
		tr.Pos.End = p.prev.End
	}

	// Optional snap suffix: [@unit]
	if p.at(lexer.LBracket) {
		p.advance() // consume [
		if p.at(lexer.At) {
			snapStart := p.cur.Start
			p.advance()
			if n, ok := p.identLike(); ok {
				tr.Snap = "@" + n
				tr.SnapSpan = ast.Span{Start: snapStart, End: p.cur.End}
				p.advance()
			} else if p.at(lexer.Duration) {
				tr.Snap = "@" + p.cur.Text
				tr.SnapSpan = ast.Span{Start: snapStart, End: p.cur.End}
				p.advance()
			}
		}
		p.expect(lexer.RBracket)
	}

	return tr
}

// ---------------------------------------------------------------------------
// Search sugar parsing (§3.1)
// ---------------------------------------------------------------------------

func (p *parser) isSearchSugarStart() bool {
	// Search sugar starts after sources/ranges, until | or EOF or ; or ].
	// It can start with: ident, string, (, not, or a comparison key=value.
	if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
		return false
	}
	// If at a stage keyword, it's not sugar — it's the next stage.
	// But not if it looks like key=value (e.g., status>=500).
	return true
}

// parseSearchSugar parses search sugar terms with standard precedence:
// not > and > or. Juxtaposition is implicit 'and'.
func (p *parser) parseSearchSugar() ast.SearchExpr {
	return p.parseSearchOr()
}

func (p *parser) parseSearchOr() ast.SearchExpr {
	left := p.parseSearchAnd()
	if left == nil {
		return nil
	}
	for p.at(lexer.KwOr) {
		start := left.SearchExprSpan().Start
		p.advance()
		right := p.parseSearchAnd()
		if right == nil {
			break
		}
		left = &ast.SearchBinary{
			Op:    "or",
			Left:  left,
			Right: right,
			Pos:   ast.Span{Start: start, End: right.SearchExprSpan().End},
		}
	}
	return left
}

func (p *parser) parseSearchAnd() ast.SearchExpr {
	left := p.parseSearchNot()
	if left == nil {
		return nil
	}
	for {
		// Explicit 'and'
		if p.at(lexer.KwAnd) {
			start := left.SearchExprSpan().Start
			p.advance()
			right := p.parseSearchNot()
			if right == nil {
				break
			}
			left = &ast.SearchBinary{
				Op:    "and",
				Left:  left,
				Right: right,
				Pos:   ast.Span{Start: start, End: right.SearchExprSpan().End},
			}
			continue
		}
		// Implicit 'and' via juxtaposition — check if next token can start a search term.
		if p.isSearchTermStart() {
			start := left.SearchExprSpan().Start
			right := p.parseSearchNot()
			if right == nil {
				break
			}
			left = &ast.SearchBinary{
				Op:    "and",
				Left:  left,
				Right: right,
				Pos:   ast.Span{Start: start, End: right.SearchExprSpan().End},
			}
			continue
		}
		break
	}
	return left
}

func (p *parser) parseSearchNot() ast.SearchExpr {
	if p.at(lexer.KwNot) {
		start := p.cur.Start
		p.advance()
		operand := p.parseSearchNot()
		if operand == nil {
			return nil
		}
		return &ast.SearchNot{
			Operand: operand,
			Pos:     ast.Span{Start: start, End: operand.SearchExprSpan().End},
		}
	}
	return p.parseSearchPrimary()
}

func (p *parser) isSearchTermStart() bool {
	if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
		return false
	}
	if p.at(lexer.KwOr) {
		return false
	}
	// Ident, string, (, not, backtick, int, float — all can start terms.
	switch p.cur.Kind {
	case lexer.Ident, lexer.BacktickIdent, lexer.String, lexer.LParen,
		lexer.KwNot, lexer.Int, lexer.Float, lexer.True, lexer.False:
		return true
	}
	if isSoftKeyword(p.cur.Kind) {
		return true
	}
	return false
}

func (p *parser) parseSearchPrimary() ast.SearchExpr {
	// Parenthesized group
	if p.at(lexer.LParen) {
		start := p.cur.Start
		p.advance()
		inner := p.parseSearchSugar()
		end := p.cur.End
		if !p.consume(lexer.RParen) {
			p.errorf(p.cur, CodeUnexpectedToken, []string{")"}, "",
				"expected ')' in search sugar, got %s", kindName(p.cur.Kind))
		} else {
			end = p.prev.End
		}
		if inner != nil {
			return &ast.SearchParen{Inner: inner, Pos: ast.Span{Start: start, End: end}}
		}
		return nil
	}

	// Quoted phrase: "connection reset"
	if p.at(lexer.String) {
		start := p.cur.Start
		raw := p.cur.Text
		text := interpretString(raw)
		end := p.cur.End
		p.advance()
		return &ast.SearchPhrase{Text: text, Raw: raw, Pos: ast.Span{Start: start, End: end}}
	}

	// Ident-like: could be bare word, key=value, or key <op> value.
	if name, ok := p.identLike(); ok {
		start := p.cur.Start
		nameEnd := p.cur.End
		p.advance()

		// Check for comparison operators: =, !=, <, <=, >, >=
		switch p.cur.Kind {
		case lexer.Eq:
			p.advance()
			val := p.parseSearchValue()
			end := p.prev.End
			return &ast.SearchKeyValue{Key: name, Op: "=", Value: val, Pos: ast.Span{Start: start, End: end}}
		case lexer.BangEq:
			p.advance()
			val := p.parseSearchValue()
			end := p.prev.End
			return &ast.SearchKeyValue{Key: name, Op: "!=", Value: val, Pos: ast.Span{Start: start, End: end}}
		case lexer.Lt:
			p.advance()
			val := p.parseSearchValue()
			end := p.prev.End
			return &ast.SearchKeyValue{Key: name, Op: "<", Value: val, Pos: ast.Span{Start: start, End: end}}
		case lexer.LtEq:
			p.advance()
			val := p.parseSearchValue()
			end := p.prev.End
			return &ast.SearchKeyValue{Key: name, Op: "<=", Value: val, Pos: ast.Span{Start: start, End: end}}
		case lexer.Gt:
			p.advance()
			val := p.parseSearchValue()
			end := p.prev.End
			return &ast.SearchKeyValue{Key: name, Op: ">", Value: val, Pos: ast.Span{Start: start, End: end}}
		case lexer.GtEq:
			p.advance()
			val := p.parseSearchValue()
			end := p.prev.End
			return &ast.SearchKeyValue{Key: name, Op: ">=", Value: val, Pos: ast.Span{Start: start, End: end}}
		case lexer.KwIn:
			p.advance()
			// Expect ( list )
			if p.at(lexer.LParen) {
				p.advance()
				var vals []ast.Expr
				if !p.at(lexer.RParen) {
					vals = append(vals, p.parseExpr())
					for p.consume(lexer.Comma) {
						vals = append(vals, p.parseExpr())
					}
				}
				end := p.cur.End
				p.expect(lexer.RParen)
				return &ast.SearchIn{Key: name, Values: vals, Pos: ast.Span{Start: start, End: end}}
			}
		}

		// Bare word — check for adjacent glob/dash run (span-adjacent)
		if p.cur.Start == nameEnd && p.runTokenOK() {
			pattern, gEnd, _ := p.readAdjacentRun(name, nameEnd)
			return &ast.SearchBareWord{Word: pattern, Pos: ast.Span{Start: start, End: gEnd}}
		}

		return &ast.SearchBareWord{Word: name, Pos: ast.Span{Start: start, End: nameEnd}}
	}

	return nil
}

func (p *parser) parseSearchValue() ast.Expr {
	// Search values are single atomic values: string, int, float, duration,
	// bool, ident (including glob patterns). They are NOT compound expressions.
	// This is critical: key="val1" and key2="val2" must NOT parse the 'and'
	// as part of the first value.
	start := p.cur.Start

	// String literal
	if p.at(lexer.String) {
		return p.parseStringLiteral()
	}

	// Raw string literal
	if p.at(lexer.RawString) {
		return p.parseRawStringLiteral()
	}

	// Integer
	if p.at(lexer.Int) {
		return p.parseIntLiteral()
	}

	// Float
	if p.at(lexer.Float) {
		return p.parseFloatLiteral()
	}

	// Duration
	if p.at(lexer.Duration) {
		return p.parseDurationLiteral()
	}

	// Boolean
	if p.at(lexer.True) {
		tok := p.cur
		p.advance()
		return &ast.Literal{Kind: ast.LitBool, Raw: tok.Text, Value: true,
			Pos: ast.Span{Start: tok.Start, End: tok.End}}
	}
	if p.at(lexer.False) {
		tok := p.cur
		p.advance()
		return &ast.Literal{Kind: ast.LitBool, Raw: tok.Text, Value: false,
			Pos: ast.Span{Start: tok.Start, End: tok.End}}
	}

	// Check for glob or dashed value: span-adjacent run starting at an ident
	if n, ok := p.identLike(); ok {
		nameEnd := p.cur.End
		p.advance()
		pattern, nameEnd, isGlob := p.readAdjacentRun(n, nameEnd)
		if isGlob {
			return &ast.SearchGlobValue{Pattern: pattern, Pos: ast.Span{Start: start, End: nameEnd}}
		}
		return &ast.Ident{Name: pattern, Pos: ast.Span{Start: start, End: nameEnd}}
	}

	// Fallback: parse a primary expression only (not a full expression).
	return p.parsePrimary()
}

// ---------------------------------------------------------------------------
// Stage parsing
// ---------------------------------------------------------------------------

func (p *parser) isStageStart() bool {
	k := p.cur.Kind
	return k.IsKeyword() && !isHardKeyword(k) && k != lexer.KwAs && k != lexer.KwBy &&
		k != lexer.KwWith && k != lexer.KwOn && k != lexer.KwExcept && k != lexer.KwLet
}

func (p *parser) parseStage() ast.Stage {
	start := p.cur.Start

	// Check for killed spellings first.
	if fix, ok := p.checkKilledSpelling(); ok {
		s := ast.Stage{
			Name:     strings.ToLower(p.cur.Text),
			NamePos:  p.curSpan(),
			Pos:      ast.Span{Start: start},
			HasError: true,
		}
		p.diags = append(p.diags, Diag{
			Code:       CodeKilledSpelling,
			Message:    fix.message,
			Span:       p.curSpan(),
			Suggestion: fix.suggestion,
		})
		p.advance() // skip the killed keyword
		p.skipToNextStage()
		s.Pos.End = p.prev.End
		return s
	}

	// Try to resolve stage name.
	name, isStage := p.resolveStage()
	if !isStage {
		// Unknown token in stage position — check if it's an ident that might
		// be a mistyped stage name.
		if n, ok := p.identLike(); ok {
			s := ast.Stage{
				Name:     n,
				NamePos:  p.curSpan(),
				Pos:      ast.Span{Start: start},
				HasError: true,
			}
			suggestion := didYouMean(n, registryStageNames())
			msg := fmt.Sprintf("unknown stage %q", n)
			if suggestion != "" {
				msg += fmt.Sprintf(", did you mean %q?", suggestion)
			}
			p.diags = append(p.diags, Diag{
				Code:       CodeUnknownStage,
				Message:    msg,
				Span:       p.curSpan(),
				Suggestion: suggestion,
			})
			p.advance()
			p.skipToNextStage()
			s.Pos.End = p.prev.End
			return s
		}

		// Completely unexpected token.
		s := ast.Stage{
			Name:     "<error>",
			NamePos:  p.curSpan(),
			Pos:      ast.Span{Start: start},
			HasError: true,
		}
		p.errorf(p.cur, CodeStageError, nil, "",
			"expected stage name, got %s", kindName(p.cur.Kind))
		p.skipToNextStage()
		s.Pos.End = p.prev.End
		return s
	}

	nameSpan := p.curSpan()
	p.advance() // consume stage keyword

	// Dispatch to typed parsers.
	stage := ast.Stage{Name: name, NamePos: nameSpan, Pos: ast.Span{Start: start}}

	// Use a recovery wrapper for each stage body parser.
	p.parseStageBody(&stage)

	stage.Pos.End = p.prev.End
	return stage
}

// parseStageBody dispatches to the correct stage body parser with error recovery.
func (p *parser) parseStageBody(s *ast.Stage) {
	defer func() {
		if r := recover(); r != nil {
			// We should never get here with our recovery model, but safety.
			s.HasError = true
		}
	}()

	// Mark position for recovery.
	switch s.Name {
	case "where":
		p.parseWhereBody(s)
	case "extend":
		p.parseExtendBody(s)
	case "rename":
		p.parseRenameBody(s)
	case "stats":
		p.parseStatsBody(s, false)
	case "eventstats":
		p.parseStatsBody(s, true)
	case "streamstats":
		p.parseStreamstatsBody(s)
	case "sort":
		p.parseSortBody(s)
	case "head":
		p.parseIntBody(s, true)
	case "tail":
		p.parseIntBody(s, false)
	case "dedup":
		p.parseDedupBody(s)
	case "keep":
		p.parseFieldPatternsBody(s, true)
	case "drop":
		p.parseFieldPatternsBody(s, false)
	case "join":
		p.parseJoinBody(s)
	case "union":
		p.parseUnionBody(s)
	case "explode":
		p.parseExplodeBody(s)
	case "describe":
		s.Describe = &ast.DescribePayload{}
	case "parse":
		p.parseParseBody(s)
	case "top":
		p.parseTopRareBody(s, true)
	case "rare":
		p.parseTopRareBody(s, false)
	case "every":
		p.parseEveryBody(s)
	case "rate":
		p.parseRateBody(s)
	case "latency":
		p.parseLatencyBody(s)
	case "percentiles":
		p.parsePercentilesBody(s)
	case "proportion":
		p.parseProportionBody(s)
	case "facets":
		p.parseFacetsBody(s)
	case "impact":
		p.parseImpactBody(s)
	case "baseline":
		p.parseBaselineBody(s)
	case "changes":
		p.parseChangesBody(s)
	case "exemplars":
		p.parseExemplarsBody(s)
	case "materialize":
		p.parseMaterializeBody(s)
	case "tee":
		p.parseTeeBody(s)
	case "use":
		p.parseUseBody(s)
	case "compare":
		p.parseCompareBody(s)
	case "transaction":
		p.parseTransactionBody(s)
	case "correlate":
		p.parseCorrelateBody(s)
	case "rollup":
		p.parseRollupBody(s)
	case "xyseries":
		p.parseXYSeriesBody(s)
	case "patterns", "outliers", "sessionize", "trace", "topology":
		p.parseGenericOptionsBody(s)
	default:
		p.parseGenericOptionsBody(s)
	}
}

// ---------------------------------------------------------------------------
// Individual stage body parsers
// ---------------------------------------------------------------------------

func (p *parser) parseWhereBody(s *ast.Stage) {
	expr := p.parseExprSafe()
	s.Where = &ast.WherePayload{Expr: expr}
}

func (p *parser) parseExtendBody(s *ast.Stage) {
	assigns := p.parseAssignList()
	s.Extend = &ast.AssignPayload{Assignments: assigns}
}

func (p *parser) parseRenameBody(s *ast.Stage) {
	var renames []ast.RenameEntry
	for {
		re := p.parseRenameEntry()
		renames = append(renames, re)
		if !p.consume(lexer.Comma) {
			break
		}
	}
	s.Rename = &ast.RenamePayload{Renames: renames}
}

func (p *parser) parseRenameEntry() ast.RenameEntry {
	start := p.cur.Start
	old := ""
	oldSpan := p.curSpan()
	if n, ok := p.identLike(); ok {
		old = n
		oldSpan = p.curSpan()
		p.advance()
	} else {
		p.errorf(p.cur, CodeStageError, []string{"identifier"}, "",
			"expected field name in rename, got %s", kindName(p.cur.Kind))
	}

	if !p.consume(lexer.KwAs) {
		p.errorf(p.cur, CodeStageError, []string{"as"}, "",
			"expected 'as' in rename, got %s", kindName(p.cur.Kind))
	}

	newName := ""
	newSpan := p.curSpan()
	if n, ok := p.identLike(); ok {
		newName = n
		newSpan = p.curSpan()
		p.advance()
	} else {
		p.errorf(p.cur, CodeStageError, []string{"identifier"}, "",
			"expected new name after 'as' in rename, got %s", kindName(p.cur.Kind))
	}

	return ast.RenameEntry{
		Old:     old,
		OldSpan: oldSpan,
		New:     newName,
		NewSpan: newSpan,
		Pos:     ast.Span{Start: start, End: p.prev.End},
	}
}

func (p *parser) parseStatsBody(s *ast.Stage, isEventstats bool) {
	payload := &ast.StatsPayload{}
	payload.Aggs = p.parseAggList()

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	if isEventstats {
		s.Eventstats = payload
	} else {
		s.Stats = payload
	}
}

func (p *parser) parseStreamstatsBody(s *ast.Stage) {
	payload := &ast.StreamstatsPayload{}

	// Parse options before agg list: window=N, current=true|false.
	for {
		if n, ok := p.identLike(); ok {
			if n == "window" && p.peekIsEq() {
				p.advance() // consume 'window'
				p.advance() // consume '='
				v := p.parseIntValue()
				vi := int(v)
				payload.Window = &vi
				continue
			}
			if n == "current" && p.peekIsEq() {
				p.advance() // consume 'current'
				p.advance() // consume '='
				bv := p.parseBoolValue()
				payload.Current = &bv
				continue
			}
		}
		break
	}

	payload.Aggs = p.parseAggList()
	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Streamstats = payload
}

func (p *parser) parseSortBody(s *ast.Stage) {
	// D22: prefix form only. 'sort by x desc' is an error.
	// Check for 'by' keyword — that's the D22 error form.
	if p.at(lexer.KwBy) {
		p.diags = append(p.diags, Diag{
			Code:       CodeSortByDesc,
			Message:    "sort uses prefix form only: sort -f desc, +f asc",
			Span:       p.curSpan(),
			Suggestion: "use sort -f, +f instead of sort by f desc",
		})
		p.advance() // skip 'by' and try to recover
		s.HasError = true
	}

	var keys []ast.SortKey
	for {
		k := p.parseSortKey()
		keys = append(keys, k)
		if !p.consume(lexer.Comma) {
			break
		}
	}
	s.Sort = &ast.SortPayload{Keys: keys}
}

func (p *parser) parseSortKey() ast.SortKey {
	start := p.cur.Start
	desc := false

	if p.at(lexer.Minus) {
		desc = true
		p.advance()
	} else if p.at(lexer.Plus) {
		p.advance()
	}

	field := p.parseExprSafe()

	// D22: check for trailing 'desc'/'asc' — error with fix-it.
	if n, ok := p.identLike(); ok && (n == "desc" || n == "asc") {
		p.diags = append(p.diags, Diag{
			Code:       CodeSortByDesc,
			Message:    fmt.Sprintf("sort uses prefix form only; use -%s instead of %s desc", field, field),
			Span:       p.curSpan(),
			Suggestion: fmt.Sprintf("use -%s instead", field),
		})
		p.advance() // skip desc/asc
	}

	return ast.SortKey{
		Field: field,
		Desc:  desc,
		Pos:   ast.Span{Start: start, End: p.prev.End},
	}
}

func (p *parser) parseIntBody(s *ast.Stage, isHead bool) {
	val := p.parseIntValue()
	payload := &ast.IntPayload{N: val, Pos: ast.Span{Start: s.NamePos.Start, End: p.prev.End}}
	if isHead {
		s.Head = payload
	} else {
		s.Tail = payload
	}
}

func (p *parser) parseDedupBody(s *ast.Stage) {
	payload := &ast.DedupPayload{N: 1}

	// Optional leading integer.
	if p.at(lexer.Int) {
		payload.N = p.parseIntValue()
	}

	payload.Fields = p.parseFieldList()
	s.Dedup = payload
}

func (p *parser) parseFieldPatternsBody(s *ast.Stage, isKeep bool) {
	payload := &ast.FieldPatternsPayload{}

	// Check for * except ...
	if p.at(lexer.Star) {
		savedStart := p.cur.Start
		p.advance()
		if p.at(lexer.KwExcept) {
			p.advance()
			payload.StarExcept = true
		} else {
			// It's just *, push it back as a pattern. But we already consumed it.
			// Add it as a glob pattern.
			payload.Patterns = append(payload.Patterns, ast.FieldPattern{
				Name: "*",
				Glob: true,
				Pos:  ast.Span{Start: savedStart, End: savedStart + 1},
			})
		}
	}

	// Parse patterns.
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		pat := p.parseFieldPattern()
		payload.Patterns = append(payload.Patterns, pat)
		if !p.consume(lexer.Comma) {
			break
		}
	}

	if isKeep {
		s.Keep = payload
	} else {
		s.Drop = payload
	}
}

func (p *parser) parseFieldPattern() ast.FieldPattern {
	start := p.cur.Start
	if n, ok := p.identLike(); ok {
		nameEnd := p.cur.End
		p.advance()
		// Check for adjacent * (glob)
		pattern := n
		isGlob := false
		for p.at(lexer.Star) && p.cur.Start == nameEnd {
			pattern += "*"
			nameEnd = p.cur.End
			isGlob = true
			p.advance()
			if nn, ok2 := p.identLike(); ok2 && p.cur.Start == nameEnd {
				pattern += nn
				nameEnd = p.cur.End
				p.advance()
			}
		}
		return ast.FieldPattern{Name: pattern, Glob: isGlob, Pos: ast.Span{Start: start, End: nameEnd}}
	}
	if p.at(lexer.BacktickIdent) {
		raw := p.cur.Text
		name := raw
		if len(raw) >= 2 && raw[0] == '`' && raw[len(raw)-1] == '`' {
			name = raw[1 : len(raw)-1]
		}
		end := p.cur.End
		p.advance()
		return ast.FieldPattern{Name: name, Pos: ast.Span{Start: start, End: end}}
	}
	// Bare star
	if p.at(lexer.Star) {
		end := p.cur.End
		p.advance()
		return ast.FieldPattern{Name: "*", Glob: true, Pos: ast.Span{Start: start, End: end}}
	}
	p.errorf(p.cur, CodeStageError, []string{"field name"}, "",
		"expected field name or pattern, got %s", kindName(p.cur.Kind))
	if p.cur.Kind != lexer.EOF && p.cur.Kind != lexer.Pipe {
		p.advance()
	}
	return ast.FieldPattern{Name: "<error>", Pos: ast.Span{Start: start, End: p.prev.End}}
}

func (p *parser) parseJoinBody(s *ast.Stage) {
	payload := &ast.JoinPayload{Type: "inner"}

	// Parse options: type=inner|left|outer, on=field1[,field2]
	for {
		if n, ok := p.identLike(); ok {
			if n == "type" && p.peekIsEq() {
				p.advance()
				p.advance() // consume =
				payload.TypeSpan = p.curSpan()
				if v, ok2 := p.identLike(); ok2 {
					payload.Type = v
					p.advance()
				}
				continue
			}
		}
		if p.at(lexer.KwOn) {
			p.advance()
			payload.On = p.parseFieldList()
			continue
		}
		break
	}

	// Expect 'with' followed by $cte or [pipeline].
	if p.consume(lexer.KwWith) {
		sub := p.parseSubPipeline()
		payload.Right = &sub
	} else {
		p.errorf(p.cur, CodeStageError, []string{"with"}, "",
			"expected 'with' in join, got %s", kindName(p.cur.Kind))
	}

	s.Join = payload
}

func (p *parser) parseUnionBody(s *ast.Stage) {
	payload := &ast.UnionPayload{}
	for {
		sub := p.parseSubPipeline()
		payload.Sources = append(payload.Sources, sub)
		if !p.consume(lexer.Comma) {
			break
		}
	}
	s.Union = payload
}

func (p *parser) parseSubPipeline() ast.SubPipeline {
	start := p.cur.Start

	// $cte reference
	if p.at(lexer.Dollar) {
		p.advance()
		name := ""
		if n, ok := p.identLike(); ok {
			name = n
			p.advance()
		}
		return ast.SubPipeline{CTERef: name, Pos: ast.Span{Start: start, End: p.prev.End}}
	}

	// [pipeline]
	if p.at(lexer.LBracket) {
		p.advance()
		pip := p.parsePipeline()
		end := p.cur.End
		if !p.consume(lexer.RBracket) {
			p.errorf(p.cur, CodeUnexpectedToken, []string{"]"}, "",
				"expected ']' after sub-pipeline, got %s", kindName(p.cur.Kind))
		} else {
			end = p.prev.End
		}
		return ast.SubPipeline{Pipeline: &pip, Pos: ast.Span{Start: start, End: end}}
	}

	p.errorf(p.cur, CodeStageError, []string{"$cte or [pipeline]"}, "",
		"expected '$cte' or '[pipeline]' in sub-pipeline, got %s", kindName(p.cur.Kind))
	return ast.SubPipeline{Pos: ast.Span{Start: start, End: p.cur.End}}
}

func (p *parser) parseExplodeBody(s *ast.Stage) {
	payload := &ast.ExplodePayload{}
	payload.Array = p.parseExprSafe()
	if p.consume(lexer.KwAs) {
		if n, ok := p.identLike(); ok {
			payload.As = n
			payload.AsPos = p.curSpan()
			p.advance()
		}
	}
	s.Explode = payload
}

func (p *parser) parseParseBody(s *ast.Stage) {
	payload := &ast.ParsePayload{}

	// Check for first_of(f1, f2, ...)
	if n, ok := p.identLike(); ok && n == "first_of" {
		payload.FirstOfPos = p.curSpan()
		p.advance() // consume first_of
		if p.consume(lexer.LParen) {
			for {
				if fn, ok2 := p.identLike(); ok2 {
					payload.FirstOf = append(payload.FirstOf, fn)
					p.advance()
				} else if p.at(lexer.String) {
					payload.FirstOf = append(payload.FirstOf, interpretString(p.cur.Text))
					p.advance()
				}
				if !p.consume(lexer.Comma) {
					break
				}
			}
			p.expect(lexer.RParen)
		}
	} else if ok {
		// Format name
		payload.Format = n
		payload.FormatPos = p.curSpan()
		p.advance()

		// Optional format args: regex r"...", pattern "...", kv(sep=..., assign=...)
		if p.at(lexer.RawString) || p.at(lexer.String) {
			payload.FormatArgs = append(payload.FormatArgs, p.parseExprSafe())
		} else if p.at(lexer.LParen) {
			// kv(sep=..., assign=...)
			payload.FormatArgs = append(payload.FormatArgs, p.parseParen())
		}
	}

	// Parse options: from, into, prefix, on_error
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		if n, ok := p.identLike(); ok {
			switch n {
			case "from":
				p.advance()
				payload.From = p.parseExprSafe()
				continue
			case "into":
				p.advance()
				if p.consume(lexer.LParen) {
					for {
						cf := p.parseCaptureField()
						payload.Into = append(payload.Into, cf)
						if !p.consume(lexer.Comma) {
							break
						}
					}
					p.expect(lexer.RParen)
				}
				continue
			case "prefix":
				p.advance()
				if p.at(lexer.String) {
					payload.Prefix = interpretString(p.cur.Text)
					p.advance()
				} else if n2, ok2 := p.identLike(); ok2 {
					// The prefix may be a dotted name like "log." — assemble it.
					payload.Prefix = n2
					prefEnd := p.cur.End
					p.advance()
					for p.at(lexer.Dot) {
						payload.Prefix += "."
						prefEnd = p.cur.End
						p.advance()
						if nn, ok3 := p.identLike(); ok3 {
							payload.Prefix += nn
							prefEnd = p.cur.End
							p.advance()
						}
					}
					_ = prefEnd
				}
				continue
			case "on_error":
				p.advance()
				if n2, ok2 := p.identLike(); ok2 {
					payload.OnError = n2
					p.advance()
				}
				continue
			}
		}
		break
	}

	s.Parse = payload
}

func (p *parser) parseCaptureField() ast.CaptureField {
	cf := ast.CaptureField{Pos: p.curSpan()}
	if n, ok := p.identLike(); ok {
		cf.Name = n
		cf.Pos = p.curSpan()
		p.advance()
	}
	if p.consume(lexer.KwAs) {
		if t, ok := p.identLike(); ok {
			cf.Type = t
			p.advance()
		}
	}
	return cf
}

func (p *parser) parseTopRareBody(s *ast.Stage, isTop bool) {
	payload := &ast.TopRarePayload{}

	// Optional N
	if p.at(lexer.Int) {
		n := p.parseIntValue()
		payload.N = &n
	}

	payload.Field = p.parseExprSafe()

	if isTop {
		s.Top = payload
	} else {
		s.Rare = payload
	}
}

func (p *parser) parseEveryBody(s *ast.Stage) {
	payload := &ast.EveryPayload{}

	// Duration
	payload.Span = p.parseExprSafe()

	// Optional 'by' clause before 'stats'
	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	// Expect 'stats' keyword
	if p.at(lexer.KwStats) {
		p.advance()
	} else {
		p.errorf(p.cur, CodeStageError, []string{"stats"}, "",
			"expected 'stats' in every stage, got %s", kindName(p.cur.Kind))
	}

	payload.Aggs = p.parseAggList()

	s.Every = payload
}

func (p *parser) parseRateBody(s *ast.Stage) {
	payload := &ast.RatePayload{}

	// Options: per=duration, by=fields
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		if n, ok := p.identLike(); ok {
			if n == "per" {
				p.advance()
				payload.Per = p.parseExprSafe()
				continue
			}
		}
		if p.at(lexer.KwBy) {
			p.advance()
			payload.By = p.parseGroupByList()
			continue
		}
		break
	}

	s.Rate = payload
}

func (p *parser) parseLatencyBody(s *ast.Stage) {
	payload := &ast.LatencyPayload{}
	payload.Field = p.parseExprSafe()

	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		if n, ok := p.identLike(); ok && n == "every" {
			p.advance()
			payload.Every = p.parseExprSafe()
			continue
		}
		if p.at(lexer.KwEvery) {
			p.advance()
			payload.Every = p.parseExprSafe()
			continue
		}
		if p.at(lexer.KwBy) {
			p.advance()
			payload.By = p.parseGroupByList()
			continue
		}
		break
	}

	s.Latency = payload
}

func (p *parser) parsePercentilesBody(s *ast.Stage) {
	payload := &ast.PercentilesPayload{}
	payload.Field = p.parseExprSafe()

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Percentiles = payload
}

func (p *parser) parseProportionBody(s *ast.Stage) {
	payload := &ast.ProportionPayload{}
	payload.Predicate = p.parseExprSafe()

	if p.consume(lexer.KwAs) {
		if n, ok := p.identLike(); ok {
			payload.Alias = n
			payload.AliasSpan = p.curSpan()
			p.advance()
		}
	}

	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		if n, ok := p.identLike(); ok && n == "every" {
			p.advance()
			payload.Every = p.parseExprSafe()
			continue
		}
		if p.at(lexer.KwEvery) {
			p.advance()
			payload.Every = p.parseExprSafe()
			continue
		}
		if p.at(lexer.KwBy) {
			p.advance()
			payload.By = p.parseGroupByList()
			continue
		}
		break
	}

	s.Proportion = payload
}

func (p *parser) parseFacetsBody(s *ast.Stage) {
	payload := &ast.FacetsPayload{}
	payload.Fields = p.parseFieldList()

	// Optional limit=N
	if n, ok := p.identLike(); ok && n == "limit" && p.peekIsEq() {
		p.advance() // consume 'limit'
		p.advance() // consume '='
		v := p.parseIntValue()
		payload.Limit = &v
	}

	s.Facets = payload
}

func (p *parser) parseImpactBody(s *ast.Stage) {
	payload := &ast.ImpactPayload{}

	// Optional agg before 'by'
	if !p.at(lexer.KwBy) {
		aggs := p.parseAggList()
		if len(aggs) > 0 {
			payload.Agg = &aggs[0]
		}
	}

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Impact = payload
}

func (p *parser) parseBaselineBody(s *ast.Stage) {
	payload := &ast.BaselinePayload{}
	payload.Field = p.parseExprSafe()

	// Required window=N
	if n, ok := p.identLike(); ok && n == "window" && p.peekIsEq() {
		p.advance()
		p.advance() // consume =
		payload.Window = p.parseIntValue()
	}

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Baseline = payload
}

func (p *parser) parseChangesBody(s *ast.Stage) {
	payload := &ast.ChangesPayload{}
	payload.Field = p.parseExprSafe()

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Changes = payload
}

func (p *parser) parseExemplarsBody(s *ast.Stage) {
	payload := &ast.ExemplarsPayload{}

	if p.at(lexer.Int) {
		n := p.parseIntValue()
		payload.N = &n
	}

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Exemplars = payload
}

func (p *parser) parseMaterializeBody(s *ast.Stage) {
	payload := &ast.MaterializePayload{}

	if p.at(lexer.String) {
		payload.Name = interpretString(p.cur.Text)
		p.advance()
	} else {
		p.errorf(p.cur, CodeStageError, []string{"string"}, "",
			"expected quoted name for materialize, got %s", kindName(p.cur.Kind))
	}

	// Optional retention=duration
	if n, ok := p.identLike(); ok && n == "retention" && p.peekIsEq() {
		p.advance()
		p.advance() // consume =
		payload.Retention = p.parseExprSafe()
	}

	s.Materialize = payload
}

func (p *parser) parseTeeBody(s *ast.Stage) {
	payload := &ast.TeePayload{}
	if p.at(lexer.String) {
		payload.Sink = interpretString(p.cur.Text)
		p.advance()
	} else {
		p.errorf(p.cur, CodeStageError, []string{"string"}, "",
			"expected quoted sink for tee, got %s", kindName(p.cur.Kind))
	}
	s.Tee = payload
}

func (p *parser) parseUseBody(s *ast.Stage) {
	payload := &ast.UsePayload{}
	// The fragment name may contain special chars like @ and /
	// Read until pipe or EOF.
	start := p.cur.Start
	for p.cur.Kind != lexer.Pipe && p.cur.Kind != lexer.EOF && p.cur.Kind != lexer.Semicolon {
		p.advance()
	}
	payload.Fragment = strings.TrimSpace(p.src[start:p.prev.End])
	s.Use = payload
}

func (p *parser) parseCompareBody(s *ast.Stage) {
	payload := &ast.ComparePayload{}

	// Optional 'previous' keyword
	if n, ok := p.identLike(); ok && n == "previous" {
		payload.Previous = true
		p.advance()
	}

	payload.Shift = p.parseExprSafe()
	s.Compare = payload
}

func (p *parser) parseTransactionBody(s *ast.Stage) {
	payload := &ast.TransactionPayload{}

	// Parse field list first (until we hit an option)
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		// Check for option keywords
		if n, ok := p.identLike(); ok {
			if (n == "maxspan" || n == "startswith" || n == "endswith") && p.peekIsEq() {
				break
			}
		}
		payload.Fields = append(payload.Fields, p.parseExprSafe())
		if !p.consume(lexer.Comma) {
			break
		}
	}

	// Parse options
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		if n, ok := p.identLike(); ok && p.peekIsEq() {
			switch n {
			case "maxspan":
				p.advance()
				p.advance() // consume =
				payload.MaxSpan = p.parseExprSafe()
				continue
			case "startswith":
				p.advance()
				p.advance()
				payload.StartsWith = p.parseExprSafe()
				continue
			case "endswith":
				p.advance()
				p.advance()
				payload.EndsWith = p.parseExprSafe()
				continue
			}
		}
		break
	}

	s.Transaction = payload
}

func (p *parser) parseCorrelateBody(s *ast.Stage) {
	payload := &ast.CorrelatePayload{}
	payload.Field1 = p.parseExprSafe()
	payload.Field2 = p.parseExprSafe()

	// Optional method=pearson|spearman
	if n, ok := p.identLike(); ok && n == "method" && p.peekIsEq() {
		p.advance()
		p.advance() // consume =
		if v, ok2 := p.identLike(); ok2 {
			payload.Method = v
			p.advance()
		}
	}

	s.Correlate = payload
}

func (p *parser) parseRollupBody(s *ast.Stage) {
	payload := &ast.RollupPayload{}

	// Parse duration list
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) || p.at(lexer.KwBy) {
			break
		}
		payload.Resolutions = append(payload.Resolutions, p.parseExprSafe())
		if !p.consume(lexer.Comma) {
			break
		}
	}

	if p.consume(lexer.KwBy) {
		payload.By = p.parseGroupByList()
	}

	s.Rollup = payload
}

func (p *parser) parseXYSeriesBody(s *ast.Stage) {
	payload := &ast.XYSeriesPayload{}
	payload.X = p.parseExprSafe()
	payload.Y = p.parseExprSafe()
	payload.Value = p.parseExprSafe()
	s.Xyseries = payload
}

func (p *parser) parseGenericOptionsBody(s *ast.Stage) {
	payload := &ast.GenericOptionsPayload{}

	// Parse positionals and options generically from the registry.
	op, found := registry.LookupOperator(s.Name)

	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}

		// Check if this looks like option=value.
		if n, ok := p.identLike(); ok && p.peekIsEq() {
			// Validate against registry.
			if found {
				valid := false
				for _, opt := range op.Options {
					if opt.Name == n {
						valid = true
						break
					}
				}
				if !valid {
					suggestion := didYouMeanOption(n, op.Options)
					msg := fmt.Sprintf("unknown option %q for stage %q", n, s.Name)
					if suggestion != "" {
						msg += fmt.Sprintf(", did you mean %q?", suggestion)
					}
					p.diags = append(p.diags, Diag{
						Code:       CodeStageError,
						Message:    msg,
						Span:       p.curSpan(),
						Suggestion: suggestion,
					})
				}
			}
			nameSpan := p.curSpan()
			p.advance() // consume name
			p.advance() // consume =
			val := p.parseExprSafe()
			payload.Options = append(payload.Options, ast.Option{
				Name:     n,
				NameSpan: nameSpan,
				Value:    val,
				ValuePos: val.ExprSpan(),
			})
			continue
		}

		// Positional.
		payload.Positionals = append(payload.Positionals, p.parseExprSafe())
	}

	switch s.Name {
	case "patterns":
		s.Patterns = payload
	case "outliers":
		s.Outliers = payload
	case "sessionize":
		s.Sessionize = payload
	case "trace":
		s.Trace = payload
	case "topology":
		s.Topology = payload
	default:
		s.Generic = payload
	}
}

// ---------------------------------------------------------------------------
// Aggregate list parsing (stats, eventstats, streamstats, every, impact, etc.)
// ---------------------------------------------------------------------------

func (p *parser) parseAggList() []ast.AggExpr {
	var aggs []ast.AggExpr
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) ||
			p.at(lexer.RBracket) || p.at(lexer.KwBy) {
			break
		}
		agg := p.parseAggExpr()
		aggs = append(aggs, agg)
		if !p.consume(lexer.Comma) {
			break
		}
	}
	return aggs
}

func (p *parser) parseAggExpr() ast.AggExpr {
	start := p.cur.Start
	agg := ast.AggExpr{Pos: ast.Span{Start: start}}

	// D28: check for count without parens.
	if n, ok := p.identLike(); ok && strings.ToLower(n) == "count" && !p.nextIsLParen() {
		p.diags = append(p.diags, Diag{
			Code:       CodeCountNoParens,
			Message:    "count requires parentheses: count()",
			Span:       p.curSpan(),
			Suggestion: "count()",
		})
		// Recover by treating it as count().
		nameSpan := p.curSpan()
		p.advance()
		agg.Func = &ast.Call{Callee: "count", Pos: nameSpan}
	} else {
		// Parse the expression — this should be a call (e.g., count(), avg(dur)).
		expr := p.parseExprSafe()

		// Check for where clause inside the call.
		if call, ok2 := expr.(*ast.Call); ok2 {
			// Look for a 'where' argument: the last arg may be a where predicate.
			// We need to check if any argument was parsed as an ident "where"
			// followed by an expression. The expression parser doesn't know about
			// aggregate where clauses, so we handle it here by re-examining the args.
			agg.Func = p.extractWhereFromCall(call, &agg)
		} else {
			agg.Func = expr
		}
	}

	// Optional 'as' alias.
	if p.consume(lexer.KwAs) {
		if n, ok := p.identLike(); ok {
			agg.Alias = n
			agg.AliasSpan = p.curSpan()
			p.advance()
		}
	}

	agg.Pos.End = p.prev.End
	return agg
}

// extractWhereFromCall looks for a 'where' keyword argument in a call's arg list.
// In the aggregate context, count(where p) and sum(x, where p) are valid.
// The expression parser may have parsed 'where' as an Ident and the predicate
// as the next expression, or as a Binary(where, ==, p) which we need to
// re-interpret.
//
// Actually, since 'where' is a reserved keyword, the lexer emits KwWhere.
// The expression parser sees it in primary position and will produce an
// Ident{Name: "where"} because it's a soft keyword. Then the next token
// would be an expression. But our parseExpr is called for the whole call,
// so the args list would have 'where' as an ident and the predicate as a
// separate expression after a comma? No — the comma separates args.
//
// Let's look more carefully: count(where status >= 500).
// The lexer sees: count ( where status >= 500 )
// The expr parser calls parseCallArgsNode which calls parseArgList.
// parseArgList calls parseArgExpr -> parseExpr for each arg.
// The first arg is: where status >= 500.
// 'where' is KwWhere, which is a soft keyword, so identLike returns "where".
// parsePrimary produces Ident{Name: "where"}.
// Then parsePostfix sees nothing (no . or [ or ().
// Then parseComparison: 'where' is an Ident, no comparison op after it...
// Wait, 'status' would be the next thing. So we'd have Ident{where},
// then the parser returns from parseExpr, then the arg list sees no comma,
// so args = [Ident{where}], but that's wrong.
//
// The real issue is that 'where' in agg calls is a syntactic construct.
// We need to handle it specially during agg parsing, not during expression
// parsing. Let me re-examine.
//
// For Phase 2c, the practical approach: After parsing the Call, check if
// any arg is an Ident with Name="where". If so, the rest is the predicate.
// But that won't work because parseExpr stops at the first arg.
//
// Better approach: before parsing the call expression, peek ahead. If the
// function is a known aggregate and the next token after ( is 'where',
// handle it specially. Let me restructure.
func (p *parser) extractWhereFromCall(call *ast.Call, agg *ast.AggExpr) ast.Expr {
	// Look for a synthetic Call{callee: "where"} wrapper in the args list.
	// This is produced by parseArgExpr when it encounters 'where' keyword
	// in an aggregate call argument position (RFC-002 §4.2).
	newArgs := make([]ast.Expr, 0, len(call.Args))
	for _, arg := range call.Args {
		if wc, ok := arg.(*ast.Call); ok && wc.Callee == "where" && len(wc.Args) == 1 {
			// This is a where-clause marker. Extract the predicate.
			agg.WhereCond = wc.Args[0]
			continue
		}
		newArgs = append(newArgs, arg)
	}
	call.Args = newArgs
	return call
}

// nextIsLParen checks if the next token after the current is (.
func (p *parser) nextIsLParen() bool {
	pos := p.cur.End
	for pos < len(p.src) && (p.src[pos] == ' ' || p.src[pos] == '\t' || p.src[pos] == '\n' || p.src[pos] == '\r') {
		pos++
	}
	return pos < len(p.src) && p.src[pos] == '('
}

// ---------------------------------------------------------------------------
// Assignment list parsing (extend)
// ---------------------------------------------------------------------------

func (p *parser) parseAssignList() []ast.Assignment {
	var assigns []ast.Assignment
	for {
		a := p.parseAssignment()
		assigns = append(assigns, a)
		if !p.consume(lexer.Comma) {
			break
		}
	}
	return assigns
}

func (p *parser) parseAssignment() ast.Assignment {
	start := p.cur.Start
	name := ""
	nameSpan := p.curSpan()

	if n, ok := p.identLike(); ok {
		name = n
		nameSpan = p.curSpan()
		p.advance()
	} else {
		p.errorf(p.cur, CodeStageError, []string{"identifier"}, "",
			"expected field name in assignment, got %s", kindName(p.cur.Kind))
	}

	if !p.consume(lexer.Eq) {
		p.errorf(p.cur, CodeStageError, []string{"="}, "",
			"expected '=' in assignment, got %s", kindName(p.cur.Kind))
	}

	value := p.parseExprSafe()

	return ast.Assignment{
		Name:     name,
		NameSpan: nameSpan,
		Value:    value,
		Pos:      ast.Span{Start: start, End: p.prev.End},
	}
}

// ---------------------------------------------------------------------------
// Field list parsing
// ---------------------------------------------------------------------------

func (p *parser) parseFieldList() []ast.Expr {
	var fields []ast.Expr
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) ||
			p.at(lexer.RBracket) || p.at(lexer.KwBy) || p.at(lexer.KwWith) {
			break
		}
		fields = append(fields, p.parseExprSafe())
		if !p.consume(lexer.Comma) {
			break
		}
	}
	return fields
}

func (p *parser) parseGroupByList() []ast.Expr {
	var keys []ast.Expr
	for {
		if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
			break
		}
		keys = append(keys, p.parseExprSafe())
		if !p.consume(lexer.Comma) {
			break
		}
	}
	return keys
}

// ---------------------------------------------------------------------------
// Value helpers
// ---------------------------------------------------------------------------

func (p *parser) parseIntValue() int64 {
	if p.at(lexer.Int) {
		v, _ := parseInt(p.cur.Text)
		p.advance()
		return v
	}
	p.errorf(p.cur, CodeStageError, []string{"integer"}, "",
		"expected integer, got %s", kindName(p.cur.Kind))
	return 0
}

func (p *parser) parseBoolValue() bool {
	if p.at(lexer.True) {
		p.advance()
		return true
	}
	if p.at(lexer.False) {
		p.advance()
		return false
	}
	if n, ok := p.identLike(); ok {
		p.advance()
		return n == "true"
	}
	p.errorf(p.cur, CodeStageError, []string{"true or false"}, "",
		"expected boolean, got %s", kindName(p.cur.Kind))
	return false
}

// peekIsEq checks if the token after the current one is = (assignment).
func (p *parser) peekIsEq() bool {
	pos := p.cur.End
	for pos < len(p.src) && (p.src[pos] == ' ' || p.src[pos] == '\t' || p.src[pos] == '\n' || p.src[pos] == '\r') {
		pos++
	}
	return pos < len(p.src) && p.src[pos] == '=' && (pos+1 >= len(p.src) || p.src[pos+1] != '=')
}

// parseExprSafe parses an expression, catching stage-boundary tokens.
func (p *parser) parseExprSafe() ast.Expr {
	if p.at(lexer.Pipe) || p.at(lexer.EOF) || p.at(lexer.Semicolon) || p.at(lexer.RBracket) {
		span := p.curSpan()
		return &ast.ErrorExpr{Message: "expected expression", Pos: span}
	}
	return p.parseExpr()
}

// ---------------------------------------------------------------------------
// Stage resolution
// ---------------------------------------------------------------------------

func (p *parser) resolveStage() (string, bool) {
	if p.cur.Kind == lexer.KwFrom {
		return "from", true
	}
	// Check if current token is a stage keyword.
	name := ""
	switch {
	case p.cur.Kind.IsKeyword() && !isHardKeyword(p.cur.Kind):
		name = strings.ToLower(p.cur.Text)
		// Make sure it's actually a known operator.
		if _, ok := registry.LookupOperator(name); ok {
			return name, true
		}
		// Also check the structural keywords that are NOT stage names.
		switch p.cur.Kind {
		case lexer.KwAs, lexer.KwBy, lexer.KwWith, lexer.KwOn, lexer.KwExcept, lexer.KwLet:
			return "", false
		}
		return name, true // keyword but not in registry — we'll handle it
	case p.cur.Kind == lexer.Ident:
		name = strings.ToLower(p.cur.Text)
		if _, ok := registry.LookupOperator(name); ok {
			return name, true
		}
	}
	return "", false
}

// skipToNextStage skips tokens until the next | or EOF or ; or ].
func (p *parser) skipToNextStage() {
	for p.cur.Kind != lexer.Pipe && p.cur.Kind != lexer.EOF &&
		p.cur.Kind != lexer.Semicolon && p.cur.Kind != lexer.RBracket {
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// Killed spelling detection (RFC-001 -> v2 migration fix-its)
// ---------------------------------------------------------------------------

type killedFix struct {
	message    string
	suggestion string
}

var killedSpellings = map[string]killedFix{
	"eval":          {message: "eval is renamed to extend in v2", suggestion: "extend"},
	"fields":        {message: "fields is replaced by keep/drop in v2", suggestion: "keep or drop"},
	"table":         {message: "table is replaced by keep in v2", suggestion: "keep"},
	"timechart":     {message: "timechart is replaced by every in v2", suggestion: "every <dur> stats ..."},
	"search":        {message: "search is not a pipeline stage in v2; use where has(...) for text search", suggestion: "where has(...)"},
	"rex":           {message: "rex is replaced by parse regex in v2", suggestion: "parse regex r\"...\""},
	"fillnull":      {message: "fillnull is replaced by extend with ?? in v2", suggestion: "extend field = field ?? default"},
	"take":          {message: "take is replaced by head in v2", suggestion: "head"},
	"limit":         {message: "limit is replaced by head in v2", suggestion: "head"},
	"order":         {message: "order is replaced by sort in v2", suggestion: "sort"},
	"omit":          {message: "omit is replaced by drop in v2", suggestion: "drop"},
	"enrich":        {message: "enrich is replaced by eventstats in v2", suggestion: "eventstats"},
	"running":       {message: "running is replaced by streamstats in v2", suggestion: "streamstats"},
	"glimpse":       {message: "glimpse is replaced by describe in v2", suggestion: "describe"},
	"select":        {message: "select is replaced by keep in v2", suggestion: "keep"},
	"filter":        {message: "filter is replaced by where in v2", suggestion: "where"},
	"append":        {message: "append is replaced by union in v2", suggestion: "union"},
	"multisearch":   {message: "multisearch is replaced by union in v2", suggestion: "union"},
	"slowest":       {message: "slowest is replaced by stats + sort + head in v2", suggestion: "stats max(field) as m by key | sort -m | head N"},
	"topby":         {message: "topby is replaced by stats + sort + head in v2", suggestion: "stats ... | sort | head"},
	"bottomby":      {message: "bottomby is replaced by stats + sort + head in v2", suggestion: "stats ... | sort | head"},
	"group":         {message: "group is replaced by stats ... by in v2", suggestion: "stats ... by"},
	"unpack_json":   {message: "unpack_json is replaced by parse json in v2", suggestion: "parse json"},
	"unpack_logfmt": {message: "unpack_logfmt is replaced by parse logfmt in v2", suggestion: "parse logfmt"},
	"unpack_kv":     {message: "unpack_kv is replaced by parse kv in v2", suggestion: "parse kv"},
}

func (p *parser) checkKilledSpelling() (killedFix, bool) {
	name := strings.ToLower(p.cur.Text)
	fix, ok := killedSpellings[name]
	return fix, ok
}

// ---------------------------------------------------------------------------
// Edit distance and did-you-mean
// ---------------------------------------------------------------------------

func registryStageNames() []string {
	ops := registry.Operators()
	names := make([]string, len(ops))
	for i, op := range ops {
		names[i] = op.Name
	}
	return names
}

// didYouMean returns the closest name from candidates using edit distance,
// or "" if nothing is close enough.
func didYouMean(input string, candidates []string) string {
	input = strings.ToLower(input)
	bestDist := len(input)/2 + 1 // threshold
	bestName := ""
	for _, c := range candidates {
		d := editDistance(input, c)
		if d < bestDist {
			bestDist = d
			bestName = c
		}
	}
	return bestName
}

// didYouMeanOption returns the closest option name.
func didYouMeanOption(input string, options []registry.Option) string {
	candidates := make([]string, len(options))
	for i, o := range options {
		candidates[i] = o.Name
	}
	return didYouMean(input, candidates)
}

// editDistance computes the Levenshtein distance between two strings.
func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}

	// Use single-row DP.
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}

	for i := 1; i <= la; i++ {
		curr := make([]int, lb+1)
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = curr
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
