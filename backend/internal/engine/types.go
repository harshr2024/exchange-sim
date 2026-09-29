// Package engine implements a deterministic, continuous central limit order
// book with FIFO price-time priority.
//
// Nothing in this package is safe for concurrent use. A Book is owned by exactly
// one goroutine; see the market package for the single-writer actor that owns it.
package engine

import (
	"encoding/json"
	"fmt"
)

// PriceTicks is a price expressed as a whole number of ticks.
//
// Prices are integers everywhere on the matching path. Decimal dollar amounts
// are not exactly representable in binary floating point, so using float64 as a
// matching key would make price equality and level lookup unreliable. Ticks are
// converted to a display string only at the presentation boundary.
type PriceTicks int64

// OrderID identifies an order within a market. IDs are allocated by the owning
// market from a counter, which is what makes them reproducible under replay.
type OrderID uint64

// TradeID identifies an execution within a market.
type TradeID uint64

// Side is the direction of an order.
type Side uint8

const (
	// Buy is the bid side.
	Buy Side = iota + 1
	// Sell is the ask side.
	Sell
)

// Opposite returns the side an order of this side matches against.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// Valid reports whether s is a recognised side.
func (s Side) Valid() bool { return s == Buy || s == Sell }

func (s Side) String() string {
	switch s {
	case Buy:
		return "buy"
	case Sell:
		return "sell"
	default:
		return "invalid"
	}
}

// MarshalJSON encodes the side as a readable string so session logs and market
// data events stay human-inspectable.
func (s Side) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON decodes the string form written by MarshalJSON.
func (s *Side) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch raw {
	case "buy":
		*s = Buy
	case "sell":
		*s = Sell
	default:
		return fmt.Errorf("engine: invalid side %q", raw)
	}
	return nil
}

// OrderType distinguishes priced from unpriced orders.
type OrderType uint8

const (
	// Limit orders carry a limit price and rest on the book when unfilled.
	Limit OrderType = iota + 1
	// Market orders carry no price and never rest; any unfilled remainder is
	// canceled rather than left on the book at an undefined price.
	Market
)

// Valid reports whether t is a recognised order type.
func (t OrderType) Valid() bool { return t == Limit || t == Market }

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "limit"
	case Market:
		return "market"
	default:
		return "invalid"
	}
}

// MarshalJSON encodes the order type as a readable string.
func (t OrderType) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// UnmarshalJSON decodes the string form written by MarshalJSON.
func (t *OrderType) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	switch raw {
	case "limit":
		*t = Limit
	case "market":
		*t = Market
	default:
		return fmt.Errorf("engine: invalid order type %q", raw)
	}
	return nil
}

// Order is a single instruction resting on, or passing through, the book.
//
// PrioritySequence is the exchange sequence number assigned when the market
// accepted the order, and it is the only tiebreaker used within a price level.
// Wall-clock timestamps are deliberately absent from this struct so that they
// cannot accidentally influence priority.
type Order struct {
	ID               OrderID
	ParticipantID    string
	Symbol           string
	Side             Side
	Type             OrderType
	LimitPrice       PriceTicks
	OriginalQty      int64
	RemainingQty     int64
	FilledQty        int64
	CanceledQty      int64
	PrioritySequence uint64
}

// NewOrder builds an order with quantities initialised consistently.
func NewOrder(id OrderID, seq uint64, participant, symbol string, side Side, typ OrderType, limit PriceTicks, qty int64) *Order {
	if typ == Market {
		limit = 0
	}
	return &Order{
		ID:               id,
		ParticipantID:    participant,
		Symbol:           symbol,
		Side:             side,
		Type:             typ,
		LimitPrice:       limit,
		OriginalQty:      qty,
		RemainingQty:     qty,
		PrioritySequence: seq,
	}
}

func (o *Order) fill(qty int64) {
	o.RemainingQty -= qty
	o.FilledQty += qty
}

func (o *Order) cancelRemaining() {
	o.CanceledQty += o.RemainingQty
	o.RemainingQty = 0
}

// Conserved reports whether the order's quantity accounting balances:
// OriginalQty == FilledQty + RemainingQty + CanceledQty.
func (o *Order) Conserved() bool {
	return o.OriginalQty == o.FilledQty+o.RemainingQty+o.CanceledQty
}

// View is an immutable snapshot of an order, safe to hand to other goroutines.
// The engine keeps mutating the *Order it owns, so events and API responses
// always carry a View rather than a pointer.
type View struct {
	ID               OrderID    `json:"id"`
	ParticipantID    string     `json:"participant"`
	Symbol           string     `json:"symbol"`
	Side             Side       `json:"side"`
	Type             OrderType  `json:"type"`
	LimitPrice       PriceTicks `json:"limit_price,omitempty"`
	OriginalQty      int64      `json:"original_qty"`
	RemainingQty     int64      `json:"remaining_qty"`
	FilledQty        int64      `json:"filled_qty"`
	CanceledQty      int64      `json:"canceled_qty"`
	PrioritySequence uint64     `json:"priority_sequence"`
}

// View copies the order into an immutable view.
func (o *Order) View() View {
	return View{
		ID:               o.ID,
		ParticipantID:    o.ParticipantID,
		Symbol:           o.Symbol,
		Side:             o.Side,
		Type:             o.Type,
		LimitPrice:       o.LimitPrice,
		OriginalQty:      o.OriginalQty,
		RemainingQty:     o.RemainingQty,
		FilledQty:        o.FilledQty,
		CanceledQty:      o.CanceledQty,
		PrioritySequence: o.PrioritySequence,
	}
}

// Trade is one execution between an aggressing order and a resting order.
//
// Price is always the resting (passive) order's limit price: the resting order
// published that price first and is entitled to it, so the aggressor receives
// any price improvement.
type Trade struct {
	ID            TradeID    `json:"id"`
	Symbol        string     `json:"symbol"`
	BuyOrderID    OrderID    `json:"buy_order_id"`
	SellOrderID   OrderID    `json:"sell_order_id"`
	Price         PriceTicks `json:"price"`
	Quantity      int64      `json:"quantity"`
	AggressorSide Side       `json:"aggressor_side"`
	Sequence      uint64     `json:"sequence"`
}
