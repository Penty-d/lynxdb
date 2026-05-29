package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/config"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/model"
	"github.com/lynxbase/lynxdb/pkg/storage/segment"
)

// newShutdownTestEngine builds an in-memory engine WITHOUT the auto-Shutdown
// cleanup that newTestEngine registers. These tests drive Shutdown themselves,
// and Shutdown is not designed to be called twice.
func newShutdownTestEngine(t *testing.T) *Engine {
	t.Helper()

	queryCfg := config.DefaultConfig().Query
	queryCfg.SpillDir = t.TempDir()

	cfg := Config{
		DataDir: "",
		Storage: config.DefaultConfig().Storage,
		Logger:  discardLogger(),
		Query:   queryCfg,
	}

	e := NewEngine(cfg)
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("engine start: %v", err)
	}

	return e
}

// makeMmapSegmentHandle creates a segmentHandle backed by a real on-disk,
// memory-mapped .lsg file so the test can observe mmap close/unmap behavior.
func makeMmapSegmentHandle(t *testing.T, id string, count int) *segmentHandle {
	t.Helper()

	path := filepath.Join(t.TempDir(), id+".lsg")
	events := make([]*event.Event, count)
	now := time.Now()
	for i := range events {
		events[i] = &event.Event{
			Time: now.Add(time.Duration(i) * time.Millisecond),
			Raw:  fmt.Sprintf("event %d from %s", i, id),
			Host: "test-host",
		}
	}

	var buf bytes.Buffer
	w := segment.NewWriter(&buf)
	if _, err := w.Write(events); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ms, err := segment.OpenSegmentFile(path)
	if err != nil {
		t.Fatalf("open segment file: %v", err)
	}

	return &segmentHandle{
		reader: ms.Reader(),
		mmap:   ms,
		meta: model.SegmentMeta{
			ID:         id,
			Index:      "main",
			EventCount: int64(count),
			SizeBytes:  int64(buf.Len()),
		},
		index: "main",
	}
}

// TestShutdownDoesNotUnmapWhileQueriesInFlight reproduces the use-after-unmap
// SIGSEGV from the crash report: when the query drain times out with readers
// still active, Shutdown must NOT Munmap segments — an in-flight query reads the
// mapped bytes without a per-read ref, so unmapping faults its goroutine.
//
// The old code force-closed every retired segment unconditionally after the
// epoch-drain wait, even on the timeout branch. This test fails (mmap closed)
// against that code and passes against the gated fix.
func TestShutdownDoesNotUnmapWhileQueriesInFlight(t *testing.T) {
	e := newShutdownTestEngine(t)
	sh := makeMmapSegmentHandle(t, "inflight", 100)

	// Publish the segment in the current epoch (sh.refs -> 1).
	e.mu.Lock()
	e.advanceEpoch([]*segmentHandle{sh}, nil)
	e.mu.Unlock()

	// Simulate a long-running query that cannot drain within the timeout:
	//   - pinEpoch keeps the segment ref alive, so the decRef path cannot close
	//     the mmap; the ONLY thing that could close it is the buggy force-close.
	//   - jobsWG.Add(1) keeps the query drain from completing, so the fix's
	//     guard (queriesDrained) stays false and the force-close is skipped.
	ep := e.pinEpoch()
	e.jobsWG.Add(1)
	e.activeJobs.Add(1)

	// Short timeout so the query drain and epoch drain both time out quickly.
	_ = e.Shutdown(50 * time.Millisecond)

	// Regression check: the mapping must still be live.
	if sh.mmap.Closed() {
		t.Fatal("segment mmap was unmapped while a query was still in flight (use-after-unmap)")
	}
	// Reading through the still-valid mapping must not fault.
	if got := sh.reader.EventCount(); got != 100 {
		t.Fatalf("reader.EventCount = %d, want 100", got)
	}

	// Now let the "query" finish: the segment ref drains to 0 and decRef unmaps
	// the segment — confirming the deferred cleanup path still works.
	e.jobsWG.Done()
	e.activeJobs.Add(-1)
	ep.unpin()

	select {
	case <-ep.done:
	case <-time.After(2 * time.Second):
		t.Fatal("epoch done not signaled after unpin")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !sh.mmap.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("segment mmap not closed after the query finished and the epoch drained")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestShutdownUnmapsWhenFullyDrained verifies the safe path is unaffected by the
// fix: with no in-flight queries, Shutdown closes segment mmaps before returning
// (preserving the close-before-return guarantee for data-dir reuse).
func TestShutdownUnmapsWhenFullyDrained(t *testing.T) {
	e := newShutdownTestEngine(t)
	sh := makeMmapSegmentHandle(t, "drained", 100)

	e.mu.Lock()
	e.advanceEpoch([]*segmentHandle{sh}, nil)
	e.mu.Unlock()

	// No pin and no in-flight jobs: the drain completes, so queriesDrained is
	// true and the gated force-close runs.
	if err := e.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	if !sh.mmap.Closed() {
		t.Fatal("segment mmap should be closed after a fully-drained shutdown")
	}
}
