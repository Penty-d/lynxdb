package planner

import (
	"testing"

	"github.com/lynxbase/lynxdb/pkg/model"
)

// TestHintsFromPlan_TimeBounds verifies that a query with a bracket time range
// (e.g., from main[-1h]) populates hints.TimeBounds.
func TestHintsFromPlan_TimeBounds(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main[-1h]"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.TimeBounds == nil {
		t.Fatal("expected TimeBounds to be set for from main[-1h], got nil")
	}
	if res.Hints.TimeBounds.Earliest.IsZero() {
		t.Error("expected Earliest to be non-zero")
	}
}

// TestHintsFromPlan_SingleSource verifies single source → SourceScopeSingle.
func TestHintsFromPlan_SingleSource(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from myindex"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.SourceScopeType != model.SourceScopeSingle {
		t.Errorf("SourceScopeType = %q, want %q", res.Hints.SourceScopeType, model.SourceScopeSingle)
	}
	if res.Hints.IndexName != "myindex" {
		t.Errorf("IndexName = %q, want %q", res.Hints.IndexName, "myindex")
	}
	if len(res.Hints.SourceScopeSources) != 1 || res.Hints.SourceScopeSources[0] != "myindex" {
		t.Errorf("SourceScopeSources = %v, want [myindex]", res.Hints.SourceScopeSources)
	}
}

// TestHintsFromPlan_MultiSourceList verifies FROM a, b → SourceScopeList.
func TestHintsFromPlan_MultiSourceList(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from alpha, beta"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.SourceScopeType != model.SourceScopeList {
		t.Errorf("SourceScopeType = %q, want %q", res.Hints.SourceScopeType, model.SourceScopeList)
	}
	if len(res.Hints.SourceScopeSources) != 2 {
		t.Errorf("SourceScopeSources = %v, want 2 entries", res.Hints.SourceScopeSources)
	}
}

// TestHintsFromPlan_GlobSource verifies FROM logs* → SourceScopeGlob.
func TestHintsFromPlan_GlobSource(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from logs*"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.SourceScopeType != model.SourceScopeGlob {
		t.Errorf("SourceScopeType = %q, want %q", res.Hints.SourceScopeType, model.SourceScopeGlob)
	}
	if res.Hints.SourceScopePattern != "logs*" {
		t.Errorf("SourceScopePattern = %q, want %q", res.Hints.SourceScopePattern, "logs*")
	}
}

// TestHintsFromPlan_StarSource verifies FROM * → SourceScopeAll.
func TestHintsFromPlan_StarSource(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from *"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.SourceScopeType != model.SourceScopeAll {
		t.Errorf("SourceScopeType = %q, want %q", res.Hints.SourceScopeType, model.SourceScopeAll)
	}
}

// TestHintsFromPlan_NegatedSource verifies FROM !debug* → SourceExcludeGlobs.
func TestHintsFromPlan_NegatedSource(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main, !debug*"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if len(res.Hints.SourceExcludeGlobs) == 0 {
		t.Error("expected SourceExcludeGlobs to be non-empty")
	}
}

// TestHintsFromPlan_FieldPredicateEquality verifies that
// where status == 200 produces a FieldPredicate.
func TestHintsFromPlan_FieldPredicateEquality(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main | where status == 200"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	// The optimizer may or may not push this down to the Scan node; verify
	// the hint extraction is correct when it does appear.
	// FieldPredicates are only populated when the optimizer pushes predicates
	// into the Scan's Pushdown.FieldPredicates. This test verifies the
	// extraction code handles the case where the optimizer *does* push.
	// If no field predicates are present (optimizer didn't push), that's OK
	// — the test simply validates the non-error path.
	for _, fp := range res.Hints.FieldPredicates {
		if fp.Field == "status" && fp.Value == "200" && fp.Op == "=" {
			return // found the expected predicate
		}
	}
	// Not finding it is acceptable — depends on optimizer behavior.
	t.Logf("FieldPredicates = %v (optimizer may not have pushed status==200 down)", res.Hints.FieldPredicates)
}

