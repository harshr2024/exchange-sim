package engine

import (
	"errors"
	"testing"
)

// Prices in these tests are ticks at one cent, so 10000 reads as $100.00.

func TestEmptyBook_MarketOrderNeitherTradesNorRests(t *testing.T) {
	h := newHarness(t)

	id, res := h.market("taker", Buy, 100)

	if len(res.Trades) != 0 {
		t.Fatalf("got %d trades against an empty book, want 0", len(res.Trades))
	}
	if res.Rested {
		t.Fatal("market order rested on the book")
	}
	if res.CanceledQty != 100 {
		t.Fatalf("canceled %d, want the full 100", res.CanceledQty)
	}
	h.requireGone(id)
	h.requireDepth(Buy, nil)
	h.requireDepth(Sell, nil)

	if _, ok := h.book.LastTradePrice(); ok {
		t.Fatal("last traded price is set although nothing traded")
	}
}

func TestEmptyBook_LimitOrderRests(t *testing.T) {
	h := newHarness(t)

	id, res := h.limit("maker", Buy, 10000, 50)

	if len(res.Trades) != 0 {
		t.Fatalf("got %d trades against an empty book, want 0", len(res.Trades))
	}
	if !res.Rested {
		t.Fatal("limit order did not rest")
	}
	h.requireResting(id, 50)
	h.requireDepth(Buy, []LevelView{level(10000, 50, 1)})

	bid, ok := h.book.BestBid()
	if !ok || bid != 10000 {
		t.Fatalf("best bid is (%d, %v), want (10000, true)", bid, ok)
	}
	if _, ok := h.book.BestAsk(); ok {
		t.Fatal("best ask exists although only a bid was submitted")
	}
}

func TestExactFill(t *testing.T) {
	h := newHarness(t)

	sellID, _ := h.limit("maker", Sell, 10000, 10)
	buyID, res := h.limit("taker", Buy, 10000, 10)

	h.requireTrades([2]int64{10000, 10})
	if res.Rested {
		t.Fatal("fully filled order rested on the book")
	}
	h.requireGone(sellID)
	h.requireGone(buyID)
	h.requireDepth(Buy, nil)
	h.requireDepth(Sell, nil)

	if last, ok := h.book.LastTradePrice(); !ok || last != 10000 {
		t.Fatalf("last traded price is (%d, %v), want (10000, true)", last, ok)
	}
}

func TestPartialPassiveFill(t *testing.T) {
	h := newHarness(t)

	sellID, _ := h.limit("maker", Sell, 10000, 100)
	h.limit("taker", Buy, 10000, 40)

	h.requireTrades([2]int64{10000, 40})
	h.requireResting(sellID, 60)
	h.requireDepth(Sell, []LevelView{level(10000, 60, 1)})
	h.requireDepth(Buy, nil)

	sell := h.order(sellID)
	if sell.FilledQty != 40 {
		t.Fatalf("resting sell filled %d, want 40", sell.FilledQty)
	}
}

func TestPartialAggressiveFill(t *testing.T) {
	h := newHarness(t)

	sellID, _ := h.limit("maker", Sell, 10000, 40)
	buyID, res := h.limit("taker", Buy, 10000, 100)

	h.requireTrades([2]int64{10000, 40})
	if !res.Rested {
		t.Fatal("unfilled remainder of the limit order did not rest")
	}
	h.requireGone(sellID)
	h.requireResting(buyID, 60)
	h.requireDepth(Buy, []LevelView{level(10000, 60, 1)})
	h.requireDepth(Sell, nil)
}

func TestMultipleLevelSweep(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10000, 10)
	h.limit("maker", Sell, 10100, 20)
	h.limit("maker", Sell, 10200, 30)

	// Buying 25 at 101.00 clears 100.00 entirely then takes 15 of 101.00.
	_, res := h.limit("taker", Buy, 10100, 25)

	h.requireTrades([2]int64{10000, 10}, [2]int64{10100, 15})
	if res.Rested {
		t.Fatal("fully filled sweep rested on the book")
	}
	h.requireDepth(Sell, []LevelView{level(10100, 5, 1), level(10200, 30, 1)})
	h.requireDepth(Buy, nil)
}

