// Package physical — source.go implements the production Source callback that
// bridges logical.Scan nodes to the storage engine's in-memory event store.
//
// This is a v1 implementation for the Ephemeral (CLI) storage engine. It
// resolves source names/globs against the engine's index map and converts
// Pushdown hints where the old scan API accepts them.
//
// Pushdown mapping status:
//   - TimeRange/TimeBounds: NOT mapped (Ephemeral engine has no time-range
//     pruning; all events are in memory). When the server engine is integrated,
//     time bounds from Scan.TimeRange will be resolved to absolute time.Time
//     values using BuildOptions.Now and passed to the streaming scan API.
//   - FieldPredicates: NOT mapped (the old pipeline.ServerIndexStore does not
//     accept field predicate pushdown; evaluation happens in the FilterIterator).
//   - BloomTerms: NOT mapped (no bloom filter in ephemeral mode).
//   - RawTerms: NOT mapped (same reason as BloomTerms).
//   - Columns (projection pushdown): NOT mapped. The old IndexStore returns
//     full *event.Event objects; column pruning is done by ProjectIterator.
//   - Reverse: NOT mapped. The old IndexStore does not support reverse scans.
//     Tail is handled by the TailIterator in the pipeline.
package physical

import (
	"fmt"
	"time"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// EphemeralStore is the interface that the Ephemeral storage engine implements.
// It is deliberately narrow: only MaterializeEvents (which returns the in-memory
// event slice for an index) is required. This avoids importing pkg/storage.
type EphemeralStore interface {
	MaterializeEvents(index string) []*event.Event
}

// mapAdapter wraps a raw map[string][]*event.Event so it satisfies EphemeralStore.
type mapAdapter struct {
	events map[string][]*event.Event
}

func (m *mapAdapter) MaterializeEvents(index string) []*event.Event {
	return m.events[index]
}

// NewStorageSource returns a Source callback that resolves Scan nodes against
// the given EphemeralStore. The defaultIndex is used when the scan has no
// explicit source (SourceStar or empty Sources list). The now parameter is used
// to resolve relative time bounds (not yet wired in v1).
func NewStorageSource(store EphemeralStore, defaultIndex string, now time.Time) func(*logical.Scan) (pipeline.Iterator, error) {
	return func(scan *logical.Scan) (pipeline.Iterator, error) {
		events := resolveEvents(store, scan, defaultIndex)
		return pipeline.NewRowScanIterator(eventsToRows(events), pipeline.DefaultBatchSize), nil
	}
}

// NewStorageSourceFromMap is a convenience for tests and the Execute helper:
// wraps a raw event map into a Source callback.
func NewStorageSourceFromMap(events map[string][]*event.Event, defaultIndex string) func(*logical.Scan) (pipeline.Iterator, error) {
	return NewStorageSource(&mapAdapter{events: events}, defaultIndex, time.Now())
}

// resolveEvents collects events from the store based on the Scan's source patterns.
func resolveEvents(store EphemeralStore, scan *logical.Scan, defaultIndex string) []*event.Event {
	if len(scan.Sources) == 0 {
		// No explicit source: scan the default index.
		return store.MaterializeEvents(defaultIndex)
	}

	// Check for star source.
	for _, s := range scan.Sources {
		if s.Kind == ast.SourceStar {
			return store.MaterializeEvents(defaultIndex)
		}
	}

	// Collect from named/glob sources, applying excludes.
	var result []*event.Event
	seen := make(map[string]bool)

	for _, s := range scan.Sources {
		switch s.Kind {
		case ast.SourceName:
			idx := s.Name
			if idx == "" {
				idx = defaultIndex
			}
			if !seen[idx] {
				seen[idx] = true
				result = append(result, store.MaterializeEvents(idx)...)
			}
		case ast.SourceGlob:
			// For ephemeral mode, glob resolution is limited. We try the pattern
			// as a literal name. Full glob resolution across all indices would
			// require an AllIndices() method on the store.
			idx := s.Pattern
			if !seen[idx] {
				seen[idx] = true
				result = append(result, store.MaterializeEvents(idx)...)
			}
		case ast.SourceNegated:
			// Negated sources are excludes — skip for now (would need AllIndices).
		case ast.SourceCTE:
			// CTE sources should have been handled by the builder before calling
			// the Source hook. If we get here, it's an internal error.
			// Return empty rather than panic.
		}
	}

	if len(result) == 0 && !seen[defaultIndex] {
		return store.MaterializeEvents(defaultIndex)
	}

	return result
}

// eventsToRows converts []*event.Event into the []map[string]event.Value format
// that RowScanIterator expects.
func eventsToRows(events []*event.Event) []map[string]event.Value {
	if len(events) == 0 {
		return nil
	}
	rows := make([]map[string]event.Value, len(events))
	for i, ev := range events {
		row := make(map[string]event.Value, len(ev.Fields)+4)

		// Core fields.
		if !ev.Time.IsZero() {
			row["_time"] = event.TimestampValue(ev.Time)
		}
		if ev.Raw != "" {
			row["_raw"] = event.StringValue(ev.Raw)
		}
		if ev.Source != "" {
			row["_source"] = event.StringValue(ev.Source)
		}
		if ev.SourceType != "" {
			row["_sourcetype"] = event.StringValue(ev.SourceType)
		}

		// Extracted fields.
		for k, v := range ev.Fields {
			row[k] = v
		}

		rows[i] = row
	}
	return rows
}

// StorageSourceInfo documents what could and could not be mapped.
var StorageSourceInfo = fmt.Sprintf(
	"v1 pushdown mapping: time_range=NO, field_predicates=NO, bloom_terms=NO, " +
		"raw_terms=NO, columns=NO, reverse=NO. " +
		"Reason: Ephemeral engine holds all events in memory; no pruning API. " +
		"Server engine integration (with RowGroupFilter pushdown) is Phase 5.")
