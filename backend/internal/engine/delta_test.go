package engine

import (
	"testing"
)

// applyAndTake returns the level changes produced by one command, which is what the
// market data layer publishes instead of a full snapshot.
func (h *harness) changes() []LevelChange {
	h.t.Helper()
	return h.book.TakeLevelChanges()
}

func TestLevelChangesReportNewLiquidity(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10010, 30)
	changes := h.changes()

	want := []LevelChange{{Side: Sell, Price: 10010, Quantity: 30, Orders: 1}}
	assertChanges(t, changes, want)

	// A second order at the same price reports the level's new total, not the delta.
	h.limit("maker", Sell, 10010, 20)
	assertChanges(t, h.changes(), []LevelChange{
		{Side: Sell, Price: 10010, Quantity: 50, Orders: 2},
	})
}

func TestLevelChangesReportAPartialFill(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10010, 100)
	h.changes()

	// Taking 40 leaves the level in place with less on it, which is a change to the
	// advertised quantity with no order removed.
	h.market("taker", Buy, 40)
	assertChanges(t, h.changes(), []LevelChange{
		{Side: Sell, Price: 10010, Quantity: 60, Orders: 1},
	})
}

func TestLevelChangesReportADrainedLevelAsZero(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10010, 30)
	h.changes()

	h.market("taker", Buy, 30)
	assertChanges(t, h.changes(), []LevelChange{
		{Side: Sell, Price: 10010, Quantity: 0, Orders: 0},
	})
}

// TestLevelChangesCoverAWholeSweep is the case incremental updates have to get right:
// one command touching many levels on one side and creating one on the other.
func TestLevelChangesCoverAWholeSweep(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10010, 10)
	h.limit("maker", Sell, 10020, 20)
	h.limit("maker", Sell, 10030, 30)
	h.changes()

	// Buys 45: clears 100.10 and 100.20, takes 15 of 100.30, nothing rests.
	h.limit("taker", Buy, 10030, 45)

	assertChanges(t, h.changes(), []LevelChange{
		{Side: Sell, Price: 10010, Quantity: 0, Orders: 0},
		{Side: Sell, Price: 10020, Quantity: 0, Orders: 0},
		{Side: Sell, Price: 10030, Quantity: 15, Orders: 1},
	})
}

func TestLevelChangesCoverBothSidesWhenAnOrderRests(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10010, 10)
	h.changes()

	// Buys 50 at 100.10: fills 10, rests 40 as a new bid level.
	h.limit("taker", Buy, 10010, 50)

	assertChanges(t, h.changes(), []LevelChange{
		{Side: Buy, Price: 10010, Quantity: 40, Orders: 1},
		{Side: Sell, Price: 10010, Quantity: 0, Orders: 0},
	})
}

func TestLevelChangesReportACancellation(t *testing.T) {
	h := newHarness(t)

	a, _ := h.limit("A", Buy, 9990, 10)
	h.limit("B", Buy, 9990, 25)
	h.changes()

	if err := h.cancel(a); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	assertChanges(t, h.changes(), []LevelChange{
		{Side: Buy, Price: 9990, Quantity: 25, Orders: 1},
	})
}

func TestLevelChangesAreEmptyWhenNothingHappens(t *testing.T) {
	h := newHarness(t)

	// A market order against an empty book trades nothing and rests nothing.
	h.market("taker", Buy, 50)
	if got := h.changes(); len(got) != 0 {
		t.Fatalf("got %d level changes for a command that changed nothing: %+v", len(got), got)
	}
}

func TestLevelChangesAreClearedAfterEachCommand(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10010, 30)
	if got := h.changes(); len(got) != 1 {
		t.Fatalf("got %d changes, want 1", len(got))
	}
	// Taking them a second time must report nothing: they belong to one command.
	if got := h.changes(); len(got) != 0 {
		t.Fatalf("changes were reported twice: %+v", got)
	}
}

