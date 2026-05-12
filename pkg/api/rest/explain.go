package rest

import (
	"net/http"

	"github.com/lynxbase/lynxdb/pkg/spl2"
	"github.com/lynxbase/lynxdb/pkg/usecases"
)

func (s *Server) handleQueryExplain(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		q = r.URL.Query().Get("query")
	}
	if q == "" {
		respondError(w, ErrCodeValidationError, http.StatusBadRequest, "q parameter is required")

		return
	}
	if !s.checkQueryLength(w, q) {
		return
	}

	// EXPLAIN ANALYZE: execute the query with profiling and return plan + stats.
	if r.URL.Query().Get("analyze") == "true" {
		s.handleExplainAnalyze(w, r, q)

		return
	}

	result, err := s.queryService.Explain(r.Context(), usecases.ExplainRequest{
		Query: q,
		From:  r.URL.Query().Get("from"),
		To:    r.URL.Query().Get("to"),
	})
	if err != nil {
		handlePlanError(w, err)

		return
	}

	respondExplainResult(w, result)
}

// handleExplainAnalyze runs both EXPLAIN and actual execution with profiling,
// returning the logical plan alongside actual execution statistics.
func (s *Server) handleExplainAnalyze(w http.ResponseWriter, r *http.Request, q string) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")

	explainResult, err := s.queryService.Explain(r.Context(), usecases.ExplainRequest{
		Query: q, From: from, To: to,
	})
	if err != nil {
		handlePlanError(w, err)

		return
	}
	if !explainResult.IsValid {
		respondExplainResult(w, explainResult)

		return
	}

	// Execute with full profiling.
	normalizedQuery, rewrites := spl2.NormalizeQueryWithRewrites(q)
	submitResult, err := s.queryService.Submit(r.Context(), usecases.SubmitRequest{
		Query:    normalizedQuery,
		From:     from,
		To:       to,
		Mode:     usecases.QueryModeSync,
		Profile:  "full",
		Rewrites: rewrites,
	})
	if err != nil {
		handlePlanError(w, err)

		return
	}

	if submitResult.Done {
		applyAnalyzedRangePredicates(explainResult, submitResult.Stats.RangePredicates)
	}

	// Build the combined response: plan + actual execution stats.
	resp := buildExplainResponse(explainResult)
	if submitResult.Done {
		ms := searchStatsToMeta(&submitResult.Stats)
		resp["execution"] = ms
	}

	respondData(w, http.StatusOK, resp)
}

func applyAnalyzedRangePredicates(result *usecases.ExplainResult, preds []spl2.RangePredicate) {
	if result == nil || result.Parsed == nil || len(preds) == 0 {
		return
	}
	result.Parsed.RangePredicates = make([]usecases.ExplainRangePredicate, 0, len(preds))
	for _, pred := range preds {
		rgStrategy := "zone-map"
		rowStrategy := "per-row"
		if pred.LoweredToBSI {
			rgStrategy = "bsi"
			rowStrategy = "handled_by=bsi"
		}
		result.Parsed.RangePredicates = append(result.Parsed.RangePredicates, usecases.ExplainRangePredicate{
			Field:            pred.Field,
			Min:              pred.Min,
			Max:              pred.Max,
			LoweredToBSI:     pred.LoweredToBSI,
			RGFilterStrategy: rgStrategy,
			RowVMStrategy:    rowStrategy,
		})
	}
}

// respondExplainResult writes the standard explain response.
func respondExplainResult(w http.ResponseWriter, result *usecases.ExplainResult) {
	if !result.IsValid {
		errs := make([]map[string]interface{}, len(result.Errors))
		for i, e := range result.Errors {
			errs[i] = map[string]interface{}{
				"message":    e.Message,
				"suggestion": e.Suggestion,
			}
		}
		body := map[string]interface{}{
			"is_valid": false,
			"errors":   errs,
		}
		if len(result.Rewrites) > 0 {
			body["rewrites"] = result.Rewrites
		}
		respondData(w, http.StatusOK, body)

		return
	}

	respondData(w, http.StatusOK, buildExplainResponse(result))
}