func TestFIFOWithinPriceLevel(t *testing.T) {
	h := newHarness(t)

	first, _ := h.limit("A", Sell, 10000, 10)
	second, _ := h.limit("B", Sell, 10000, 10)

	h.limit("taker", Buy, 10000, 15)

	h.requireTrades([2]int64{10000, 10}, [2]int64{10000, 5})
	if h.trades[0].SellOrderID != first {
		t.Fatalf("first fill hit order %d, want the older order %d", h.trades[0].SellOrderID, first)
	}
	if h.trades[1].SellOrderID != second {
		t.Fatalf("second fill hit order %d, want the newer order %d", h.trades[1].SellOrderID, second)
	}
	h.requireGone(first)
	h.requireResting(second, 5)
}

func TestPricePriorityBeatsTimePriority(t *testing.T) {
	h := newHarness(t)

	// The older ask is at a worse (higher) price than the newer one.
	older, _ := h.limit("A", Sell, 10100, 10)
	newer, _ := h.limit("B", Sell, 10000, 10)

	h.limit("taker", Buy, 10100, 10)

	h.requireTrades([2]int64{10000, 10})
	if h.trades[0].SellOrderID != newer {
		t.Fatalf("fill hit order %d, want the better-priced order %d", h.trades[0].SellOrderID, newer)
	}
	h.requireResting(older, 10)
}

func TestCancelMiddleOfQueuePreservesRemainingOrder(t *testing.T) {
	h := newHarness(t)

	a, _ := h.limit("A", Sell, 10000, 10)
	b, _ := h.limit("B", Sell, 10000, 10)
	c, _ := h.limit("C", Sell, 10000, 10)

	if err := h.cancel(b); err != nil {
		t.Fatalf("canceling the middle order: %v", err)
	}
	h.requireGone(b)
	h.requireDepth(Sell, []LevelView{level(10000, 20, 2)})

	canceled := h.order(b)
	if canceled.CanceledQty != 10 || canceled.RemainingQty != 0 {
		t.Fatalf("canceled order has canceled=%d remaining=%d, want 10 and 0",
			canceled.CanceledQty, canceled.RemainingQty)
	}

	// A must still fill before C.
	h.limit("taker", Buy, 10000, 20)
	if h.trades[0].SellOrderID != a || h.trades[1].SellOrderID != c {
		t.Fatalf("fills hit orders %d then %d, want %d then %d",
			h.trades[0].SellOrderID, h.trades[1].SellOrderID, a, c)
	}
}

func TestCancelEmptiesLevelAndClearsBestPrice(t *testing.T) {
	h := newHarness(t)

	top, _ := h.limit("A", Sell, 10000, 10)
	h.limit("B", Sell, 10100, 10)

	if err := h.cancel(top); err != nil {
		t.Fatalf("canceling the top of book: %v", err)
	}
	if ask, ok := h.book.BestAsk(); !ok || ask != 10100 {
		t.Fatalf("best ask is (%d, %v), want (10100, true)", ask, ok)
	}
	h.requireDepth(Sell, []LevelView{level(10100, 10, 1)})
}

func TestCancelUnknownOrderIsRejected(t *testing.T) {
	h := newHarness(t)

	if err := h.cancel(OrderID(999)); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("canceling an unknown order returned %v, want ErrUnknownOrder", err)
	}
}

func TestCancelTwiceIsRejected(t *testing.T) {
	h := newHarness(t)

	id, _ := h.limit("A", Sell, 10000, 10)
	if err := h.cancel(id); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if err := h.cancel(id); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("second cancel returned %v, want ErrUnknownOrder", err)
	}
	// The first cancel must not be double-counted by the second attempt.
	if got := h.order(id).CanceledQty; got != 10 {
		t.Fatalf("order reports %d canceled, want 10", got)
	}
}

