package usecases

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/lynxbase/lynxdb/pkg/config"
	enginepipeline "github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/model"
	"github.com/lynxbase/lynxdb/pkg/planner"
	"github.com/lynxbase/lynxdb/pkg/server"
)

// QueryService orchestrates query planning and execution.
type QueryService struct {
	planner  planner.Planner
	engine   QueryEngine
	queryCfg atomic.Pointer[config.QueryConfig]
}

// NewQueryService creates a QueryService.
func NewQueryService(p planner.Planner, engine QueryEngine, cfg config.QueryConfig) *QueryService {
	svc := &QueryService{
		planner: p,
		engine:  engine,
	}
	svc.ReloadConfig(cfg)

	return svc
}

// ReloadConfig swaps the live query config snapshot used for new requests.
func (s *QueryService) ReloadConfig(cfg config.QueryConfig) {
	cfgCopy := cfg
	s.queryCfg.Store(&cfgCopy)
}

func (s *QueryService) currentQueryConfig() config.QueryConfig {
	if cfg := s.queryCfg.Load(); cfg != nil {
		return *cfg
	}

	return config.QueryConfig{}
}

// Explain parses and analyses a query without executing it.
func (s *QueryService) Explain(_ context.Context, req ExplainRequest) (*ExplainResult, error) {
	query := req.Query // RFC-002: spl2 normalization removed
	plan, err := s.planner.Plan(planner.PlanRequest{
		Query: query,
		From:  req.From,
		To:    req.To,
	})
	if err != nil {
		var pe *planner.ParseError
		if errors.As(err, &pe) {
			return &ExplainResult{
				IsValid: false,
				Errors: []ExplainError{{
					Message:    pe.Message,
					Suggestion: pe.Suggestion,
				}},
			}, nil
		}

		return nil, err
	}

	var catalogFields []string
	if s.engine != nil {
		catalogFields = s.engine.ListFieldNames()
	}
	stages := annotatePipelineFields(plan.Program, catalogFields)

	// Account for external time bounds when evaluating cost.
	hasTimeBounds := plan.Hints.TimeBounds != nil || plan.ExternalTimeBounds != nil
	cost := "low"
	if !hasTimeBounds && len(plan.Hints.SearchTerms) == 0 {
		cost = "high"
	} else if !hasTimeBounds || len(plan.Hints.SearchTerms) == 0 {
		cost = "medium"
	}

	usesFullScan := !hasTimeBounds && len(plan.Hints.SearchTerms) == 0

	// Build physical plan from optimizer annotations on the AST.
	physPlan := extractPhysicalPlan(plan.Program)

	// Convert optimizer rule details for the explain response.
	var ruleDetails []ExplainRuleDetail
	for _, rd := range plan.RuleDetails {
		ruleDetails = append(ruleDetails, ExplainRuleDetail{
			Name:        rd.Rule,
			Description: "",
			Count:       rd.Count,
		})
	}

	var sourceScope *ExplainSourceScope
	if plan.Hints.SourceScopeType != "" {
		var totalAvailable int
		if s.engine != nil {
			totalAvailable = s.engine.SourceCount()
		}
		sourceScope = &ExplainSourceScope{
			Type:                  plan.Hints.SourceScopeType,
			Sources:               plan.Hints.SourceScopeSources,
			Pattern:               plan.Hints.SourceScopePattern,
			TotalSourcesAvailable: totalAvailable,
		}
	}

	// Extract optimizer diagnostic messages and warnings from AST annotations.
	var optMessages, optWarnings []string
	if plan.Program != nil {
		// RFC-002: annotations removed.
	}

	return &ExplainResult{
		IsValid:  true,
		Errors:   nil,
		Rewrites: plan.Rewrites,
		Parsed: &ExplainParsed{
			Pipeline:          stages,
			ResultType:        string(plan.ResultType),
			EstimatedCost:     cost,
			UsesFullScan:      usesFullScan,
			FieldsRead:        plan.Hints.RequiredCols,
			SearchTerms:       plan.Hints.SearchTerms,
			HasTimeBounds:     hasTimeBounds,
			OptimizerStats:    plan.OptimizerStats,
			PhysicalPlan:      physPlan,
			SourceScope:       sourceScope,
			RangePredicates:   explainRangePredicates(plan.Hints.RangePredicates),
			ParseMS:           float64(plan.ParseDuration.Microseconds()) / 1000,
			OptimizeMS:        float64(plan.OptimizeDuration.Microseconds()) / 1000,
			RuleDetails:       ruleDetails,
			TotalRules:        plan.TotalRules,
			OptimizerMessages: optMessages,
			OptimizerWarnings: optWarnings,
		},
		HasMVAccel: plan.Accel != nil,
	}, nil
}

