package engine

import (
	"fmt"
	"io"
)

// WriteCanonical writes a stable, exhaustive textual encoding of the book.
//
// It exists so two books can be compared for exact equality by digesting this
// output. That is stricter than comparing snapshots: it includes every price
// level rather than the published depth, and every resting order's identity,
// priority and quantities rather than just the level totals. Levels are written
// in priority order and orders in FIFO order, so the encoding never depends on
// Go's map or heap iteration order.
func (b *Book) WriteCanonical(w io.Writer) {
	fmt.Fprintf(w, "book %s\n", b.Symbol)
	if last, ok := b.LastTradePrice(); ok {
		fmt.Fprintf(w, "last %d\n", last)
	} else {
		fmt.Fprint(w, "last none\n")
	}
	fmt.Fprintf(w, "trades %d\n", b.nextTradeID)

	for _, side := range []Side{Buy, Sell} {
		fmt.Fprintf(w, "side %s\n", side)
		for _, lvl := range b.sideFor(side).sortedLevels(0) {
			fmt.Fprintf(w, "level %d qty %d orders %d\n", lvl.Price, lvl.TotalQty, lvl.Len())
			for _, o := range lvl.Orders() {
				fmt.Fprintf(w, "order %d %s seq %d orig %d rem %d filled %d\n",
					o.ID, o.ParticipantID, o.PrioritySequence, o.OriginalQty, o.RemainingQty, o.FilledQty)
			}
		}
	}
}

// WriteCanonicalTrade writes a stable encoding of one trade, for the same purpose.
func WriteCanonicalTrade(w io.Writer, t Trade) {
	fmt.Fprintf(w, "trade %d seq %d %d @ %d buy %d sell %d aggressor %s\n",
		t.ID, t.Sequence, t.Quantity, t.Price, t.BuyOrderID, t.SellOrderID, t.AggressorSide)
}
