package engine

// Result reports what happened to one submitted order.
type Result struct {
	// Order is an immutable view of the order after matching finished.
	Order View
	// Trades are the executions the order caused, in the order they happened.
	Trades []Trade
	// Rested is true when an unfilled remainder was added to the book.
	Rested bool
	// CanceledQty is the quantity given up rather than rested. This is non-zero
	// only for a market order that exhausted the opposite side.
	CanceledQty int64
	// Filled is the post-execution state of each resting order this one traded against,
	// in the order they were hit.
	//
	// An execution changes two orders, and the passive side's owner needs to be told
	// about it as much as the aggressor's does. Without this, the only evidence that a
	// resting order had been filled would be a trade carrying its ID, leaving the owner
	// to reconstruct quantities from the tape.
	//
	// Each resting order appears at most once: a fill either exhausts it, in which case
	// it leaves the book, or exhausts the incoming order, in which case matching stops.
	Filled []View
}

// Submit runs one order through the book.
//
// The order is matched against the opposite side in price-time priority: best
// price first, and oldest priority sequence first within a price. Each execution
// prints at the resting order's price. Whatever remains afterwards rests on the
// book if it is a limit order and is canceled if it is a market order.
//
// Submit takes ownership of o and mutates it. The caller should read the returned
// View rather than keeping the pointer.
func (b *Book) Submit(o *Order) Result {
	trades, filled := b.match(o)

	res := Result{Trades: trades, Filled: filled}
	if o.RemainingQty > 0 {
		switch o.Type {
		case Market:
			// A market order has no price, so a remainder cannot rest without
			// inventing one. It is canceled instead.
			res.CanceledQty = o.RemainingQty
			o.cancelRemaining()
		default:
			own := b.sideFor(o.Side)
			lvl, n := own.push(o)
			b.index[o.ID] = &locator{side: o.Side, level: lvl, node: n}
			res.Rested = true
		}
	}
	res.Order = o.View()
	return res
}

// match consumes resting liquidity on the opposite side until the order is
// filled, the book is exhausted, or the next price is outside the order's limit.
//
// It returns the executions and the post-execution state of every resting order it
// touched.
func (b *Book) match(o *Order) ([]Trade, []View) {
	opp := b.sideFor(o.Side.Opposite())

	var trades []Trade
	var filled []View
	for o.RemainingQty > 0 {
		lvl := opp.bestLevel()
		if lvl == nil {
			break
		}
		if o.Type == Limit && !acceptable(o.Side, o.LimitPrice, lvl.Price) {
			break
		}
		for o.RemainingQty > 0 && !lvl.Empty() {
			n := lvl.head
			resting := n.order

			qty := min(o.RemainingQty, resting.RemainingQty)
			trades = append(trades, b.recordTrade(o, resting, lvl.Price, qty))

			o.fill(qty)
			resting.fill(qty)
			lvl.reduce(qty)
			// A partial fill changes the level's advertised quantity without removing
			// anything, so it has to be recorded for market data explicitly.
			opp.touch(lvl.Price)

			if resting.RemainingQty == 0 {
				opp.remove(lvl, n)
				delete(b.index, resting.ID)
			}

			// Captured after the fill, so it is the resting order's new state rather
			// than the one it had a moment ago.
			filled = append(filled, resting.View())
		}
	}
	return trades, filled
}

// recordTrade mints a trade for one fill and advances the last traded price.
func (b *Book) recordTrade(aggressor, resting *Order, price PriceTicks, qty int64) Trade {
	b.nextTradeID++
	t := Trade{
		ID:            TradeID(b.nextTradeID),
		Symbol:        b.Symbol,
		Price:         price,
		Quantity:      qty,
		AggressorSide: aggressor.Side,
		Sequence:      aggressor.PrioritySequence,
	}
	if aggressor.Side == Buy {
		t.BuyOrderID, t.SellOrderID = aggressor.ID, resting.ID
	} else {
		t.BuyOrderID, t.SellOrderID = resting.ID, aggressor.ID
	}

	b.lastTrade = price
	b.hasLastTrade = true
	return t
}

// acceptable reports whether an order with the given limit price is willing to
// trade at restingPrice. A buyer will not pay more than its limit and a seller
// will not accept less, which is what stops a sweep at the limit boundary.
func acceptable(side Side, limit, restingPrice PriceTicks) bool {
	if side == Buy {
		return restingPrice <= limit
	}
	return restingPrice >= limit
}