func explainRangePredicates(preds []model.RangePredicate) []ExplainRangePredicate {
	if len(preds) == 0 {
		return nil
	}
	out := make([]ExplainRangePredicate, 0, len(preds))
	for _, pred := range preds {
		rgStrategy := "zone-map"
		rowStrategy := "per-row"
		if pred.LoweredToBSI {
			rgStrategy = "bsi"
			rowStrategy = "handled_by=bsi"
		}
		out = append(out, ExplainRangePredicate{
			Field:            pred.Field,
			Min:              pred.Min,
			Max:              pred.Max,
			LoweredToBSI:     pred.LoweredToBSI,
			RGFilterStrategy: rgStrategy,
			RowVMStrategy:    rowStrategy,
		})
	}

	return out
}

// commandName returns a human-readable name for a pipeline command.
func commandName(_ logical.Node) string {
	// RFC-002: spl2 command type switch removed.
	// The LynxFlow path uses logical.Node.String() for command names.
	return "unknown"
}

// annotatePipelineFields walks a query's commands sequentially, maintaining a
// running ordered field set, and computes field additions/removals per command.
// The result drives the Lynx Flow sidebar's per-stage field tracking.
func annotatePipelineFields(_ *logical.Plan, _ []string) []PipelineStage {
	// RFC-002: spl2 AST pipeline annotation removed.
	// TODO(RFC-002): implement pipeline stage annotation from logical plan.
	return nil
}

// aggOutputName returns the output field name for an aggregation expression,
// following the same convention as the pipeline builder in pkg/engine/pipeline.
func aggOutputName(_ interface{}) string { return "" }

