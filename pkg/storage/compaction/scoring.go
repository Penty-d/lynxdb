package compaction

import "math"

const defaultFlushBytesForScore = int64(64 << 20)

// Score describes compaction debt for one partition level.
type Score struct {
	Level        int
	Value        float64
	Count        int
	Bytes        int64
	TargetBytes  int64
	PendingBytes int64
}

// PlanFilter excludes inputs that are already queued or active.
type PlanFilter struct {
	ExcludedInputIDs map[string]struct{}
}

func (f PlanFilter) includes(plan *Plan) bool {
	if len(f.ExcludedInputIDs) == 0 {
		return true
	}
	for _, seg := range plan.InputSegments {
		if _, ok := f.ExcludedInputIDs[seg.Meta.ID]; ok {
			return false
		}
	}

	return true
}

func (c Config) l0ByteTarget() int64 {
	flushBytes := c.FlushBytes
	if flushBytes <= 0 {
		flushBytes = defaultFlushBytesForScore
	}
	threshold := c.L0Threshold
	if threshold <= 0 {
		threshold = L0CompactionThreshold
	}

	return flushBytes * int64(threshold)
}

// LevelScore computes RocksDB-style compaction pressure for a partition level.
func LevelScore(cfg Config, level int, segments []*SegmentInfo, pendingInputIDs map[string]struct{}) Score {
	cfg = cfg.withDefaults()

	var count int
	var bytes int64
	var pendingBytes int64
	for _, seg := range segments {
		if seg.Meta.Level != level {
			continue
		}
		if _, ok := pendingInputIDs[seg.Meta.ID]; ok {
			pendingBytes += seg.Meta.SizeBytes
			continue
		}
		count++
		bytes += seg.Meta.SizeBytes
	}

	score := Score{
		Level:        level,
		Count:        count,
		Bytes:        bytes,
		PendingBytes: pendingBytes,
	}

	switch level {
	case L0:
		score.TargetBytes = cfg.l0ByteTarget()
		countScore := float64(count) / float64(cfg.L0Threshold)
		byteScore := float64(bytes) / float64(score.TargetBytes)
		score.Value = math.Max(countScore, byteScore)
	case L1:
		score.TargetBytes = cfg.L2TargetSize
		score.Value = float64(bytes) / float64(score.TargetBytes)
	case L2:
		score.TargetBytes = cfg.L2TargetSize
		score.Value = float64(bytes) / float64(score.TargetBytes)
	default:
		score.TargetBytes = cfg.L2TargetSize
		score.Value = float64(bytes) / float64(score.TargetBytes)
	}

	return score
}

func planInputIDs(plan *Plan) []string {
	ids := make([]string, 0, len(plan.InputSegments))
	for _, seg := range plan.InputSegments {
		ids = append(ids, seg.Meta.ID)
	}

	return ids
}

func planInputBytes(plan *Plan) int64 {
	var bytes int64
	for _, seg := range plan.InputSegments {
		bytes += seg.Meta.SizeBytes
	}

	return bytes
}

func planBaseLevel(plan *Plan) int {
	if len(plan.InputSegments) == 0 {
		return plan.OutputLevel
	}

	return plan.InputSegments[0].Meta.Level
}
