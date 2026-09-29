package engine

import (
	"errors"
	"math/rand"
	"testing"
)

// TestRandomFlowHoldsEveryInvariant pushes a long pseudo-random command stream
// through the book and checks every invariant after every command.
//
// Hand-written examples only cover the cases we thought of. This covers the
// interleavings we did not: partially filled orders being canceled, levels being
// drained and recreated, sweeps that stop mid-level, and the heap compaction path.
// The seeds are fixed so a failure is reproducible.
func TestRandomFlowHoldsEveryInvariant(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 1337, 20240115} {
		t.Run("", func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			h := newHarness(t)

			var resting []OrderID
			var trades, cancels, rejectedCancels int

			const commands = 4000
			for i := 0; i < commands; i++ {
				side := Buy
				if rng.Intn(2) == 0 {
					side = Sell
				}
				qty := int64(1 + rng.Intn(50))

				switch {
				// Cancel a previously rested order.
				case len(resting) > 0 && rng.Intn(100) < 20:
					pick := rng.Intn(len(resting))
					id := resting[pick]
					resting = append(resting[:pick], resting[pick+1:]...)
					if err := h.cancel(id); err != nil {
						if !errors.Is(err, ErrUnknownOrder) {
							t.Fatalf("cancel returned an unexpected error: %v", err)
						}
						rejectedCancels++
					} else {
						cancels++
					}

				// Market order.
				case rng.Intn(100) < 15:
					_, res := h.market("noise", side, qty)
					trades += len(res.Trades)

				// Limit order somewhere around the reference price.
				default:
					price := PriceTicks(10000 + rng.Intn(41) - 20)
					id, res := h.limit("noise", side, price, qty)
					trades += len(res.Trades)
					if res.Rested {
						resting = append(resting, id)
					}
				}
			}

			// The stream has to actually exercise matching, otherwise the
			// invariants passed vacuously.
			if trades == 0 {
				t.Fatalf("seed %d produced no trades", seed)
			}
			if cancels == 0 {
				t.Fatalf("seed %d canceled nothing", seed)
			}

			// Every share that traded was filled once on each side, so the total
			// filled quantity across all orders must be exactly twice the total
			// traded quantity.
			var traded, filled, canceled, remaining int64
			for _, tr := range h.trades {
				traded += tr.Quantity
			}
			for _, o := range h.all {
				filled += o.FilledQty
				canceled += o.CanceledQty
				remaining += o.RemainingQty
			}
			if filled != 2*traded {
				t.Fatalf("orders report %d filled shares against %d traded shares (want %d)",
					filled, traded, 2*traded)
			}

			// And nothing may leak: every submitted share is filled, resting or canceled.
			var submitted int64
			for _, o := range h.all {
				submitted += o.OriginalQty
			}
			if submitted != filled+canceled+remaining {
				t.Fatalf("submitted %d shares but account for %d filled + %d canceled + %d resting",
					submitted, filled, canceled, remaining)
			}

			t.Logf("seed %d: %d commands, %d trades, %d cancels (%d rejected), %d orders resting",
				seed, commands, trades, cancels, rejectedCancels, h.book.RestingCount())
		})
	}
}

// TestDrainedLevelsAreReclaimed checks that repeatedly creating and emptying
// levels does not grow the book's internal price bookkeeping without bound.
//
// Empty levels are kept until they surface at the heap root or a compaction runs,
// which is what lets the heap and the level map stay in step without a separate
// membership set. This test pins down that the reclamation actually happens.
func TestDrainedLevelsAreReclaimed(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < 5000; i++ {
		price := PriceTicks(10000 + i%500)
		sellID, _ := h.limit("maker", Sell, price, 10)
		h.market("taker", Buy, 10)
		h.requireGone(sellID)
	}

	if got := h.book.RestingCount(); got != 0 {
		t.Fatalf("%d orders rest on the book after every order was filled", got)
	}
	if got := len(h.book.asks.levels); got > compactThreshold {
		t.Fatalf("ask side retains %d levels after draining all of them, want at most %d",
			got, compactThreshold)
	}
	if got := h.book.asks.prices.Len(); got != len(h.book.asks.levels) {
		t.Fatalf("ask heap holds %d prices but the level map holds %d", got, len(h.book.asks.levels))
	}
}
