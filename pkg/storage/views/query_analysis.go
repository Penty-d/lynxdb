package views

import "fmt"

// AnalyzeQuery is the legacy SPL2 analysis entry point. Since SPL2 has been
// removed (RFC-002), this always returns an error directing users to migrate
// their views to LynxFlow.
func AnalyzeQuery(_ string) error {
	return fmt.Errorf("views.AnalyzeQuery: SPL2 query language has been removed; migrate this view to LynxFlow with `lynxdb mv migrate <name> --query '<lynxflow query>'`")
}

// MVAutoCountAlias is the alias used for auto-injected count aggregations.
const MVAutoCountAlias = "__mv_auto_count"
