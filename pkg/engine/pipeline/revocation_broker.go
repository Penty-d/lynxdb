package pipeline

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/lynxbase/lynxdb/pkg/memgov"
)

var revocationBrokers sync.Map
var revocationFreedSpillableBytes atomic.Int64

// RevocationFreedSpillableBytes returns the cumulative bytes freed by
// governor-driven spillable revocation callbacks in this process.
func RevocationFreedSpillableBytes() int64 {
	return revocationFreedSpillableBytes.Load()
}

// RegisterRevocationCoordinator registers a query coordinator with the
// process-level spillable revocation broker for this governor.
func RegisterRevocationCoordinator(gov memgov.Governor, mc *MemoryCoordinator) func() {
	if gov == nil || mc == nil {
		return func() {}
	}
	actual, loaded := revocationBrokers.LoadOrStore(gov, newRevocationBroker())
	broker := actual.(*RevocationBroker)
	if !loaded {
		gov.OnPressure(memgov.ClassSpillable, broker.HandleRevocation)
	}

	return broker.Register(mc)
}

// RevocationBroker fans out governor spillable pressure across active query
// coordinators, weighted by each coordinator's current revocable usage.
type RevocationBroker struct {
	mu           sync.Mutex
	coordinators map[*MemoryCoordinator]struct{}
}

func newRevocationBroker() *RevocationBroker {
	return &RevocationBroker{
		coordinators: make(map[*MemoryCoordinator]struct{}),
	}
}

func (rb *RevocationBroker) Register(mc *MemoryCoordinator) func() {
	rb.mu.Lock()
	rb.coordinators[mc] = struct{}{}
	rb.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			rb.mu.Lock()
			delete(rb.coordinators, mc)
			rb.mu.Unlock()
		})
	}
}

func (rb *RevocationBroker) HandleRevocation(target int64) int64 {
	if target <= 0 {
		return 0
	}

	type candidate struct {
		mc   *MemoryCoordinator
		used int64
	}

	rb.mu.Lock()
	candidates := make([]candidate, 0, len(rb.coordinators))
	for mc := range rb.coordinators {
		used := mc.RevocableUsage()
		if used > 0 {
			candidates = append(candidates, candidate{mc: mc, used: used})
		}
	}
	rb.mu.Unlock()
	if len(candidates) == 0 {
		return 0
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].used > candidates[j].used
	})

	var totalUsed int64
	for _, c := range candidates {
		totalUsed += c.used
	}

	var totalFreed int64
	remaining := target
	remainingUsed := totalUsed
	for _, c := range candidates {
		if remaining <= 0 {
			break
		}
		share := remaining
		if remainingUsed > 0 {
			share = remaining * c.used / remainingUsed
			if share <= 0 {
				share = 1
			}
		}
		if share > remaining {
			share = remaining
		}
		freed := c.mc.HandleRevocation(share)
		if freed > 0 {
			totalFreed += freed
			remaining -= freed
			if remaining < 0 {
				remaining = 0
			}
		}
		remainingUsed -= c.used
	}
	if totalFreed > 0 {
		revocationFreedSpillableBytes.Add(totalFreed)
	}

	return totalFreed
}
