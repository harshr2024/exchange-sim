package engine

// DefaultDepth is how many price levels per side the market data feed publishes.
const DefaultDepth = 10

// LevelView is one aggregated price level as published to clients.
type LevelView struct {
	Price    PriceTicks `json:"price"`
	Quantity int64      `json:"quantity"`
	Orders   int        `json:"orders"`
}

// BookSnapshot is an immutable point-in-time view of the book, safe to hand to
// other goroutines. Sequence is filled in by the owning market.
type BookSnapshot struct {
	Symbol   string `json:"symbol"`
	Sequence uint64 `json:"sequence"`

	Bids []LevelView `json:"bids"`
	Asks []LevelView `json:"asks"`

	BestBid    PriceTicks `json:"best_bid,omitempty"`
	HasBestBid bool       `json:"has_best_bid"`
	BestAsk    PriceTicks `json:"best_ask,omitempty"`
	HasBestAsk bool       `json:"has_best_ask"`

	Spread    PriceTicks `json:"spread,omitempty"`
	HasSpread bool       `json:"has_spread"`
	Mid       PriceTicks `json:"mid,omitempty"`
	HasMid    bool       `json:"has_mid"`

	LastTradePrice PriceTicks `json:"last_trade_price,omitempty"`
	HasLastTrade   bool       `json:"has_last_trade"`
}

// Depth returns up to maxLevels aggregated levels for one side, best price first.
// A maxLevels of 0 or less returns the whole side.
func (b *Book) Depth(side Side, maxLevels int) []LevelView {
	levels := b.sideFor(side).sortedLevels(maxLevels)
	out := make([]LevelView, 0, len(levels))
	for _, lvl := range levels {
		out = append(out, LevelView{Price: lvl.Price, Quantity: lvl.TotalQty, Orders: lvl.Len()})
	}
	return out
}

// Snapshot captures the top maxLevels of both sides plus the derived quotes.
func (b *Book) Snapshot(maxLevels int) BookSnapshot {
	snap := BookSnapshot{
		Symbol: b.Symbol,
		Bids:   b.Depth(Buy, maxLevels),
		Asks:   b.Depth(Sell, maxLevels),
	}
	snap.BestBid, snap.HasBestBid = b.BestBid()
	snap.BestAsk, snap.HasBestAsk = b.BestAsk()
	snap.Spread, snap.HasSpread = b.Spread()
	snap.Mid, snap.HasMid = b.MidPrice()
	snap.LastTradePrice, snap.HasLastTrade = b.LastTradePrice()
	return snap
}
