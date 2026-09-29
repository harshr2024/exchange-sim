// Package market owns the exchange's write path.
//
// A Market is a single-threaded state machine: it sequences commands, records
// them, drives the matching engine and publishes the resulting events. An Actor
// wraps one Market in one goroutine, which is the only goroutine allowed to touch
// that market's matching state. Concurrency lives around the matching engine
// rather than inside it, so a given command stream always produces the same
// trades and the same final book.
package market

import (
	"errors"
	"fmt"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
)

// Kind identifies a command.
type Kind string

const (
	// SubmitLimit places a priced order that rests when it cannot fully fill.
	SubmitLimit Kind = "submit_limit"
	// SubmitMarket places an unpriced order that never rests.
	SubmitMarket Kind = "submit_market"
	// Cancel withdraws a resting order.
	Cancel Kind = "cancel"
)

// Command is a normalised instruction for one market. It is the unit that gets
// sequenced, recorded to the session log and replayed.
//
// Commands carry no symbol: routing to a market happens before sequencing, and
// each market records to its own log file. They also carry no timestamp, because
// nothing about matching may depend on wall-clock time.
type Command struct {
	Kind          Kind              `json:"kind"`
	ParticipantID string            `json:"participant"`
	Side          engine.Side       `json:"side,omitempty"`
	Quantity      int64             `json:"qty,omitempty"`
	LimitPrice    engine.PriceTicks `json:"price,omitempty"`
	OrderID       engine.OrderID    `json:"order_id,omitempty"`
}

// Validation failures. These are deterministic functions of the command alone,
// which is what lets a replay reject exactly what the live session rejected.
var (
	ErrUnknownKind        = errors.New("unknown command kind")
	ErrMissingParticipant = errors.New("participant is required")
	ErrInvalidSide        = errors.New("side must be buy or sell")
	ErrInvalidQuantity    = errors.New("quantity must be positive")
	ErrInvalidPrice       = errors.New("limit price must be positive")
	ErrPricedMarketOrder  = errors.New("market order must not carry a limit price")
	ErrMissingOrderID     = errors.New("order id is required")
	ErrUnexpectedOrderID  = errors.New("submit must not carry an order id")
	ErrUnexpectedField    = errors.New("cancel must not carry side, quantity or price")
	ErrNotOrderOwner      = errors.New("order belongs to another participant")
)

// Validate reports whether the command is well formed.
func (c Command) Validate() error {
	if c.ParticipantID == "" {
		return ErrMissingParticipant
	}

	switch c.Kind {
	case SubmitLimit:
		if !c.Side.Valid() {
			return ErrInvalidSide
		}
		if c.Quantity <= 0 {
			return ErrInvalidQuantity
		}
		if c.LimitPrice <= 0 {
			return ErrInvalidPrice
		}
		if c.OrderID != 0 {
			return ErrUnexpectedOrderID
		}
	case SubmitMarket:
		if !c.Side.Valid() {
			return ErrInvalidSide
		}
		if c.Quantity <= 0 {
			return ErrInvalidQuantity
		}
		// A market order with a price would be neither one thing nor the other:
		// the engine ignores the price, so accepting it would silently do
		// something the caller did not ask for.
		if c.LimitPrice != 0 {
			return ErrPricedMarketOrder
		}
		if c.OrderID != 0 {
			return ErrUnexpectedOrderID
		}
	case Cancel:
		if c.OrderID == 0 {
			return ErrMissingOrderID
		}
		if c.Side != 0 || c.Quantity != 0 || c.LimitPrice != 0 {
			return ErrUnexpectedField
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnknownKind, c.Kind)
	}
	return nil
}

// LimitOrder builds a limit command.
func LimitOrder(participant string, side engine.Side, price engine.PriceTicks, qty int64) Command {
	return Command{Kind: SubmitLimit, ParticipantID: participant, Side: side, LimitPrice: price, Quantity: qty}
}

// MarketOrder builds a market command.
func MarketOrder(participant string, side engine.Side, qty int64) Command {
	return Command{Kind: SubmitMarket, ParticipantID: participant, Side: side, Quantity: qty}
}

// CancelOrder builds a cancel command.
func CancelOrder(participant string, id engine.OrderID) Command {
	return Command{Kind: Cancel, ParticipantID: participant, OrderID: id}
}

// Ack is the synchronous answer to a submitted command.
type Ack struct {
	// Sequence is the exchange sequence number the command was given, which is
	// what establishes its priority. It is 0 when the command was never
	// sequenced at all.
	Sequence uint64 `json:"sequence"`
	// Accepted is false when the command was sequenced but rejected.
	Accepted bool `json:"accepted"`
	// Rejection explains a refusal, and is empty when Accepted is true.
	Rejection string `json:"rejection,omitempty"`
	// Err is the same refusal as a value, so callers can classify it with errors.Is
	// instead of matching on the message. The API layer uses it to choose a status
	// code. Not serialised: Rejection is the wire form.
	Err error `json:"-"`

	OrderID     engine.OrderID `json:"order_id,omitempty"`
	Order       *engine.View   `json:"order,omitempty"`
	Trades      []engine.Trade `json:"trades,omitempty"`
	Rested      bool           `json:"rested"`
	CanceledQty int64          `json:"canceled_qty,omitempty"`
}
