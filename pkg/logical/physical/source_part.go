// source_part.go — Part-backed Source callback for disk-based storage.
//
// NewPartSource creates a Source that reads from .lsg part files using
// the segment reader's inverted index, bloom filter, and predicate APIs.
// This bridges the LynxFlow v2 logical.Scan pushdown hints to the same
// storage mechanisms used by the old SPL2 path (server.readSegmentEvents).
//
// Pushdown mapping (Part-backed):
//
//   - RawTerms: each term is looked up in the part's inverted index
//     (SerializedIndex.Search). All terms are AND'd into a searchBitmap.
//     If the resulting bitmap is empty, the part is skipped entirely.
//     This is the SAME mechanism as the old path's SearchTerms -> searchBitmap.
//
//   - BloomTerms: same inverted index path. BloomTerms are lowercased
//     literal substrings extracted by the optimizer from contains/glob/matches.
//     They are less selective than RawTerms (may produce false positives at
//     the row level) but still enable part-level skip when the inverted
//     index returns an empty bitmap.
//
//   - FieldPredicates: converted to []segment.Predicate{Field, Op, Value string}
//     and passed to ReadEventsFiltered. The segment reader applies per-column
//     bloom, zone map, and dict-filter optimizations internally.
//
//   - Columns: passed as the columns []string parameter to
//     ReadEventsFiltered/ReadEventsByBitmap for column projection.
//
//   - TimeBounds: enforced via segment.QueryHints.MinTime/MaxTime. Both
//     Scan.TimeRange (bracket syntax) and Scan.Pushdown.TimeBounds
//     (optimizer-consumed _time predicates) are resolved to absolute
//     time.Time using the now parameter and passed to ReadEventsWithHints,
//     enabling row-group-level pruning. This is critical because the
//     optimizer REMOVES _time conjuncts from the Filter node.
//
//   - Reverse: NOT mapped (segment reader does not support reverse scan).
package physical

import (
	"fmt"
	"time"

	"github.com/RoaringBitmap/roaring"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
	"github.com/lynxbase/lynxdb/pkg/storage/segment"
	"github.com/lynxbase/lynxdb/pkg/storage/segment/index"
)

// PartHandle bundles a segment reader with its associated indexes.
// Tests create these by writing parts via part.Writer and opening them
// via segment.OpenSegmentFile.
type PartHandle struct {
	Reader      *segment.Reader
	InvertedIdx *index.SerializedIndex // may be nil if part has no inverted index
	Index       string                 // index name this part belongs to
}

// NewPartSource returns a Source callback that reads from disk-backed parts.
// The parts slice contains all available parts; the callback selects parts
// matching the scan's source and applies pushdown hints.
//
// The now parameter is used to resolve relative time bounds (e.g. -1h) to
// absolute time.Time values. The resolved bounds are passed as
// segment.QueryHints.MinTime/MaxTime for row-group-level time pruning.
// This is critical because the optimizer REMOVES _time conjuncts from the
// Filter node (consumed=true), and bracket ranges have no compensating Filter.
//
// When stats is non-nil, scan statistics are accumulated for observability.
func NewPartSource(parts []PartHandle, defaultIndex string, now time.Time, stats *ScanStats) func(*logical.Scan) (pipeline.Iterator, error) {
	return func(scan *logical.Scan) (pipeline.Iterator, error) {
		// Resolve which indexes to scan.
		targetIndexes := resolveTargetIndexes(scan, defaultIndex)

		pd := scan.Pushdown

		// Resolve time bounds for RG-level pruning.
		tb := mergedTimeBounds(scan)
		minTime, maxTime := resolveTimeBoundsToAbsolute(tb, now)

		// Resolve columns for projection.
		var columns []string
		if len(pd.Columns) > 0 {
			columns = pd.Columns
		}

		// Build segment predicates from FieldPredicates.
		var segPreds []segment.Predicate
		for _, expr := range pd.FieldPredicates {
			if sp, ok := exprToSegmentPredicate(expr); ok {
				segPreds = append(segPreds, sp)
			}
		}

		// Merge RawTerms + BloomTerms for inverted index lookup.
		// Both are lowercased tokens suitable for inverted index search.
		var searchTerms []string
		searchTerms = append(searchTerms, pd.RawTerms...)
		searchTerms = append(searchTerms, pd.BloomTerms...)

		hasTimeHints := minTime != nil || maxTime != nil

		var allRows []map[string]event.Value

		for i := range parts {
			p := &parts[i]

			// Filter by index name.
			if !matchesIndex(p.Index, targetIndexes) {
				continue
			}

			if stats != nil {
				stats.PartsTotal.Add(1)
				stats.TotalEvents.Add(p.Reader.EventCount())
			}

			// Inverted index search: build a bitmap of candidate event IDs.
			var searchBitmap *roaring.Bitmap
			skipped := false

			if len(searchTerms) > 0 && p.InvertedIdx != nil {
				for j, term := range searchTerms {
					bm, err := p.InvertedIdx.Search(term)
					if err != nil {
						continue // degrade gracefully
					}
					if j == 0 {
						searchBitmap = bm
					} else {
						searchBitmap.And(bm)
					}
					if searchBitmap.GetCardinality() == 0 {
						break
					}
				}
				if searchBitmap != nil && searchBitmap.GetCardinality() == 0 {
					// Inverted index proves no events match — skip this part.
					skipped = true
					if stats != nil {
						stats.PartsSkipped.Add(1)
					}
				}
			}

			if skipped {
				continue
			}

			// Read events with the best available method.
			// When time bounds are available and we would otherwise fall back
			// to the unfiltered ReadEvents path, use ReadEventsWithHints to
			// enable row-group-level time pruning.
			var events []*event.Event
			var err error

			if len(segPreds) > 0 {
				events, err = p.Reader.ReadEventsFiltered(segPreds, searchBitmap, columns)
			} else if searchBitmap != nil {
				events, err = p.Reader.ReadEventsByBitmap(searchBitmap, columns)
			} else if hasTimeHints {
				events, err = p.Reader.ReadEventsWithHints(segment.QueryHints{
					MinTime: minTime,
					MaxTime: maxTime,
					Columns: columns,
				})
			} else {
				events, err = p.Reader.ReadEvents()
			}
			if err != nil {
				return nil, fmt.Errorf("physical.PartSource: read part %q: %w", p.Index, err)
			}

			// When time hints are set but we used a code path that does not
			// natively apply them (ReadEventsFiltered, ReadEventsByBitmap),
			// filter events post-read to ensure correctness. The optimizer
			// REMOVES time conjuncts from the Filter, so we must enforce here.
			if hasTimeHints && (len(segPreds) > 0 || searchBitmap != nil) {
				events = filterEventsByAbsoluteTime(events, minTime, maxTime)
			}

			if stats != nil {
				stats.FilteredEvents.Add(int64(len(events)))
			}

			rows := eventsToRows(events)
			allRows = append(allRows, rows...)
		}

		return pipeline.NewRowScanIterator(allRows, pipeline.DefaultBatchSize), nil
	}
}

