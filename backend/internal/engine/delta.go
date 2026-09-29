package engine

import "sort"

// LevelChange is the state of one price level after a command touched it.
//
// Quantity is absolute, not a difference: it is what rests at that price now. A
// Quantity of zero means the level is gone and a client should drop it. Absolute
// values make the update idempotent, so applying the same change twice is harmless and
// a client never has to reason about whether it has already accounted for a fill.
type LevelChange struct {
	Side     Side       `json:"side"`
	Price    PriceTicks `json:"price"`
	Quantity int64      `json:"quantity"`
	Orders   int        `json:"orders"`
}

// touchedKeepCapacity is the largest touched-set that is worth reusing between commands.
//
// An ordinary command touches one or two levels, so the set is reused. A command that swept
// hundreds of levels leaves behind a map with hundreds of buckets, and that is not worth
// keeping: see resetTouched.
const touchedKeepCapacity = 64

// touch records that a price level on this side changed during the current command.
func (s *bookSide) touch(price PriceTicks) {
	if s.touched == nil {
		s.touched = make(map[PriceTicks]struct{}, 8)
		s.touchedPeak = 0
	}
	s.touched[price] = struct{}{}
	if n := len(s.touched); n > s.touchedPeak {
		s.touchedPeak = n
	}
}

// resetTouched empties the record of changed levels, ready for the next command.
//
// Clearing a Go map is proportional to its bucket count, not to the number of keys left in
// it, and a map never gives capacity back. So a single order that swept a thousand levels
// would leave a thousand-bucket map behind, and every subsequent one-level command would pay
// to walk all of it — a transient burst permanently taxing the steady state. Past a
// threshold the map is dropped instead of cleared, and the next touch allocates a small one.
func (s *bookSide) resetTouched() {
	if s.touchedPeak > touchedKeepCapacity {
		s.touched = nil
		s.touchedPeak = 0
		return
	}
	clear(s.touched)
}

// TakeLevelChanges returns the net change to every price level the last command
// touched, and clears the record.
//
// This is the incremental alternative to Snapshot for live market data. Snapshot has to
// collect and sort every level on the book to publish the top ten, which costs
// O(L log L) in the number of distinct prices and is measurably more expensive than the
// matching underneath it once a book is deep. This costs O(k log k) in the number of
// levels a single command actually changed, which is one or two for an ordinary order
// and one per level consumed for a sweep.
//
// Levels are reported once each with their final state, however many times the command
// touched them, and sorted, so Go's map iteration order never reaches a client.
func (b *Book) TakeLevelChanges() []LevelChange {
	n := len(b.bids.touched) + len(b.asks.touched)
	if n == 0 {
		return nil
	}

	out := make([]LevelChange, 0, n)
	for _, side := range []*bookSide{b.bids, b.asks} {
		for price := range side.touched {
			change := LevelChange{Side: side.side, Price: price}
			// A level absent from the map, or present but drained, is reported as
			// zero quantity. Both mean the same thing to a subscriber: the price no
			// longer has liquidity.
			if lvl, ok := side.levels[price]; ok {
				change.Quantity = lvl.TotalQty
				change.Orders = lvl.Len()
			}
			out = append(out, change)
		}
		side.resetTouched()
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Side != out[j].Side {
			return out[i].Side < out[j].Side
		}
		return out[i].Price < out[j].Price
	})
	return out
}

// DiscardLevelChanges clears the record without building anything, for callers that do
// not publish market data.
func (b *Book) DiscardLevelChanges() {
	b.bids.resetTouched()
	b.asks.resetTouched()
}