func TestCancelFilledOrderIsRejected(t *testing.T) {
	h := newHarness(t)

	id, _ := h.limit("A", Sell, 10000, 10)
	h.limit("taker", Buy, 10000, 10)

	if err := h.cancel(id); !errors.Is(err, ErrUnknownOrder) {
		t.Fatalf("canceling a filled order returned %v, want ErrUnknownOrder", err)
	}
	if got := h.order(id).CanceledQty; got != 0 {
		t.Fatalf("filled order reports %d canceled, want 0", got)
	}
}

func TestMarketOrderWalksEveryLevel(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10000, 10)
	h.limit("maker", Sell, 10100, 20)
	h.limit("maker", Sell, 10200, 30)

	_, res := h.market("taker", Buy, 55)

	h.requireTrades([2]int64{10000, 10}, [2]int64{10100, 20}, [2]int64{10200, 25})
	if res.Rested || res.CanceledQty != 0 {
		t.Fatalf("fully filled market order: rested=%v canceled=%d", res.Rested, res.CanceledQty)
	}
	h.requireDepth(Sell, []LevelView{level(10200, 5, 1)})
	if last, _ := h.book.LastTradePrice(); last != 10200 {
		t.Fatalf("last traded price is %d, want 10200", last)
	}
}

func TestMarketOrderCancelsUnfillableRemainder(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10000, 70)

	id, res := h.market("taker", Buy, 100)

	h.requireTrades([2]int64{10000, 70})
	if res.Rested {
		t.Fatal("market order remainder rested on the book")
	}
	if res.CanceledQty != 30 {
		t.Fatalf("canceled %d, want 30", res.CanceledQty)
	}
	h.requireGone(id)
	h.requireDepth(Buy, nil)

	o := h.order(id)
	if o.FilledQty != 70 || o.CanceledQty != 30 || o.RemainingQty != 0 {
		t.Fatalf("market order: filled=%d canceled=%d remaining=%d, want 70, 30, 0",
			o.FilledQty, o.CanceledQty, o.RemainingQty)
	}
}

func TestLimitBoundaryDoesNotCross(t *testing.T) {
	h := newHarness(t)

	askID, _ := h.limit("maker", Sell, 10001, 10)
	buyID, res := h.limit("taker", Buy, 10000, 10)

	if len(res.Trades) != 0 {
		t.Fatalf("a 100.00 bid traded against a 100.01 ask: %s", h.tradeSummary())
	}
	h.requireResting(askID, 10)
	h.requireResting(buyID, 10)

	// The resulting book must be tight but not crossed.
	bid, _ := h.book.BestBid()
	ask, _ := h.book.BestAsk()
	if bid >= ask {
		t.Fatalf("book is crossed: bid %d, ask %d", bid, ask)
	}
	if spread, ok := h.book.Spread(); !ok || spread != 1 {
		t.Fatalf("spread is (%d, %v), want (1, true)", spread, ok)
	}
}

func TestSweepStopsAtLimitPrice(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Sell, 10000, 10)
	h.limit("maker", Sell, 10100, 20)
	h.limit("maker", Sell, 10200, 30)

	// Willing to pay up to 101.00, so the 102.00 level must be untouched and the
	// remainder must rest at the bid.
	buyID, res := h.limit("taker", Buy, 10100, 100)

	h.requireTrades([2]int64{10000, 10}, [2]int64{10100, 20})
	if !res.Rested {
		t.Fatal("remainder did not rest after the sweep stopped at the limit")
	}
	h.requireResting(buyID, 70)
	h.requireDepth(Sell, []LevelView{level(10200, 30, 1)})
	h.requireDepth(Buy, []LevelView{level(10100, 70, 1)})
}

