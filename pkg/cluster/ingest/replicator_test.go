package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lynxbase/lynxdb/pkg/cluster/sharding"
	"github.com/lynxbase/lynxdb/pkg/event"
)

func TestBatcherReplicator_AckAllAcquireHonorsContext(t *testing.T) {
	tracker := NewISRTracker()
	tracker.SetISR("shard-a", []sharding.NodeID{"node-1", "node-2"})
	replicator := NewBatcherReplicator(
		"node-1",
		nil,
		AckAll,
		tracker,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	for i := 0; i < cap(replicator.asyncSem); i++ {
		replicator.asyncSem <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := replicator.ReplicateBatch(
		ctx,
		"shard-a",
		[]*event.Event{event.NewEvent(time.Now(), "test")},
		1,
		map[sharding.NodeID]string{"node-2": "127.0.0.1:1"},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReplicateBatch: got %v, want %v", err, context.Canceled)
	}
}