// TestLevelChangesReconstructTheBook is the property the whole incremental feed rests
// on: a client that starts empty and applies every level change in order must end up
// with exactly the book that Snapshot reports.
//
// If this held only for simple cases, the feed would drift from the real book over a
// long session and no single update would look wrong.
func TestLevelChangesReconstructTheBook(t *testing.T) {
	for _, seed := range []int64{1, 19, 7777} {
		t.Run("", func(t *testing.T) {
			h := newHarness(t)
			rng := newRand(seed)

			// The client's view: side -> price -> quantity.
			view := map[Side]map[PriceTicks]int64{
				Buy:  {},
				Sell: {},
			}

			var resting []OrderID
			for i := 0; i < 3000; i++ {
				side := Buy
				if rng.Intn(2) == 0 {
					side = Sell
				}
				qty := int64(1 + rng.Intn(40))

				switch {
				case len(resting) > 0 && rng.Intn(100) < 20:
					pick := rng.Intn(len(resting))
					id := resting[pick]
					resting = append(resting[:pick], resting[pick+1:]...)
					_ = h.cancel(id)
				case rng.Intn(100) < 20:
					h.market("noise", side, qty)
				default:
					price := PriceTicks(10000 + rng.Intn(61) - 30)
					id, res := h.limit("noise", side, price, qty)
					if res.Rested {
						resting = append(resting, id)
					}
				}

				// Apply the incremental update the way a subscriber would.
				for _, c := range h.book.TakeLevelChanges() {
					if c.Quantity == 0 {
						delete(view[c.Side], c.Price)
						continue
					}
					view[c.Side][c.Price] = c.Quantity
				}
			}

			// Compare against the authoritative book, level for level.
			for _, side := range []Side{Buy, Sell} {
				actual := h.book.Depth(side, 0)
				if len(actual) != len(view[side]) {
					t.Fatalf("%s: reconstructed view has %d levels, book has %d",
						side, len(view[side]), len(actual))
				}
				for _, lvl := range actual {
					got, ok := view[side][lvl.Price]
					if !ok {
						t.Fatalf("%s: reconstructed view is missing price %d", side, lvl.Price)
					}
					if got != lvl.Quantity {
						t.Fatalf("%s %d: reconstructed quantity %d, book has %d",
							side, lvl.Price, got, lvl.Quantity)
					}
				}
			}
			if len(view[Buy])+len(view[Sell]) == 0 {
				t.Fatal("the reconstructed book is empty, so the comparison is vacuous")
			}
		})
	}
}

// TestTouchedSetIsNotRetainedAfterALargeSweep guards a performance bug that benchmarking
// found: a single order sweeping hundreds of levels used to leave a hundreds-of-buckets map
// behind, and because clearing a Go map costs its bucket count rather than its key count,
// every subsequent one-level command then paid to walk all of it. A transient burst
// permanently taxed the steady state.
//
// Measured on a 2,000-level book, collecting the changes for a two-level command cost
// 1,119 ns before this and 325 ns after, and is now flat in the size of the book.
func TestTouchedSetIsNotRetainedAfterALargeSweep(t *testing.T) {
	const levels = 500

	book := NewBook("TEST")
	var bld builder
	for i := range levels {
		book.Submit(bld.limit(Sell, PriceTicks(10000+i), 10))
	}
	book.DiscardLevelChanges()

	// One command that touches every level.
	book.Submit(bld.market(Buy, levels*10))
	if got := len(book.TakeLevelChanges()); got != levels {
		t.Fatalf("the sweep reported %d changed levels, want %d", got, levels)
	}

	// The oversized set must have been dropped rather than kept for reuse.
	if book.asks.touched != nil {
		t.Fatalf("the touched set was retained with %d entries of capacity after a sweep",
			len(book.asks.touched))
	}
	if book.asks.touchedPeak != 0 {
		t.Fatalf("the peak was not reset: %d", book.asks.touchedPeak)
	}

	// An ordinary command afterwards still works and keeps its small set.
	book.Submit(bld.limit(Sell, 10000, 10))
	assertChanges(t, book.TakeLevelChanges(), []LevelChange{
		{Side: Sell, Price: 10000, Quantity: 10, Orders: 1},
	})
	if book.asks.touched == nil {
		t.Fatal("a small touched set should be reused between commands, not dropped")
	}
}

func assertChanges(t *testing.T, got, want []LevelChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d level changes, want %d\ngot:  %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("level change %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}
