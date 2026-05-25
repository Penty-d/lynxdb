package memgov

import (
	"sync"
	"sync/atomic"
)

// Lease represents a scoped memory reservation. Must be Released when no
// longer needed. Safe to call Release multiple times (idempotent).
//
// Leases are the primary API for consumer-level memory management, replacing
// the old Grow/Shrink/Close pattern. The Borrow() → *Lease flow with
// defer lease.Release() makes leaks harder than the old Grow()/Shrink()/Close()
// pattern where forgetting Shrink() or Close() silently leaked bytes.
type Lease struct {
	gov       Governor
	class     MemoryClass
	bytes     int64
	released  atomic.Bool
	onRelease func(MemoryClass, int64)
}

// Release returns the reserved bytes to the governor.
// Safe to call multiple times (idempotent). Nil-safe.
func (l *Lease) Release() {
	if l == nil || !l.released.CompareAndSwap(false, true) {
		return
	}
	if l.gov != nil && l.bytes > 0 {
		l.gov.Release(l.class, l.bytes)
	}
	if l.onRelease != nil {
		l.onRelease(l.class, l.bytes)
	}
}

// Bytes returns the number of bytes held by this lease.
// Nil-safe: returns 0 if lease is nil.
func (l *Lease) Bytes() int64 {
	if l == nil {
		return 0
	}

	return l.bytes
}

// IsReleased reports whether this lease has been released.
// Nil-safe: returns true if lease is nil.
func (l *Lease) IsReleased() bool {
	if l == nil {
		return true
	}

	return l.released.Load()
}

// QueryBudget manages per-query memory with lease tracking.
// Replaces the combination of budget tracking and per-operator accounting.
//
// Thread-safe. Protected by an internal mutex to support concurrent branch
// goroutines (ConcurrentUnionIterator).
type QueryBudget interface {
	// Borrow requests n bytes for the given class.
	// Returns a Lease that MUST be Released.
	Borrow(class MemoryClass, n int64) (*Lease, error)

	// TryBorrow is non-blocking: returns (nil, false) on pressure.
	TryBorrow(class MemoryClass, n int64) (*Lease, bool)

	// PinnedBytes returns bytes in ClassNonRevocable.
	PinnedBytes() int64

	// RevocableBytes returns bytes in ClassRevocable + ClassSpillable.
	RevocableBytes() int64

	// Close releases all outstanding leases. Logs leak warnings in debug mode.
	Close()
}

// queryBudget is the concrete QueryBudget implementation.
type queryBudget struct {
	mu     sync.Mutex
	gov    Governor
	label  string
	leases []*Lease
	closed bool

	// Per-class tracking for fast PinnedBytes/RevocableBytes queries.
	byClass [numClasses]int64
	peak    [numClasses]int64
}

// NewQueryBudget creates a per-query budget backed by the global governor.
func NewQueryBudget(gov Governor, label string) QueryBudget {
	return &queryBudget{
		gov:   gov,
		label: label,
	}
}

func (qb *queryBudget) Borrow(class MemoryClass, n int64) (*Lease, error) {
	if n <= 0 {
		return releasedLease(qb.gov, class), nil
	}

	qb.mu.Lock()
	if qb.closed {
		qb.mu.Unlock()
		return nil, ErrClosed
	}
	qb.mu.Unlock()

	if err := qb.gov.Reserve(class, n); err != nil {
		return nil, err
	}

	lease := qb.newLease(class, n)
	trackLease(lease)

	qb.mu.Lock()
	if qb.closed {
		qb.mu.Unlock()
		lease.Release()
		return nil, ErrClosed
	}
	qb.leases = append(qb.leases, lease)
	qb.byClass[class] += n
	if qb.byClass[class] > qb.peak[class] {
		qb.peak[class] = qb.byClass[class]
	}
	qb.mu.Unlock()

	return lease, nil
}

func (qb *queryBudget) TryBorrow(class MemoryClass, n int64) (*Lease, bool) {
	if n <= 0 {
		return releasedLease(qb.gov, class), true
	}

	qb.mu.Lock()
	if qb.closed {
		qb.mu.Unlock()
		return nil, false
	}
	qb.mu.Unlock()

	if !qb.gov.TryReserve(class, n) {
		return nil, false
	}

	lease := qb.newLease(class, n)
	trackLease(lease)

	qb.mu.Lock()
	if qb.closed {
		qb.mu.Unlock()
		lease.Release()
		return nil, false
	}
	qb.leases = append(qb.leases, lease)
	qb.byClass[class] += n
	if qb.byClass[class] > qb.peak[class] {
		qb.peak[class] = qb.byClass[class]
	}
	qb.mu.Unlock()

	return lease, true
}

func (qb *queryBudget) PinnedBytes() int64 {
	qb.mu.Lock()
	defer qb.mu.Unlock()

	return qb.byClass[ClassNonRevocable]
}

func (qb *queryBudget) RevocableBytes() int64 {
	qb.mu.Lock()
	defer qb.mu.Unlock()

	return qb.byClass[ClassRevocable] + qb.byClass[ClassSpillable]
}

func (qb *queryBudget) Close() {
	qb.mu.Lock()
	if qb.closed {
		qb.mu.Unlock()
		return
	}
	qb.closed = true
	leases := qb.leases
	qb.leases = nil
	qb.mu.Unlock()

	for _, lease := range leases {
		lease.Release()
	}

	qb.mu.Lock()
	for i := range qb.byClass {
		qb.byClass[i] = 0
	}
	qb.mu.Unlock()
}

func (qb *queryBudget) newLease(class MemoryClass, n int64) *Lease {
	return &Lease{
		gov:   qb.gov,
		class: class,
		bytes: n,
		onRelease: func(class MemoryClass, n int64) {
			qb.mu.Lock()
			qb.byClass[class] -= n
			if qb.byClass[class] < 0 {
				qb.byClass[class] = 0
			}
			qb.mu.Unlock()
		},
	}
}

func releasedLease(gov Governor, class MemoryClass) *Lease {
	lease := &Lease{gov: gov, class: class}
	lease.released.Store(true)
	return lease
}
