package compaction

import (
	"fmt"
	"sort"
	"strings"
)

// Strategy produces compaction plans for a set of segments.
type Strategy interface {
	// Plan returns zero or more compaction plans for the given segments.
	Plan(segments []*SegmentInfo) []*Plan
}

// JobPriority determines the scheduling order for compaction jobs.
type JobPriority int

const (
	PriorityL0ToL1    JobPriority = 0 // highest — flush pressure
	PriorityL1ToL2Hot JobPriority = 1 // hot data, recent queries
	PriorityL1ToL2    JobPriority = 2 // warm data
	PriorityIntraL2   JobPriority = 3 // L2 self-merge: consolidate small L2 parts
	PriorityL2ToL3    JobPriority = 4 // cold partition archive
	PriorityMaint     JobPriority = 5 // lowest — maintenance
)

// Job wraps a Plan with scheduling metadata.
type Job struct {
	Plan       *Plan
	Priority   JobPriority
	Index      string
	Partition  string // time partition key (e.g., "2026-03-02"); empty for in-memory mode
	Score      float64
	InputIDs   []string
	InputBytes int64
}

// DedupeKey identifies the same logical compaction job across repeated
// reactive and periodic scheduler submissions.
func (j *Job) DedupeKey() string {
	if j == nil || j.Plan == nil {
		return ""
	}
	ids := append([]string(nil), j.InputIDs...)
	if len(ids) == 0 {
		ids = planInputIDs(j.Plan)
	}
	sort.Strings(ids)

	return fmt.Sprintf("%s\x00%s\x00%d\x00%s",
		j.Index, j.Partition, j.Plan.OutputLevel, strings.Join(ids, "\x00"))
}
