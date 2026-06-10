package opt

import (
	"strings"
	"testing"

	"github.com/lynxbase/lynxdb/pkg/logical"
)

// ---------------------------------------------------------------------------
// Test: partial-agg
// ---------------------------------------------------------------------------

func TestPartialAgg(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:     "count_decomposable",
			query:    "from main | stats count()",
			contains: []string{"[partial]"},
		},
		{
			name:     "sum_decomposable",
			query:    "from main | stats sum(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "avg_decomposable",
			query:    "from main | stats avg(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "min_max_decomposable",
			query:    "from main | stats min(x), max(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "dc_decomposable",
			query:    "from main | stats dc(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "percentiles_decomposable",
			query:    "from main | stats p50(x), p90(x), p99(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "stdev_decomposable",
			query:    "from main | stats stdev(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "earliest_latest_decomposable",
			query:    "from main | stats earliest(x), latest(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "values_decomposable",
			query:    "from main | stats values(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "mode_decomposable",
			query:    "from main | stats mode(x)",
			contains: []string{"[partial]"},
		},
		{
			name:     "list_blocks_partial",
			query:    "from main | stats list(x)",
			excludes: []string{"[partial]"},
		},
		{
			name:     "list_with_decomposable_blocks",
			query:    "from main | stats count(), list(x)",
			excludes: []string{"[partial]"},
		},
		{
			name:     "eventstats_never_partial",
			query:    "from main | eventstats count()",
			excludes: []string{"[partial]"},
		},
		{
			name:     "streamstats_never_partial",
			query:    "from main | streamstats count()",
			excludes: []string{"[partial]"},
		},
		{
			name:     "conditional_where_does_not_affect",
			query:    `from main | stats count(where level == "error")`,
			contains: []string{"[partial]"},
		},
		{
			name:     "with_group_by",
			query:    "from main | stats count() by service",
			contains: []string{"[partial]"},
		},
		{
			name:     "with_timebin",
			query:    "from main | stats count() by bin(_time, 1h)",
			contains: []string{"[partial]"},
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
// Test: topk-into-agg
// ---------------------------------------------------------------------------

func TestTopKIntoAgg(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:  "topk_hint_set_node_kept",
			query: "from main | stats count() as cnt by service | sort -cnt | head 5",
			contains: []string{
				"[topk=5]",
				"TopK(5, -cnt)", // TopK node still present
			},
		},
		{
			name:  "topk_hint_asc",
			query: "from main | stats count() as cnt by service | sort cnt | head 3",
			contains: []string{
				"[topk=3]",
				"TopK(3, +cnt)",
			},
		},
		{
			name:     "sort_key_not_in_agg_output_blocks",
			query:    "from main | stats count() by level, service | sort level, -count, service | head 10",
			excludes: []string{"[topk="},
		},
		{
			name:     "eventstats_blocks",
			query:    "from main | eventstats count() as cnt | sort -cnt | head 5",
			excludes: []string{"[topk="},
		},
		{
			name:     "topk_not_directly_above_agg",
			query:    "from main | stats count() as cnt by service | where cnt > 10 | sort -cnt | head 5",
			excludes: []string{"[topk="},
		},
		{
			name:  "multiple_sort_keys_all_in_agg",
			query: "from main | stats count() as cnt, avg(x) as avg_x by svc | sort -cnt, avg_x | head 10",
			contains: []string{
				"[topk=10]",
				"TopK(10, -cnt, +avg_x)",
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
// Test: tail-scan
// ---------------------------------------------------------------------------

func TestTailScan(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:  "tail_through_filter_sets_reverse",
			query: `from main | where level == "error" | tail 10`,
			contains: []string{
				"Scan(main, reverse)",
				"Limit(10)",
			},
			excludes: []string{"Limit(tail"},
		},
		{
			name:  "tail_through_extend_sets_reverse",
			query: "from main | extend x = 1 | tail 5",
			contains: []string{
				"Scan(main, reverse)",
				"Limit(5)",
			},
			excludes: []string{"Limit(tail"},
		},
		{
			name:  "tail_through_filter_and_extend",
			query: `from main | where level == "error" | extend x = 1 | tail 3`,
			contains: []string{
				"Scan(main, reverse)",
				"Limit(3)",
				"Filter(",
				"Extend(",
			},
			excludes: []string{"Limit(tail"},
		},
		{
			name:  "tail_through_project",
			query: `from main | keep level, status | tail 7`,
			contains: []string{
				"Scan(main, reverse)",
				"Limit(7)",
			},
			excludes: []string{"Limit(tail"},
		},
		{
			name:     "sort_blocks_tail_scan",
			query:    "from main | sort level | tail 10",
			excludes: []string{"reverse"},
			contains: []string{"Limit(tail 10)"},
		},
		{
			name:     "aggregate_blocks_tail_scan",
			query:    "from main | stats count() | tail 10",
			excludes: []string{"reverse"},
			contains: []string{"Limit(tail 10)"},
		},
		{
			name:     "dedup_blocks_tail_scan",
			query:    "from main | dedup level | tail 10",
			excludes: []string{"reverse"},
			contains: []string{"Limit(tail 10)"},
		},
		{
			name:  "direct_tail_on_scan",
			query: "from main | tail 20",
			contains: []string{
				"Scan(main, reverse)",
				"Limit(20)",
			},
			excludes: []string{"Limit(tail"},
		},
		{
			name:  "tail_through_parse_with_captures",
			query: `from main | parse regex r"(?P<host>\S+)" | tail 5`,
			contains: []string{
				"Scan(main, reverse)",
				"Limit(5)",
			},
			excludes: []string{"Limit(tail"},
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
// Test: limit-pushdown
// ---------------------------------------------------------------------------

func TestLimitPushdown(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:  "limit_swaps_below_extend",
			query: "from main | extend x = 1 | head 10",
			// After pushdown: Extend -> Limit -> Scan
			contains: []string{
				"Extend(",
				"Limit(10)",
			},
		},
		{
			name:  "limit_swaps_below_project",
			query: "from main | keep level, status | head 10",
			contains: []string{
				"Project(",
				"Limit(10)",
			},
		},
		{
			name:  "limit_not_below_filter",
			query: `from main | where level == "error" | head 10`,
			// Limit stays above filter.
			contains: []string{
				"Limit(10)",
				"Filter(",
			},
		},
		{
			name:  "limit_below_extend_above_filter",
			query: `from main | where level == "error" | extend x = 1 | head 10`,
			// Limit pushes below extend but stops at filter.
			contains: []string{
				"Extend(",
				"Limit(10)",
				"Filter(",
			},
		},
		{
			name:  "parse_on_error_drop_blocks",
			query: `from main | parse json on_error drop | head 10`,
			// Limit stays above parse(drop).
			contains: []string{"Limit(10)"},
		},
		{
			name:  "parse_default_on_error_allows",
			query: `from main | parse json | head 10`,
			// parse without on_error=drop (default "propagate") is safe.
			contains: []string{
				"Parse(",
				"Limit(10)",
			},
		},
		{
			name:     "tail_not_pushed",
			query:    "from main | extend x = 1 | tail 10",
			excludes: []string{},
			// Tail is converted to reverse scan + head by tail-scan rule,
			// and then limit-pushdown can push the new head below extend.
			contains: []string{"reverse"},
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

	// Verify the relative ordering after limit pushdown below extend.
	t.Run("limit_below_extend_ordering", func(t *testing.T) {
		dump := optimizedDump(t, "from main | extend x = 1 | head 10")
		extendIdx := strings.Index(dump, "Extend(")
		limitIdx := strings.Index(dump, "Limit(10)")
		if extendIdx == -1 || limitIdx == -1 {
			t.Fatalf("expected both Extend and Limit in dump\n%s", dump)
		}
		if extendIdx >= limitIdx {
			t.Errorf("expected Extend above Limit (lower line number), got Extend@%d Limit@%d\nDump:\n%s",
				extendIdx, limitIdx, dump)
		}
	})
}

// ---------------------------------------------------------------------------
// Test: column-pruning
// ---------------------------------------------------------------------------

func TestColumnPruning(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		contains []string
		excludes []string
	}{
		{
			name:  "where_stats_prunes",
			query: `from main | where status >= 500 | stats count() by service`,
			contains: []string{
				"pushdown.columns: [_raw, _source, _time, service, status]",
			},
		},
		{
			name:     "filter_only_no_pruning",
			query:    `from main | where status >= 500`,
			excludes: []string{"pushdown.columns:"},
		},
		{
			name:  "extend_stats_prunes_correctly",
			query: "from main | extend x = duration_ms * 2 | stats avg(x) by service",
			// Aggregate resets set to: x, service. Extend removes x, adds
			// duration_ms. So scan needs: duration_ms, service + builtins.
			contains: []string{
				"pushdown.columns: [_raw, _source, _time, duration_ms, service]",
			},
		},
		{
			name:     "describe_disables_pruning",
			query:    "from main | describe",
			excludes: []string{"pushdown.columns:"},
		},
		{
			name:     "helper_disables_pruning",
			query:    "from main | transaction user_id",
			excludes: []string{"pushdown.columns:"},
		},
		{
			name:     "parse_without_into_disables",
			query:    "from main | parse json | stats count()",
			excludes: []string{"pushdown.columns:"},
		},
		{
			name:  "parse_with_into_allows",
			query: `from main | parse json into (host, status) | stats count() by host`,
			// Parse with into (captures) has known produced fields;
			// column pruning can work through it.
			contains: []string{
				"pushdown.columns:",
			},
		},
		{
			name:  "project_keep_prunes",
			query: "from main | keep level, status | head 10",
			contains: []string{
				"pushdown.columns: [_raw, _source, _time, level, status]",
			},
		},
		{
			name:  "builtins_always_included",
			query: "from main | stats count() by service",
			// Should include _time, _raw, _source even though not referenced.
			contains: []string{
				"pushdown.columns: [_raw, _source, _time, service]",
			},
		},
		{
			name:  "eventstats_prunes_through",
			query: "from main | keep service, duration_ms | eventstats avg(duration_ms) as avg_d",
			contains: []string{
				"pushdown.columns: [_raw, _source, _time, duration_ms, service]",
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
// Test: batch-3 determinism
// ---------------------------------------------------------------------------

func TestBatch3Determinism(t *testing.T) {
	queries := []string{
		`from main | stats count(), avg(x) by service`,
		`from main | where level == "error" | tail 10`,
		`from main | stats count() as cnt by service | sort -cnt | head 5`,
		`from main | extend x = 1 | head 10`,
		`from main | where status >= 500 | stats count() by service`,
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
// Test: batch-3 purity (original plan unchanged)
// ---------------------------------------------------------------------------

func TestBatch3Purity(t *testing.T) {
	queries := []string{
		`from main | stats count()`,
		`from main | where level == "error" | tail 10`,
		`from main | stats count() as cnt by service | sort -cnt | head 5`,
	}

	for _, q := range queries {
		plan := parseDesugarLower(t, q)
		dumpBefore := plan.Dump()

		plan2 := parseDesugarLower(t, q)
		_, _ = Optimize(plan2)

		dumpAfter := plan.Dump()
		if dumpBefore != dumpAfter {
			t.Errorf("original plan changed for %q:\n--- before ---\n%s\n--- after ---\n%s",
				q, dumpBefore, dumpAfter)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Applied reporting includes batch-3 rules
// ---------------------------------------------------------------------------

func TestAppliedIncludesBatch3Rules(t *testing.T) {
	// partial-agg should fire.
	plan := parseDesugarLower(t, "from main | stats count()")
	_, applied := Optimize(plan)
	findRule(t, applied, "partial-agg")

	// topk-into-agg should fire.
	plan2 := parseDesugarLower(t, "from main | stats count() as cnt by service | sort -cnt | head 5")
	_, applied2 := Optimize(plan2)
	findRule(t, applied2, "topk-into-agg")

	// tail-scan should fire.
	plan3 := parseDesugarLower(t, "from main | tail 10")
	_, applied3 := Optimize(plan3)
	findRule(t, applied3, "tail-scan")

	// limit-pushdown should fire.
	plan4 := parseDesugarLower(t, "from main | extend x = 1 | head 10")
	_, applied4 := Optimize(plan4)
	findRule(t, applied4, "limit-pushdown")

	// column-pruning should fire.
	plan5 := parseDesugarLower(t, "from main | stats count() by service")
	_, applied5 := Optimize(plan5)
	findRule(t, applied5, "column-pruning")
}

func findRule(t *testing.T, applied []Applied, name string) {
	t.Helper()
	for _, a := range applied {
		if a.Rule == name && a.Count > 0 {
			return
		}
	}
	t.Errorf("expected rule %q in Applied, got %v", name, applied)
}

// ---------------------------------------------------------------------------
// Test: Scan.Reverse in Dump
// ---------------------------------------------------------------------------

func TestScanReverseDump(t *testing.T) {
	scan := &logical.Scan{
		Sources:      []logical.SourcePattern{{Name: "main"}},
		Reverse:      true,
		OutputSchema: nil,
	}
	s := scan.String()
	if !strings.Contains(s, "reverse") {
		t.Errorf("Scan.String() should contain 'reverse', got %q", s)
	}
}

// ---------------------------------------------------------------------------
// Test: Aggregate.TopK in Dump
// ---------------------------------------------------------------------------

func TestAggregateTopKDump(t *testing.T) {
	dump := optimizedDump(t, "from main | stats count() as cnt by service | sort -cnt | head 5")
	if !strings.Contains(dump, "[topk=5]") {
		t.Errorf("expected [topk=5] in dump, got:\n%s", dump)
	}
	if !strings.Contains(dump, "[partial]") {
		t.Errorf("expected [partial] in dump, got:\n%s", dump)
	}
}

// ---------------------------------------------------------------------------
// Test: Pushdown.Columns in Dump
// ---------------------------------------------------------------------------

func TestPushdownColumnsDump(t *testing.T) {
	dump := optimizedDump(t, "from main | stats count() by service")
	if !strings.Contains(dump, "pushdown.columns:") {
		t.Errorf("expected pushdown.columns in dump, got:\n%s", dump)
	}
	if !strings.Contains(dump, "service") {
		t.Errorf("expected 'service' in pushdown.columns, got:\n%s", dump)
	}
}
