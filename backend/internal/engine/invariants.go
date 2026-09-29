package engine

import (
	"errors"
	"fmt"
)

// The book's invariants come in two classes, and the distinction is worth keeping
// straight.
//
// Semantic invariants are properties any correct continuous limit order book must
// hold, whatever it is built out of. "The resting book is never crossed" and "a
// trade never fills more than either order had left" are statements about what an
// exchange means, and they would still be required if the whole data structure were
// replaced with a balanced tree or a flat sorted slice.
//
// Implementation invariants are properties of this particular design: that the price
// heap and the level map describe the same set of prices, that the order index
// locator points at the node it claims to, that the cached counters match reality.
// They are how *this* code keeps its semantic promises, and a different design would
// have entirely different ones.
//
// Both are worth testing. Only the first class is worth describing as an exchange
// invariant.

// CheckInvariants verifies both classes and returns every violation it finds, joined,
// so a failure reports the whole picture rather than the first symptom.
func (b *Book) CheckInvariants() error {
	return errors.Join(b.CheckSemanticInvariants(), b.CheckImplementationInvariants())
}

// CheckSemanticInvariants verifies the properties a correct order book must hold
// regardless of how it is built.
//
// These matter more than any individual matching example. Most of the bugs worth
// fearing here surface as a violation of one of these long before they surface as an
// obviously wrong trade.
func (b *Book) CheckSemanticInvariants() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	// The resting book is never crossed. A best bid at or above the best ask means
	// the engine failed to execute liquidity that was available to it.
	bid, hasBid := b.BestBid()
	ask, hasAsk := b.BestAsk()
	if hasBid && hasAsk && bid >= ask {
		add("crossed book: best bid %d >= best ask %d", bid, ask)
	}

	// Order IDs are unique among live orders, so an ID names at most one thing.
	seenIDs := make(map[OrderID]PriceTicks)

	for _, side := range []*bookSide{b.bids, b.asks} {
		name := side.side.String()

		for price, lvl := range side.levels {
			var sum int64
			var prevSeq uint64
			for n := lvl.head; n != nil; n = n.next {
				o := n.order
				sum += o.RemainingQty

				// A resting order always has something left to trade.
				if o.RemainingQty <= 0 {
					add("%s %d: order %d rests with remaining qty %d", name, price, o.ID, o.RemainingQty)
				}

				// Quantity is conserved: nothing is created or lost.
				if !o.Conserved() {
					add("order %d does not conserve quantity: original %d != filled %d + remaining %d + canceled %d",
						o.ID, o.OriginalQty, o.FilledQty, o.RemainingQty, o.CanceledQty)
				}

				// An order rests on the side and at the price it asked for.
				if o.Side != side.side {
					add("%s %d: order %d has side %s", name, price, o.ID, o.Side)
				}
				if o.Type == Limit && o.LimitPrice != price {
					add("%s %d: order %d has limit price %d", name, price, o.ID, o.LimitPrice)
				}

				// A market order has no price, so it can never rest.
				if o.Type == Market {
					add("%s %d: market order %d is resting", name, price, o.ID)
				}

				// Time priority within a price level: strictly increasing sequence,
				// oldest at the front. This is the half of price-time priority that
				// a queue bug would silently break.
				if prevSeq != 0 && prevSeq >= o.PrioritySequence {
					add("%s %d: order %d (seq %d) is queued behind sequence %d",
						name, price, o.ID, o.PrioritySequence, prevSeq)
				}
				prevSeq = o.PrioritySequence

				if at, dup := seenIDs[o.ID]; dup {
					add("order %d rests at both %d and %d", o.ID, at, price)
				}
				seenIDs[o.ID] = price
			}

			// Published depth is truthful: a level's advertised quantity is exactly
			// what is resting there.
			if sum != lvl.TotalQty {
				add("%s %d: level advertises %d but holds %d", name, price, lvl.TotalQty, sum)
			}
		}
	}

	// Price priority across levels: every bid is below every ask. The crossed-book
	// check above covers the touch; this covers the whole book.
	if hasBid && hasAsk {
		for price, lvl := range b.bids.levels {
			if !lvl.Empty() && price >= ask {
				add("bid level %d is at or above the best ask %d", price, ask)
			}
		}
		for price, lvl := range b.asks.levels {
			if !lvl.Empty() && price <= bid {
				add("ask level %d is at or below the best bid %d", price, bid)
			}
		}
	}

	// Every live order is addressable by ID, and nothing else is.
	for id := range seenIDs {
		if _, ok := b.index[id]; !ok {
			add("order %d rests on the book but cannot be looked up by id", id)
		}
	}
	if len(b.index) != len(seenIDs) {
		add("%d orders are addressable by id but %d rest on the book", len(b.index), len(seenIDs))
	}

	return errors.Join(problems...)
}

