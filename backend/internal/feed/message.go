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
	// Update is set on TypeUpdate.
	Update *market.Update `json:"update,omitempty"`
	// Order and Event are set on TypeOrder.
	Order *engine.View `json:"order,omitempty"`
	Event string       `json:"event,omitempty"`
	// Reason and Resync are set on TypeError.
	Reason string `json:"reason,omitempty"`
	Resync bool   `json:"resync,omitempty"`
}

// SnapshotMessage wraps a book snapshot.
func SnapshotMessage(snap engine.BookSnapshot) Message {
	return Message{Type: TypeSnapshot, Symbol: snap.Symbol, Sequence: snap.Sequence, Book: &snap}
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
