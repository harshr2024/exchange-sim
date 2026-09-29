package engine

import (
	"strings"
	"testing"
)

// TestInvariantClassesCatchDifferentFaults checks that the split between semantic and
// implementation invariants is real rather than editorial: each class has to detect
// faults the other misses.
//
// It corrupts book state deliberately, which is only possible from inside the package.
func TestInvariantClassesCatchDifferentFaults(t *testing.T) {
	t.Run("a broken cached counter is an implementation fault", func(t *testing.T) {
		book := seededBook(t)

		// Nothing a client can observe has changed: the levels, the queues and the
		// quantities are all still correct. Only the counter that decides when
		// compaction runs is wrong.
		book.bids.resting++

		if err := book.CheckSemanticInvariants(); err != nil {
			t.Fatalf("the semantic check fired on a purely internal fault: %v", err)
		}
		err := book.CheckImplementationInvariants()
		if err == nil {
			t.Fatal("the implementation check missed a corrupted counter")
		}
		if !strings.Contains(err.Error(), "resting orders") {
			t.Fatalf("unexpected complaint: %v", err)
		}
	})

	t.Run("a broken quantity is a semantic fault", func(t *testing.T) {
		book := seededBook(t)

		// Every pointer, counter and level total is still intact, so the structure is
		// sound. But the order now claims to have filled quantity it never had, which
		// is a statement about the exchange being wrong.
		for _, loc := range book.index {
			loc.node.order.FilledQty += 5
			break
		}

		if err := book.CheckImplementationInvariants(); err != nil {
			t.Fatalf("the implementation check fired on a purely semantic fault: %v", err)
		}
		err := book.CheckSemanticInvariants()
		if err == nil {
			t.Fatal("the semantic check missed a quantity that does not conserve")
		}
		if !strings.Contains(err.Error(), "conserve") {
			t.Fatalf("unexpected complaint: %v", err)
		}
	})

	t.Run("a crossed book is a semantic fault", func(t *testing.T) {
		book := seededBook(t)

		// Insert a bid above the best ask without matching it, which is precisely
		// what a broken matching loop would leave behind.
		o := NewOrder(999, 999, "saboteur", "TEST", Buy, Limit, 10500, 10)
		lvl, n := book.bids.push(o)
		book.index[o.ID] = &locator{side: Buy, level: lvl, node: n}

		if err := book.CheckImplementationInvariants(); err != nil {
			t.Fatalf("the implementation check fired on a crossed book: %v", err)
		}
		err := book.CheckSemanticInvariants()
		if err == nil {
			t.Fatal("the semantic check missed a crossed book")
		}
		if !strings.Contains(err.Error(), "crossed") {
			t.Fatalf("unexpected complaint: %v", err)
		}
	})

	t.Run("a stale index entry is an implementation fault", func(t *testing.T) {
		book := seededBook(t)

		// Point an order's locator at the wrong level. Cancellation would then
		// corrupt a level it does not belong to, but nothing is visibly wrong yet.
		var moved bool
		for _, loc := range book.index {
			if loc.side == Buy {
				for price, lvl := range book.bids.levels {
					if price != loc.level.Price {
						loc.level = lvl
						moved = true
						break
					}
				}
			}
			if moved {
				break
			}
		}
		if !moved {
			t.Skip("the seeded book had too few bid levels to redirect a locator")
		}

		if err := book.CheckSemanticInvariants(); err != nil {
			t.Fatalf("the semantic check fired on a stale locator: %v", err)
		}
		if err := book.CheckImplementationInvariants(); err == nil {
			t.Fatal("the implementation check missed a stale locator")
		}
	})
}

// seededBook returns a small two-sided book that satisfies every invariant.
func seededBook(t *testing.T) *Book {
	t.Helper()

	book := NewBook("TEST")
	var bld builder
	for i := range 4 {
		book.Submit(bld.limit(Buy, PriceTicks(9990-i), 10))
		book.Submit(bld.limit(Sell, PriceTicks(10010+i), 10))
	}
	if err := book.CheckInvariants(); err != nil {
		t.Fatalf("the seeded book is already broken: %v", err)
	}
	return book
}