// filterEventsByAbsoluteTime filters events by already-resolved absolute time
// bounds. Events without a _time field are conservatively kept.
func filterEventsByAbsoluteTime(events []*event.Event, minTime, maxTime *time.Time) []*event.Event {
	if minTime == nil && maxTime == nil {
		return events
	}
	result := make([]*event.Event, 0, len(events))
	for _, ev := range events {
		if ev.Time.IsZero() {
			result = append(result, ev)
			continue
		}
		if minTime != nil && ev.Time.Before(*minTime) {
			continue
		}
		if maxTime != nil && ev.Time.After(*maxTime) {
			continue
		}
		result = append(result, ev)
	}
	return result
}

// exprToSegmentPredicate converts a LynxFlow ast.Expr (field op literal) into
// a segment.Predicate. Returns ok=false if the expression is not a simple
// field comparison.
func exprToSegmentPredicate(expr ast.Expr) (segment.Predicate, bool) {
	b, ok := expr.(*ast.Binary)
	if !ok {
		return segment.Predicate{}, false
	}

	id, ok := b.Left.(*ast.Ident)
	if !ok {
		return segment.Predicate{}, false
	}

	lit, ok := b.Right.(*ast.Literal)
	if !ok {
		return segment.Predicate{}, false
	}

	var opStr string
	switch b.Op {
	case ast.OpEq:
		opStr = "="
	case ast.OpNotEq:
		opStr = "!="
	case ast.OpLt:
		opStr = "<"
	case ast.OpLtEq:
		opStr = "<="
	case ast.OpGt:
		opStr = ">"
	case ast.OpGtEq:
		opStr = ">="
	default:
		return segment.Predicate{}, false
	}

	// Convert the literal value to string (v1 Predicate contract).
	valStr := fmt.Sprintf("%v", lit.Value)

	// Map virtual field names to physical column names.
	field := id.Name
	switch field {
	case "source":
		field = "_source"
	case "sourcetype":
		field = "_sourcetype"
	}

	return segment.Predicate{
		Field: field,
		Op:    opStr,
		Value: valStr,
	}, true
}

// resolveTargetIndexes returns the set of index names the scan targets.
func resolveTargetIndexes(scan *logical.Scan, defaultIndex string) map[string]bool {
	if len(scan.Sources) == 0 {
		return map[string]bool{defaultIndex: true}
	}
	result := make(map[string]bool)
	for _, s := range scan.Sources {
		switch s.Kind {
		case ast.SourceStar:
			// Star means all — return nil to match everything.
			return nil
		case ast.SourceName:
			name := s.Name
			if name == "" {
				name = defaultIndex
			}
			result[name] = true
		case ast.SourceGlob:
			// Simple: treat glob pattern as literal for now.
			result[s.Pattern] = true
		}
	}
	if len(result) == 0 {
		return map[string]bool{defaultIndex: true}
	}
	return result
}

// matchesIndex checks if a part's index matches the target set.
// A nil targetIndexes means "match all".
func matchesIndex(partIndex string, targets map[string]bool) bool {
	if targets == nil {
		return true
	}
	return targets[partIndex]
}
