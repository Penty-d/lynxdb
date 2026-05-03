package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/memgov"
)

// SessionizeIterator groups events into sessions based on time gaps.
// Input is sorted by group keys and _time via the existing sort iterator, so
// sessionize only buffers one active session while streaming finalized rows.
type SessionizeIterator struct {
	child     Iterator
	maxPause  time.Duration
	groupBy   []string
	batchSize int
	acct      memgov.MemoryAccount

	currentBatch *Batch
	batchOffset  int

	haveSession   bool
	sessionID     int64
	currentGroup  string
	currentStart  time.Time
	currentEnd    time.Time
	currentRows   []map[string]event.Value
	currentBytes  int64
	pending       []map[string]event.Value
	pendingBytes  int64
	pendingOffset int
	eof           bool
}

// NewSessionizeIterator creates a sessionize operator.
func NewSessionizeIterator(child Iterator, maxPause string, groupBy []string, batchSize int) *SessionizeIterator {
	return NewSessionizeIteratorWithBudget(child, maxPause, groupBy, batchSize, memgov.NopAccount(), memgov.NopAccount(), nil)
}

// NewSessionizeIteratorWithBudget creates a sessionize operator that uses a
// spill-capable sort for ordering and accounts for the active session buffer.
func NewSessionizeIteratorWithBudget(
	child Iterator,
	maxPause string,
	groupBy []string,
	batchSize int,
	sortAcct memgov.MemoryAccount,
	sessionAcct memgov.MemoryAccount,
	mgr *SpillManager,
) *SessionizeIterator {
	dur := parseDuration(maxPause)
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}

	sortFields := make([]SortField, 0, len(groupBy)+1)
	for _, field := range groupBy {
		sortFields = append(sortFields, SortField{Name: field})
	}
	sortFields = append(sortFields, SortField{Name: "_time"})

	var sorted Iterator
	if mgr != nil {
		sorted = NewSortIteratorWithSpill(child, sortFields, batchSize, memgov.EnsureAccount(sortAcct), mgr)
	} else {
		sorted = NewSortIteratorWithBudget(child, sortFields, batchSize, memgov.EnsureAccount(sortAcct))
	}

	return &SessionizeIterator{
		child:     sorted,
		maxPause:  dur,
		groupBy:   groupBy,
		batchSize: batchSize,
		acct:      memgov.EnsureAccount(sessionAcct),
	}
}

func (s *SessionizeIterator) Init(ctx context.Context) error {
	return s.child.Init(ctx)
}

func (s *SessionizeIterator) Next(ctx context.Context) (*Batch, error) {
	out := NewBatch(s.batchSize)
	for out.Len < s.batchSize {
		if s.pendingOffset >= len(s.pending) {
			if s.pendingBytes > 0 {
				s.acct.Shrink(s.pendingBytes)
				s.pendingBytes = 0
			}
			s.pending = nil
			s.pendingOffset = 0
			ok, err := s.readNextFinalizedSession(ctx)
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
		}

		for out.Len < s.batchSize && s.pendingOffset < len(s.pending) {
			out.AddRow(s.pending[s.pendingOffset])
			s.pendingOffset++
		}
	}

	if out.Len == 0 {
		return nil, nil
	}

	return out, nil
}

func (s *SessionizeIterator) readNextFinalizedSession(ctx context.Context) (bool, error) {
	for {
		row, ok, err := s.nextRow(ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			s.eof = true
			if !s.haveSession {
				return false, nil
			}
			s.finalizeCurrentSession()

			return true, nil
		}

		groupKey := s.groupKey(row)
		rowTime := getTime(row)
		if !s.haveSession {
			if err := s.startSession(groupKey, rowTime, row); err != nil {
				return false, err
			}
			continue
		}

		if groupKey != s.currentGroup || (!s.currentEnd.IsZero() && rowTime.Sub(s.currentEnd) > s.maxPause) {
			s.finalizeCurrentSession()
			if err := s.startSession(groupKey, rowTime, row); err != nil {
				return false, err
			}

			return true, nil
		}

		if err := s.appendCurrentRow(row, rowTime); err != nil {
			return false, err
		}
	}
}

func (s *SessionizeIterator) nextRow(ctx context.Context) (map[string]event.Value, bool, error) {
	for {
		if s.currentBatch != nil && s.batchOffset < s.currentBatch.Len {
			row := s.currentBatch.Row(s.batchOffset)
			s.batchOffset++

			return row, true, nil
		}
		if s.eof {
			return nil, false, nil
		}

		batch, err := s.child.Next(ctx)
		if err != nil {
			return nil, false, err
		}
		if batch == nil {
			return nil, false, nil
		}
		s.currentBatch = batch
		s.batchOffset = 0
	}
}

func (s *SessionizeIterator) startSession(groupKey string, rowTime time.Time, row map[string]event.Value) error {
	s.sessionID++
	s.haveSession = true
	s.currentGroup = groupKey
	s.currentStart = rowTime
	s.currentEnd = rowTime
	s.currentRows = nil
	s.currentBytes = 0

	return s.appendCurrentRow(row, rowTime)
}

func (s *SessionizeIterator) appendCurrentRow(row map[string]event.Value, rowTime time.Time) error {
	rowBytes := EstimateRowBytes(row)
	if err := s.acct.Grow(rowBytes); err != nil {
		return fmt.Errorf("sessionize: memory budget exceeded: %w", err)
	}
	s.currentRows = append(s.currentRows, row)
	s.currentBytes += rowBytes
	if rowTime.After(s.currentEnd) {
		s.currentEnd = rowTime
	}

	return nil
}

func (s *SessionizeIterator) finalizeCurrentSession() {
	for _, row := range s.currentRows {
		row["_session_id"] = event.IntValue(s.sessionID)
		row["_session_start"] = event.TimestampValue(s.currentStart)
		row["_session_end"] = event.TimestampValue(s.currentEnd)
	}
	s.pending = s.currentRows
	s.pendingBytes = s.currentBytes
	s.pendingOffset = 0
	s.currentRows = nil
	s.haveSession = false
	s.currentBytes = 0
}

func (s *SessionizeIterator) groupKey(row map[string]event.Value) string {
	groupKey := ""
	for _, f := range s.groupBy {
		if v, ok := row[f]; ok {
			groupKey += "|" + v.String()
		}
	}

	return groupKey
}

func (s *SessionizeIterator) Close() error {
	if s.currentBytes > 0 {
		s.acct.Shrink(s.currentBytes)
		s.currentBytes = 0
	}
	if s.pendingBytes > 0 {
		s.acct.Shrink(s.pendingBytes)
		s.pendingBytes = 0
	}
	s.acct.Close()

	return s.child.Close()
}

// MemoryUsed returns the current tracked memory for this operator.
func (s *SessionizeIterator) MemoryUsed() int64 { return s.acct.Used() }

func (s *SessionizeIterator) Schema() []FieldInfo {
	return append(s.child.Schema(),
		FieldInfo{Name: "_session_id", Type: "int"},
		FieldInfo{Name: "_session_start", Type: "timestamp"},
		FieldInfo{Name: "_session_end", Type: "timestamp"},
	)
}

// String returns a debug representation.
func (s *SessionizeIterator) String() string {
	if len(s.groupBy) > 0 {
		return fmt.Sprintf("Sessionize(maxpause=%s, by=%v)", s.maxPause, s.groupBy)
	}

	return fmt.Sprintf("Sessionize(maxpause=%s)", s.maxPause)
}
