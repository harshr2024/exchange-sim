package sim

import (
	"math/rand"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// MakerConfig configures the liquidity provider.
type MakerConfig struct {
	// Participant is the identity it quotes under. Defaults to "bot-mm".
	Participant string
	// Levels is how many price levels to quote on each side.
	Levels int
	// Size is the target quantity per level.
	Size int64
	// HalfSpread is how far from the reference the first quote on each side sits,
	// in ticks, so the quoted spread is twice this.
	HalfSpread engine.PriceTicks
	// LevelStep is the gap in ticks between successive levels on a side.
	LevelStep engine.PriceTicks
	// SizeJitter is the largest random amount added to Size on a new quote. Zero
	// quotes exactly Size every time.
	SizeJitter int64
	// RefillBelow replaces a quote once its remaining size falls to this fraction
	// of Size or less. This is what makes partially consumed quotes get refreshed.
	RefillBelow float64
	// Seed seeds the size jitter.
	Seed int64
}

func (c MakerConfig) withDefaults() MakerConfig {
	if c.Participant == "" {
		c.Participant = "bot-mm"
	}
	if c.Levels <= 0 {
		c.Levels = 3
	}
	if c.Size <= 0 {
		c.Size = 100
	}
	if c.HalfSpread <= 0 {
		c.HalfSpread = 2
	}
	if c.LevelStep <= 0 {
		c.LevelStep = 1
	}
	if c.RefillBelow <= 0 {
		c.RefillBelow = 0.5
	}
	return c
}

// MarketMaker keeps both sides of the book populated.
//
// Its whole strategy is "maintain a ladder of quotes around the reference price and
// replace them when they go stale". There is no inventory model, no skew, no
// Avellaneda-Stoikov, no learning. Its job is to give the book a visible spread and
// depth, and to exercise the engine's cancel/replace path as its quotes get eaten;
// anything cleverer would be strategy code rather than exchange code.
type MarketMaker struct {
	cfg MakerConfig
	rng *rand.Rand
}

// NewMarketMaker builds a market maker.
func NewMarketMaker(cfg MakerConfig) *MarketMaker {
	cfg = cfg.withDefaults()
	return &MarketMaker{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}
}

// Participant returns the maker's identity.
func (m *MarketMaker) Participant() string { return m.cfg.Participant }

// Decide cancels quotes that are stale or depleted and replaces anything missing
// from the ladder.
func (m *MarketMaker) Decide(v View) []market.Command {
	ref := v.Reference()
	want := map[engine.Side][]engine.PriceTicks{
		engine.Buy:  m.ladder(engine.Buy, ref, v.Book),
		engine.Sell: m.ladder(engine.Sell, ref, v.Book),
	}

	wanted := func(side engine.Side, price engine.PriceTicks) bool {
		for _, p := range want[side] {
			if p == price {
				return true
			}
		}
		return false
	}

	// A quote is worth keeping only if it is still on the ladder and still carries
	// enough size to be useful.
	floor := int64(float64(m.cfg.Size) * m.cfg.RefillBelow)
	type slot struct {
		side  engine.Side
		price engine.PriceTicks
	}
	keeping := make(map[slot]bool, len(v.Open))

	var cmds []market.Command
	for _, o := range v.Open {
		if wanted(o.Side, o.LimitPrice) && o.RemainingQty > floor {
			keeping[slot{o.Side, o.LimitPrice}] = true
			continue
		}
		cmds = append(cmds, market.CancelOrder(m.cfg.Participant, o.ID))
	}

	// Anything on the ladder without a surviving quote gets a fresh one. Cancels
	// are emitted first, so a depleted quote is withdrawn before its replacement is
	// submitted at the same price.
	for _, side := range []engine.Side{engine.Buy, engine.Sell} {
		for _, price := range want[side] {
			if keeping[slot{side, price}] {
				continue
			}
			cmds = append(cmds, market.LimitOrder(m.cfg.Participant, side, price, m.size()))
		}
	}
	return cmds
}

// ladder returns the prices the maker wants to be quoting on one side, best first.
func (m *MarketMaker) ladder(side engine.Side, ref engine.PriceTicks, book engine.BookSnapshot) []engine.PriceTicks {
	top := ref - m.cfg.HalfSpread
	if side == engine.Sell {
		top = ref + m.cfg.HalfSpread
	}

	// Never quote through the opposite side. A market maker whose quote crossed
	// would be taking liquidity rather than providing it, and because there is no
	// self-trade prevention it could match its own resting quote on the other side.
	// Shifting the whole ladder rather than clamping each level keeps the levels
	// distinct.
	switch {
	case side == engine.Buy && book.HasBestAsk && top >= book.BestAsk:
		top = book.BestAsk - 1
	case side == engine.Sell && book.HasBestBid && top <= book.BestBid:
		top = book.BestBid + 1
	}

	out := make([]engine.PriceTicks, 0, m.cfg.Levels)
	for i := range m.cfg.Levels {
		step := engine.PriceTicks(i) * m.cfg.LevelStep
		price := top - step
		if side == engine.Sell {
			price = top + step
		}
		if price <= 0 {
			break
		}
		out = append(out, price)
	}
	return out
}

func (m *MarketMaker) size() int64 {
	if m.cfg.SizeJitter <= 0 {
		return m.cfg.Size
	}
	return m.cfg.Size + m.rng.Int63n(m.cfg.SizeJitter+1)
}
