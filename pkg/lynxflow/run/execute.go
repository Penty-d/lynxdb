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

	source := physical.NewStorageSourceFromMap(events, defaultSrc)

	iter, err := physical.Build(plan, physical.BuildOptions{
		Source:    source,
		BatchSize: opts.BatchSize,
		Now:       now,
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
