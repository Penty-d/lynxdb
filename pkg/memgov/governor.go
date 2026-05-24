package memgov

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// ErrMemoryPressure is returned when the governor cannot satisfy a reservation
// even after attempting to reclaim memory from lower-priority classes.
var ErrMemoryPressure = errors.New("memory pressure: unable to reserve requested bytes")

// Governor manages the process-wide memory budget across all memory classes.
// It replaces the legacy memory management types from pkg/stats.
//
// Thread-safe. All methods may be called concurrently.
type Governor interface {
	// Reserve requests n bytes of the given class.
	// May trigger pressure callbacks to reclaim memory from lower-priority classes.
	// Returns ErrMemoryPressure if admission is denied after reclamation attempts.
	Reserve(class MemoryClass, n int64) error

	// TryReserve is non-blocking: returns false instead of attempting reclamation.
	TryReserve(class MemoryClass, n int64) bool

	// Release returns n bytes of the given class.
	Release(class MemoryClass, n int64)

	// ClassUsage returns current/peak/limit for a class.
	ClassUsage(class MemoryClass) ClassStats

	// TotalUsage returns aggregate stats across all classes.
	TotalUsage() TotalStats

	// OnPressure registers a callback invoked when the governor needs to
	// reclaim memory. Called in priority order:
	//   1. Revocable  2. PageCache  3. Spillable  4. Metadata  5. TempIO
	OnPressure(class MemoryClass, cb PressureCallback)
}

// globalGovernor is the concrete Governor implementation.
type globalGovernor struct {
	mu    sync.Mutex
	limit int64 // total RSS budget (0 = unlimited)

	allocated [numClasses]int64
	peak      [numClasses]int64
	limits    [numClasses]int64 // per-class limits (0 = no per-class limit)

	totalAllocated int64
	totalPeak      int64

	pressure *pressureRegistry

	classPeakHistory [numClasses]rollingPeakWindow
	totalPeakHistory rollingPeakWindow

	// Metrics (atomic for lock-free reads in hot paths).
	reserveCount  atomic.Int64
	releaseCount  atomic.Int64
	pressureCount atomic.Int64
}

type peakBucket struct {
	minute int64
	peak   int64
}

type rollingPeakWindow struct {
	buckets [24 * 60]peakBucket
}

// GovernorConfig configures the global governor.
type GovernorConfig struct {
	// TotalLimit is the total RSS budget in bytes. 0 = unlimited (tracking only).
	TotalLimit int64

	// ClassLimits optionally sets per-class limits. 0 = no per-class limit.
	ClassLimits [numClasses]int64
}

// NewGovernor creates a new global memory governor.
func NewGovernor(cfg GovernorConfig) Governor {
	g := &globalGovernor{
		limit:    cfg.TotalLimit,
		limits:   cfg.ClassLimits,
		pressure: newPressureRegistry(),
	}

	return g
}

func (g *globalGovernor) Reserve(class MemoryClass, n int64) error {
	if n <= 0 {
		return nil
	}

	g.reserveCount.Add(1)

	g.mu.Lock()

	if g.limits[class] > 0 && g.allocated[class]+n > g.limits[class] {
		current := g.allocated[class]
		limit := g.limits[class]
		g.mu.Unlock()

		return fmt.Errorf("%w: class %s limit exceeded (requested=%d, current=%d, limit=%d)",
			ErrMemoryPressure, class, n, current, limit)
	}

	if g.limit > 0 && g.totalAllocated+n > g.limit {
		// Need to reclaim memory. Release lock before invoking callbacks.
		deficit := g.totalAllocated + n - g.limit
		g.mu.Unlock()

		g.pressureCount.Add(1)
		freed := g.pressure.invoke(deficit)

		// Re-acquire lock and re-check after reclamation.
		g.mu.Lock()
		if g.limit > 0 && g.totalAllocated+n > g.limit {
			current := g.totalAllocated
			limit := g.limit
			g.mu.Unlock()
			_ = freed // reclamation wasn't enough

			return fmt.Errorf("%w: total limit exceeded (requested=%d, current=%d, limit=%d, freed=%d)",
				ErrMemoryPressure, n, current, limit, freed)
		}
	}

	// Admit the reservation.
	g.allocated[class] += n
	if g.allocated[class] > g.peak[class] {
		g.peak[class] = g.allocated[class]
	}
	g.totalAllocated += n
	if g.totalAllocated > g.totalPeak {
		g.totalPeak = g.totalAllocated
	}
	g.recordRollingPeakLocked(class)

	g.mu.Unlock()

	return nil
}

