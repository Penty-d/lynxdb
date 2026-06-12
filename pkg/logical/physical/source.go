// Package physical — source.go implements Source callbacks that bridge
// logical.Scan nodes to storage engines.
//
// Two Source implementations:
//
//  1. EphemeralStore (CLI pipe mode): events live in memory as []*event.Event.
//     Pushdown mapping:
//     - RawTerms: applied in-memory via hasToken matching (same tokenizer
//     contract as §6.1) to pre-filter events before pipeline.
//     - FieldPredicates: applied in-memory via ast.Expr evaluation (converted
//     to field==literal string comparison for the v1 contract).
//     - BloomTerms: applied in-memory via substring containment check (bloom
//     terms are literal substrings extracted from contains/glob/matches; in
//     the absence of a bloom filter, we check _raw directly).
//     - Columns: NOT mapped (ephemeral returns full events; ProjectIterator
//     handles pruning). No segment-level column projection API.
//     - TimeBounds: enforced in-memory by filterByTimeBounds. Both
//     Scan.TimeRange (bracket syntax, e.g. from main[-1h]) and
//     Scan.Pushdown.TimeBounds (optimizer-consumed _time predicates) are
//     resolved to absolute time.Time using the now parameter and applied
//     as an event filter. This is critical because the optimizer REMOVES
//     _time conjuncts from the Filter node (consumed=true), and bracket
//     ranges have no compensating Filter.
//     - Reverse: NOT mapped (ephemeral has no sorted-timestamp guarantee).
//
//  2. PartStore (disk-backed parts with inverted index + bloom): events are
//     in .lsg part files on disk. Pushdown mapping:
//     - RawTerms -> inverted index term lookup (SerializedIndex.Search),
//     producing a roaring bitmap of matching event IDs. Multiple terms are
//     AND'd. If bitmap is empty, the part is skipped entirely.
//     - BloomTerms -> same inverted index path but with less-selective terms
//     (substring-derived). Used as candidate filter before full verification.
//     - FieldPredicates -> segment.Predicate{Field, Op, Value} for the
//     reader's ReadEventsFiltered method, which applies per-column bloom,
//     zone map, and dict-encoded filtering.
//     - Columns -> column projection in ReadEventsFiltered/ReadEventsByBitmap.
//     - TimeBounds -> enforced via segment.QueryHints.MinTime/MaxTime for
//     row-group-level pruning (NewPartSource resolves relative bounds using
//     the now parameter). When ReadEventsFiltered or ReadEventsByBitmap is
//     used (which lack native time hints), events are post-filtered.
//     - Reverse -> NOT mapped (part reader does not support reverse scan).
package physical

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/lynxbase/lynxdb/pkg/engine/pipeline"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical"
	"github.com/lynxbase/lynxdb/pkg/lynxflow/ast"
)

// ScanStats collects scan-level statistics for observability and testing.
// A pointer to ScanStats can be passed into NewStorageSource / NewPartSource;
// the source callback atomically increments the counters during execution.
type ScanStats struct {
	// TotalEvents is the total number of events across all scanned sources
	// BEFORE any pushdown filtering.
	TotalEvents atomic.Int64

	// FilteredEvents is the number of events that survived pushdown filtering
	// (RawTerms + BloomTerms + FieldPredicates). This is the count fed into
	// the pipeline iterator.
	FilteredEvents atomic.Int64

	// PartsTotal is the number of parts considered (PartStore only).
	PartsTotal atomic.Int64

	// PartsSkipped is the number of parts skipped because the inverted index
	// determined zero matching events (PartStore only).
	PartsSkipped atomic.Int64
}

// EphemeralStore is the interface that the Ephemeral storage engine implements.
// It is deliberately narrow: only MaterializeEvents (which returns the in-memory
// event slice for an index) is required. This avoids importing pkg/storage.
type EphemeralStore interface {
	MaterializeEvents(index string) []*event.Event

	// AllEvents returns events from ALL indexes. Implementations that don't
	// support multi-index enumeration may return nil, causing the caller to
	// fall back to MaterializeEvents(defaultIndex).
	AllEvents() []*event.Event
}

