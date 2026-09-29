package sim

import (
	"math/rand"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// NoiseConfig configures the order-flow trader. Probabilities are percentages.
type NoiseConfig struct {
	// Participant is the identity it trades under. Defaults to "bot-noise".
	Participant string
	// Seed seeds every decision it makes.
	Seed int64
	// ActPercent is the chance of doing anything at all at a decision point, so
	// the trader is bursty rather than submitting on every single tick.
	ActPercent int
	// AggressivePercent is the share of actions that take liquidity.
	AggressivePercent int
	// CancelPercent is the share of actions that withdraw one of its own resting
	// orders.
	CancelPercent int
	// MaxQty is the largest order it sends; quantities are uniform in [1, MaxQty].
	MaxQty int64
	// PassiveOffset is the closest a passive limit order sits behind the touch, in
	// ticks.
	PassiveOffset engine.PriceTicks
	// PassiveSpread is how many distinct ticks behind the touch passive orders are
	// spread across, starting at PassiveOffset. Without this the trader would stack
	// every passive order on one price and the book would grow a single absurdly
	// deep level instead of a plausible queue.
	PassiveSpread int
	// MarketableOffset is how far through the touch a marketable limit order is
	// priced, in ticks.
	MarketableOffset engine.PriceTicks
}

func (c NoiseConfig) withDefaults() NoiseConfig {
	if c.Participant == "" {
		c.Participant = "bot-noise"
	}
	if c.ActPercent <= 0 {
		c.ActPercent = 60
	}
	if c.AggressivePercent <= 0 {
		c.AggressivePercent = 55
	}
	if c.CancelPercent <= 0 {
		c.CancelPercent = 10
	}
	if c.MaxQty <= 0 {
		c.MaxQty = 25
	}
	if c.PassiveOffset <= 0 {
		c.PassiveOffset = 1
	}
	if c.PassiveSpread <= 0 {
		c.PassiveSpread = 4
	}
	if c.MarketableOffset <= 0 {
		c.MarketableOffset = 2
	}
	return c
}

// NoiseTrader generates order flow so that trades actually happen and the price
// moves through the liquidity the maker provides.
//
// Every decision comes from a seeded PRNG, so a run driven by the deterministic
// scheduler can be reproduced exactly from its seed. It has no view of value and no
// opinion about direction; it is a source of flow, not a strategy.
type NoiseTrader struct {
	cfg NoiseConfig
	rng *rand.Rand
}

// NewNoiseTrader builds a noise trader.
func NewNoiseTrader(cfg NoiseConfig) *NoiseTrader {
	cfg = cfg.withDefaults()
	return &NoiseTrader{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}
}

// Participant returns the trader's identity.
func (n *NoiseTrader) Participant() string { return n.cfg.Participant }

// Decide returns at most one command.
func (n *NoiseTrader) Decide(v View) []market.Command {
	if n.rng.Intn(100) >= n.cfg.ActPercent {
		return nil
	}

	side := engine.Buy
	if n.rng.Intn(2) == 0 {
		side = engine.Sell
	}
	qty := 1 + n.rng.Int63n(n.cfg.MaxQty)

	roll := n.rng.Intn(100)
	switch {
	case roll < n.cfg.CancelPercent && len(v.Open) > 0:
		pick := v.Open[n.rng.Intn(len(v.Open))]
		return []market.Command{market.CancelOrder(n.cfg.Participant, pick.ID)}

	case roll < n.cfg.CancelPercent+n.cfg.AggressivePercent:
		if cmd, ok := n.aggressive(v, side, qty); ok {
			return []market.Command{cmd}
		}
		// Nothing to trade against, so provide liquidity instead of doing nothing.
		fallthrough

	default:
		if cmd, ok := n.passive(v, side, qty); ok {
			return []market.Command{cmd}
		}
		return nil
	}
}

// aggressive takes liquidity, either with a market order or with a limit priced
// through the touch. It reports false when the opposite side is empty.
func (n *NoiseTrader) aggressive(v View, side engine.Side, qty int64) (market.Command, bool) {
	touch, ok := oppositeTouch(v.Book, side)
	if !ok {
		return market.Command{}, false
	}

	if n.rng.Intn(2) == 0 {
		return market.MarketOrder(n.cfg.Participant, side, qty), true
	}

	// A marketable limit crosses the touch by a couple of ticks: it executes now
	// but caps what it will pay, which is the more realistic of the two.
	price := touch + n.cfg.MarketableOffset
	if side == engine.Sell {
		price = touch - n.cfg.MarketableOffset
	}
	if price <= 0 {
		return market.MarketOrder(n.cfg.Participant, side, qty), true
	}
	return market.LimitOrder(n.cfg.Participant, side, price, qty), true
}

// passive rests behind the touch on its own side.
func (n *NoiseTrader) passive(v View, side engine.Side, qty int64) (market.Command, bool) {
	anchor, ok := sameSideTouch(v.Book, side)
	if !ok {
		// Nothing on this side to join, so fall back to the reference price.
		anchor = v.Reference()
	}

	offset := n.cfg.PassiveOffset + engine.PriceTicks(n.rng.Intn(n.cfg.PassiveSpread))
	price := anchor - offset
	if side == engine.Sell {
		price = anchor + offset
	}
	if price <= 0 {
		return market.Command{}, false
	}
	return market.LimitOrder(n.cfg.Participant, side, price, qty), true
}

// oppositeTouch is the best price an order of this side would trade against.
func oppositeTouch(book engine.BookSnapshot, side engine.Side) (engine.PriceTicks, bool) {
	if side == engine.Buy {
		return book.BestAsk, book.HasBestAsk
	}
	return book.BestBid, book.HasBestBid
}

// sameSideTouch is the best price already resting on this order's own side.
func sameSideTouch(book engine.BookSnapshot, side engine.Side) (engine.PriceTicks, bool) {
	if side == engine.Buy {
		return book.BestBid, book.HasBestBid
	}
	return book.BestAsk, book.HasBestAsk
}