func (g *globalGovernor) TryReserve(class MemoryClass, n int64) bool {
	if n <= 0 {
		return true
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.limits[class] > 0 && g.allocated[class]+n > g.limits[class] {
		return false
	}

	if g.limit > 0 && g.totalAllocated+n > g.limit {
		return false
	}

	// Admit the reservation.
	g.allocated[class] += n
	if g.allocated[class] > g.peak[class] {
		g.peak[class] = g.allocated[class]
	}
	g.totalAllocated += n
	if g.totalAllocated > g.totalPeak {
		g.totalPeak = g.totalAllocated
	}
	g.recordRollingPeakLocked(class)

	return true
}

func (g *globalGovernor) Release(class MemoryClass, n int64) {
	if n <= 0 {
		return
	}

	g.releaseCount.Add(1)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.allocated[class] -= n
	if g.allocated[class] < 0 {
		g.allocated[class] = 0
	}
	g.totalAllocated -= n
	if g.totalAllocated < 0 {
		g.totalAllocated = 0
	}
}

func (g *globalGovernor) ClassUsage(class MemoryClass) ClassStats {
	g.mu.Lock()
	defer g.mu.Unlock()

	return ClassStats{
		Allocated: g.allocated[class],
		Peak:      g.peak[class],
		Peak60s:   g.classPeakHistory[class].maxSince(currentMinute(), 1),
		Peak24h:   g.classPeakHistory[class].maxSince(currentMinute(), 24*60),
		Limit:     g.limits[class],
	}
}

func (g *globalGovernor) TotalUsage() TotalStats {
	g.mu.Lock()
	defer g.mu.Unlock()

	ts := TotalStats{
		Allocated:      g.totalAllocated,
		Peak:           g.totalPeak,
		Limit:          g.limit,
		ReserveEvents:  g.reserveCount.Load(),
		ReleaseEvents:  g.releaseCount.Load(),
		PressureEvents: g.pressureCount.Load(),
	}
	nowMinute := currentMinute()
	for i := MemoryClass(0); i < numClasses; i++ {
		ts.ByClass[i] = ClassStats{
			Allocated: g.allocated[i],
			Peak:      g.peak[i],
			Peak60s:   g.classPeakHistory[i].maxSince(nowMinute, 1),
			Peak24h:   g.classPeakHistory[i].maxSince(nowMinute, 24*60),
			Limit:     g.limits[i],
		}
	}

	return ts
}

func (g *globalGovernor) OnPressure(class MemoryClass, cb PressureCallback) {
	g.pressure.register(class, cb)
}

func (g *globalGovernor) recordRollingPeakLocked(class MemoryClass) {
	now := currentMinute()
	g.classPeakHistory[class].record(now, g.allocated[class])
	g.totalPeakHistory.record(now, g.totalAllocated)
}

func currentMinute() int64 {
	return time.Now().Unix() / 60
}

func (w *rollingPeakWindow) record(minute, value int64) {
	idx := minute % int64(len(w.buckets))
	if idx < 0 {
		idx += int64(len(w.buckets))
	}
	b := &w.buckets[idx]
	if b.minute != minute {
		b.minute = minute
		b.peak = value
		return
	}
	if value > b.peak {
		b.peak = value
	}
}

func (w *rollingPeakWindow) maxSince(nowMinute, windowMinutes int64) int64 {
	var max int64
	cutoff := nowMinute - windowMinutes + 1
	for i := range w.buckets {
		b := w.buckets[i]
		if b.minute < cutoff || b.minute > nowMinute {
			continue
		}
		if b.peak > max {
			max = b.peak
		}
	}

	return max
}
