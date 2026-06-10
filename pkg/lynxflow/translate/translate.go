// Package translate provides a restricted SPL2-to-LynxFlow v2 translator
// for materialized view and saved query migration.
//
// The translator supports the restricted MV/saved-query grammar:
// FROM/index= prefix, search terms, where, eval, stats/eventstats,
// bin/timechart, fields/table, rename, sort, head/tail, dedup, top/rare,
// fillnull, and the materialize management command.
//
// Any unsupported command or construct returns an error listing the
// unsupported element. The translator is best-effort and never silently
// wrong: every translation is validated by parsing the output with the
// LynxFlow parser.
//
// Notes carry semantic-change warnings (e.g., substr 1->0 based indexing,
// integer division behavior change).
package translate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// Note describes a semantic difference between the SPL2 and LynxFlow forms
// of a translated query. Notes are informational — the translation is still
// valid, but the user should review the noted differences.
type Note struct {
	// Code is a machine-readable identifier (e.g., "substr-reindex",
	// "int-division", "case-sensitivity").
	Code string
	// Message is a human-readable description of the semantic change.
	Message string
}

// SPL2ToLynxFlow translates a restricted SPL2 query to LynxFlow v2.
//
// Returns the translated query string, any semantic notes, and an error if
// the query contains unsupported constructs or if the output fails to parse
// as valid LynxFlow.
func SPL2ToLynxFlow(q string) (string, []Note, error) {
	normalized := spl2.NormalizeQuery(q)
	prog, err := spl2.ParseProgram(normalized)
	if err != nil {
		return "", nil, fmt.Errorf("translate.SPL2ToLynxFlow: spl2 parse: %w", err)
	}

	t := &translator{notes: nil}

	result, err := t.translateProgram(prog)
	if err != nil {
		return "", nil, fmt.Errorf("translate.SPL2ToLynxFlow: %w", err)
	}

	// Validate: the output must parse as valid LynxFlow with zero error diagnostics.
	_, diags := parser.Parse(result)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return "", nil, fmt.Errorf("translate.SPL2ToLynxFlow: output failed LynxFlow validation: %s (generated: %s)", d.Message, result)
		}
	}

	return result, t.notes, nil
}

type translator struct {
	notes []Note
}

func (t *translator) addNote(code, message string) {
	t.notes = append(t.notes, Note{Code: code, Message: message})
}

func (t *translator) translateProgram(prog *spl2.Program) (string, error) {
	var parts []string

	// CTEs
	for _, ds := range prog.Datasets {
		body, err := t.translateQuery(ds.Query)
		if err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("let $%s = %s;", ds.Name, body))
	}

	if prog.Main == nil {
		return "", fmt.Errorf("empty program")
	}

	main, err := t.translateQuery(prog.Main)
	if err != nil {
		return "", err
	}
	parts = append(parts, main)

	return strings.Join(parts, "\n"), nil
}

func (t *translator) translateQuery(q *spl2.Query) (string, error) {
	var stages []string

	// FROM clause
	if q.Source != nil {
		from := "from "
		if q.Source.IsVariable {
			from += "$" + q.Source.Index
		} else if q.Source.Index != "" {
			from += q.Source.Index
		} else {
			from += "main"
		}
		if q.Source.TimeRange != nil {
			from += "[" + q.Source.TimeRange.Relative
			if q.Source.TimeRange.End != "" {
				from += ".." + q.Source.TimeRange.End
			}
			from += "]"
			if q.Source.TimeRange.SnapTo != "" {
				from += "[@" + q.Source.TimeRange.SnapTo + "]"
			}
		}
		stages = append(stages, from)
	}

	// Pipeline commands
	for _, cmd := range q.Commands {
		translated, err := t.translateCommand(cmd)
		if err != nil {
			return "", err
		}
		stages = append(stages, translated)
	}

	if len(stages) == 0 {
		return "from main", nil
	}

	// Join with " | " for single-line, or "\n| " for multi-line.
	if len(stages) == 1 {
		return stages[0], nil
	}
	return stages[0] + "\n" + joinPipeStages(stages[1:]), nil
}

