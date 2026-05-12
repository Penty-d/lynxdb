package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

// ReplaceIterator replaces field values using SPL replace command rules.
type ReplaceIterator struct {
	child  Iterator
	rules  []replaceRule
	fields []string
}

type replaceRule struct {
	old       string
	new       string
	wildcards int
	re        *regexp.Regexp
}

// NewReplaceIterator creates a streaming replace operator.
func NewReplaceIterator(child Iterator, pairs []spl2.ReplacePair, fields []string) (*ReplaceIterator, error) {
	rules := make([]replaceRule, 0, len(pairs))
	for _, pair := range pairs {
		rule, err := newReplaceRule(pair)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}

	return &ReplaceIterator{child: child, rules: rules, fields: fields}, nil
}

func (r *ReplaceIterator) Init(ctx context.Context) error {
	return r.child.Init(ctx)
}

func (r *ReplaceIterator) Next(ctx context.Context) (*Batch, error) {
	batch, err := r.child.Next(ctx)
	if err != nil || batch == nil {
		return batch, err
	}

	fields := r.fields
	if len(fields) == 0 {
		fields = batch.ColumnNames()
	}
	for _, field := range fields {
		if len(r.fields) == 0 && strings.HasPrefix(field, "_") {
			continue
		}
		col, ok := batch.Columns[field]
		if !ok {
			continue
		}
		for i := range col {
			if col[i].IsNull() {
				continue
			}
			if value, ok := r.replaceValue(col[i].String()); ok {
				col[i] = event.StringValue(value)
			}
		}
		batch.Columns[field] = col
	}

	return batch, nil
}

func (r *ReplaceIterator) Close() error {
	return r.child.Close()
}

func (r *ReplaceIterator) Schema() []FieldInfo {
	return r.child.Schema()
}

func (r *ReplaceIterator) replaceValue(value string) (string, bool) {
	out := value
	changed := false
	for _, rule := range r.rules {
		next, ok := rule.apply(out)
		if ok {
			out = next
			changed = true
		}
	}

	return out, changed
}

func newReplaceRule(pair spl2.ReplacePair) (replaceRule, error) {
	oldWildcards := strings.Count(pair.Old, "*")
	newWildcards := strings.Count(pair.New, "*")
	if oldWildcards > 0 && newWildcards != 0 && newWildcards != oldWildcards {
		return replaceRule{}, fmt.Errorf("replace wildcard count mismatch for %q WITH %q", pair.Old, pair.New)
	}
	rule := replaceRule{old: pair.Old, new: pair.New, wildcards: oldWildcards}
	if oldWildcards == 0 {
		return rule, nil
	}

	var pattern strings.Builder
	pattern.WriteByte('^')
	parts := strings.Split(pair.Old, "*")
	for i, part := range parts {
		if i > 0 {
			pattern.WriteString("(.*?)")
		}
		pattern.WriteString(regexp.QuoteMeta(part))
	}
	pattern.WriteByte('$')
	rule.re = regexp.MustCompile(pattern.String())

	return rule, nil
}

func (r replaceRule) apply(value string) (string, bool) {
	if r.wildcards == 0 {
		if value == r.old {
			return r.new, true
		}

		return "", false
	}
	matches := r.re.FindStringSubmatch(value)
	if matches == nil {
		return "", false
	}
	if strings.Count(r.new, "*") == 0 {
		return r.new, true
	}

	var out strings.Builder
	parts := strings.Split(r.new, "*")
	for i, part := range parts {
		if i > 0 && i < len(matches) {
			out.WriteString(matches[i])
		}
		out.WriteString(part)
	}

	return out.String(), true
}
