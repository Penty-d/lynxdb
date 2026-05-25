package ingest

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/lynxbase/lynxdb/pkg/cluster/rpc"
	clusterpb "github.com/lynxbase/lynxdb/pkg/cluster/rpc/proto"
	"github.com/lynxbase/lynxdb/pkg/cluster/sharding"
	"github.com/lynxbase/lynxdb/pkg/event"
)

// AckLevel controls the durability guarantee for replicated batches.
type AckLevel int

const (
	// AckNone means fire-and-forget — no durability guarantee beyond local commit.
	AckNone AckLevel = iota
	// AckOne means the batch is acknowledged after the local primary commits.
	AckOne
	// AckAll means the batch is acknowledged only after all ISR members confirm.
	AckAll
)

// ParseAckLevel converts a config string to an AckLevel.
// Returns AckOne for unrecognized values.
func ParseAckLevel(s string) AckLevel {
	switch s {
	case "none":
		return AckNone
	case "all":
		return AckAll
	default:
		return AckOne
	}
}

// maxAsyncReplicateGoroutines caps concurrent fire-and-forget replication
// goroutines to prevent unbounded goroutine growth during network partitions.
const maxAsyncReplicateGoroutines = 64

// BatcherReplicator replicates event batches to ISR peer nodes after
// the primary commits locally. It supports three ack levels:
//   - AckNone: fire-and-forget replication (async, best-effort)
//   - AckOne: return after local commit (replication happens async)
//   - AckAll: wait for all ISR members to acknowledge before returning
//
// Thread-safe.
type BatcherReplicator struct {
	selfID     sharding.NodeID
	clientPool *rpc.ClientPool
	ackLevel   AckLevel
	isrTracker *ISRTracker
	logger     *slog.Logger
	asyncSem   chan struct{} // bounds concurrent fire-and-forget goroutines
}

// NewBatcherReplicator creates a new replicator.
func NewBatcherReplicator(
	selfID sharding.NodeID,
	pool *rpc.ClientPool,
	ackLevel AckLevel,
	tracker *ISRTracker,
	logger *slog.Logger,
) *BatcherReplicator {
	return &BatcherReplicator{
		selfID:     selfID,
		clientPool: pool,
		ackLevel:   ackLevel,
		isrTracker: tracker,
		logger:     logger,
		asyncSem:   make(chan struct{}, maxAsyncReplicateGoroutines),
	}
}

// ReplicateBatch sends a batch of events to all ISR peers for the given shard.
// The behavior depends on the configured AckLevel:
//   - AckNone: fire-and-forget, returns immediately
//   - AckOne: returns immediately (replication is async)
//   - AckAll: blocks until all ISR peers acknowledge
//
// peerAddrs maps NodeID to gRPC address for the ISR peers.
func (r *BatcherReplicator) ReplicateBatch(
	ctx context.Context,
	shardID string,
	events []*event.Event,
	batchSeq uint64,
	peerAddrs map[sharding.NodeID]string,
) error {
	members := r.isrTracker.ISRMembers(shardID)
	if len(members) == 0 {
		return nil // No ISR peers — nothing to replicate.
	}

	// Serialize events once for all peers.
	eventsData, err := msgpack.Marshal(events)
	if err != nil {
		return fmt.Errorf("ingest.BatcherReplicator.ReplicateBatch: marshal events: %w", err)
	}

	entry := &clusterpb.ReplicateEntry{
		ShardId:  shardID,
		Events:   eventsData,
		BatchSeq: batchSeq,
	}

	// Filter to peers that are not us.
	var peers []sharding.NodeID
	for _, m := range members {
		if m != r.selfID {
			peers = append(peers, m)
		}
	}

	if len(peers) == 0 {
		return nil
	}

	switch r.ackLevel {
	case AckNone, AckOne:
		// Fire-and-forget: send in goroutines with bounded concurrency.
		// If the semaphore is full, drop the replication attempt and log a warning.
		for _, peer := range peers {
			peer := peer
			select {
			case r.asyncSem <- struct{}{}:
				go func() {
					defer func() { <-r.asyncSem }()
					if err := r.sendToReplica(ctx, peer, peerAddrs[peer], shardID, entry); err != nil {
						r.logger.Warn("async replication failed",
							"shard_id", shardID,
							"peer", peer,
							"ack_level", r.ackLevel,
							"error", err)
					}
				}()
			default:
				r.logger.Warn("async replication dropped: concurrency limit reached",
					"shard_id", shardID,
					"peer", peer,
					"limit", maxAsyncReplicateGoroutines)
			}
		}

		return nil

	case AckAll:
		// Wait for all peers to acknowledge.
		// Use asyncSem to bound concurrent goroutines — blocks when the
		// semaphore is full rather than dropping (AckAll must guarantee delivery).
		type result struct {
			nodeID sharding.NodeID
			err    error
		}

		ch := make(chan result, len(peers))
		for _, peer := range peers {
			peer := peer
			if err := r.acquireAsyncSlot(ctx); err != nil {
				return fmt.Errorf("ingest.BatcherReplicator.ReplicateBatch: acquire async slot: %w", err)
			}
			go func() {
				defer func() { <-r.asyncSem }()
				err := r.sendToReplica(ctx, peer, peerAddrs[peer], shardID, entry)
				ch <- result{nodeID: peer, err: err}
			}()
		}

		var firstErr error
		for range peers {
			var res result
			select {
			case res = <-ch:
			case <-ctx.Done():
				return ctx.Err()
			}
			if res.err != nil && firstErr == nil {
				firstErr = res.err
				r.logger.Warn("ISR replication failed",
					"shard_id", shardID,
					"peer", res.nodeID,
					"error", res.err)
			}
		}

		return firstErr

	default:
		return nil
	}
}

func (r *BatcherReplicator) acquireAsyncSlot(ctx context.Context) error {
	select {
	case r.asyncSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// sendToReplica sends a single ReplicateEntry to a peer via gRPC streaming.
// On success, it updates the ISR tracker with the peer's confirmed batch_seq.
func (r *BatcherReplicator) sendToReplica(
	ctx context.Context,
	peerID sharding.NodeID,
	peerAddr string,
	shardID string,
	entry *clusterpb.ReplicateEntry,
) error {
	if peerAddr == "" {
		return fmt.Errorf("no address for peer %s", peerID)
	}

	conn, err := r.clientPool.GetConn(ctx, peerAddr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", peerAddr, err)
	}

	client := clusterpb.NewIngestServiceClient(conn)
	stream, err := client.ReplicateBatch(ctx)
	if err != nil {
		return fmt.Errorf("open stream to %s: %w", peerID, err)
	}

	if err := stream.Send(entry); err != nil {
		return fmt.Errorf("send to %s: %w", peerID, err)
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("close stream to %s: %w", peerID, err)
	}

	if resp.Ok {
		r.isrTracker.UpdateReplicatedSeq(shardID, peerID, resp.LastBatchSeq)
	}

	return nil
}
