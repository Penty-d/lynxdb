package sharding

import (
	"fmt"
	"testing"
	"time"
)

func TestAssignShard_Deterministic(t *testing.T) {
	cfg := ShardConfig{
		TimeBucketSize:        24 * time.Hour,
		VirtualPartitionCount: 1024,
	}
	ts := time.Date(2026, 3, 9, 14, 30, 0, 0, time.UTC)

	s1 := AssignShard("nginx", "web-01", "main", ts, cfg)
	s2 := AssignShard("nginx", "web-01", "main", ts, cfg)

	if s1 != s2 {
		t.Errorf("AssignShard not deterministic: %v != %v", s1, s2)
	}
}

func TestAssignShard_TimeBucketing(t *testing.T) {
	cfg := ShardConfig{
		TimeBucketSize:        24 * time.Hour,
		VirtualPartitionCount: 1024,
	}

	// Two timestamps in the same day should land in the same bucket.
	ts1 := time.Date(2026, 3, 9, 2, 0, 0, 0, time.UTC)
	ts2 := time.Date(2026, 3, 9, 23, 59, 59, 0, time.UTC)

	s1 := AssignShard("nginx", "web-01", "main", ts1, cfg)
	s2 := AssignShard("nginx", "web-01", "main", ts2, cfg)

	if s1.TimeBucket != s2.TimeBucket {
		t.Errorf("same-day timestamps got different buckets: %v vs %v", s1.TimeBucket, s2.TimeBucket)
	}

	// Different day should be a different bucket.
	ts3 := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	s3 := AssignShard("nginx", "web-01", "main", ts3, cfg)

	if s1.TimeBucket.Equal(s3.TimeBucket) {
		t.Errorf("different-day timestamps got same bucket: %v", s1.TimeBucket)
	}
}

func TestAssignShard_Distribution(t *testing.T) {
	cfg := ShardConfig{
		TimeBucketSize:        24 * time.Hour,
		VirtualPartitionCount: 64,
	}
	ts := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)

	// Generate many shards with diverse keys and check distribution.
	partitions := make(map[uint32]int)
	n := 10000

	for i := 0; i < n; i++ {
		source := fmt.Sprintf("source-%d", i)
		host := fmt.Sprintf("host-%d", i%100)
		s := AssignShard(source, host, "main", ts, cfg)
		partitions[s.Partition]++
	}

	// All 64 partitions should be hit given 10000 unique keys.
	if len(partitions) < 50 {
		t.Errorf("poor distribution: only %d of 64 partitions used", len(partitions))
	}
}

func TestAssignShard_DifferentHosts(t *testing.T) {
	cfg := ShardConfig{
		TimeBucketSize:        24 * time.Hour,
		VirtualPartitionCount: 1024,
	}
	ts := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)

	s1 := AssignShard("nginx", "web-01", "main", ts, cfg)
	s2 := AssignShard("nginx", "web-02", "main", ts, cfg)

	// Different hosts should (likely) get different partitions.
	// This is probabilistic but with 1024 partitions it's very unlikely to collide.
	if s1.Partition == s2.Partition {
		t.Log("warning: different hosts got same partition (statistically possible but unlikely)")
	}
}

func TestShardID_String(t *testing.T) {
	s := ShardID{
		Index:      "main",
		TimeBucket: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC),
		Partition:  42,
	}

	want := "main/t2026-03-09/p42"
	if got := s.String(); got != want {
		t.Errorf("ShardID.String() = %q, want %q", got, want)
	}
}
