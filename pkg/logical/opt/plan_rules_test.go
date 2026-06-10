package opt

import (
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/logical"
)

// ---------------------------------------------------------------------------
// Helper: parse, desugar, lower, optimize, dump
// ---------------------------------------------------------------------------

func optimizedDump(t *testing.T, query string) string {
	t.Helper()
	plan := parseDesugarLower(t, query)
	plan, _ = Optimize(plan)
	return plan.Dump()
}

// ---------------------------------------------------------------------------
// Test: filter-elim (Filter(true) -> removed)
// ---------------------------------------------------------------------------

func TestFilterElim(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains string
		excludes string
	}{
		{
			name:     "where_true_literal",
			query:    "from main | where true",
			contains: "Scan(main)",
			excludes: "Filter",
		},
		{
			name:     "where_1_eq_1_folded",
			query:    "from main | where 1 == 1",
			contains: "Scan(main)",
			excludes: "Filter",
		},
		{
			name:     "where_true_and_true",
			query:    "from main | where true and true",
			contains: "Scan(main)",
			excludes: "Filter",
		},
		{
			name:     "filter_true_under_aggregate",
			query:    "from main | where true | stats count()",
			contains: "Aggregate",
			excludes: "Filter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			if !strings.Contains(dump, tt.contains) {
				t.Errorf("dump does not contain %q\nGot:\n%s", tt.contains, dump)
			}
			if tt.excludes != "" && strings.Contains(dump, tt.excludes) {
				t.Errorf("dump should not contain %q\nGot:\n%s", tt.excludes, dump)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: filter-false-to-empty (Filter(false/null) -> Empty)
// ---------------------------------------------------------------------------

func TestFilterFalseToEmpty(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains string
	}{
		{
			name:     "where_false",
			query:    "from main | where false",
			contains: "Empty()",
		},
		{
			name:     "where_null",
			query:    "from main | where null",
			contains: "Empty()",
		},
		{
			name:     "where_1_eq_2_folded",
			query:    "from main | where 1 == 2",
			contains: "Empty()",
		},
		{
			name:     "where_false_and_anything",
			query:    "from main | where false and level == \"error\"",
			contains: "Empty()",
		},
		{
			name:     "empty_preserves_schema",
			query:    "from main | where false",
			contains: "Empty()",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			if !strings.Contains(dump, tt.contains) {
				t.Errorf("dump does not contain %q\nGot:\n%s", tt.contains, dump)
			}
		})
	}

	// Verify Empty schema matches what the input would have had.
	t.Run("empty_schema_fields", func(t *testing.T) {
		plan := parseDesugarLower(t, "from main | where false")
		plan, _ = Optimize(plan)
		empty, ok := plan.Root.(*logical.Empty)
		if !ok {
			t.Fatalf("expected Empty, got %T", plan.Root)
		}
		if len(empty.OutputSchema) != 6 {
			t.Errorf("Empty schema should have 6 builtin fields, got %d: %v",
				len(empty.OutputSchema), empty.OutputSchema)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: filter-merge (adjacent Filter(a) | Filter(b) -> Filter(a AND b))
// ---------------------------------------------------------------------------

func TestFilterMerge(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains string
	}{
		{
			name:     "two_adjacent_filters",
			query:    "from main | where level == \"error\" | where status >= 500",
			contains: "Filter(((level == \"error\") and (status >= 500)))",
		},
		{
			name:     "three_adjacent_filters",
			query:    "from main | where level == \"error\" | where status >= 500 | where host == \"web-01\"",
			contains: `Filter(`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			if !strings.Contains(dump, tt.contains) {
				t.Errorf("dump does not contain %q\nGot:\n%s", tt.contains, dump)
			}
		})
	}

	// Verify three filters produce a single Filter node.
	t.Run("three_filters_single_node", func(t *testing.T) {
		dump := optimizedDump(t, `from main | where level == "error" | where status >= 500 | where host == "web-01"`)
		filterCount := strings.Count(dump, "Filter(")
		if filterCount != 1 {
			t.Errorf("expected 1 Filter node, got %d\nDump:\n%s", filterCount, dump)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: predicate-pushdown — time bounds
// ---------------------------------------------------------------------------

func TestPredicatePushdown_TimeBounds(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:  "time_gte_consumed",
			query: `from main | where _time >= 1000`,
			contains: []string{
				"Scan(main)",
				"pushdown.time_bounds:",
			},
			excludes: []string{"Filter("},
		},
		{
			name:  "time_lte_consumed",
			query: `from main | where _time <= 2000`,
			contains: []string{
				"Scan(main)",
				"pushdown.time_bounds:",
			},
			excludes: []string{"Filter("},
		},
		{
			name:  "time_range_both_bounds",
			query: `from main | where _time >= 1000 and _time <= 2000`,
			contains: []string{
				"pushdown.time_bounds: [1000..2000]",
			},
			excludes: []string{"Filter("},
		},
		{
			name:  "time_with_other_predicates_kept",
			query: `from main | where _time >= 1000 and status >= 500`,
			contains: []string{
				"pushdown.time_bounds:",
				"pushdown.field_predicate: (status >= 500)",
				"Filter((status >= 500))",
			},
		},
		{
			name:  "time_with_bracket_range_conflict_stays",
			query: `from main[-1h] | where _time >= 1000`,
			// Bracket range already exists on Scan.TimeRange. The WHERE
			// _time conjunct conflicts (both set start). The conjunct stays
			// in the Filter and is NOT pushed (cannot merge representably).
			contains: []string{
				"Filter((_time >= 1000))",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			for _, c := range tt.contains {
				if !strings.Contains(dump, c) {
					t.Errorf("dump does not contain %q\nGot:\n%s", c, dump)
				}
			}
			for _, exc := range tt.excludes {
				if strings.Contains(dump, exc) {
					t.Errorf("dump should not contain %q\nGot:\n%s", exc, dump)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: predicate-pushdown — has() -> RawTerms
// ---------------------------------------------------------------------------

func TestPredicatePushdown_HasRawTerms(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
	}{
		{
			name:  "single_has_raw",
			query: `from main | where has(_raw, "timeout")`,
			contains: []string{
				`pushdown.raw_term: "timeout"`,
				`Filter(has(_raw, "timeout"))`,
			},
		},
		{
			name:  "has_lowercased",
			query: `from main | where has(_raw, "TimeOut")`,
			contains: []string{
				`pushdown.raw_term: "timeout"`,
			},
		},
		{
			name:  "multi_word_has_tokenized",
			query: `from main | where has(_raw, "connection refused")`,
			contains: []string{
				`pushdown.raw_term: "connection"`,
				`pushdown.raw_term: "refused"`,
				`Filter(has(_raw, "connection refused"))`,
			},
		},
		{
			name:  "two_has_conjuncts",
			query: `from main | where has(_raw, "payment") and has(_raw, "failed")`,
			contains: []string{
				`pushdown.raw_term: "payment"`,
				`pushdown.raw_term: "failed"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			for _, c := range tt.contains {
				if !strings.Contains(dump, c) {
					t.Errorf("dump does not contain %q\nGot:\n%s", c, dump)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: predicate-pushdown — field predicates
// ---------------------------------------------------------------------------

func TestPredicatePushdown_FieldPredicates(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
	}{
		{
			name:  "field_eq_literal",
			query: `from main | where status == 500`,
			contains: []string{
				`pushdown.field_predicate: (status == 500)`,
				`Filter((status == 500))`,
			},
		},
		{
			name:  "field_gte_literal",
			query: `from main | where status >= 500`,
			contains: []string{
				`pushdown.field_predicate: (status >= 500)`,
				`Filter((status >= 500))`,
			},
		},
		{
			name:  "field_neq_string",
			query: `from main | where level != "info"`,
			contains: []string{
				`pushdown.field_predicate: (level != "info")`,
				`Filter((level != "info"))`,
			},
		},
		{
			name:  "time_not_pushed_as_field_pred",
			query: `from main | where _time >= 1000`,
			// _time goes to time bounds, not field predicates.
			contains: []string{
				`pushdown.time_bounds:`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			for _, c := range tt.contains {
				if !strings.Contains(dump, c) {
					t.Errorf("dump does not contain %q\nGot:\n%s", c, dump)
				}
			}
			// _time should never appear in field_predicate.
			if strings.Contains(dump, "pushdown.field_predicate: (_time") {
				t.Errorf("_time should not be a field_predicate\nGot:\n%s", dump)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: predicate-pushdown — contains/glob/matches -> BloomTerms
// ---------------------------------------------------------------------------

func TestPredicatePushdown_BloomTerms(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:  "contains_raw",
			query: `from main | where contains(_raw, "connection refused")`,
			contains: []string{
				`pushdown.bloom_term: "connection"`,
				`pushdown.bloom_term: "refused"`,
				`Filter(contains(_raw, "connection refused"))`,
			},
		},
		{
			name:  "contains_short_tokens_excluded",
			query: `from main | where contains(_raw, "a b")`,
			// Tokens "a" and "b" are < 3 chars, excluded from bloom.
			excludes: []string{`pushdown.bloom_term:`},
		},
		{
			name:  "glob_literal_extraction",
			query: `from main | where glob(_raw, "error*timeout")`,
			contains: []string{
				`pushdown.bloom_term: "error"`,
				`pushdown.bloom_term: "timeout"`,
			},
		},
		{
			name:  "glob_short_literal_excluded",
			query: `from main | where glob(_raw, "ab*cd")`,
			// "ab" and "cd" are < 3 chars tokens, excluded.
			excludes: []string{`pushdown.bloom_term:`},
		},
		{
			name:  "matches_literal_extraction",
			query: `from main | where matches(_raw, r"error\d+timeout")`,
			contains: []string{
				`pushdown.bloom_term: "error"`,
				`pushdown.bloom_term: "timeout"`,
			},
		},
		{
			name:  "matches_no_literals_regex_only",
			query: `from main | where matches(_raw, r"\d+")`,
			// No literal chars in the regex — nothing to extract.
			excludes: []string{`pushdown.bloom_term:`},
		},
		{
			name:  "matches_alternation_safe",
			query: `from main | where matches(_raw, r"error|warning")`,
			// Alternation: can't require literals from either branch
			// individually. But each side is flushed. The result should
			// include both as separate runs.
			contains: []string{
				`pushdown.bloom_term: "error"`,
				`pushdown.bloom_term: "warning"`,
			},
		},
		{
			name:  "contains_non_raw_field_ignored",
			query: `from main | where contains(message, "error pattern")`,
			// Not on _raw — no bloom extraction.
			excludes: []string{`pushdown.bloom_term:`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			for _, c := range tt.contains {
				if !strings.Contains(dump, c) {
					t.Errorf("dump does not contain %q\nGot:\n%s", c, dump)
				}
			}
			for _, exc := range tt.excludes {
				if strings.Contains(dump, exc) {
					t.Errorf("dump should not contain %q\nGot:\n%s", exc, dump)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: mixed conjunct decomposition
// ---------------------------------------------------------------------------

func TestPredicatePushdown_MixedConjuncts(t *testing.T) {
	// One consumed (time), two hinted (field + has), remainder kept.
	t.Run("mixed_conjuncts", func(t *testing.T) {
		dump := optimizedDump(t, `from main | where _time >= 1000 and status == 500 and has(_raw, "timeout")`)
		// Time consumed -> pushdown.time_bounds, removed from Filter.
		if !strings.Contains(dump, "pushdown.time_bounds:") {
			t.Errorf("expected pushdown.time_bounds\nGot:\n%s", dump)
		}
		// Field predicate pushed + kept.
		if !strings.Contains(dump, `pushdown.field_predicate: (status == 500)`) {
			t.Errorf("expected pushdown.field_predicate\nGot:\n%s", dump)
		}
		// has() raw term pushed + kept.
		if !strings.Contains(dump, `pushdown.raw_term: "timeout"`) {
			t.Errorf("expected pushdown.raw_term\nGot:\n%s", dump)
		}
		// Filter should have status and has() but NOT _time.
		if strings.Contains(dump, "Filter((_time") {
			t.Errorf("_time should be consumed from filter\nGot:\n%s", dump)
		}
		if !strings.Contains(dump, "Filter(") {
			t.Errorf("Filter should remain for non-consumed conjuncts\nGot:\n%s", dump)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: predicate pushdown does NOT push through Parse/Extend/Project
// ---------------------------------------------------------------------------

func TestPredicatePushdown_NotThroughBarrier(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		excludes []string
	}{
		{
			name:     "not_through_extend",
			query:    `from main | extend x = 1 | where status == 500`,
			excludes: []string{"pushdown.field_predicate:"},
		},
		{
			name:     "not_through_parse",
			query:    `from main | parse json | where status == 500`,
			excludes: []string{"pushdown.field_predicate:"},
		},
		{
			name:     "not_through_project",
			query:    `from main | keep level, status | where status == 500`,
			excludes: []string{"pushdown.field_predicate:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dump := optimizedDump(t, tt.query)
			for _, exc := range tt.excludes {
				if strings.Contains(dump, exc) {
					t.Errorf("dump should not contain %q (pushdown should not cross barrier)\nGot:\n%s", exc, dump)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: determinism (run twice, same result)
// ---------------------------------------------------------------------------

func TestPlanRulesDeterminism(t *testing.T) {
	queries := []string{
		`from main | where status >= 500 and has(_raw, "error")`,
		`from main | where _time >= 1000 and _time <= 2000`,
		`from main | where false`,
		`from main | where true`,
		`from main | where level == "error" | where status >= 500`,
	}

	for _, q := range queries {
		plan1 := parseDesugarLower(t, q)
		plan2 := parseDesugarLower(t, q)

		plan1, applied1 := Optimize(plan1)
		plan2, applied2 := Optimize(plan2)

		dump1 := plan1.Dump()
		dump2 := plan2.Dump()

		if dump1 != dump2 {
			t.Errorf("non-deterministic Dump for %q:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", q, dump1, dump2)
		}

		if len(applied1) != len(applied2) {
			t.Errorf("non-deterministic Applied for %q: %d vs %d", q, len(applied1), len(applied2))
		}
	}
}

// ---------------------------------------------------------------------------
// Test: purity (original plan unchanged by plan rules)
// ---------------------------------------------------------------------------

func TestPlanRulesPurity(t *testing.T) {
	query := `from main | where status >= 500 and has(_raw, "timeout")`
	plan := parseDesugarLower(t, query)
	dumpBefore := plan.Dump()

	// Take a copy of the plan so optimization doesn't affect our reference.
	plan2 := parseDesugarLower(t, query)
	_, _ = Optimize(plan2)

	dumpAfter := plan.Dump()
	if dumpBefore != dumpAfter {
		t.Errorf("original plan changed:\n--- before ---\n%s\n--- after ---\n%s", dumpBefore, dumpAfter)
	}
}

// ---------------------------------------------------------------------------
// Test: Applied reporting includes plan rules
// ---------------------------------------------------------------------------

func TestAppliedIncludesPlanRules(t *testing.T) {
	// filter-elim should fire.
	plan := parseDesugarLower(t, "from main | where true")
	_, applied := Optimize(plan)
	foundElim := false
	for _, a := range applied {
		if a.Rule == "filter-elim" && a.Count > 0 {
			foundElim = true
		}
	}
	if !foundElim {
		t.Errorf("expected filter-elim in Applied, got %v", applied)
	}

	// filter-false-to-empty should fire.
	plan2 := parseDesugarLower(t, "from main | where false")
	_, applied2 := Optimize(plan2)
	foundEmpty := false
	for _, a := range applied2 {
		if a.Rule == "filter-false-to-empty" && a.Count > 0 {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Errorf("expected filter-false-to-empty in Applied, got %v", applied2)
	}

	// filter-merge should fire.
	plan3 := parseDesugarLower(t, `from main | where level == "error" | where status >= 500`)
	_, applied3 := Optimize(plan3)
	foundMerge := false
	for _, a := range applied3 {
		if a.Rule == "filter-merge" && a.Count > 0 {
			foundMerge = true
		}
	}
	if !foundMerge {
		t.Errorf("expected filter-merge in Applied, got %v", applied3)
	}

	// predicate-pushdown should fire.
	plan4 := parseDesugarLower(t, `from main | where status >= 500`)
	_, applied4 := Optimize(plan4)
	foundPushdown := false
	for _, a := range applied4 {
		if a.Rule == "predicate-pushdown" && a.Count > 0 {
			foundPushdown = true
		}
	}
	if !foundPushdown {
		t.Errorf("expected predicate-pushdown in Applied, got %v", applied4)
	}
}

// ---------------------------------------------------------------------------
// Test: regex literal extraction edge cases
// ---------------------------------------------------------------------------

func TestExtractRegexLiterals(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    []string // expected literal runs
	}{
		{
			name:    "simple_literal",
			pattern: "error",
			want:    []string{"error"},
		},
		{
			name:    "escaped_dot",
			pattern: `error\.log`,
			want:    []string{"error.log"},
		},
		{
			name:    "quantified_optional",
			pattern: "errors?",
			want:    []string{"error"},
		},
		{
			name:    "quantified_plus_required",
			pattern: "error+",
			want:    []string{"error"},
		},
		{
			name:    "dot_star_between",
			pattern: "error.*timeout",
			want:    []string{"error", "timeout"},
		},
		{
			name:    "char_class_flushed",
			pattern: "error[0-9]timeout",
			want:    []string{"error", "timeout"},
		},
		{
			name:    "alternation",
			pattern: "error|warning",
			want:    []string{"error", "warning"},
		},
		{
			name:    "group_flushed",
			pattern: "abc(def)ghi",
			want:    []string{"abc", "def", "ghi"},
		},
		{
			name:    "only_metachar",
			pattern: `\d+`,
			want:    nil,
		},
		{
			name:    "anchor_ignored",
			pattern: "^error$",
			want:    []string{"error"},
		},
		{
			name:    "complex_no_literals",
			pattern: `[a-z]+\s+\d{3}`,
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegexLiterals(tt.pattern)
			if len(got) != len(tt.want) {
				t.Fatalf("extractRegexLiterals(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("extractRegexLiterals(%q)[%d] = %q, want %q", tt.pattern, i, g, tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: glob literal extraction
// ---------------------------------------------------------------------------

func TestExtractGlobLiterals(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    []string
	}{
		{
			name:    "no_metachar",
			pattern: "error",
			want:    []string{"error"},
		},
		{
			name:    "star_between",
			pattern: "error*timeout",
			want:    []string{"error", "timeout"},
		},
		{
			name:    "leading_star",
			pattern: "*timeout",
			want:    []string{"timeout"},
		},
		{
			name:    "trailing_star",
			pattern: "error*",
			want:    []string{"error"},
		},
		{
			name:    "question_mark",
			pattern: "err?r",
			want:    []string{"err", "r"},
		},
		{
			name:    "only_stars",
			pattern: "***",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGlobLiterals(tt.pattern)
			if len(got) != len(tt.want) {
				t.Fatalf("extractGlobLiterals(%q) = %v, want %v", tt.pattern, got, tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("extractGlobLiterals(%q)[%d] = %q, want %q", tt.pattern, i, g, tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: tokenizer
// ---------------------------------------------------------------------------

func TestTokenize(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"hello world", []string{"hello", "world"}},
		{"connection-refused", []string{"connection", "refused"}},
		{"error_code:42", []string{"error", "code", "42"}},
		{"", nil},
		{"---", nil},
		{"abc123def", []string{"abc123def"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := tokenize(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("tokenize(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i, g := range got {
				if g != tt.want[i] {
					t.Errorf("tokenize(%q)[%d] = %q, want %q", tt.input, i, g, tt.want[i])
				}
			}
		})
	}
}
