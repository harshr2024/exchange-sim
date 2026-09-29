package market

import (
	"sync"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
)

// EventKind identifies a market data event.
type EventKind string

const (
	OrderAccepted EventKind = "order_accepted"
	OrderRejected EventKind = "order_rejected"
	OrderRested   EventKind = "order_rested"
	OrderCanceled EventKind = "order_canceled"
	TradeExecuted EventKind = "trade_executed"
	BookChanged   EventKind = "book_changed"
)

// Event is one immutable market data message.
//
// Every field is a value or a pointer to a copy, never a pointer into the book.
// The market goroutine keeps mutating its own state after publishing, so an event
// that aliased that state would be a data race waiting to happen.
type Event struct {
	Kind   EventKind `json:"kind"`
	Symbol string    `json:"symbol"`
	// Sequence is the sequence of the command that produced this event. Several
	// events can share a sequence: one command can accept an order, print
	// several trades and change the book.
	Sequence uint64 `json:"sequence"`

	Order *engine.View  `json:"order,omitempty"`
	Trade *engine.Trade `json:"trade,omitempty"`
	// Update is set on BookChanged and carries the command's incremental market data.
	// It is a delta, not a snapshot: rebuilding published depth costs more than the
	// matching underneath it once a book is deep, so snapshots stay an occasional
	// synchronisation operation and this is the live path.
	Update *Update `json:"update,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// Sink receives market data events from the market goroutine.
//
// Publish must not block. The matching engine calls it inline, so a Sink that
// waits on a slow consumer would apply backpressure straight into matching and
// stall the whole market. Implementations are expected to hand off to a bounded
// buffer and drop or disconnect consumers that fall behind.
type Sink interface {
	Publish(Event)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Event)

// Publish calls f.
func (f SinkFunc) Publish(e Event) { f(e) }

type nopSink struct{}

func (nopSink) Publish(Event) {}

// DiscardSink drops every event. It is the default for replay, where nobody is
// listening and the events would only be rebuilt from the same commands anyway.
func DiscardSink() Sink { return nopSink{} }

// Recorder is a Sink that keeps every event it receives. It is meant for tests
// and the terminal tooling, and it is safe to read from another goroutine.
type Recorder struct {
	mu     sync.Mutex
	events []Event
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Publish appends the event. It never blocks on a consumer.
func (r *Recorder) Publish(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns a copy of everything recorded so far.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Count returns how many events were recorded, optionally filtered by kind.
func (r *Recorder) Count(kinds ...EventKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(kinds) == 0 {
		return len(r.events)
	}
	want := make(map[EventKind]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	n := 0
	for _, e := range r.events {
		if want[e.Kind] {
			n++
		}
	}
	return n
}
