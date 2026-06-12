package run

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/logical/physical"
)

// TestTimeBounds_WhereRelative verifies that `from main | where _time >= -1h | stats count()`
// correctly filters events when the optimizer consumes the _time predicate into
// Scan.Pushdown.TimeBounds (removing the Filter conjunct). Before the fix, this
// returned count=2 instead of count=1 because the ephemeral source did not
// enforce the consumed time bounds.
func TestTimeBounds_WhereRelative(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	events := map[string][]*event.Event{
		"main": {
			// 3h old — should be excluded by _time >= -1h.
			makeTimedEvent(now.Add(-3*time.Hour), "old event"),
			// 30min old — should be included.
			makeTimedEvent(now.Add(-30*time.Minute), "recent event"),
		},
	}

	rows, err := Execute(context.Background(),
		`from main | where _time >= -1h | stats count()`,
		events,
		Options{Now: now},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	if count.AsInt() != 1 {
		t.Errorf("expected count()=1, got %v", count)
	}
}

// TestTimeBounds_BracketRange verifies that `from main[-1h] | stats count()`
// correctly filters events via the bracket time range (Scan.TimeRange).
// Bracket ranges have NO compensating Filter in the pipeline; the source must
// enforce them directly.
func TestTimeBounds_BracketRange(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	events := map[string][]*event.Event{
		"main": {
			// 3h old — outside [-1h] window.
			makeTimedEvent(now.Add(-3*time.Hour), "old event"),
			// 30min old — inside [-1h] window.
			makeTimedEvent(now.Add(-30*time.Minute), "recent event"),
		},
	}

	rows, err := Execute(context.Background(),
		`from main[-1h] | stats count()`,
		events,
		Options{Now: now},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	if count.AsInt() != 1 {
		t.Errorf("expected count()=1, got %v", count)
	}
}

// TestTimeBounds_AbsoluteRange verifies that absolute time bounds using Unix
// epoch integer values in WHERE are correctly enforced by the source.
func TestTimeBounds_AbsoluteRange(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-1 * time.Hour)

	events := map[string][]*event.Event{
		"main": {
			// Before cutoff.
			makeTimedEvent(now.Add(-3*time.Hour), "old event"),
			// After cutoff.
			makeTimedEvent(now.Add(-30*time.Minute), "recent event"),
		},
	}

	// Use _time >= <unix epoch> as an absolute bound.
	query := fmt.Sprintf(`from main | where _time >= %d | stats count()`, cutoff.Unix())
	rows, err := Execute(context.Background(), query, events, Options{Now: now})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	if count.AsInt() != 1 {
		t.Errorf("expected count()=1, got %v (query: %s)", count, query)
	}
}

// TestTimeBounds_NoTimeBounds verifies that queries without time predicates
// return all events (regression guard — time filtering should only apply when
// bounds are present).
func TestTimeBounds_NoTimeBounds(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	events := map[string][]*event.Event{
		"main": {
			makeTimedEvent(now.Add(-3*time.Hour), "old event"),
			makeTimedEvent(now.Add(-30*time.Minute), "recent event"),
		},
	}

	rows, err := Execute(context.Background(),
		`from main | stats count()`,
		events,
		Options{Now: now},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	if count.AsInt() != 2 {
		t.Errorf("expected count()=2 (no filter), got %v", count)
	}
}

// TestTimeBounds_WhereRelative_StatsTracking verifies that ScanStats correctly
// reflect event counts when time filtering is active.
func TestTimeBounds_WhereRelative_StatsTracking(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	events := map[string][]*event.Event{
		"main": {
			makeTimedEvent(now.Add(-3*time.Hour), "old event"),
			makeTimedEvent(now.Add(-30*time.Minute), "recent event"),
		},
	}

	ss := &physical.ScanStats{}
	rows, err := Execute(context.Background(),
		`from main | where _time >= -1h | stats count()`,
		events,
		Options{Now: now, ScanStats: ss},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	if count.AsInt() != 1 {
		t.Errorf("expected count()=1, got %v", count)
	}
}

// TestTimeBounds_BothBounds verifies a range query with both lower and upper bounds.
func TestTimeBounds_BothBounds(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	events := map[string][]*event.Event{
		"main": {
			makeTimedEvent(now.Add(-5*time.Hour), "very old"),
			makeTimedEvent(now.Add(-2*time.Hour), "middle"),
			makeTimedEvent(now.Add(-30*time.Minute), "recent"),
		},
	}

	// Events between -3h and -1h: only the middle event.
	rows, err := Execute(context.Background(),
		`from main | where _time >= -3h and _time <= -1h | stats count()`,
		events,
		Options{Now: now},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	if count.AsInt() != 1 {
		t.Errorf("expected count()=1 (only middle event), got %v", count)
	}
}

// TestTimeBounds_EventsWithoutTimestamp verifies that events lacking a _time
// field are conservatively kept (not filtered out) when time bounds are active.
func TestTimeBounds_EventsWithoutTimestamp(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	noTimeEvent := event.NewEvent(time.Time{}, "no timestamp event")
	timedEvent := makeTimedEvent(now.Add(-30*time.Minute), "recent event")
	oldEvent := makeTimedEvent(now.Add(-3*time.Hour), "old event")

	events := map[string][]*event.Event{
		"main": {noTimeEvent, timedEvent, oldEvent},
	}

	rows, err := Execute(context.Background(),
		`from main | where _time >= -1h | stats count()`,
		events,
		Options{Now: now},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(rows))
	}
	count := rows[0]["count()"]
	// noTimeEvent + timedEvent survive (old is filtered); noTimeEvent is
	// conservatively kept because it has no timestamp.
	if count.AsInt() != 2 {
		t.Errorf("expected count()=2 (no-time event + recent), got %v", count)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makeTimedEvent(ts time.Time, raw string) *event.Event {
	return event.NewEvent(ts, raw)
}