// truncateDesc truncates a description to maxLen characters, appending "..."
// if truncation occurred.
func truncateDesc(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// diffFields returns elements in 'from' that are not in 'keep'.
func diffFields(from []string, keep []string) []string {
	keepSet := make(map[string]struct{}, len(keep))
	for _, f := range keep {
		keepSet[f] = struct{}{}
	}
	var diff []string
	for _, f := range from {
		if _, ok := keepSet[f]; !ok {
			diff = append(diff, f)
		}
	}
	return diff
}

// declareUnpackFields creates a parser for the given UnpackCommand and returns
// its FieldDeclaration, or nil if the parser cannot be created or does not
// implement FieldDeclarer.
func declareUnpackFields(_ interface{}) interface{} {
	return nil
}

// applyPrefix prepends a prefix (with dot separator) to each field name.
// Returns the original slice unchanged if prefix is empty.
func applyPrefix(prefix string, fields []string) []string {
	if prefix == "" {
		return fields
	}
	result := make([]string, len(fields))
	for i, f := range fields {
		result[i] = prefix + f
	}
	return result
}

// extractPhysicalPlan inspects optimizer annotations on the AST to build
// a PhysicalPlan that describes the runtime execution strategy. This surfaces
// optimizations that are invisible in the logical pipeline stages (e.g.,
// count(*) metadata shortcut, partial aggregation pushdown, topK heap merge).
func extractPhysicalPlan(_ *logical.Plan) *PhysicalPlan {
	// RFC-002: spl2 AST physical plan extraction removed.
	return nil
}

func queryHasRexLiteralPreFilter(_ *logical.Plan) bool {
	// RFC-002: spl2 AST rex prefilter check removed.
	return false
}

// Submit plans and executes a query with sync/hybrid/async dispatch.
func (s *QueryService) Submit(ctx context.Context, req SubmitRequest) (*SubmitResult, error) {
	queryCfg := s.currentQueryConfig()

	plan, err := s.planner.Plan(planner.PlanRequest{
		Query: req.Query,
		From:  req.From,
		To:    req.To,
	})
	if err != nil {
		return nil, err
	}

	// Sync/hybrid queries derive from the caller's context so client disconnect
	// cancels the query. Async queries use Background since they outlive the request.
	queryCtx := ctx
	if req.Mode == QueryModeAsync {
		queryCtx = context.Background()
	}

	// Concurrency limit is enforced atomically inside SubmitQuery (CAS loop).
	// Propagate MV acceleration metadata from the planner if the optimizer
	// rewrote the query to scan a materialized view.
	var accelBy, mvStatus string
	if plan.Accel != nil {
		accelBy = plan.Accel.ViewName
		mvStatus = plan.Accel.Status
	}

	job, err := s.engine.SubmitQuery(queryCtx, server.QueryParams{
		Query:              plan.RawQuery,
		Program:            plan.Program,
		Hints:              plan.Hints,
		ExternalTimeBounds: plan.ExternalTimeBounds,
		SkipResultCache:    plan.SkipResultCache || req.SkipResultCache,
		ResultType:         server.ResultType(plan.ResultType),
		ProfileLevel:       req.Profile,
		ParseDuration:      plan.ParseDuration,
		OptimizeDuration:   plan.OptimizeDuration,
		RuleDetails:        plan.RuleDetails,
		TotalRules:         plan.TotalRules,
		AcceleratedBy:      accelBy,
		MVStatus:           mvStatus,
	})
	if err != nil {
		return nil, err
	}
	job.SetLintOptions(!req.NoLint, req.LintLimit, req.LintFull)

	limit := req.Limit
	if limit <= 0 {
		limit = queryCfg.DefaultResultLimit
	}
	if queryCfg.MaxResultLimit > 0 && limit > queryCfg.MaxResultLimit {
		limit = queryCfg.MaxResultLimit
	}

	// Collect any user-facing warnings from query hints.
	var warnings []string
	if plan.Hints != nil && len(plan.Hints.Warnings) > 0 {
		warnings = plan.Hints.Warnings
	}
	var analysisLints []model.QueryLint
	if !req.NoLint || !req.NoSuggestions {
		// Lints come from the planner (sema + lint passes on the desugared AST).
		analysisLints = model.PrepareQueryLints(plan.Lints)
	}
	var lints []model.QueryLint
	if !req.NoLint {
		lints = applyLintOutputLimit(analysisLints, req.LintLimit, req.LintFull)
	}
	var suggestions []model.QuerySuggestion
	if !req.NoSuggestions {
		suggestions = model.SuggestionsFromLints(analysisLints)
	}
	// Merge planner rewrites (desugar) with request rewrites (user-facing normalizer).
	allRewrites := append([]model.QueryRewrite(nil), plan.Rewrites...)
	allRewrites = append(allRewrites, req.Rewrites...)
	job.SetAdvisoryMetadata(warnings, lints, suggestions, allRewrites)

	// buildSync builds an inline result from the (completed) job, attaching the
	// advisory metadata consistently across every synchronous return path.
	buildSync := func() *SubmitResult {
		r := buildSyncResult(job, limit, req.Offset)
		r.Warnings = warnings
		r.Lints = lints
		r.Suggestions = suggestions
		r.Rewrites = allRewrites

		return r
	}

	// promoteToAsync hands back a job handle after the wait budget expires. It
	// first re-checks job.Done() without blocking: select picks randomly among
	// ready cases, so a job that completed at the same instant the timer fired
	// would otherwise return a 202 and force the client to poll for a result
	// that is already available.
	promoteToAsync := func() *SubmitResult {
		select {
		case <-job.Done():
			return buildSync()
		default:
		}
		// Detach from the HTTP context so the job survives client disconnect.
		job.Detach()

		return buildJobHandle(job)
	}

	switch req.Mode {
	case QueryModeSync:
		syncTimeout := queryCfg.SyncTimeout
		if syncTimeout == 0 {
			syncTimeout = 30 * time.Second
		}
		timer := time.NewTimer(syncTimeout)
		defer timer.Stop()
		select {
		case <-job.Done():
			return buildSync(), nil
		case <-timer.C:
			return promoteToAsync(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}

	case QueryModeHybrid:
		timer := time.NewTimer(req.Wait)
		defer timer.Stop()
		select {
		case <-job.Done():
			return buildSync(), nil
		case <-timer.C:
			return promoteToAsync(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}

	case QueryModeAsync:
		return buildJobHandle(job), nil
	}

	return buildJobHandle(job), nil
}

// Stream plans a query and returns a streaming iterator.
func (s *QueryService) Stream(ctx context.Context, req StreamRequest) (enginepipeline.Iterator, server.StreamingStats, error) {
	plan, err := s.planner.Plan(planner.PlanRequest{
		Query: req.Query,
		From:  req.From,
		To:    req.To,
	})
	if err != nil {
		return nil, server.StreamingStats{}, err
	}

	return s.engine.BuildStreamingPipeline(ctx, plan.Program, plan.Hints, plan.ExternalTimeBounds)
}

// Histogram computes event count buckets over a time range.
// It uses segment metadata (zone maps) to estimate bucket counts without
// loading all events into memory, then scans memtable events individually.
func (s *QueryService) Histogram(ctx context.Context, req HistogramRequest) (*HistogramResult, error) {
	now := time.Now()
	fromStr := req.From
	if fromStr == "" {
		fromStr = "-1h"
	}
	toStr := req.To
	if toStr == "" {
		toStr = "now"
	}

	fromTime, err := ParseTimeParam(fromStr, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFrom, err)
	}
	toTime, err := ParseTimeParam(toStr, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTo, err)
	}

	bucketCount := req.Buckets
	if bucketCount <= 0 {
		bucketCount = 60
	}

	totalDuration := toTime.Sub(fromTime)
	if totalDuration <= 0 {
		return nil, ErrFromBeforeTo
	}
	intervalNs := totalDuration.Nanoseconds() / int64(bucketCount)
	interval := SnapInterval(time.Duration(intervalNs))

	srvBuckets := make([]server.HistogramBucket, bucketCount)
	for i := 0; i < bucketCount; i++ {
		srvBuckets[i] = server.HistogramBucket{
			Time: fromTime.Add(time.Duration(i) * interval),
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	total, err := s.engine.HistogramFromMetadata(ctx, req.Index, fromTime, toTime, interval, srvBuckets)
	if err != nil {
		return nil, err
	}

	buckets := make([]HistogramBucket, len(srvBuckets))
	for i, b := range srvBuckets {
		buckets[i] = HistogramBucket{Time: b.Time, Count: b.Count}
	}

	return &HistogramResult{
		Interval: interval.String(),
		Buckets:  buckets,
		Total:    total,
	}, nil
}

// GroupedHistogram computes event count buckets grouped by a field value.
// Uses ReadEventsWithColumns to extract both timestamps and the grouping field.
func (s *QueryService) GroupedHistogram(ctx context.Context, req HistogramRequest) (*GroupedHistogramResult, error) {
	now := time.Now()
	fromStr := req.From
	if fromStr == "" {
		fromStr = "-1h"
	}
	toStr := req.To
	if toStr == "" {
		toStr = "now"
	}

	fromTime, err := ParseTimeParam(fromStr, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidFrom, err)
	}
	toTime, err := ParseTimeParam(toStr, now)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTo, err)
	}

	bucketCount := req.Buckets
	if bucketCount <= 0 {
		bucketCount = 60
	}

	totalDuration := toTime.Sub(fromTime)
	if totalDuration <= 0 {
		return nil, ErrFromBeforeTo
	}
	intervalNs := totalDuration.Nanoseconds() / int64(bucketCount)
	interval := SnapInterval(time.Duration(intervalNs))

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	srvBuckets, total, err := s.engine.HistogramByFieldFromMetadata(
		ctx, req.Index, fromTime, toTime, interval, req.GroupBy, bucketCount,
	)
	if err != nil {
		return nil, err
	}

	buckets := make([]GroupedHistogramBucket, len(srvBuckets))
	for i, b := range srvBuckets {
		buckets[i] = GroupedHistogramBucket{Time: b.Time, Counts: b.Counts}
	}

	return &GroupedHistogramResult{
		Interval: interval.String(),
		Buckets:  buckets,
		Total:    total,
	}, nil
}