func TestAggressorReceivesPriceImprovement(t *testing.T) {
	h := newHarness(t)

	// The resting ask is better than the buyer's limit, so the trade prints at
	// the resting price rather than at the aggressor's willingness to pay.
	h.limit("maker", Sell, 9990, 10)
	h.limit("taker", Buy, 10000, 10)

	h.requireTrades([2]int64{9990, 10})
}

func TestSellSweepIsSymmetric(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Buy, 10000, 10)
	h.limit("maker", Buy, 9900, 20)
	h.limit("maker", Buy, 9800, 30)

	_, res := h.limit("taker", Sell, 9900, 40)

	h.requireTrades([2]int64{10000, 10}, [2]int64{9900, 20})
	if !res.Rested {
		t.Fatal("remainder of the aggressive sell did not rest")
	}
	h.requireDepth(Buy, []LevelView{level(9800, 30, 1)})
	h.requireDepth(Sell, []LevelView{level(9900, 10, 1)})
}

func TestDepthAggregatesOrdersAtAPrice(t *testing.T) {
	h := newHarness(t)

	h.limit("A", Buy, 10000, 10)
	h.limit("B", Buy, 10000, 25)
	h.limit("C", Buy, 9900, 5)

	h.requireDepth(Buy, []LevelView{level(10000, 35, 2), level(9900, 5, 1)})
}

func TestDepthIsTruncatedAndOrderedByPriority(t *testing.T) {
	h := newHarness(t)

	for i := int64(0); i < 15; i++ {
		h.limit("maker", Buy, PriceTicks(10000-i), 1)
		h.limit("maker", Sell, PriceTicks(10100+i), 1)
	}

	bids := h.book.Depth(Buy, DefaultDepth)
	asks := h.book.Depth(Sell, DefaultDepth)
	if len(bids) != DefaultDepth || len(asks) != DefaultDepth {
		t.Fatalf("depth returned %d bids and %d asks, want %d of each", len(bids), len(asks), DefaultDepth)
	}
	// Bids descend from the best price, asks ascend.
	for i := 1; i < len(bids); i++ {
		if bids[i].Price >= bids[i-1].Price {
			t.Fatalf("bid level %d (%d) is not below level %d (%d)", i, bids[i].Price, i-1, bids[i-1].Price)
		}
		if asks[i].Price <= asks[i-1].Price {
			t.Fatalf("ask level %d (%d) is not above level %d (%d)", i, asks[i].Price, i-1, asks[i-1].Price)
		}
	}
	if bids[0].Price != 10000 || asks[0].Price != 10100 {
		t.Fatalf("top of book is %d / %d, want 10000 / 10100", bids[0].Price, asks[0].Price)
	}
}

func TestMidPriceIsNotTheLastPrice(t *testing.T) {
	h := newHarness(t)

	h.limit("maker", Buy, 9900, 10)
	h.limit("maker", Sell, 10100, 10)

	mid, ok := h.book.MidPrice()
	if !ok || mid != 10000 {
		t.Fatalf("mid is (%d, %v), want (10000, true)", mid, ok)
	}
	if _, ok := h.book.LastTradePrice(); ok {
		t.Fatal("last traded price is set although no trade has happened")
	}
}

func TestOpenOrdersAreScopedToParticipantAndOrderedBySequence(t *testing.T) {
	h := newHarness(t)

	first, _ := h.limit("human", Buy, 9900, 10)
	h.limit("bot", Buy, 9800, 10)
	second, _ := h.limit("human", Sell, 10100, 10)

	open := h.book.OpenOrders("human")
	if len(open) != 2 {
		t.Fatalf("human has %d open orders, want 2", len(open))
	}
	if open[0].ID != first || open[1].ID != second {
		t.Fatalf("open orders are %d, %d; want %d, %d in sequence order",
			open[0].ID, open[1].ID, first, second)
	}
	if all := h.book.OpenOrders(""); len(all) != 3 {
		t.Fatalf("book reports %d open orders in total, want 3", len(all))
	}
}
