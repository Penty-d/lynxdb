package server

import (
	"testing"

	enginepipeline "github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/storage/views"
)

// TestInjectAutoCount verifies that the hidden __mv_auto_count aggregation is
// appended to the plan's Aggregate node when (and only when) the view's
// AggSpec carries the auto-injected count needed for weighted avg merges.
func TestInjectAutoCount(t *testing.T) {
	planFor := func(t *testing.T, query string) *logical.Plan {
		t.Helper()
		res, err := parseMVQuery(query)
		if err != nil {
			t.Fatalf("plan %q: %v", query, err)
		}
		return res.Program
	}

	findAgg := func(plan *logical.Plan) *logical.Aggregate {
		n := plan.Root
		for n != nil {
			if agg, ok := n.(*logical.Aggregate); ok {
				return agg
			}
			children := n.Children()
			if len(children) == 0 {
				return nil
			}
			n = children[0]
		}
		return nil
	}

	specWithAutoCount := &enginepipeline.PartialAggSpec{
		Funcs: []enginepipeline.PartialAggFunc{
			{Name: "avg", Field: "duration"},
			{Name: "count", Alias: views.MVAutoCountAlias, Hidden: true},
		},
	}
	specWithoutAutoCount := &enginepipeline.PartialAggSpec{
		Funcs: []enginepipeline.PartialAggFunc{
			{Name: "count"},
		},
	}

	t.Run("appends hidden count for avg spec", func(t *testing.T) {
		plan := planFor(t, "from main | stats avg(duration) by service")
		before := len(findAgg(plan).Aggs)

		injectAutoCount(plan, specWithAutoCount)

		agg := findAgg(plan)
		if len(agg.Aggs) != before+1 {
			t.Fatalf("aggs = %d, want %d", len(agg.Aggs), before+1)
		}
		last := agg.Aggs[len(agg.Aggs)-1]
		if last.Alias != views.MVAutoCountAlias {
			t.Fatalf("appended alias = %q, want %q", last.Alias, views.MVAutoCountAlias)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		plan := planFor(t, "from main | stats avg(duration) by service")
		injectAutoCount(plan, specWithAutoCount)
		n := len(findAgg(plan).Aggs)
		injectAutoCount(plan, specWithAutoCount)
		if got := len(findAgg(plan).Aggs); got != n {
			t.Fatalf("second inject changed aggs: %d -> %d", n, got)
		}
	})

	t.Run("no-op without auto count in spec", func(t *testing.T) {
		plan := planFor(t, "from main | stats count() by service")
		before := len(findAgg(plan).Aggs)
		injectAutoCount(plan, specWithoutAutoCount)
		if got := len(findAgg(plan).Aggs); got != before {
			t.Fatalf("inject without auto-count changed aggs: %d -> %d", before, got)
		}
	})

	t.Run("nil-safe", func(t *testing.T) {
		injectAutoCount(nil, specWithAutoCount)
		injectAutoCount(planFor(t, "from main | head 5"), specWithAutoCount) // no Aggregate node
	})
}