// mapAdapter wraps a raw map[string][]*event.Event so it satisfies EphemeralStore.
type mapAdapter struct {
	events map[string][]*event.Event
}

func (m *mapAdapter) MaterializeEvents(index string) []*event.Event {
	return m.events[index]
}

func (m *mapAdapter) AllEvents() []*event.Event {
	var all []*event.Event
	for _, evs := range m.events {
		all = append(all, evs...)
	}
	return all
}

// NewStorageSource returns a Source callback that resolves Scan nodes against
// the given EphemeralStore. The defaultIndex is used when the scan has no
// explicit source (SourceStar or empty Sources list). The now parameter is used
// to resolve relative time bounds (e.g. -1h in bracket ranges or _time
// predicates pushed down by the optimizer). The optimizer CONSUMES _time
// comparisons from Filter nodes into Scan.Pushdown.TimeBounds (removing the
// Filter conjunct entirely), and bracket ranges like from main[-1h] land in
// Scan.TimeRange with no compensating Filter. Therefore this source MUST
// enforce both TimeBounds and TimeRange — failure to do so produces
// silently wrong results.
//
// When stats is non-nil, scan statistics are accumulated for observability.
func NewStorageSource(store EphemeralStore, defaultIndex string, now time.Time, stats *ScanStats) func(*logical.Scan) (pipeline.Iterator, error) {
	return func(scan *logical.Scan) (pipeline.Iterator, error) {
		events := resolveEvents(store, scan, defaultIndex)

		total := int64(len(events))
		if stats != nil {
			stats.TotalEvents.Add(total)
		}

		// Time bound enforcement: the optimizer consumes _time predicates
		// out of Filter (consumed=true), and bracket ranges have no
		// compensating Filter — so the source must apply them.
		events = filterByTimeBounds(events, scan, now)

		// Apply pushdown filters in-memory only when stats tracking is
		// requested. When stats is nil (default/backward-compatible path),
		// filtering happens entirely in the Filter iterator. This preserves
		// EXPLAIN ANALYZE row counts for the Scan node. The predicate is
		// always KEPT in Filter (consumed=false), so correctness is
		// maintained either way; the choice is performance vs observability.
		filtered := events
		if stats != nil {
			filtered = applyEphemeralPushdown(events, scan)
			stats.FilteredEvents.Add(int64(len(filtered)))
		}

		return pipeline.NewRowScanIterator(eventsToRows(filtered), pipeline.DefaultBatchSize), nil
	}
}

// NewStorageSourceFromMap is a convenience for tests and non-CLI callers:
// wraps a raw event map into a Source callback. Uses time.Now() for resolving
// relative time bounds. For test-injected time, use NewStorageSourceFromMapWithNow.
func NewStorageSourceFromMap(events map[string][]*event.Event, defaultIndex string) func(*logical.Scan) (pipeline.Iterator, error) {
	return NewStorageSource(&mapAdapter{events: events}, defaultIndex, time.Now(), nil)
}

// NewStorageSourceFromMapWithStats is like NewStorageSourceFromMap but also
// collects scan statistics into the provided ScanStats.
func NewStorageSourceFromMapWithStats(events map[string][]*event.Event, defaultIndex string, ss *ScanStats) func(*logical.Scan) (pipeline.Iterator, error) {
	return NewStorageSource(&mapAdapter{events: events}, defaultIndex, time.Now(), ss)
}

