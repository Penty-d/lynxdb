package planner

import (
	"strings"
	"unicode"

	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// DynamicTimeBounds reports whether REST-style from/to bounds depend on now.
func DynamicTimeBounds(from, to string) bool {
	return IsDynamicTimeValue(from) || IsDynamicTimeValue(to)
}

// IsDynamicTimeValue reports whether a single time literal is relative to now.
func IsDynamicTimeValue(value string) bool {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if lower == "now" || lower == "now()" {
		return true
	}
	if strings.HasPrefix(lower, "@") {
		return true
	}
	if lower[0] != '-' && lower[0] != '+' {
		return false
	}

	return isRelativeDuration(lower[1:])
}

// QueryUsesDynamicTimeSyntax reports whether query text contains time syntax
// whose value changes as wall-clock time advances.
func QueryUsesDynamicTimeSyntax(query string) bool {
	return hasDynamicSourceRange(query) || hasDynamicTimePredicate(query) || hasNowFunction(query)
}

func programUsesDynamicTime(prog *spl2.Program) bool {
	if prog == nil {
		return false
	}
	for _, ds := range prog.Datasets {
		if queryUsesDynamicTime(ds.Query) {
			return true
		}
	}

	return queryUsesDynamicTime(prog.Main)
}

func queryUsesDynamicTime(q *spl2.Query) bool {
	if q == nil {
		return false
	}
	if q.Source != nil && q.Source.TimeRange != nil {
		return true
	}
	for _, cmd := range q.Commands {
		switch c := cmd.(type) {
		case *spl2.WhereCommand:
			if exprUsesDynamicTime(c.Expr, true) {
				return true
			}
		case *spl2.EvalCommand:
			if exprUsesDynamicTime(c.Expr, false) {
				return true
			}
			for _, assign := range c.Assignments {
				if exprUsesDynamicTime(assign.Expr, false) {
					return true
				}
			}
		case *spl2.FieldformatCommand:
			if exprUsesDynamicTime(c.Expr, false) {
				return true
			}
		}
	}

	return false
}

func exprUsesDynamicTime(expr spl2.Expr, requireTimeField bool) bool {
	switch e := expr.(type) {
	case *spl2.CompareExpr:
		leftTime := isTimeField(e.Left)
		rightTime := isTimeField(e.Right)
		if leftTime && exprUsesDynamicValue(e.Right) {
			return true
		}
		if rightTime && exprUsesDynamicValue(e.Left) {
			return true
		}
		return !requireTimeField && (exprUsesDynamicValue(e.Left) || exprUsesDynamicValue(e.Right))
	case *spl2.BinaryExpr:
		return exprUsesDynamicTime(e.Left, requireTimeField) || exprUsesDynamicTime(e.Right, requireTimeField)
	case *spl2.NotExpr:
		return exprUsesDynamicTime(e.Expr, requireTimeField)
	case *spl2.InExpr:
		if requireTimeField && !isTimeField(e.Field) {
			return false
		}
		if exprUsesDynamicValue(e.Field) {
			return true
		}
		for _, value := range e.Values {
			if exprUsesDynamicValue(value) {
				return true
			}
		}
	case *spl2.ArithExpr:
		return !requireTimeField && exprUsesDynamicValue(e)
	case *spl2.FuncCallExpr:
		return !requireTimeField && exprUsesDynamicValue(e)
	}

	return false
}

func exprUsesDynamicValue(expr spl2.Expr) bool {
	switch e := expr.(type) {
	case *spl2.LiteralExpr:
		return IsDynamicTimeValue(e.Value)
	case *spl2.FieldExpr:
		return strings.EqualFold(e.Name, "now")
	case *spl2.FuncCallExpr:
		if strings.EqualFold(e.Name, "now") {
			return true
		}
		for _, arg := range e.Args {
			if exprUsesDynamicValue(arg) {
				return true
			}
		}
	case *spl2.ArithExpr:
		return exprUsesDynamicValue(e.Left) || exprUsesDynamicValue(e.Right)
	case *spl2.BinaryExpr:
		return exprUsesDynamicValue(e.Left) || exprUsesDynamicValue(e.Right)
	case *spl2.NotExpr:
		return exprUsesDynamicValue(e.Expr)
	case *spl2.CompareExpr:
		return exprUsesDynamicValue(e.Left) || exprUsesDynamicValue(e.Right)
	}

	return false
}

func isTimeField(expr spl2.Expr) bool {
	field, ok := expr.(*spl2.FieldExpr)
	return ok && field.Name == "_time"
}

func hasDynamicSourceRange(query string) bool {
	for i := 0; i < len(query); i++ {
		if query[i] != '[' {
			continue
		}
		end := strings.IndexByte(query[i+1:], ']')
		if end < 0 {
			return false
		}
		content := query[i+1 : i+1+end]
		for _, part := range strings.Split(content, "..") {
			if IsDynamicTimeValue(part) {
				return true
			}
		}
		i += end + 1
	}

	return false
}

func hasDynamicTimePredicate(query string) bool {
	lower := strings.ToLower(query)
	for i := 0; i <= len(lower)-len("_time"); i++ {
		if lower[i:i+len("_time")] != "_time" || !isStandaloneIdent(lower, i, i+len("_time")) {
			continue
		}
		pos := skipSpace(lower, i+len("_time"))
		if strings.HasPrefix(lower[pos:], "between") && isWordBoundary(lower, pos+len("between")) {
			segment := untilPipe(lower[pos+len("between"):])
			if containsDynamicTimeValue(segment) {
				return true
			}
			continue
		}
		if _, afterOp := readCompareOperator(lower, pos); afterOp > pos {
			segment := untilPipe(lower[afterOp:])
			if containsDynamicTimeValue(segment) {
				return true
			}
		}
	}

	return false
}

func hasNowFunction(query string) bool {
	lower := strings.ToLower(query)
	for offset := 0; ; {
		idx := strings.Index(lower[offset:], "now")
		if idx < 0 {
			return false
		}
		idx += offset
		if !isStandaloneIdent(lower, idx, idx+len("now")) {
			offset = idx + 1
			continue
		}
		pos := skipSpace(lower, idx+len("now"))
		if pos < len(lower) && lower[pos] == '(' {
			return true
		}
		offset = idx + 1
	}
}

func containsDynamicTimeValue(s string) bool {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ')' || r == ',' || r == '"' || r == '\''
	})
	for _, field := range fields {
		if IsDynamicTimeValue(field) {
			return true
		}
	}

	return false
}

