package market

import "github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"

// Update is the incremental market data produced by one command.
//
// This is what the live feed carries. It holds only what changed: the new state of the
// price levels the command touched, the trades it printed, and the quotes that follow.
// A subscriber that has a snapshot and every subsequent Update in sequence order has
// the book.
//
// Deliberately not a full snapshot. Rebuilding the published depth means collecting and
// sorting every level on the book, which costs more than the matching underneath it once
// a book has many distinct prices. Snapshots stay an occasional synchronisation
// operation; this is the hot path.
//
// Level quantities are absolute rather than differences, so applying an Update twice is
// harmless and a subscriber never has to track whether it already accounted for a fill.
type Update struct {
	Symbol   string `json:"symbol"`
	Sequence uint64 `json:"sequence"`

	// Levels is the new state of each price level the command changed. A zero
	// Quantity means the level no longer exists.
	Levels []engine.LevelChange `json:"levels,omitempty"`
	// Trades are the executions the command printed, in order.
	Trades []engine.Trade `json:"trades,omitempty"`

	BestBid    engine.PriceTicks `json:"best_bid,omitempty"`
	HasBestBid bool              `json:"has_best_bid"`
	BestAsk    engine.PriceTicks `json:"best_ask,omitempty"`
	HasBestAsk bool              `json:"has_best_ask"`

	LastTradePrice engine.PriceTicks `json:"last_trade_price,omitempty"`
	HasLastTrade   bool              `json:"has_last_trade"`
}

// Empty reports whether the update carries nothing a subscriber needs.
func (u Update) Empty() bool { return len(u.Levels) == 0 && len(u.Trades) == 0 }
