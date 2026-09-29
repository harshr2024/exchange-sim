package engine

import (
	"errors"
	"fmt"
)

// CheckInvariants verifies every structural property the book is supposed to hold
// after a command finishes. It returns all violations it finds, joined, so a
// failing test reports the whole picture rather than the first symptom.
//
// These properties matter more than any individual matching example: most of the
// bugs worth fearing here (a stale index entry, a level total that drifted from
// its queue, liquidity left unmatched) show up as an invariant violation long
// before they show up as an obviously wrong trade.
func (b *Book) CheckInvariants() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	// No crossed resting book. If the best bid is at or above the best ask, the
	// engine failed to execute liquidity that was available to it.
	bid, hasBid := b.BestBid()
	ask, hasAsk := b.BestAsk()
	if hasBid && hasAsk && bid >= ask {
		add("crossed book: best bid %d >= best ask %d", bid, ask)
	}

	indexed := 0
	for _, side := range []*bookSide{b.bids, b.asks} {
		name := side.side.String()

		// The price heap and the level map must describe the same set of prices.
		if side.prices.Len() != len(side.levels) {
			add("%s: heap has %d prices but map has %d levels", name, side.prices.Len(), len(side.levels))
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

			var sum int64
			count := 0
			for n := lvl.head; n != nil; n = n.next {
				o := n.order
				count++
				sum += o.RemainingQty

				if o.RemainingQty <= 0 {
					add("%s %d: order %d rests with remaining qty %d", name, price, o.ID, o.RemainingQty)
				}
				if !o.Conserved() {
					add("order %d does not conserve quantity: original %d != filled %d + remaining %d + canceled %d",
						o.ID, o.OriginalQty, o.FilledQty, o.RemainingQty, o.CanceledQty)
				}
				if o.Side != side.side {
					add("%s %d: order %d has side %s", name, price, o.ID, o.Side)
				}
				if o.Type == Limit && o.LimitPrice != price {
					add("%s %d: order %d has limit price %d", name, price, o.ID, o.LimitPrice)
				}
				if o.Type == Market {
					add("%s %d: market order %d is resting", name, price, o.ID)
				}

				// Every resting order needs exactly one index entry, and that
				// entry must point back at this node in this level.
				loc, ok := b.index[o.ID]
				switch {
				case !ok:
					add("order %d rests at %s %d but is not indexed", o.ID, name, price)
				case loc.node != n:
					add("order %d is indexed to a different node", o.ID)
				case loc.level != lvl:
					add("order %d is indexed to a different level", o.ID)
				case loc.side != side.side:
					add("order %d is indexed on side %s but rests on %s", o.ID, loc.side, name)
				}

				// FIFO links must be consistent in both directions.
				if n.next != nil && n.next.prev != n {
					add("%s %d: broken forward link after order %d", name, price, o.ID)
				}
				if n.prev == nil && lvl.head != n {
					add("%s %d: order %d has no predecessor but is not the head", name, price, o.ID)
				}
				if n.next == nil && lvl.tail != n {
					add("%s %d: order %d has no successor but is not the tail", name, price, o.ID)
				}

				// Priority within a level must be strictly increasing by sequence.
				if n.prev != nil && n.prev.order.PrioritySequence >= o.PrioritySequence {
					add("%s %d: order %d (seq %d) queued behind order %d (seq %d)",
						name, price, o.ID, o.PrioritySequence, n.prev.order.ID, n.prev.order.PrioritySequence)
				}
			}

			if count != lvl.count {
				add("%s %d: level counts %d orders but its queue holds %d", name, price, lvl.count, count)
			}
			if sum != lvl.TotalQty {
				add("%s %d: level total %d != sum of remaining quantities %d", name, price, lvl.TotalQty, sum)
			}
			resting += count
			indexed += count
		}

		if nonEmpty != side.nonEmpty {
			add("%s: tracks %d non-empty levels but has %d", name, side.nonEmpty, nonEmpty)
		}
		if resting != side.resting {
			add("%s: tracks %d resting orders but has %d", name, side.resting, resting)
		}
	}

	// No index entry may point at an order that is not resting.
	if indexed != len(b.index) {
		add("index holds %d entries but %d orders rest on the book", len(b.index), indexed)
	}

	return errors.Join(problems...)
}

// CheckTrade verifies a trade against the remaining quantities its two orders had
// before it executed.
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