// FieldValues returns the top values for a given field name.
// Uses streaming scan with context cancellation instead of loading all events.
func (s *QueryService) FieldValues(ctx context.Context, req FieldValuesRequest) (*FieldValuesResult, error) {
	now := time.Now()
	var from, to time.Time
	if req.From != "" {
		if t, err := ParseTimeParam(req.From, now); err == nil {
			from = t
		}
	}
	if req.To != "" {
		if t, err := ParseTimeParam(req.To, now); err == nil {
			to = t
		}
	}

	srvResult, err := s.engine.FieldValuesFromMetadata(ctx, req.FieldName, req.Index, from, to, req.Limit)
	if err != nil {
		return nil, err
	}

	values := make([]FieldValue, len(srvResult.Values))
	for i, v := range srvResult.Values {
		values[i] = FieldValue{
			Value:   v.Value,
			Count:   v.Count,
			Percent: v.Percent,
		}
	}

	return &FieldValuesResult{
		Field:       srvResult.Field,
		Values:      values,
		UniqueCount: srvResult.UniqueCount,
		TotalCount:  srvResult.TotalCount,
	}, nil
}

// ListSources returns all distinct event sources.
// Uses streaming scan with context cancellation instead of loading all events.
func (s *QueryService) ListSources(ctx context.Context) (*SourcesResult, error) {
	srvResult, err := s.engine.ListSourcesFromMetadata(ctx, "", time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}

	result := make([]SourceInfo, len(srvResult.Sources))
	for i, si := range srvResult.Sources {
		result[i] = SourceInfo{
			Name:       si.Name,
			EventCount: si.EventCount,
			FirstEvent: si.FirstEvent,
			LastEvent:  si.LastEvent,
		}
	}

	return &SourcesResult{Sources: result}, nil
}

