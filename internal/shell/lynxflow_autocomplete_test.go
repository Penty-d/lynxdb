package shell

import (
	"strings"
	"testing"
)

// TestLynxFlowOperatorNames verifies the LynxFlow registry operators are exposed.
func TestLynxFlowOperatorNames(t *testing.T) {
	names := LynxFlowOperatorNames()
	if len(names) == 0 {
		t.Fatal("expected non-empty operator names from LynxFlow registry")
	}

	// Check a few core operators that must be present.
	required := []string{"extend", "keep", "drop", "describe", "explode"}
	for _, req := range required {
		found := false
		for _, name := range names {
			if name == req {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("operator %q not found in LynxFlow registry", req)
		}
	}
}

// TestLynxFlowFunctionNames verifies the LynxFlow registry functions are exposed.
func TestLynxFlowFunctionNames(t *testing.T) {
	names := LynxFlowFunctionNames()
	if len(names) == 0 {
		t.Fatal("expected non-empty function names from LynxFlow registry")
	}

	// Check a few functions that must be present.
	required := []string{"has", "contains", "exists", "glob"}
	for _, req := range required {
		found := false
		for _, name := range names {
			if name == req {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("function %q not found in LynxFlow registry", req)
		}
	}
}

// TestLynxFlowAggregateNames verifies the LynxFlow registry aggregates are exposed.
func TestLynxFlowAggregateNames(t *testing.T) {
	names := LynxFlowAggregateNames()
	if len(names) == 0 {
		t.Fatal("expected non-empty aggregate names from LynxFlow registry")
	}

	required := []string{"count", "avg", "p95", "dc"}
	for _, req := range required {
		found := false
		for _, name := range names {
			if name == req {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("aggregate %q not found in LynxFlow registry", req)
		}
	}
}

// TestCompleter_PipePosition_IncludesExtend verifies that after | the completer
// suggests "extend" (a LynxFlow-only operator not present in SPL2).
func TestCompleter_PipePosition_IncludesExtend(t *testing.T) {
	c := NewCompleter()
	items := c.SuggestAll("from main | ext")

	found := false
	for _, item := range items {
		if strings.ToLower(item.Text) == "extend" {
			found = true
			break
		}
	}
	if !found {
		var names []string
		for _, item := range items {
			names = append(names, item.Text)
		}
		t.Errorf("expected 'extend' in pipe completions, got: %v", names)
	}
}

// TestCompleter_FunctionPosition_IncludesHas verifies that in eval/where
// context the completer suggests "has" (a LynxFlow-only function).
func TestCompleter_FunctionPosition_IncludesHas(t *testing.T) {
	c := NewCompleter()
	items := c.SuggestAll("from main | where ha")

	found := false
	for _, item := range items {
		if strings.ToLower(item.Text) == "has" {
			found = true
			break
		}
	}
	if !found {
		var names []string
		for _, item := range items {
			names = append(names, item.Text)
		}
		t.Errorf("expected 'has' in function completions, got: %v", names)
	}
}

// TestCompleter_MergedDeduped verifies that shared names between SPL2 and LynxFlow
// are not duplicated.
func TestCompleter_MergedDeduped(t *testing.T) {
	c := NewCompleter()
	items := c.SuggestAll("from main | stat")

	count := 0
	for _, item := range items {
		if strings.ToLower(item.Text) == "stats" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("found 'stats' %d times in completions, expected at most 1", count)
	}
}
