package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/config"
	"github.com/lynxbase/lynxdb/pkg/event"
	"github.com/lynxbase/lynxdb/pkg/optimizer"
	"github.com/lynxbase/lynxdb/pkg/spl2"
)

func TestQueryCacheUsesGenerationAfterPreQueryFlush(t *testing.T) {
	queryCfg := config.DefaultConfig().Query
	queryCfg.SpillDir = t.TempDir()
	fsync := false

	e := NewEngine(Config{
		DataDir: t.TempDir(),
		Storage: config.DefaultConfig().Storage,
		Logger:  discardLogger(),
		Query:   queryCfg,
		Ingest:  config.IngestConfig{FSync: &fsync},
	})

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		cancel()
		t.Fatalf("engine start: %v", err)
	}
	defer cancel()
	defer func() {
		if err := e.Shutdown(5 * time.Second); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}()

	base := time.Now().UTC()
	events := make([]*event.Event, 8)
	for i := range events {
		ev := event.NewEvent(base.Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("event-%d", i))
		ev.Index = "main"
		events[i] = ev
	}
	if err := e.Ingest(events); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	query := "from main | stats count"
	first := submitAndWait(t, e, query)
	if first.Stats.CacheHit {
		t.Fatalf("first query unexpectedly hit cache")
	}
	if first.Status != JobStatusDone {
		t.Fatalf("first query status = %s, error=%s", first.Status, first.Error)
	}

	second := submitAndWait(t, e, query)
	if second.Status != JobStatusDone {
		t.Fatalf("second query status = %s, error=%s", second.Status, second.Error)
	}
	if !second.Stats.CacheHit {
		t.Fatalf("second query did not hit cache")
	}
}

func TestQueryReadsBufferedEventsWithoutCreatingParts(t *testing.T) {
	queryCfg := config.DefaultConfig().Query
	queryCfg.SpillDir = t.TempDir()
	fsync := false
	storageCfg := config.DefaultConfig().Storage
	storageCfg.FlushThreshold = 1 * config.GB
	storageCfg.FlushIdleTimeout = time.Hour

	e := NewEngine(Config{
		DataDir: t.TempDir(),
		Storage: storageCfg,
		Logger:  discardLogger(),
		Query:   queryCfg,
		Ingest:  config.IngestConfig{FSync: &fsync},
	})

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Start(ctx); err != nil {
		cancel()
		t.Fatalf("engine start: %v", err)
	}
	defer cancel()
	defer func() {
		if err := e.Shutdown(5 * time.Second); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}()

	base := time.Now().UTC()
	events := make([]*event.Event, 3)
	for i := range events {
		ev := event.NewEvent(base.Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("buffered-%d", i))
		ev.Index = "main"
		events[i] = ev
	}
	if err := e.Ingest(events); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := e.partRegistry.Count(); got != 0 {
		t.Fatalf("parts before query: got %d, want 0", got)
	}

	for i := 0; i < 3; i++ {
		result := submitAndWait(t, e, "from main | stats count")
		if result.Status != JobStatusDone {
			t.Fatalf("query[%d] status = %s, error=%s", i, result.Status, result.Error)
		}
		if got := e.partRegistry.Count(); got != 0 {
			t.Fatalf("parts after query[%d]: got %d, want 0", i, got)
		}
	}
}

func submitAndWait(t *testing.T, e *Engine, query string) JobSnapshot {
	t.Helper()

	normalized := spl2.NormalizeQuery(query)
	prog, err := spl2.ParseProgram(normalized)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	opt := optimizer.New()
	prog.Main = opt.Optimize(prog.Main)
	for i := range prog.Datasets {
		prog.Datasets[i].Query = opt.Optimize(prog.Datasets[i].Query)
	}
	hints := spl2.ExtractQueryHints(prog)

	job, err := e.SubmitQuery(context.Background(), QueryParams{
		Query:      normalized,
		Program:    prog,
		Hints:      hints,
		ResultType: DetectResultType(prog),
	})
	if err != nil {
		t.Fatalf("submit query: %v", err)
	}

	select {
	case <-job.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("query timed out")
	}

	e.jobsWG.Wait()
	return job.Snapshot()
}