// NewStorageSourceFromMapWithNow is like NewStorageSourceFromMapWithStats but
// accepts an explicit now parameter for resolving relative time bounds. This is
// the correct entry point for callers that need deterministic time resolution
// (e.g. pkg/lynxflow/run.Execute, tests). The stats parameter may be nil.
func NewStorageSourceFromMapWithNow(events map[string][]*event.Event, defaultIndex string, now time.Time, stats *ScanStats) func(*logical.Scan) (pipeline.Iterator, error) {
	return NewStorageSource(&mapAdapter{events: events}, defaultIndex, now, stats)
}

// ---------------------------------------------------------------------------
// Ephemeral pushdown: in-memory filtering using Scan.Pushdown hints
// ---------------------------------------------------------------------------

// applyEphemeralPushdown filters events in-memory using the Scan's pushdown
// hints. This implements the same semantic contracts as the disk-based path:
//
//   - RawTerms: each term must be present as a whole token (case-insensitive)
//     in the event's _raw field. Matches the tokenizer contract (§6.1).
//   - BloomTerms: each term must appear as a substring in _raw (CI).
//     In the disk path these are bloom-assisted candidates; here we verify
//     directly since there is no bloom filter.
//   - FieldPredicates: ast.Expr of shape (field op literal); evaluated via
//     string comparison against event field values.
//
// The filter returns a new slice (or the original if nothing was filtered).
func applyEphemeralPushdown(events []*event.Event, scan *logical.Scan) []*event.Event {
	pd := scan.Pushdown
	hasRaw := len(pd.RawTerms) > 0
	hasBloom := len(pd.BloomTerms) > 0
	hasFP := len(pd.FieldPredicates) > 0

	if !hasRaw && !hasBloom && !hasFP {
		return events
	}

	result := make([]*event.Event, 0, len(events))
	for _, ev := range events {
		if hasRaw && !matchRawTerms(ev.Raw, pd.RawTerms) {
			continue
		}
		if hasBloom && !matchBloomTerms(ev.Raw, pd.BloomTerms) {
			continue
		}
		if hasFP && !matchFieldPredicates(ev, pd.FieldPredicates) {
			continue
		}
		result = append(result, ev)
	}
	return result
}

// matchRawTerms checks that every term in terms appears as a whole token
// (case-insensitive) in the raw string, per the tokenizer contract (§6.1).
// This mirrors has(_raw, "term") semantics.
func matchRawTerms(raw string, terms []string) bool {
	if raw == "" {
		return false
	}
	// Tokenize raw into a set for O(n) matching.
	rawLower := strings.ToLower(raw)
	tokens := tokenizeString(rawLower)
	set := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		set[t] = struct{}{}
	}
	for _, term := range terms {
		// Terms are already lowercased by the optimizer's extractHasRawTerms.
		if _, ok := set[term]; !ok {
			return false
		}
	}
	return true
}

// matchBloomTerms checks that every bloom term appears as a substring in raw
// (case-insensitive). Bloom terms are literal substrings extracted from
// contains/glob/matches calls; in the disk path they are bloom-filter
// candidates, but here we verify directly.
func matchBloomTerms(raw string, terms []string) bool {
	if raw == "" {
		return false
	}
	rawLower := strings.ToLower(raw)
	for _, term := range terms {
		if !strings.Contains(rawLower, term) {
			return false
		}
	}
	return true
}

// matchFieldPredicates evaluates field-comparison expressions against an event.
// Each expression should be of the shape: field op literal (e.g., status == 500).
// We convert event field values to strings for comparison, matching the v1
// Predicate{Field, Op, Value string} contract.
func matchFieldPredicates(ev *event.Event, exprs []ast.Expr) bool {
	for _, expr := range exprs {
		if !evalFieldPredicate(ev, expr) {
			return false
		}
	}
	return true
}