// CheckImplementationInvariants verifies the bookkeeping this particular design
// relies on: the heap and level map agreeing, the order index locators pointing where
// they claim, the linked lists being intact and the cached counters being accurate.
//
// A violation here is not by itself a wrong answer to a client, but it is how wrong
// answers start.
func (b *Book) CheckImplementationInvariants() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	indexed := 0
	for _, side := range []*bookSide{b.bids, b.asks} {
		name := side.side.String()

		// The price heap and the level map must describe exactly the same set of
		// prices, with no duplicates. This is what lets drained levels be discarded
		// lazily without a separate membership set.
		if side.prices.Len() != len(side.levels) {
			add("%s: heap holds %d prices but the map holds %d levels", name, side.prices.Len(), len(side.levels))
		}
		seen := make(map[PriceTicks]bool, side.prices.Len())
		for _, price := range side.prices.prices {
			if seen[price] {
				add("%s: price %d appears twice in the heap", name, price)
			}
			seen[price] = true
			if _, ok := side.levels[price]; !ok {
				add("%s: heap holds price %d with no level", name, price)
			}
		}
		for price := range side.levels {
			if !seen[price] {
				add("%s: level at price %d is missing from the heap", name, price)
			}
		}

		nonEmpty, resting := 0, 0
		for price, lvl := range side.levels {
			if lvl.Price != price {
				add("%s: level keyed at %d reports price %d", name, price, lvl.Price)
			}
			if !lvl.Empty() {
				nonEmpty++
			}

			count := 0
			for n := lvl.head; n != nil; n = n.next {
				o := n.order
				count++

				// The order index must point at this exact node in this exact level,
				// which is what makes cancellation an unlink rather than a search.
				loc, ok := b.index[o.ID]
				switch {
				case !ok:
					add("order %d rests at %s %d but has no index entry", o.ID, name, price)
				case loc.node != n:
					add("order %d is indexed to a different node", o.ID)
				case loc.level != lvl:
					add("order %d is indexed to a different level", o.ID)
				case loc.side != side.side:
					add("order %d is indexed on side %s but rests on %s", o.ID, loc.side, name)
				}

				// The FIFO queue's links agree in both directions.
				if n.next != nil && n.next.prev != n {
					add("%s %d: broken forward link after order %d", name, price, o.ID)
				}
				if n.prev != nil && n.prev.next != n {
					add("%s %d: broken backward link before order %d", name, price, o.ID)
				}
				if n.prev == nil && lvl.head != n {
					add("%s %d: order %d has no predecessor but is not the head", name, price, o.ID)
				}
				if n.next == nil && lvl.tail != n {
					add("%s %d: order %d has no successor but is not the tail", name, price, o.ID)
				}
			}

			if count != lvl.count {
				add("%s %d: level counts %d orders but its queue holds %d", name, price, lvl.count, count)
			}
			resting += count
			indexed += count
		}

		// The cached counters that drive compaction must match reality, or levels
		// will be reclaimed at the wrong time.
		if nonEmpty != side.nonEmpty {
			add("%s: tracks %d non-empty levels but has %d", name, side.nonEmpty, nonEmpty)
		}
		if resting != side.resting {
			add("%s: tracks %d resting orders but has %d", name, side.resting, resting)
		}
	}

	if indexed != len(b.index) {
		add("index holds %d entries but %d orders rest on the book", len(b.index), indexed)
	}

	return errors.Join(problems...)
}

// CheckTrade verifies a trade against the remaining quantities its two orders had
// before it executed. This is a semantic check: an execution may never create
// quantity that was not there to trade.
func CheckTrade(t Trade, aggressorPre, restingPre int64) error {
	var problems []error
	if t.Quantity <= 0 {
		problems = append(problems, fmt.Errorf("trade %d has quantity %d", t.ID, t.Quantity))
	}
	if t.Quantity > aggressorPre {
		problems = append(problems, fmt.Errorf("trade %d fills %d against an aggressor holding %d", t.ID, t.Quantity, aggressorPre))
	}
	if t.Quantity > restingPre {
		problems = append(problems, fmt.Errorf("trade %d fills %d against a resting order holding %d", t.ID, t.Quantity, restingPre))
	}
	if t.Price <= 0 {
		problems = append(problems, fmt.Errorf("trade %d has price %d", t.ID, t.Price))
	}
	return errors.Join(problems...)
}