func joinPipeStages(stages []string) string {
	var b strings.Builder
	for i, s := range stages {
		b.WriteString("| ")
		b.WriteString(s)
		if i < len(stages)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func (t *translator) translateCommand(cmd spl2.Command) (string, error) {
	switch c := cmd.(type) {
	case *spl2.SearchCommand:
		return t.translateSearch(c)
	case *spl2.WhereCommand:
		return t.translateWhere(c)
	case *spl2.EvalCommand:
		return t.translateEval(c)
	case *spl2.StatsCommand:
		return t.translateStats(c)
	case *spl2.EventstatsCommand:
		return t.translateEventstats(c)
	case *spl2.SortCommand:
		return t.translateSort(c)
	case *spl2.HeadCommand:
		return fmt.Sprintf("head %d", c.Count), nil
	case *spl2.TailCommand:
		return fmt.Sprintf("tail %d", c.Count), nil
	case *spl2.FieldsCommand:
		return t.translateFields(c)
	case *spl2.TableCommand:
		return t.translateTable(c)
	case *spl2.RenameCommand:
		return t.translateRename(c)
	case *spl2.DedupCommand:
		return t.translateDedup(c)
	case *spl2.TopCommand:
		return t.translateTop(c)
	case *spl2.RareCommand:
		return t.translateRare(c)
	case *spl2.BinCommand:
		return t.translateBin(c)
	case *spl2.TimechartCommand:
		return t.translateTimechart(c)
	case *spl2.FillnullCommand:
		return t.translateFillnull(c)
	case *spl2.MaterializeCommand:
		return t.translateMaterialize(c)
	case *spl2.FromCommand:
		return fmt.Sprintf("from %s", c.ViewName), nil

	// Unsupported commands — clear errors
	case *spl2.JoinCommand:
		return "", fmt.Errorf("unsupported command: join (cross-source correlation is not supported by the restricted translator)")
	case *spl2.MultisearchCommand:
		return "", fmt.Errorf("unsupported command: multisearch (use union in LynxFlow)")
	case *spl2.AppendCommand:
		return "", fmt.Errorf("unsupported command: append (use union in LynxFlow)")
	case *spl2.RexCommand:
		return "", fmt.Errorf("unsupported command: rex (use 'parse regex' in LynxFlow)")
	case *spl2.StreamstatsCommand:
		return "", fmt.Errorf("unsupported command: streamstats (order-dependent running aggregate)")
	case *spl2.TransactionCommand:
		return "", fmt.Errorf("unsupported command: transaction (cross-event session state)")
	case *spl2.RegexCommand:
		return "", fmt.Errorf("unsupported command: regex (use 'where matches(...)' in LynxFlow)")
	case *spl2.ReverseCommand:
		return "", fmt.Errorf("unsupported command: reverse (use 'sort' in LynxFlow)")
	case *spl2.ChartCommand:
		return "", fmt.Errorf("unsupported command: chart (use 'stats' + 'xyseries' in LynxFlow)")
	case *spl2.XYSeriesCommand:
		return t.translateXYSeries(c)
	case *spl2.GlimpseCommand:
		return "describe", nil
	case *spl2.DescribeCommand:
		return "describe", nil
	default:
		return "", fmt.Errorf("unsupported command: %T", cmd)
	}
}

// ---- Command translators ----

func (t *translator) translateSearch(c *spl2.SearchCommand) (string, error) {
	if c.Expression != nil {
		expr, err := t.translateSearchExpr(c.Expression)
		if err != nil {
			return "", err
		}
		return "where " + expr, nil
	}
	if c.Term != "" {
		// Single-token term -> has(_raw, "term")
		return fmt.Sprintf("where has(_raw, %s)", quoteStr(c.Term)), nil
	}
	return "where true", nil
}

func (t *translator) translateSearchExpr(se spl2.SearchExpr) (string, error) {
	switch s := se.(type) {
	case *spl2.SearchKeywordExpr:
		// Bare word or quoted phrase searching _raw
		if strings.Contains(s.Value, " ") || strings.Contains(s.Value, "\t") {
			// Multi-word phrase -> contains
			return fmt.Sprintf("contains(_raw, %s)", quoteStr(s.Value)), nil
		}
		// Single token -> has
		return fmt.Sprintf("has(_raw, %s)", quoteStr(s.Value)), nil
	case *spl2.SearchCompareExpr:
		field := s.Field
		value := s.Value

		// Wildcard patterns
		if s.HasWildcard {
			return t.translateSearchWildcard(field, value)
		}

		// Existence test: field=*
		if value == "*" {
			return fmt.Sprintf("exists(%s)", field), nil
		}

		// Normal comparison
		op := translateSearchCompareOp(s.Op)
		return fmt.Sprintf("%s %s %s", field, op, translateLiteralValue(value)), nil
	case *spl2.SearchInExpr:
		var vals []string
		for _, v := range s.Values {
			vals = append(vals, translateLiteralValue(v.Value))
		}
		return fmt.Sprintf("%s in [%s]", s.Field, strings.Join(vals, ", ")), nil
	case *spl2.SearchAndExpr:
		left, err := t.translateSearchExpr(s.Left)
		if err != nil {
			return "", err
		}
		right, err := t.translateSearchExpr(s.Right)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s and %s", left, right), nil
	case *spl2.SearchOrExpr:
		left, err := t.translateSearchExpr(s.Left)
		if err != nil {
			return "", err
		}
		right, err := t.translateSearchExpr(s.Right)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(%s or %s)", left, right), nil
	case *spl2.SearchNotExpr:
		inner, err := t.translateSearchExpr(s.Operand)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("not %s", inner), nil
	default:
		return "", fmt.Errorf("unsupported search expression type: %T", se)
	}
}

func translateSearchCompareOp(op spl2.CompareOp) string {
	switch op {
	case spl2.OpEq:
		return "=="
	case spl2.OpNotEq:
		return "!="
	case spl2.OpLt:
		return "<"
	case spl2.OpLte:
		return "<="
	case spl2.OpGt:
		return ">"
	case spl2.OpGte:
		return ">="
	case spl2.OpLike:
		// Should not reach here; LIKE is handled separately
		return "=="
	default:
		return "=="
	}
}

func (t *translator) translateSearchWildcard(field, value string) (string, error) {
	// Single wildcard: field=* -> exists(field)
	if value == "*" {
		return fmt.Sprintf("exists(%s)", field), nil
	}

	// *value* -> contains
	// value* -> starts_with
	// *value -> ends_with
	if len(value) >= 3 && strings.HasPrefix(value, "*") && strings.HasSuffix(value, "*") {
		inner := value[1 : len(value)-1]
		if !strings.Contains(inner, "*") && !strings.Contains(inner, "?") {
			return fmt.Sprintf("contains(%s, %s)", field, quoteStr(inner)), nil
		}
	}
	if strings.HasSuffix(value, "*") && len(value) > 1 {
		prefix := value[:len(value)-1]
		if !strings.Contains(prefix, "*") && !strings.Contains(prefix, "?") {
			return fmt.Sprintf("starts_with(%s, %s)", field, quoteStr(prefix)), nil
		}
	}
	if strings.HasPrefix(value, "*") && len(value) > 1 {
		suffix := value[1:]
		if !strings.Contains(suffix, "*") && !strings.Contains(suffix, "?") {
			return fmt.Sprintf("ends_with(%s, %s)", field, quoteStr(suffix)), nil
		}
	}
	// Fall back to glob
	return fmt.Sprintf("glob(%s, %s)", field, quoteStr(value)), nil
}

func (t *translator) translateWhere(c *spl2.WhereCommand) (string, error) {
	expr, err := t.translateExpr(c.Expr)
	if err != nil {
		return "", err
	}
	return "where " + expr, nil
}

func (t *translator) translateEval(c *spl2.EvalCommand) (string, error) {
	var assigns []string
	// Multi-assignment eval
	if len(c.Assignments) > 0 {
		for _, a := range c.Assignments {
			expr, err := t.translateExpr(a.Expr)
			if err != nil {
				return "", err
			}
			assigns = append(assigns, fmt.Sprintf("%s = %s", a.Field, expr))
		}
	} else if c.Field != "" && c.Expr != nil {
		// Single-assignment eval
		expr, err := t.translateExpr(c.Expr)
		if err != nil {
			return "", err
		}
		assigns = append(assigns, fmt.Sprintf("%s = %s", c.Field, expr))
	}
	return "extend " + strings.Join(assigns, ", "), nil
}

func (t *translator) translateStats(c *spl2.StatsCommand) (string, error) {
	aggs, err := t.translateAggExprs(c.Aggregations)
	if err != nil {
		return "", err
	}
	result := "stats " + strings.Join(aggs, ", ")
	if len(c.GroupBy) > 0 {
		result += " by " + strings.Join(c.GroupBy, ", ")
	}
	return result, nil
}

func (t *translator) translateEventstats(c *spl2.EventstatsCommand) (string, error) {
	aggs, err := t.translateAggExprs(c.Aggregations)
	if err != nil {
		return "", err
	}
	result := "eventstats " + strings.Join(aggs, ", ")
	if len(c.GroupBy) > 0 {
		result += " by " + strings.Join(c.GroupBy, ", ")
	}
	return result, nil
}

func (t *translator) translateSort(c *spl2.SortCommand) (string, error) {
	var fields []string
	for _, sf := range c.Fields {
		prefix := ""
		if sf.Desc {
			prefix = "-"
		}
		fields = append(fields, prefix+sf.Name)
	}
	return "sort " + strings.Join(fields, ", "), nil
}

func (t *translator) translateFields(c *spl2.FieldsCommand) (string, error) {
	if c.Remove {
		return "drop " + strings.Join(c.Fields, ", "), nil
	}
	return "keep " + strings.Join(c.Fields, ", "), nil
}

func (t *translator) translateTable(c *spl2.TableCommand) (string, error) {
	return "keep " + strings.Join(c.Fields, ", "), nil
}

func (t *translator) translateRename(c *spl2.RenameCommand) (string, error) {
	var renames []string
	for _, r := range c.Renames {
		renames = append(renames, fmt.Sprintf("%s as %s", r.Old, r.New))
	}
	return "rename " + strings.Join(renames, ", "), nil
}

func (t *translator) translateDedup(c *spl2.DedupCommand) (string, error) {
	if c.Limit > 1 {
		return fmt.Sprintf("dedup %d %s", c.Limit, strings.Join(c.Fields, ", ")), nil
	}
	return "dedup " + strings.Join(c.Fields, ", "), nil
}

func (t *translator) translateTop(c *spl2.TopCommand) (string, error) {
	n := c.N
	if n <= 0 {
		n = 10
	}
	return fmt.Sprintf("top %d %s", n, c.Field), nil
}

func (t *translator) translateRare(c *spl2.RareCommand) (string, error) {
	n := c.N
	if n <= 0 {
		n = 10
	}
	return fmt.Sprintf("rare %d %s", n, c.Field), nil
}

func (t *translator) translateBin(c *spl2.BinCommand) (string, error) {
	// bin field span=5m [as alias] -> extend [alias|field] = bin(field, 5m)
	alias := c.Alias
	if alias == "" {
		alias = c.Field
	}
	return fmt.Sprintf("extend %s = bin(%s, %s)", alias, c.Field, c.Span), nil
}

func (t *translator) translateTimechart(c *spl2.TimechartCommand) (string, error) {
	aggs, err := t.translateAggExprs(c.Aggregations)
	if err != nil {
		return "", err
	}
	byKeys := c.GroupBy
	return fmt.Sprintf("every %s%s stats %s",
		c.Span,
		func() string {
			if len(byKeys) > 0 {
				return " by " + strings.Join(byKeys, ", ")
			}
			return ""
		}(),
		strings.Join(aggs, ", ")), nil
}

func (t *translator) translateFillnull(c *spl2.FillnullCommand) (string, error) {
	if len(c.Fields) == 0 {
		return "", fmt.Errorf("fillnull without field list not supported by the restricted translator")
	}
	val := c.Value
	if val == "" {
		val = "0"
	}
	var assigns []string
	for _, f := range c.Fields {
		assigns = append(assigns, fmt.Sprintf("%s = %s ?? %s", f, f, translateLiteralValue(val)))
	}
	return "extend " + strings.Join(assigns, ", "), nil
}

func (t *translator) translateMaterialize(c *spl2.MaterializeCommand) (string, error) {
	s := fmt.Sprintf("materialize %s", quoteStr(c.Name))
	if c.Retention != "" {
		s += " retention=" + c.Retention
	}
	return s, nil
}

func (t *translator) translateXYSeries(c *spl2.XYSeriesCommand) (string, error) {
	return fmt.Sprintf("xyseries %s %s %s", c.XField, c.YField, c.ValueField), nil
}

// ---- Aggregation expression translator ----

func (t *translator) translateAggExprs(aggs []spl2.AggExpr) ([]string, error) {
	var result []string
	for _, agg := range aggs {
		translated, err := t.translateAggExpr(agg)
		if err != nil {
			return nil, err
		}
		result = append(result, translated)
	}
	return result, nil
}

func (t *translator) translateAggExpr(agg spl2.AggExpr) (string, error) {
	fn := strings.ToLower(agg.Func)

	// Map function names
	lfFunc := fn
	switch fn {
	case "count":
		// Check for count(eval(predicate)) -> count(where predicate)
		if len(agg.Args) == 1 {
			if fc, ok := agg.Args[0].(*spl2.FuncCallExpr); ok && strings.ToLower(fc.Name) == "eval" && len(fc.Args) == 1 {
				pred, err := t.translateExpr(fc.Args[0])
				if err != nil {
					return "", err
				}
				s := fmt.Sprintf("count(where %s)", pred)
				if agg.Alias != "" {
					s += " as " + agg.Alias
				}
				return s, nil
			}
		}
		// count without args -> count()
		if len(agg.Args) == 0 {
			lfFunc = "count"
		}
	case "dc":
		lfFunc = "dc"
	case "mean":
		lfFunc = "avg"
	case "perc50", "percentile50", "exactperc50", "upperperc50", "median":
		lfFunc = "p50"
	case "perc75", "percentile75", "exactperc75", "upperperc75":
		lfFunc = "p75"
	case "perc90", "percentile90", "exactperc90", "upperperc90":
		lfFunc = "p90"
	case "perc95", "percentile95", "exactperc95", "upperperc95":
		lfFunc = "p95"
	case "perc99", "percentile99", "exactperc99", "upperperc99":
		lfFunc = "p99"
	case "sum", "avg", "min", "max", "stdev", "var", "earliest", "latest", "values", "first", "last", "mode":
		lfFunc = fn
	default:
		// If it looks like a percentile alias
		if strings.HasPrefix(fn, "perc") || strings.HasPrefix(fn, "percentile") || strings.HasPrefix(fn, "exactperc") || strings.HasPrefix(fn, "upperperc") {
			return "", fmt.Errorf("unsupported aggregation function %q; use perc(field, N) in LynxFlow", fn)
		}
		lfFunc = fn
	}

	// Build the translated aggregation
	var args []string
	for _, arg := range agg.Args {
		a, err := t.translateExpr(arg)
		if err != nil {
			return "", err
		}
		args = append(args, a)
	}

	var s string
	if len(args) == 0 {
		s = lfFunc + "()"
	} else {
		s = lfFunc + "(" + strings.Join(args, ", ") + ")"
	}

	if agg.Alias != "" {
		s += " as " + agg.Alias
	}

	return s, nil
}

// ---- Expression translator ----

func (t *translator) translateExpr(e spl2.Expr) (string, error) {
	if e == nil {
		return "null", nil
	}
	switch x := e.(type) {
	case *spl2.FieldExpr:
		return x.Name, nil
	case *spl2.LiteralExpr:
		return translateLiteralValue(x.Value), nil
	case *spl2.CompareExpr:
		left, err := t.translateExpr(x.Left)
		if err != nil {
			return "", err
		}
		right, err := t.translateExpr(x.Right)
		if err != nil {
			return "", err
		}
		op := translateCompareOp(x.Op)
		return fmt.Sprintf("%s %s %s", left, op, right), nil
	case *spl2.BinaryExpr:
		left, err := t.translateExpr(x.Left)
		if err != nil {
			return "", err
		}
		right, err := t.translateExpr(x.Right)
		if err != nil {
			return "", err
		}
		op := strings.ToLower(x.Op)
		if op == "xor" {
			return fmt.Sprintf("(%s != %s)", left, right), nil
		}
		return fmt.Sprintf("(%s %s %s)", left, op, right), nil
	case *spl2.ArithExpr:
		return t.translateArith(x)
	case *spl2.NotExpr:
		inner, err := t.translateExpr(x.Expr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("not %s", inner), nil
	case *spl2.FuncCallExpr:
		return t.translateFuncCall(x)
	case *spl2.InExpr:
		field, err := t.translateExpr(x.Field)
		if err != nil {
			return "", err
		}
		var vals []string
		for _, v := range x.Values {
			tv, err := t.translateExpr(v)
			if err != nil {
				return "", err
			}
			vals = append(vals, tv)
		}
		neg := ""
		if x.Negated {
			neg = "not "
		}
		return fmt.Sprintf("%s%s in [%s]", neg, field, strings.Join(vals, ", ")), nil
	case *spl2.FStringExpr:
		// F-strings removed in LynxFlow; convert to string concat
		t.addNote("fstring-removed", "F-string interpolation removed; converted to string concatenation")
		return t.translateFString(x)
	case *spl2.GlobExpr:
		return quoteStr(x.Pattern), nil
	default:
		return "", fmt.Errorf("unsupported expression type: %T", e)
	}
}

func (t *translator) translateArith(x *spl2.ArithExpr) (string, error) {
	left, err := t.translateExpr(x.Left)
	if err != nil {
		return "", err
	}
	right, err := t.translateExpr(x.Right)
	if err != nil {
		return "", err
	}

	op := x.Op

	// Float division rule: SPL2 / is always float division.
	// LynxFlow int/int truncates. To preserve SPL2 semantics,
	// wrap: float(x) / y when both operands are not literal floats.
	// Chosen rule: translate `x / y` to `float(x) / y` when the
	// left operand is not a literal float.
	if op == "/" {
		if !isLiteralFloat(x.Left) {
			t.addNote("int-division",
				"SPL2 division is always float; LynxFlow int/int truncates. "+
					"Wrapped left operand with float() to preserve float division semantics.")
			left = "float(" + left + ")"
		}
	}

	return fmt.Sprintf("(%s %s %s)", left, op, right), nil
}

// isLiteralFloat checks if an SPL2 expression is a literal float value.
func isLiteralFloat(e spl2.Expr) bool {
	lit, ok := e.(*spl2.LiteralExpr)
	if !ok {
		return false
	}
	return strings.Contains(lit.Value, ".") || strings.Contains(lit.Value, "e") || strings.Contains(lit.Value, "E")
}

func (t *translator) translateFuncCall(fc *spl2.FuncCallExpr) (string, error) {
	name := strings.ToLower(fc.Name)

	// Function name mapping per RFC-002 section 15.4
	lfName := name
	switch name {
	case "isnotnull":
		lfName = "exists"
	case "isnull":
		lfName = "is_null"
	case "match":
		// match(field, regex) -> matches(field, r"regex")
		lfName = "matches"
		if len(fc.Args) == 2 {
			field, err := t.translateExpr(fc.Args[0])
			if err != nil {
				return "", err
			}
			pattern, err := t.translateExpr(fc.Args[1])
			if err != nil {
				return "", err
			}
			// Wrap pattern as raw string if it's a quoted string
			rawPattern := toRawString(pattern)
			return fmt.Sprintf("matches(%s, %s)", field, rawPattern), nil
		}
	case "cidrmatch":
		lfName = "cidr_match"
	case "like":
		// LIKE pattern -> contains/starts_with/ends_with/glob
		if len(fc.Args) == 2 {
			return t.translateLikeToFunction(fc.Args[0], fc.Args[1])
		}
	case "tonumber":
		lfName = "int"
		t.addNote("tonumber-to-int", "tonumber() maps to int(); use float() for float conversion")
	case "tostring":
		lfName = "string"
	case "todouble":
		lfName = "float"
	case "tobool":
		lfName = "bool"
	case "startswith":
		lfName = "starts_with"
	case "endswith":
		lfName = "ends_with"
	case "contains":
		// SPL2 contains is case-sensitive; LynxFlow contains is CI.
		lfName = "contains"
		t.addNote("contains-case", "SPL2 contains() was case-sensitive; LynxFlow contains() is case-insensitive. Use contains_cs() for case-sensitive matching.")
	case "now":
		lfName = "now"
	case "round", "abs", "floor", "ceil", "sqrt", "ln", "log", "exp", "pow",
		"len", "lower", "upper", "trim", "ltrim", "rtrim", "replace", "split",
		"join", "coalesce", "if", "case", "nullif", "urldecode",
		"md5", "sha1", "sha256", "strftime", "strptime", "printf":
		lfName = name
	case "mvjoin":
		lfName = "join"
	case "mvappend":
		lfName = "array_concat"
	case "mvdedup":
		lfName = "array_distinct"
	case "substr":
		// substr(s, start, len) -- SPL2 is 1-based, LynxFlow is 0-based
		lfName = "substr"
		if len(fc.Args) >= 2 {
			return t.translateSubstr(fc)
		}
	case "null":
		return "null", nil
	case "time_bucket":
		// time_bucket(field, span) -> bin(field, span)
		lfName = "bin"
	}

	// Default: translate args
	var args []string
	for _, arg := range fc.Args {
		a, err := t.translateExpr(arg)
		if err != nil {
			return "", err
		}
		args = append(args, a)
	}

	if len(args) == 0 {
		return lfName + "()", nil
	}
	return lfName + "(" + strings.Join(args, ", ") + ")", nil
}

func (t *translator) translateSubstr(fc *spl2.FuncCallExpr) (string, error) {
	var args []string
	for i, arg := range fc.Args {
		a, err := t.translateExpr(arg)
		if err != nil {
			return "", err
		}
		if i == 1 {
			// Second arg is start index: SPL2 is 1-based, LynxFlow is 0-based
			// If it's a literal integer, subtract 1
			if lit, ok := arg.(*spl2.LiteralExpr); ok {
				if n, err := strconv.Atoi(lit.Value); err == nil {
					a = strconv.Itoa(n - 1)
					t.addNote("substr-reindex",
						fmt.Sprintf("substr start index rewritten from 1-based (%d) to 0-based (%d)", n, n-1))
				} else {
					t.addNote("substr-reindex",
						"substr start index is dynamic; LynxFlow is 0-based (SPL2 was 1-based). "+
							"Review the expression manually.")
				}
			} else {
				t.addNote("substr-reindex",
					"substr start index is a non-literal expression; LynxFlow is 0-based (SPL2 was 1-based). "+
						"Review the expression manually.")
			}
		}
		args = append(args, a)
	}
	return "substr(" + strings.Join(args, ", ") + ")", nil
}

func (t *translator) translateLikeToFunction(fieldExpr, patternExpr spl2.Expr) (string, error) {
	field, err := t.translateExpr(fieldExpr)
	if err != nil {
		return "", err
	}

	// Extract pattern string
	lit, ok := patternExpr.(*spl2.LiteralExpr)
	if !ok {
		// Dynamic pattern - fall back to glob
		pattern, err := t.translateExpr(patternExpr)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("glob(%s, %s)", field, pattern), nil
	}

	pattern := unquoteStr(lit.Value)

	// LIKE patterns: % = any, _ = single char
	// Map to starts_with/ends_with/contains/glob
	if strings.HasPrefix(pattern, "%") && strings.HasSuffix(pattern, "%") {
		inner := pattern[1 : len(pattern)-1]
		if !strings.ContainsAny(inner, "%_") {
			return fmt.Sprintf("contains(%s, %s)", field, quoteStr(inner)), nil
		}
	}
	if strings.HasSuffix(pattern, "%") && !strings.ContainsAny(pattern[:len(pattern)-1], "%_") {
		prefix := pattern[:len(pattern)-1]
		return fmt.Sprintf("starts_with(%s, %s)", field, quoteStr(prefix)), nil
	}
	if strings.HasPrefix(pattern, "%") && !strings.ContainsAny(pattern[1:], "%_") {
		suffix := pattern[1:]
		return fmt.Sprintf("ends_with(%s, %s)", field, quoteStr(suffix)), nil
	}

	// General LIKE -> glob (convert % -> *, _ -> ?)
	globPattern := strings.ReplaceAll(pattern, "%", "*")
	globPattern = strings.ReplaceAll(globPattern, "_", "?")
	return fmt.Sprintf("glob(%s, %s)", field, quoteStr(globPattern)), nil
}

func (t *translator) translateFString(fs *spl2.FStringExpr) (string, error) {
	var parts []string
	for _, p := range fs.Parts {
		if p.Literal != "" {
			parts = append(parts, quoteStr(p.Literal))
		}
		if p.ParsedExpr != nil {
			e, err := t.translateExpr(p.ParsedExpr)
			if err != nil {
				return "", err
			}
			// Wrap in string() to ensure concatenation works
			parts = append(parts, "string("+e+")")
		}
	}
	if len(parts) == 0 {
		return `""`, nil
	}
	return strings.Join(parts, " + "), nil
}

// ---- Helper functions ----

func translateCompareOp(op string) string {
	switch op {
	case "=":
		return "=="
	case "!=", "<>":
		return "!="
	case "<", "<=", ">", ">=":
		return op
	case "==":
		return "=="
	default:
		return op
	}
}

func translateLiteralValue(v string) string {
	if v == "" {
		return `""`
	}
	// Already quoted
	if isDelimited(v) {
		return v
	}
	// Boolean/null
	switch strings.ToLower(v) {
	case "true", "false", "null":
		return strings.ToLower(v)
	}
	// Number
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return v
	}
	// Otherwise, quote as string
	return quoteStr(v)
}

func isDelimited(v string) bool {
	if len(v) < 2 {
		return false
	}
	return (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')
}

func quoteStr(s string) string {
	return strconv.Quote(s)
}

func unquoteStr(s string) string {
	if isDelimited(s) {
		u, err := strconv.Unquote(s)
		if err == nil {
			return u
		}
		// Single-quoted: just strip quotes
		if s[0] == '\'' {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// toRawString converts a quoted string literal to a raw string r"...".
// If the input is already a raw string or not a string, returns it unchanged.
func toRawString(s string) string {
	if strings.HasPrefix(s, `r"`) {
		return s
	}
	unquoted, err := strconv.Unquote(s)
	if err != nil {
		return s
	}
	return `r"` + unquoted + `"`
}