func isRelativeDuration(value string) bool {
	if value == "" {
		return false
	}
	if idx := strings.IndexByte(value, '@'); idx >= 0 {
		if !isSnapUnit(value[idx+1:]) {
			return false
		}
		value = value[:idx]
	}
	if len(value) < 2 {
		return false
	}
	unit := value[len(value)-1]
	if unit != 's' && unit != 'm' && unit != 'h' && unit != 'd' && unit != 'w' {
		return false
	}
	for _, ch := range value[:len(value)-1] {
		if ch < '0' || ch > '9' {
			return false
		}
	}

	return true
}

func isSnapUnit(value string) bool {
	switch value {
	case "s", "m", "h", "d", "w":
		return true
	default:
		return false
	}
}

func skipSpace(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}

	return pos
}

func readCompareOperator(s string, pos int) (string, int) {
	if pos >= len(s) {
		return "", pos
	}
	switch s[pos] {
	case '=', '<', '>':
		if pos+1 < len(s) && s[pos+1] == '=' {
			return s[pos : pos+2], pos + 2
		}
		return s[pos : pos+1], pos + 1
	case '!':
		if pos+1 < len(s) && s[pos+1] == '=' {
			return "!=", pos + 2
		}
	}

	return "", pos
}

func untilPipe(s string) string {
	if idx := strings.IndexByte(s, '|'); idx >= 0 {
		return s[:idx]
	}

	return s
}

func isStandaloneIdent(s string, start, end int) bool {
	return (start == 0 || !isIdentChar(s[start-1])) && (end >= len(s) || !isIdentChar(s[end]))
}

func isWordBoundary(s string, pos int) bool {
	return pos >= len(s) || !isIdentChar(s[pos])
}

func isIdentChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_'
}