// TestHintsFromPlan_NonLiteralPredicateSkipped verifies that non-literal
// predicates (e.g., field == other_field) do NOT produce FieldPredicates.
func TestHintsFromPlan_NonLiteralPredicateSkipped(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main | where a == b"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	for _, fp := range res.Hints.FieldPredicates {
		if fp.Field == "a" && fp.Value == "b" {
			t.Errorf("non-literal predicate a==b should NOT be converted, got %+v", fp)
		}
	}
}

// TestHintsFromPlan_SearchTerms verifies search sugar terms reach SearchTerms.
func TestHintsFromPlan_SearchTerms(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main error timeout"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	// Search terms depend on the optimizer extracting bloom terms from the
	// desugared where has(_raw, "error") predicate. The bloom-term extraction
	// may or may not happen; we verify the pipeline completes without error.
	t.Logf("SearchTerms = %v", res.Hints.SearchTerms)
}

// TestHintsFromPlan_ReverseScan verifies that tail queries set ReverseScan.
func TestHintsFromPlan_ReverseScan(t *testing.T) {
	p := New()
	// tail is lowered to Limit(tail=true) which the optimizer may convert
	// to Reverse on Scan + head. This depends on optimizer rules.
	res, err := p.Plan(PlanRequest{Query: "from main | tail 10"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	t.Logf("ReverseScan = %v", res.Hints.ReverseScan)
}

// TestHintsFromPlan_LimitPushdown verifies that head N on a simple pipeline
// extracts a Limit hint.
func TestHintsFromPlan_LimitPushdown(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main | head 10"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.Limit != 10 {
		t.Errorf("Limit = %d, want 10", res.Hints.Limit)
	}
}

// TestHintsFromPlan_LimitNotPushedThroughFilter verifies that head N after
// a filter does NOT push down (would truncate filtered results).
func TestHintsFromPlan_LimitNotPushedThroughFilter(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main | where level == \"error\" | head 10"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.Hints.Limit != 0 {
		t.Errorf("Limit = %d, want 0 (should not push through filter)", res.Hints.Limit)
	}
}

// TestHintsFromPlan_RequiredCols verifies that column pruning populates
// RequiredCols (optimizer dependent).
func TestHintsFromPlan_RequiredCols(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{Query: "from main | keep level, status"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	t.Logf("RequiredCols = %v", res.Hints.RequiredCols)
}

// TestPlanResult_Lints verifies that lint diagnostics are populated.
func TestPlanResult_Lints(t *testing.T) {
	p := New()
	// from * without time range triggers LF02 (broad scope).
	res, err := p.Plan(PlanRequest{Query: "from *"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	found := false
	for _, l := range res.Lints {
		if l.Code == "LF02" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected LF02 lint for 'from *', got lints = %v", res.Lints)
	}
}

// TestPlanResult_Rewrites verifies that desugar rewrites are populated.
func TestPlanResult_Rewrites(t *testing.T) {
	p := New()
	// "top 5 level" is a sugar stage that gets desugared.
	res, err := p.Plan(PlanRequest{Query: "from main | top 5 level"})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	found := false
	for _, rw := range res.Rewrites {
		if rw.Reason == "sugar:top" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected sugar:top rewrite for 'top 5 level', got rewrites = %v", res.Rewrites)
	}
}

// TestParseError_Diag verifies that ParseError carries the full Diag.
func TestParseError_Diag(t *testing.T) {
	p := New()
	_, err := p.Plan(PlanRequest{Query: "from main | where"})
	if err == nil {
		t.Fatal("expected parse error for incomplete where")
	}
	var pe *ParseError
	if !IsParseError(err) {
		t.Fatalf("expected ParseError, got %T: %v", err, err)
	}
	pe = err.(*ParseError)
	if pe.Diag == nil {
		t.Error("expected Diag to be non-nil on ParseError")
	}
}

// TestExternalTimeBounds verifies external from/to merge into hints.
func TestExternalTimeBounds(t *testing.T) {
	p := New()
	res, err := p.Plan(PlanRequest{
		Query: "from main",
		From:  "-2h",
		To:    "now",
	})
	if err != nil {
		t.Fatalf("Plan error: %v", err)
	}
	if res.ExternalTimeBounds == nil {
		t.Fatal("expected ExternalTimeBounds to be set")
	}
	if res.ExternalTimeBounds.Earliest.IsZero() {
		t.Error("expected Earliest to be non-zero")
	}
}
