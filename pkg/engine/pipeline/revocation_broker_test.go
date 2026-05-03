package pipeline

import (
	"testing"

	"github.com/lynxbase/lynxdb/pkg/memgov"
)

func TestRevocationBrokerWeightsByRevocableUsage(t *testing.T) {
	budget := int64(100 << 20)
	mon := memgov.NewTestBudget("test", budget)

	mcA := NewMemoryCoordinator(budget, 0.10)
	acctA := mcA.RegisterOperator("sort-a", mon.NewAccount("sort-a"), reservationSort)
	mcA.Finalize()
	acctA.SetOnRevoke(func(target int64) int64 {
		freed := target
		if freed > acctA.Used() {
			freed = acctA.Used()
		}
		acctA.Shrink(freed)

		return freed
	})
	if err := acctA.Grow(9000); err != nil {
		t.Fatalf("grow acctA: %v", err)
	}

	mcB := NewMemoryCoordinator(budget, 0.10)
	acctB := mcB.RegisterOperator("sort-b", mon.NewAccount("sort-b"), reservationSort)
	mcB.Finalize()
	acctB.SetOnRevoke(func(target int64) int64 {
		freed := target
		if freed > acctB.Used() {
			freed = acctB.Used()
		}
		acctB.Shrink(freed)

		return freed
	})
	if err := acctB.Grow(1000); err != nil {
		t.Fatalf("grow acctB: %v", err)
	}

	broker := newRevocationBroker()
	unregA := broker.Register(mcA)
	defer unregA()
	unregB := broker.Register(mcB)
	defer unregB()

	freed := broker.HandleRevocation(1000)
	if freed != 1000 {
		t.Fatalf("freed = %d, want 1000", freed)
	}
	if got := acctA.Used(); got != 8100 {
		t.Fatalf("acctA used = %d, want 8100", got)
	}
	if got := acctB.Used(); got != 900 {
		t.Fatalf("acctB used = %d, want 900", got)
	}
}

func TestRevocationBrokerUnregister(t *testing.T) {
	budget := int64(100 << 20)
	mon := memgov.NewTestBudget("test", budget)

	mc := NewMemoryCoordinator(budget, 0.10)
	acct := mc.RegisterOperator("sort", mon.NewAccount("sort"), reservationSort)
	mc.Finalize()
	called := false
	acct.SetOnRevoke(func(target int64) int64 {
		called = true

		return 0
	})
	if err := acct.Grow(1000); err != nil {
		t.Fatalf("grow acct: %v", err)
	}

	broker := newRevocationBroker()
	unregister := broker.Register(mc)
	unregister()

	if freed := broker.HandleRevocation(1000); freed != 0 {
		t.Fatalf("freed = %d, want 0", freed)
	}
	if called {
		t.Fatal("revocation callback called after unregister")
	}
}