// evalFieldPredicate evaluates a single field op literal expression.
func evalFieldPredicate(ev *event.Event, expr ast.Expr) bool {
	b, ok := expr.(*ast.Binary)
	if !ok {
		return true // not a binary expr, skip (conservative: don't filter)
	}

	fieldName := ""
	if id, ok := b.Left.(*ast.Ident); ok {
		fieldName = id.Name
	} else {
		return true
	}

	lit, ok := b.Right.(*ast.Literal)
	if !ok {
		return true
	}

	// Get the event's field value.
	var fieldVal event.Value
	var found bool
	switch fieldName {
	case "_raw":
		fieldVal = event.StringValue(ev.Raw)
		found = ev.Raw != ""
	case "_source":
		fieldVal = event.StringValue(ev.Source)
		found = ev.Source != ""
	case "_sourcetype":
		fieldVal = event.StringValue(ev.SourceType)
		found = ev.SourceType != ""
	default:
		fieldVal, found = ev.Fields[fieldName]
	}

	if !found {
		// Missing field: for == returns false, for != returns true.
		return b.Op == ast.OpNotEq
	}

	// Convert both to strings for comparison (v1 contract).
	fieldStr := fieldVal.String()
	litStr := fmt.Sprintf("%v", lit.Value)

	return compareStrings(fieldStr, litStr, b.Op)
}

// compareStrings performs string comparison for the given operator.
func compareStrings(a, b string, op ast.BinaryOp) bool {
	switch op {
	case ast.OpEq:
		return a == b
	case ast.OpNotEq:
		return a != b
	case ast.OpLt:
		return a < b
	case ast.OpLtEq:
		return a <= b
	case ast.OpGt:
		return a > b
	case ast.OpGtEq:
		return a >= b
	default:
		return true // unknown op, don't filter
	}
}

// tokenizeString splits s into tokens per the tokenizer contract (§6.1):
// runs of ASCII alphanumerics and Unicode letters/digits, lowercased.
// The input should already be lowercased.
func tokenizeString(s string) []string {
	var tokens []string
	start := -1
	for i, r := range s {
		isToken := isTokenChar(r)
		if isToken {
			if start < 0 {
				start = i
			}
		} else {
			if start >= 0 {
				tokens = append(tokens, s[start:i])
				start = -1
			}
		}
	}
	if start >= 0 {
		tokens = append(tokens, s[start:])
	}
	return tokens
}

