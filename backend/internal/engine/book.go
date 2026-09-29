package engine

import (
	"errors"
	"sort"
)

// ErrUnknownOrder is returned by Cancel for an order ID that is not resting on
// the book. The book only knows about live resting orders, so an order that was
// never accepted, has fully filled, or has already been canceled is reported the
// same way. Callers get one consistent answer rather than three shades of
// "nothing to do".
var ErrUnknownOrder = errors.New("engine: no resting order with that id")

// locator is the order index entry. It carries everything needed to unlink an
// order without searching: which side it is on, which level holds it, and its
// node within that level's queue.
type locator struct {
	side  Side
	level *PriceLevel
	node  *node
}

// Book is the order book for one symbol.
//
// A Book is not safe for concurrent use. Exactly one goroutine owns it.
type Book struct {
	Symbol string

	bids  *bookSide
	asks  *bookSide
	index map[OrderID]*locator

	nextTradeID  uint64
	lastTrade    PriceTicks
	hasLastTrade bool
}

// NewBook returns an empty book for symbol.
func NewBook(symbol string) *Book {
	return &Book{
		Symbol: symbol,
		bids:   newBookSide(Buy),
		asks:   newBookSide(Sell),
		index:  make(map[OrderID]*locator),
	}
}

func (b *Book) sideFor(s Side) *bookSide {
	if s == Buy {
		return b.bids
	}
	return b.asks
}

// BestBid returns the highest resting bid price.
func (b *Book) BestBid() (PriceTicks, bool) { return b.bids.bestPrice() }

// BestAsk returns the lowest resting ask price.
func (b *Book) BestAsk() (PriceTicks, bool) { return b.asks.bestPrice() }

// Spread returns BestAsk - BestBid, and false unless both sides are populated.
func (b *Book) Spread() (PriceTicks, bool) {
	bid, okBid := b.BestBid()
	ask, okAsk := b.BestAsk()
	if !okBid || !okAsk {
		return 0, false
	}
	return ask - bid, true
}

// MidPrice returns the midpoint of the best bid and ask in ticks, rounded down.
// It is a derived quote, not a traded price, and never becomes the last price.
func (b *Book) MidPrice() (PriceTicks, bool) {
	bid, okBid := b.BestBid()
	ask, okAsk := b.BestAsk()
	if !okBid || !okAsk {
		return 0, false
	}
	return (bid + ask) / 2, true
}

// LastTradePrice returns the price of the most recent execution. It changes only
// when a trade prints, which is what makes the displayed price a product of
// activity in this market rather than an externally supplied number.
func (b *Book) LastTradePrice() (PriceTicks, bool) { return b.lastTrade, b.hasLastTrade }

// RestingCount returns the number of live resting orders across both sides.
func (b *Book) RestingCount() int { return len(b.index) }

// Order returns an immutable view of a resting order.
func (b *Book) Order(id OrderID) (View, bool) {
	loc, ok := b.index[id]
	if !ok {
		return View{}, false
	}
	return loc.node.order.View(), true
}

// OpenOrders returns views of every resting order for participant, ordered by the
// sequence that granted each order its priority. Passing an empty participant
// returns every resting order.
func (b *Book) OpenOrders(participant string) []View {
	out := make([]View, 0, len(b.index))
	for _, loc := range b.index {
		o := loc.node.order
		if participant == "" || o.ParticipantID == participant {
			out = append(out, o.View())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PrioritySequence < out[j].PrioritySequence })
	return out
}

// Cancel removes a resting order from the book and marks its unfilled quantity
// canceled. Already-executed quantity is untouched.
func (b *Book) Cancel(id OrderID) (View, error) {
	loc, ok := b.index[id]
	if !ok {
		return View{}, ErrUnknownOrder
	}
	o := loc.node.order
	b.sideFor(loc.side).remove(loc.level, loc.node)
	delete(b.index, id)
	o.cancelRemaining()
	return o.View(), nil
}