func buildSyncResult(job *server.SearchJob, limit, offset int) *SubmitResult {
	snap := job.Snapshot()
	if snap.Status == "error" {
		return &SubmitResult{
			Done:      true,
			Error:     snap.Error,
			ErrorCode: snap.ErrorCode,
			QueryID:   snap.ID,
		}
	}

	return &SubmitResult{
		Done:       true,
		ResultType: snap.ResultType,
		Results:    snap.Results,
		Stats:      snap.Stats,
		QueryID:    snap.ID,
	}
}

const defaultLintOutputLimit = 5

func applyLintOutputLimit(lints []model.QueryLint, limit int, full bool) []model.QueryLint {
	if full || len(lints) == 0 {
		return lints
	}
	if limit <= 0 {
		limit = defaultLintOutputLimit
	}
	if len(lints) <= limit {
		return lints
	}

	return append([]model.QueryLint(nil), lints[:limit]...)
}

func buildJobHandle(job *server.SearchJob) *SubmitResult {
	snap := job.Snapshot()
	r := &SubmitResult{
		Done:        false,
		JobID:       job.ID,
		Status:      "running",
		Warnings:    snap.Warnings,
		Lints:       snap.Lints,
		Suggestions: snap.Suggestions,
		Rewrites:    snap.Rewrites,
	}
	if p := job.Progress.Load(); p != nil {
		r.Progress = p
	}

	return r
}