// isTokenChar returns true for characters that form part of a token.
func isTokenChar(r rune) bool {
	if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r > 127 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Event resolution
// ---------------------------------------------------------------------------

// resolveEvents collects events from the store based on the Scan's source patterns.
func resolveEvents(store EphemeralStore, scan *logical.Scan, defaultIndex string) []*event.Event {
	if len(scan.Sources) == 0 {
		// No explicit source: scan the default index.
		return store.MaterializeEvents(defaultIndex)
	}

	// Check for star source: return events from all indexes.
	for _, s := range scan.Sources {
		if s.Kind == ast.SourceStar {
			if all := store.AllEvents(); all != nil {
				return all
			}
			// Fallback for stores that don't implement AllEvents.
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
		row := make(map[string]event.Value, len(ev.Fields)+6)

		// Core fields — must match the SPL2 path's EventsToBatch to ensure
		// field parity between the two execution paths.
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
		if ev.Host != "" {
			row["host"] = event.StringValue(ev.Host)
		}
		if ev.Index != "" {
			row["index"] = event.StringValue(ev.Index)
		}

		// Extracted fields.
		for k, v := range ev.Fields {
			row[k] = v
		}

		rows[i] = row
	}
	return rows
}

// ---------------------------------------------------------------------------
// Time bound resolution and filtering
// ---------------------------------------------------------------------------

// resolveTimeBoundsToAbsolute converts a logical.TimeBounds (which may contain
// relative duration expressions like -1h) into absolute time.Time values.
// Returns (minTime, maxTime) where a nil pointer means the bound is open.
func resolveTimeBoundsToAbsolute(tb *logical.TimeBounds, now time.Time) (minTime, maxTime *time.Time) {
	if tb == nil {
		return nil, nil
	}
	if tb.Start != nil {
		if t, ok := resolveTimeExpr(tb.Start, now); ok {
			minTime = &t
		}
	}
	if tb.End != nil {
		if t, ok := resolveTimeExpr(tb.End, now); ok {
			maxTime = &t
		}
	}
	return minTime, maxTime
}

// resolveTimeExpr resolves an AST expression to an absolute time.Time.
// Handles:
//   - Unary{OpNeg, Literal{LitDuration}} -> now - duration (e.g. -1h)
//   - Literal{LitDuration} with negative Value -> now + duration (already negative)
//   - Literal{LitDuration} with positive Value -> now + duration
//   - Literal{LitInt} -> Unix epoch seconds
//   - Literal{LitString} -> RFC3339 parse attempt
func resolveTimeExpr(expr ast.Expr, now time.Time) (time.Time, bool) {
	switch e := expr.(type) {
	case *ast.Unary:
		if e.Op == ast.OpNeg {
			if lit, ok := e.Operand.(*ast.Literal); ok && lit.Kind == ast.LitDuration {
				if d, ok := lit.Value.(time.Duration); ok {
					return now.Add(-d), true
				}
			}
		}
	case *ast.Literal:
		switch e.Kind {
		case ast.LitDuration:
			if d, ok := e.Value.(time.Duration); ok {
				// A negative duration (e.g. from constant folding) is relative to now.
				return now.Add(d), true
			}
		case ast.LitInt:
			if v, ok := e.Value.(int64); ok {
				return time.Unix(v, 0), true
			}
		case ast.LitString:
			if s, ok := e.Value.(string); ok {
				if t, err := time.Parse(time.RFC3339, s); err == nil {
					return t, true
				}
				if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}

// mergedTimeBounds returns the effective time bounds by combining
// Scan.TimeRange (bracket syntax) and Scan.Pushdown.TimeBounds (optimizer
// consumed predicates). The optimizer already merges bracket ranges into
// Pushdown.TimeBounds when possible; for the case where only TimeRange is
// set (no WHERE clause with _time), we fall back to TimeRange.
func mergedTimeBounds(scan *logical.Scan) *logical.TimeBounds {
	if scan.Pushdown.TimeBounds != nil {
		return scan.Pushdown.TimeBounds
	}
	return scan.TimeRange
}

// filterByTimeBounds filters events by the resolved time bounds from both
// Scan.TimeRange and Scan.Pushdown.TimeBounds. Events without a _time field
// are conservatively kept (not filtered).
func filterByTimeBounds(events []*event.Event, scan *logical.Scan, now time.Time) []*event.Event {
	tb := mergedTimeBounds(scan)
	if tb == nil {
		return events
	}

	minTime, maxTime := resolveTimeBoundsToAbsolute(tb, now)
	if minTime == nil && maxTime == nil {
		return events
	}

	result := make([]*event.Event, 0, len(events))
	for _, ev := range events {
		if ev.Time.IsZero() {
			// No timestamp — conservatively include.
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

// StorageSourceInfo documents pushdown mapping status.
var StorageSourceInfo = fmt.Sprintf(
	"v2 pushdown mapping: raw_terms=YES(ephemeral:in-memory-hasToken), " +
		"bloom_terms=YES(ephemeral:in-memory-substring), " +
		"field_predicates=YES(ephemeral:in-memory-eval), " +
		"columns=NO(ephemeral:full-events), " +
		"time_bounds=YES(ephemeral:filterByTimeBounds, part:QueryHints.MinTime/MaxTime), " +
		"reverse=NO(ephemeral:no-sorted-guarantee). " +
		"Part-backed mapping via NewPartSource: raw_terms=inverted-index, " +
		"bloom_terms=inverted-index, field_predicates=segment.Predicate, " +
		"columns=projection, time_bounds=YES(QueryHints.MinTime/MaxTime), " +
		"reverse=UNSUPPORTED.")
