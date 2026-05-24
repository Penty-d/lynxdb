package compaction

import (
	"container/heap"
	"fmt"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/model"
)

func scoreSegment(id string, level int, size int64, partition string) *SegmentInfo {
	return &SegmentInfo{Meta: model.SegmentMeta{
		ID:        id,
		Index:     "main",
		Partition: partition,
		Level:     level,
		SizeBytes: size,
		MinTime:   time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
		MaxTime:   time.Date(2026, 5, 24, 1, 0, 0, 0, time.UTC),
	}}
}

func TestLevelScoreL0UsesCountPressure(t *testing.T) {
	cfg := Config{L0Threshold: 4, FlushBytes: 100}
	segs := []*SegmentInfo{
		scoreSegment("a", L0, 1, "p0"),
		scoreSegment("b", L0, 1, "p0"),
		scoreSegment("c", L0, 1, "p0"),
	}

	score := LevelScore(cfg, L0, segs, nil)
	if score.Value != 0.75 {
		t.Fatalf("score: got %.2f, want 0.75", score.Value)
	}
}

func TestLevelScoreL0UsesBytePressure(t *testing.T) {
	cfg := Config{L0Threshold: 4, FlushBytes: 100}
	segs := []*SegmentInfo{
		scoreSegment("a", L0, 250, "p0"),
		scoreSegment("b", L0, 250, "p0"),
	}

	score := LevelScore(cfg, L0, segs, nil)
	if score.Value != 1.25 {
		t.Fatalf("score: got %.2f, want 1.25", score.Value)
	}
}

func TestLevelScoreExcludesPendingInputs(t *testing.T) {
	cfg := Config{L1Threshold: 4, L2TargetSize: 1000}
	segs := []*SegmentInfo{
		scoreSegment("a", L1, 400, "p0"),
		scoreSegment("b", L1, 400, "p0"),
		scoreSegment("c", L1, 400, "p0"),
	}
	pending := map[string]struct{}{"b": {}}

	score := LevelScore(cfg, L1, segs, pending)
	if score.Bytes != 800 {
		t.Fatalf("bytes: got %d, want 800", score.Bytes)
	}
	if score.PendingBytes != 400 {
		t.Fatalf("pending bytes: got %d, want 400", score.PendingBytes)
	}
	if score.Value != 0.8 {
		t.Fatalf("score: got %.2f, want 0.80", score.Value)
	}
}

func TestPlanAllCompactionsFilteredSkipsQueuedInputs(t *testing.T) {
	cfg := Config{L0Threshold: 4, FlushBytes: 100}
	c := NewCompactorWithConfig(cfg, testLogger())
	for i := 0; i < 4; i++ {
		c.AddSegment(scoreSegment(fmt.Sprintf("l0-%d", i), L0, 100, "p0"))
	}

	jobs := c.PlanAllCompactionsFiltered("main", PlanFilter{
		ExcludedInputIDs: map[string]struct{}{"l0-1": {}},
	})
	if len(jobs) != 0 {
		t.Fatalf("jobs with queued input: got %d, want 0", len(jobs))
	}
}

func TestJobQueueOrdersHigherScoreWithinPriority(t *testing.T) {
	q := jobQueue{}
	heap.Init(&q)
	heap.Push(&q, &Job{Priority: PriorityL0ToL1, Score: 0.75, InputBytes: 10})
	heap.Push(&q, &Job{Priority: PriorityL0ToL1, Score: 1.25, InputBytes: 10})
	heap.Push(&q, &Job{Priority: PriorityL1ToL2, Score: 100, InputBytes: 10})

	first := heap.Pop(&q).(*Job)
	if first.Score != 1.25 {
		t.Fatalf("first score: got %.2f, want 1.25", first.Score)
	}

	second := heap.Pop(&q).(*Job)
	if second.Score != 0.75 {
		t.Fatalf("second score: got %.2f, want 0.75", second.Score)
	}
}
