package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// harness drives a book the way a market would, and re-checks every engine
// invariant after every single command.
//
// Asserting invariants continuously rather than at the end of a test is the point
// of this type: a bug that corrupts the order index or a level total is usually
// invisible in the result of the command that caused it and only surfaces several
// commands later, by which time the original cause is gone.
type harness struct {
	t      *testing.T
	book   *Book
	seq    uint64
	nextID uint64

	byID   map[OrderID]*Order
	all    []*Order
	trades []Trade
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{
		t:    t,
		book: NewBook("TEST"),
		byID: make(map[OrderID]*Order),
	}
}

// limit submits a limit order and returns its ID with the matching result.
func (h *harness) limit(participant string, side Side, price PriceTicks, qty int64) (OrderID, Result) {
	h.t.Helper()
	return h.submit(participant, side, Limit, price, qty)
}

// market submits a market order and returns its ID with the matching result.
func (h *harness) market(participant string, side Side, qty int64) (OrderID, Result) {
	h.t.Helper()
	return h.submit(participant, side, Market, 0, qty)
}

func (h *harness) submit(participant string, side Side, typ OrderType, price PriceTicks, qty int64) (OrderID, Result) {
	h.t.Helper()

	h.seq++
	h.nextID++
	o := NewOrder(OrderID(h.nextID), h.seq, participant, h.book.Symbol, side, typ, price, qty)
	h.byID[o.ID] = o
	h.all = append(h.all, o)

	// Capture pre-trade remaining quantities so each fill can be validated
	// against what its two orders actually had available.
	pre := make(map[OrderID]int64, len(h.byID))
	for id, ord := range h.byID {
		pre[id] = ord.RemainingQty
	}

	res := h.book.Submit(o)

	for _, tr := range res.Trades {
		restingID := tr.BuyOrderID
		if tr.AggressorSide == Buy {
			restingID = tr.SellOrderID
		}
		if tr.AggressorSide != side {
			h.t.Fatalf("trade %d records aggressor side %s, want %s", tr.ID, tr.AggressorSide, side)
		}
		if err := CheckTrade(tr, pre[o.ID], pre[restingID]); err != nil {
			h.t.Fatalf("invalid trade: %v", err)
		}
		// Every execution must print at the passive order's limit price.
		if resting := h.byID[restingID]; resting != nil && tr.Price != resting.LimitPrice {
			h.t.Fatalf("trade %d printed at %d, want resting order %d's price %d",
				tr.ID, tr.Price, restingID, resting.LimitPrice)
		}
		pre[o.ID] -= tr.Quantity
		pre[restingID] -= tr.Quantity
	}

	h.trades = append(h.trades, res.Trades...)
	h.check()
	return o.ID, res
}

func (h *harness) cancel(id OrderID) error {
	h.t.Helper()
	h.seq++
	_, err := h.book.Cancel(id)
	h.check()
	return err
}

// check asserts the book's structural invariants plus quantity conservation
// across every order the harness has ever created, including orders that have
// already left the book.
func (h *harness) check() {
	h.t.Helper()
	if err := h.book.CheckInvariants(); err != nil {
		h.t.Fatalf("invariant violation:\n%v\nbook:\n%s", err, h.dump())
	}
	for _, o := range h.all {
		if !o.Conserved() {
			h.t.Fatalf("order %d does not conserve quantity: original %d != filled %d + remaining %d + canceled %d",
				o.ID, o.OriginalQty, o.FilledQty, o.RemainingQty, o.CanceledQty)
		}
	}
}

// order returns the tracked order, failing the test if it was never submitted.
func (h *harness) order(id OrderID) *Order {
	h.t.Helper()
	o, ok := h.byID[id]
	if !ok {
		h.t.Fatalf("order %d was never submitted", id)
	}
	return o
}

// requireResting asserts an order is on the book with the given remaining size.
func (h *harness) requireResting(id OrderID, remaining int64) {
	h.t.Helper()
	view, ok := h.book.Order(id)
	if !ok {
		h.t.Fatalf("order %d is not resting on the book", id)
	}
	if view.RemainingQty != remaining {
		h.t.Fatalf("order %d rests with %d remaining, want %d", id, view.RemainingQty, remaining)
	}
}

// requireGone asserts an order is no longer on the book.
func (h *harness) requireGone(id OrderID) {
	h.t.Helper()
	if _, ok := h.book.Order(id); ok {
		h.t.Fatalf("order %d is still resting on the book", id)
	}
}

// requireDepth asserts one side's aggregated levels, best price first.
func (h *harness) requireDepth(side Side, want []LevelView) {
	h.t.Helper()
	got := h.book.Depth(side, 0)
	if len(got) != len(want) {
		h.t.Fatalf("%s side has %d levels, want %d\ngot:  %v\nwant: %v", side, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			h.t.Fatalf("%s level %d is %+v, want %+v", side, i, got[i], want[i])
		}
	}
}

// requireTrades asserts the price/quantity sequence of every trade so far.
func (h *harness) requireTrades(want ...[2]int64) {
	h.t.Helper()
	if len(h.trades) != len(want) {
		h.t.Fatalf("got %d trades, want %d\ngot: %s", len(h.trades), len(want), h.tradeSummary())
	}
	for i, w := range want {
		if PriceTicks(w[0]) != h.trades[i].Price || w[1] != h.trades[i].Quantity {
			h.t.Fatalf("trade %d is %d @ %d, want %d @ %d",
				i, h.trades[i].Quantity, h.trades[i].Price, w[1], w[0])
		}
	}
}

func (h *harness) tradeSummary() string {
	s := ""
	for _, t := range h.trades {
		s += fmt.Sprintf("  %d @ %d (aggressor %s, buy %d, sell %d)\n",
			t.Quantity, t.Price, t.AggressorSide, t.BuyOrderID, t.SellOrderID)
	}
	return s
}

func (h *harness) dump() string {
	s := ""
	for _, side := range []Side{Sell, Buy} {
		for _, lvl := range h.book.sideFor(side).sortedLevels(0) {
			s += fmt.Sprintf("  %s %d total=%d:", side, lvl.Price, lvl.TotalQty)
			for _, o := range lvl.Orders() {
				s += fmt.Sprintf(" [id=%d seq=%d rem=%d]", o.ID, o.PrioritySequence, o.RemainingQty)
			}
			s += "\n"
		}
	}
	return s
}

func level(price, qty int64, orders int) LevelView {
	return LevelView{Price: PriceTicks(price), Quantity: qty, Orders: orders}
}

// newRand is a seeded PRNG, wrapped so the tests read the same way whichever file
// they live in.
func newRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }
