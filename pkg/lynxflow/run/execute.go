// Package run provides the end-to-end LynxFlow query execution helper.
//
// Execute is a thin orchestrator: parse -> desugar -> lower -> optimize ->
// build -> drain. It lives in pkg/lynxflow/run rather than in pkg/logical/physical
// because it imports the LynxFlow parser, desugar, sema, and optimizer packages.
// Placing it in physical would create a circular dependency (physical -> parser)
// or force physical to import the full LynxFlow frontend.
//
// This package is the primary entry point for tests and will later be exposed
// as the `lynxdb query --engine=lynxflow` CLI surface.
package run

import (
	"context"
	"fmt"
	"time"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/logical/explain"
	"github.com/lynxbase/lynxdb/pkg/logical/opt"
	"github.com/lynxbase/lynxdb/pkg/logical/physical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/desugar"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/parser"
)

// Options configures Execute behavior.
type Options struct {
	// DefaultSource is the index name used when the query has no explicit
	// from clause (e.g. "main"). Empty defaults to "main".
	DefaultSource string

	// Now is the reference time for resolving relative time bounds.
	// Zero means time.Now().
	Now time.Time

	// BatchSize controls the batch size for the pipeline iterator.
	// Zero means pipeline.DefaultBatchSize.
	BatchSize int

	// ScanStats, when non-nil, collects scan-level statistics for
	// observability and testing (e.g., events scanned, events filtered,
	// parts skipped by inverted index).
	ScanStats *physical.ScanStats

	// TeeEnabled allows tee sinks to write files. Per decision D32 this is
	// only set by the operator-controlled CLI file/pipe mode; server-mode
	// queries must leave it false so user-supplied queries cannot write
	// arbitrary files on the server.
	TeeEnabled bool
}

func (o *Options) defaultSource() string {
	if o.DefaultSource != "" {
		return o.DefaultSource
	}
	return "main"
}

// Execute parses a LynxFlow query, runs it through the full pipeline against
// the provided event store, and returns the result rows.
//
// The event store is a map[string][]*event.Event keyed by index name. For
// file-mode CLI queries, all events are under the "main" key.
//
// The pipeline: parse -> desugar(defaultSource) -> lower -> optimize -> build -> drain.
func Execute(ctx context.Context, query string, events map[string][]*event.Event, opts Options) ([]map[string]event.Value, error) {
	defaultSrc := opts.defaultSource()

	// 1. Parse
	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("lynxflow.Execute: parse: %s", d.Message)
		}
	}

	// 2. Desugar
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: defaultSrc})

	// 3. Lower
	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: defaultSrc})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("lynxflow.Execute: lower: %s", d.Message)
		}
	}

	// 4. Optimize
	plan, _ = opt.Optimize(plan)

	// 5. Build
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	source := physical.NewStorageSourceFromMapWithStats(events, defaultSrc, opts.ScanStats)

	iter, err := physical.Build(plan, physical.BuildOptions{
		Source:     source,
		BatchSize:  opts.BatchSize,
		Now:        now,
		TeeEnabled: opts.TeeEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("lynxflow.Execute: build: %w", err)
	}

	// 6. Drain
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := pipeline.CollectAll(ctx, iter)
	if err != nil {
		return nil, fmt.Errorf("lynxflow.Execute: execute: %w", err)
	}

	return rows, nil
}

// ExecuteWithSource is like Execute but uses a custom Source callback instead
// of the default ephemeral store. This enables testing with disk-backed parts.
func ExecuteWithSource(ctx context.Context, query string, source func(*logical.Scan) (pipeline.Iterator, error), opts Options) ([]map[string]event.Value, error) {
	defaultSrc := opts.defaultSource()

	// 1. Parse
	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("lynxflow.ExecuteWithSource: parse: %s", d.Message)
		}
	}

	// 2. Desugar
	desugared, _ := desugar.Desugar(q, desugar.Options{DefaultSource: defaultSrc})

	// 3. Lower
	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: defaultSrc})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			return nil, fmt.Errorf("lynxflow.ExecuteWithSource: lower: %s", d.Message)
		}
	}

	// 4. Optimize
	plan, _ = opt.Optimize(plan)

	// 5. Build
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	iter, err := physical.Build(plan, physical.BuildOptions{
		Source:     source,
		BatchSize:  opts.BatchSize,
		Now:        now,
		TeeEnabled: opts.TeeEnabled,
	})
	if err != nil {
		return nil, fmt.Errorf("lynxflow.ExecuteWithSource: build: %w", err)
	}

	// 6. Drain
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := pipeline.CollectAll(ctx, iter)
	if err != nil {
		return nil, fmt.Errorf("lynxflow.ExecuteWithSource: execute: %w", err)
	}

	return rows, nil
}

// ExecuteExplain parses, desugars, lowers, and optimizes a LynxFlow query,
// then renders the EXPLAIN tree WITHOUT executing it. This is the
// parse -> desugar -> lower -> optimize -> render path.
func ExecuteExplain(query string, opts Options) (string, error) {
	plan, info, err := prepareExplain(query, opts)
	if err != nil {
		return "", err
	}
	return explain.Render(plan, info, nil), nil
}

// ExecuteAnalyze parses, desugars, lowers, optimizes, builds (with
// instrumentation), executes, and renders the EXPLAIN ANALYZE tree with
// per-node rows/batches/wall-time statistics.
func ExecuteAnalyze(ctx context.Context, query string, events map[string][]*event.Event, opts Options) (string, error) {
	plan, info, err := prepareExplain(query, opts)
	if err != nil {
		return "", err
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	source := physical.NewStorageSourceFromMap(events, opts.defaultSource())
	collect := make(map[logical.Node]*explain.NodeStats)

	iter, err := physical.Build(plan, physical.BuildOptions{
		Source:     source,
		BatchSize:  opts.BatchSize,
		Now:        now,
		Collect:    collect,
		TeeEnabled: opts.TeeEnabled,
	})
	if err != nil {
		return "", fmt.Errorf("lynxflow.ExecuteAnalyze: build: %w", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	// Drain the iterator to collect stats.
	_, err = pipeline.CollectAll(ctx, iter)
	if err != nil {
		return "", fmt.Errorf("lynxflow.ExecuteAnalyze: execute: %w", err)
	}

	return explain.Render(plan, info, collect), nil
}

// prepareExplain runs the front-end pipeline (parse -> desugar -> lower ->
// optimize) and returns the plan and EXPLAIN metadata.
func prepareExplain(query string, opts Options) (*logical.Plan, explain.Info, error) {
	defaultSrc := opts.defaultSource()

	q, diags := parser.Parse(query)
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return nil, explain.Info{}, fmt.Errorf("lynxflow.Explain: parse: %s", d.Message)
		}
	}

	desugared, rewrites := desugar.Desugar(q, desugar.Options{DefaultSource: defaultSrc})

	plan, lowerDiags := logical.Lower(desugared, logical.Options{DefaultSource: defaultSrc})
	for _, d := range lowerDiags {
		if d.Severity == parser.SeverityError {
			return nil, explain.Info{}, fmt.Errorf("lynxflow.Explain: lower: %s", d.Message)
		}
	}

	plan, applied := opt.Optimize(plan)

	info := explain.Info{
		Rewrites: rewrites,
		Applied:  applied,
	}
	return plan, info, nil
}
