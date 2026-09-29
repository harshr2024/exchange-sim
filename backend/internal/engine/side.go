package engine

import (
	"container/heap"
	"sort"
)

// priceHeap orders the prices that have a level on one side of the book.
// Bids use a max-heap and asks use a min-heap, so the root is always the side's
// best price.
type priceHeap struct {
	prices []PriceTicks
	desc   bool
}

func (h *priceHeap) Len() int { return len(h.prices) }

func (h *priceHeap) Less(i, j int) bool {
	if h.desc {
		return h.prices[i] > h.prices[j]
	}
	return h.prices[i] < h.prices[j]
}

func (h *priceHeap) Swap(i, j int) { h.prices[i], h.prices[j] = h.prices[j], h.prices[i] }

func (h *priceHeap) Push(x any) { h.prices = append(h.prices, x.(PriceTicks)) }

func (h *priceHeap) Pop() any {
	last := len(h.prices) - 1
	v := h.prices[last]
	h.prices = h.prices[:last]
	return v
}

// compactThreshold is the smallest level count at which we bother rebuilding the
// heap to shed drained levels.
const compactThreshold = 32

// bookSide is one half of the book: a price-keyed map of levels plus a heap of
// those prices.
//
// Matching only ever needs the best price, which a heap gives in O(1) to read
// and O(log n) to maintain. Cancellation, FIFO consumption and level aggregation
// are handled by the map and the levels themselves. A balanced ordered tree would
// also work and would additionally give cheap ordered traversal, but Go has none
// in its standard library and matching never needs the full ordering.
//
// Invariant: the set of prices in the heap is exactly the set of keys in levels,
// with no duplicates. Drained levels are kept in both until they surface at the
// heap root (or until a compaction), which is what keeps that invariant true
// without an extra membership set.
type bookSide struct {
	side     Side
	levels   map[PriceTicks]*PriceLevel
	prices   priceHeap
	nonEmpty int
	resting  int
	// touched records the prices changed by the command in progress, so incremental
	// market data can be built without walking the whole side. See Book.TakeLevelChanges.
	touched map[PriceTicks]struct{}
	// touchedPeak is the largest touched has grown since it was allocated, which decides
	// whether it is worth reusing. See resetTouched.
	touchedPeak int
}

func newBookSide(side Side) *bookSide {
	return &bookSide{
		side:   side,
		levels: make(map[PriceTicks]*PriceLevel),
		prices: priceHeap{desc: side == Buy},
	}
}

// push appends an order to the back of its price level's FIFO queue, creating
// the level if needed.
func (s *bookSide) push(o *Order) (*PriceLevel, *node) {
	lvl, ok := s.levels[o.LimitPrice]
	if !ok {
		lvl = newPriceLevel(o.LimitPrice)
		s.levels[o.LimitPrice] = lvl
		heap.Push(&s.prices, o.LimitPrice)
	}
	if lvl.Empty() {
		s.nonEmpty++
	}
	n := lvl.pushBack(o)
	s.resting++
	s.touch(o.LimitPrice)
	return lvl, n
}

// remove unlinks a resting order from its level.
func (s *bookSide) remove(lvl *PriceLevel, n *node) {
	lvl.remove(n)
	s.resting--
	s.touch(lvl.Price)
	if lvl.Empty() {
		s.nonEmpty--
		s.maybeCompact()
	}
}

// bestLevel returns the level at the side's best price, discarding levels that
// have drained. Returning nil means the side is empty.
func (s *bookSide) bestLevel() *PriceLevel {
	for s.prices.Len() > 0 {
		price := s.prices.prices[0]
		if lvl, ok := s.levels[price]; ok && !lvl.Empty() {
			return lvl
		}
		heap.Pop(&s.prices)
		delete(s.levels, price)
	}
	return nil
}

func (s *bookSide) bestPrice() (PriceTicks, bool) {
	if lvl := s.bestLevel(); lvl != nil {
		return lvl.Price, true
	}
	return 0, false
}

// maybeCompact rebuilds the heap once drained levels outnumber live ones, which
// bounds the memory held by prices that were touched and then emptied.
//
// Rebuilding may permute the heap's backing array, but the root of a heap is the
// unique minimum (or maximum) of its contents, so this cannot change which level
// matching selects. Every ordered traversal in this package sorts explicitly
// rather than reading the heap's internal order.
func (s *bookSide) maybeCompact() {
	if len(s.levels) < compactThreshold || s.nonEmpty*2 > len(s.levels) {
		return
	}
	kept := make([]PriceTicks, 0, s.nonEmpty)
	for price, lvl := range s.levels {
		if lvl.Empty() {
			delete(s.levels, price)
			continue
		}
		kept = append(kept, price)
	}
	s.prices.prices = kept
	heap.Init(&s.prices)
}

// sortedLevels returns the side's non-empty levels in priority order: descending
// price for bids, ascending for asks. A limit of 0 or less returns every level.
func (s *bookSide) sortedLevels(limit int) []*PriceLevel {
	out := make([]*PriceLevel, 0, s.nonEmpty)
	for _, lvl := range s.levels {
		if !lvl.Empty() {
			out = append(out, lvl)
		}
	}
	desc := s.prices.desc
	sort.Slice(out, func(i, j int) bool {
		if desc {
			return out[i].Price > out[j].Price
		}
		return out[i].Price < out[j].Price
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