// buildExplainResponse constructs the explain JSON response from an ExplainResult.
func buildExplainResponse(result *usecases.ExplainResult) map[string]interface{} {
	stages := make([]map[string]interface{}, len(result.Parsed.Pipeline))
	for i, s := range result.Parsed.Pipeline {
		stageObj := map[string]interface{}{
			"command": s.Command,
		}
		if s.Description != "" {
			stageObj["description"] = s.Description
		}
		if len(s.FieldsAdded) > 0 {
			stageObj["fields_added"] = s.FieldsAdded
		}
		if len(s.FieldsRemoved) > 0 {
			stageObj["fields_removed"] = s.FieldsRemoved
		}
		if len(s.FieldsOut) > 0 {
			stageObj["fields_out"] = s.FieldsOut
		}
		if len(s.FieldsOptional) > 0 {
			stageObj["fields_optional"] = s.FieldsOptional
		}
		if s.FieldsUnknown {
			stageObj["fields_unknown"] = true
		}
		stages[i] = stageObj
	}

	parsed := map[string]interface{}{
		"pipeline":        stages,
		"result_type":     result.Parsed.ResultType,
		"estimated_cost":  result.Parsed.EstimatedCost,
		"uses_full_scan":  result.Parsed.UsesFullScan,
		"fields_read":     result.Parsed.FieldsRead,
		"search_terms":    result.Parsed.SearchTerms,
		"has_time_bounds": result.Parsed.HasTimeBounds,
	}
	if len(result.Parsed.OptimizerStats) > 0 {
		parsed["optimizer_stats"] = result.Parsed.OptimizerStats
	}
	if result.Parsed.PhysicalPlan != nil {
		parsed["physical_plan"] = result.Parsed.PhysicalPlan
	}
	if result.Parsed.ParseMS > 0 {
		parsed["parse_ms"] = result.Parsed.ParseMS
	}
	if result.Parsed.OptimizeMS > 0 {
		parsed["optimize_ms"] = result.Parsed.OptimizeMS
	}
	if result.Parsed.TotalRules > 0 {
		parsed["total_rules"] = result.Parsed.TotalRules
	}
	if len(result.Parsed.RuleDetails) > 0 {
		rules := make([]map[string]interface{}, len(result.Parsed.RuleDetails))
		for i, rd := range result.Parsed.RuleDetails {
			rules[i] = map[string]interface{}{
				"name":        rd.Name,
				"description": rd.Description,
				"count":       rd.Count,
			}
		}
		parsed["optimizer_rules"] = rules
	}
	if result.Parsed.SourceScope != nil {
		scope := map[string]interface{}{
			"type": result.Parsed.SourceScope.Type,
		}
		if len(result.Parsed.SourceScope.Sources) > 0 {
			scope["resolved_sources"] = result.Parsed.SourceScope.Sources
		}
		if result.Parsed.SourceScope.Pattern != "" {
			scope["pattern"] = result.Parsed.SourceScope.Pattern
		}
		if result.Parsed.SourceScope.TotalSourcesAvailable > 0 {
			scope["total_sources_available"] = result.Parsed.SourceScope.TotalSourcesAvailable
		}
		parsed["source_scope"] = scope
	}
	if len(result.Parsed.RangePredicates) > 0 {
		preds := make([]map[string]interface{}, 0, len(result.Parsed.RangePredicates))
		for _, pred := range result.Parsed.RangePredicates {
			p := map[string]interface{}{
				"field":              pred.Field,
				"rg_filter_strategy": pred.RGFilterStrategy,
				"row_vm_strategy":    pred.RowVMStrategy,
			}
			if pred.Min != "" {
				p["min"] = pred.Min
			}
			if pred.Max != "" {
				p["max"] = pred.Max
			}
			if pred.LoweredToBSI {
				p["lowered_to_bsi"] = true
			}
			preds = append(preds, p)
		}
		parsed["range_predicates"] = preds
	}

	if len(result.Parsed.OptimizerMessages) > 0 {
		parsed["optimizer_messages"] = result.Parsed.OptimizerMessages
	}
	if len(result.Parsed.OptimizerWarnings) > 0 {
		parsed["optimizer_warnings"] = result.Parsed.OptimizerWarnings
	}

	resp := map[string]interface{}{
		"is_valid": true,
		"parsed":   parsed,
		"errors":   []interface{}{},
		"acceleration": map[string]interface{}{
			"available": result.HasMVAccel,
		},
	}
	if len(result.Rewrites) > 0 {
		resp["rewrites"] = result.Rewrites
	}

	return resp
}
