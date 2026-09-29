// Package feed distributes market data to network subscribers.
//
// The hub sits between the market actor and the WebSocket connections. Its whole reason
// to exist is that the actor must never wait for a socket: a matching engine that
// blocked because one browser stopped reading would let the slowest subscriber set the
// pace of the exchange. So every subscriber gets a bounded queue, publication is
// non-blocking, and a subscriber that cannot keep up is disconnected rather than
// throttling anything upstream.
package feed

import (
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// MessageType identifies a wire message.
type MessageType string

const (
	// TypeSnapshot is the full book, sent once when a stream opens and after a resync.
	TypeSnapshot MessageType = "snapshot"
	// TypeUpdate is incremental market data for one command.
	TypeUpdate MessageType = "update"
	// TypeOrder is a lifecycle change to one of the subscriber's own orders.
	TypeOrder MessageType = "order"
	// TypeError ends the stream. Resync tells the client to refetch and reconnect.
	TypeError MessageType = "error"
)

// Message is one JSON frame on the market stream.
//
// Every message carries the market sequence it belongs to, so a client can order them
// against the snapshot it started from. Messages for a given market always leave the hub
// in sequence order, because they are published from the market's own goroutine.
type Message struct {
	Type     MessageType `json:"type"`
	Symbol   string      `json:"symbol"`
	Sequence uint64      `json:"seq"`

	// Book is set on TypeSnapshot.
	Book *engine.BookSnapshot `json:"book,omitempty"`
	// Orders is set on TypeSnapshot: the subscriber's own resting orders, authoritative
	// as of Sequence. On reconnect they replace whatever the client was holding.
	Orders []engine.View `json:"orders,omitempty"`
	// Update is set on TypeUpdate.
	Update *market.Update `json:"update,omitempty"`
	// Order and Event are set on TypeOrder.
	Order *engine.View `json:"order,omitempty"`
	Event string       `json:"event,omitempty"`
	// Reason and Resync are set on TypeError.
	Reason string `json:"reason,omitempty"`
	Resync bool   `json:"resync,omitempty"`
}

// SnapshotMessage wraps the initial synchronisation state.
//
// The book and the orders come from one read of the market, so they describe the same
// sequence. That is what lets a client discard every queued message at or below it without
// risking the current state of one of its own orders.
func SnapshotMessage(state market.SyncState) Message {
	book := state.Book
	return Message{
		Type:     TypeSnapshot,
		Symbol:   book.Symbol,
		Sequence: state.Sequence,
		Book:     &book,
		Orders:   state.Orders,
	}
}

// UpdateMessage wraps incremental market data.
func UpdateMessage(u market.Update) Message {
	return Message{Type: TypeUpdate, Symbol: u.Symbol, Sequence: u.Sequence, Update: &u}
}

// OrderMessage wraps an order lifecycle change.
func OrderMessage(symbol string, seq uint64, kind market.EventKind, order engine.View) Message {
	return Message{Type: TypeOrder, Symbol: symbol, Sequence: seq, Event: string(kind), Order: &order}
}

// ErrorMessage ends a stream. resync asks the client to refetch a snapshot and reconnect.
func ErrorMessage(symbol, reason string, resync bool) Message {
	return Message{Type: TypeError, Symbol: symbol, Reason: reason, Resync: resync}
}
