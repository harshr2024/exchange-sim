// Package display renders market state as plain text for the command-line tools.
//
// This is the only place that turns tick prices into decimal strings. Keeping the
// conversion at the presentation boundary is what lets the rest of the system work
// in integers exclusively.
package display

import (
	"fmt"
	"io"
	"strings"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
)

// Book writes the aggregated depth with asks above bids, the way a trading screen
// shows it, with the last traded price between the two sides.
func Book(w io.Writer, inst instrument.Instrument, snap engine.BookSnapshot) {
	fmt.Fprintf(w, "%s  %s   seq %d", inst.Symbol, inst.DisplayName, snap.Sequence)
	if snap.HasLastTrade {
		fmt.Fprintf(w, "   last %s", inst.Format(snap.LastTradePrice))
	} else {
		fmt.Fprint(w, "   last --")
	}
	if snap.HasSpread {
		fmt.Fprintf(w, "   spread %s", inst.Format(snap.Spread))
	}
	fmt.Fprint(w, "\n")

	fmt.Fprintf(w, "%12s %8s %7s\n", "PRICE", "QTY", "ORDERS")

	// Asks descend towards the touch so the best ask sits just above the middle.
	for i := len(snap.Asks) - 1; i >= 0; i-- {
		lvl := snap.Asks[i]
		fmt.Fprintf(w, "%12s %8d %7d   ask\n", inst.Format(lvl.Price), lvl.Quantity, lvl.Orders)
	}

	middle := "        ----"
	if snap.HasLastTrade {
		middle = fmt.Sprintf("%12s", inst.Format(snap.LastTradePrice))
	}
	fmt.Fprintf(w, "%s %8s %7s   <- last\n", middle, "", "")

	for _, lvl := range snap.Bids {
		fmt.Fprintf(w, "%12s %8d %7d   bid\n", inst.Format(lvl.Price), lvl.Quantity, lvl.Orders)
	}
	if len(snap.Bids) == 0 && len(snap.Asks) == 0 {
		fmt.Fprint(w, "        (empty book)\n")
	}
}

// Tape writes recent trades, newest last.
func Tape(w io.Writer, inst instrument.Instrument, trades []engine.Trade) {
	if len(trades) == 0 {
		fmt.Fprint(w, "  (no trades yet)\n")
		return
	}
	for _, t := range trades {
		fmt.Fprintf(w, "  seq %-6d %6d @ %-10s aggressor %-4s  buy #%d / sell #%d\n",
			t.Sequence, t.Quantity, inst.Format(t.Price), t.AggressorSide, t.BuyOrderID, t.SellOrderID)
	}
}

// OpenOrders writes a participant's resting orders.
func OpenOrders(w io.Writer, inst instrument.Instrument, orders []engine.View) {
	if len(orders) == 0 {
		fmt.Fprint(w, "  (no open orders)\n")
		return
	}
	for _, o := range orders {
		fmt.Fprintf(w, "  #%-4d %-4s %6d of %-6d @ %-10s seq %d\n",
			o.ID, o.Side, o.RemainingQty, o.OriginalQty, inst.Format(o.LimitPrice), o.PrioritySequence)
	}
}

// Rule writes a labelled separator.
func Rule(w io.Writer, label string) {
	const width = 64
	if label == "" {
		fmt.Fprintf(w, "%s\n", strings.Repeat("-", width))
		return
	}
	pad := width - len(label) - 3
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(w, "-- %s %s\n", label, strings.Repeat("-", pad))
}
