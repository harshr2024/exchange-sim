// Package sim contains the simulated traders that make the exchange demonstrable.
//
// Bots are decision functions, not goroutines. A Bot receives an immutable View of
// the market and returns the commands it would like to submit; it has no reference
// to the order book, the market or the actor, so it cannot mutate matching state
// even by accident. Deciding *when* to consult a bot is the job of a runner, and
// the two runners in this package make deliberately different guarantees:
//
//   - Driver polls bots from one goroutine in a fixed order. Same seeds and same
//     tick count produce the same command stream, so the whole simulation is
//     reproducible from its seeds alone.
//   - Live runs each bot on its own goroutine and wall-clock interval. Which
//     command reaches the market first is then up to the Go scheduler, so identical
//     seeds do not imply identical sessions. Reproducing a live run means replaying
//     its recorded command log.
//
// Those are different guarantees and the distinction is not cosmetic. Conflating
// them would mean asserting something about concurrent scheduling that is not true.
package sim

import (
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// View is everything a bot is allowed to know when it decides.
//
// Open holds only the deciding bot's own resting orders, and Book carries
// aggregated price levels with no order identities in them. A bot therefore has no
// way to even name another participant's order, let alone cancel it. Ownership is
// enforced by the market as well, but this is what makes a violation unexpressible
// rather than merely refused.
type View struct {
	Instrument instrument.Instrument
	Book       engine.BookSnapshot
	Open       []engine.View
}

// Bot decides what to do at one decision point.
type Bot interface {
	// Participant is the identity the bot submits under. It must be stable.
	Participant() string
	// Decide returns commands to submit in the order returned. It may return nil.
	Decide(View) []market.Command
}

// Reference is the price the bots quote around.
//
// It is derived from market state wherever the market can supply one, preferring
// the mid, then the last traded price, then whichever side exists. The
// instrument's bootstrap price is a last resort for a market that has never
// traded and has nothing resting.
//
// This value is only ever read. It is not written back to the market and never
// becomes the displayed price: the last price moves when a trade prints and at no
// other time, which is what keeps the quoted price a product of activity in this
// market rather than a number the bots agreed on.
func (v View) Reference() engine.PriceTicks {
	switch {
	case v.Book.HasMid:
		return v.Book.Mid
	case v.Book.HasLastTrade:
		return v.Book.LastTradePrice
	case v.Book.HasBestBid:
		return v.Book.BestBid
	case v.Book.HasBestAsk:
		return v.Book.BestAsk
	default:
		return v.Instrument.InitialReferencePrice
	}
}

// OwnAt returns the bot's resting order at one price on one side, if it has one.
func (v View) OwnAt(side engine.Side, price engine.PriceTicks) (engine.View, bool) {
	for _, o := range v.Open {
		if o.Side == side && o.LimitPrice == price {
			return o, true
		}
	}
	return engine.View{}, false
}

// OwnSide returns the bot's resting orders on one side, in priority order.
func (v View) OwnSide(side engine.Side) []engine.View {
	out := make([]engine.View, 0, len(v.Open))
	for _, o := range v.Open {
		if o.Side == side {
			out = append(out, o)
		}
	}
	return out
}
