package market

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
)

// ErrHalted is reported once a market has stopped accepting commands because it
// could no longer record them.
var ErrHalted = errors.New("market is halted")

// Market is the sequencing and matching state machine for one symbol.
//
// A Market is not safe for concurrent use. Use an Actor to own one.
type Market struct {
	instrument instrument.Instrument
	book       *engine.Book
	journal    Journal
	sink       Sink
	depth      int

	// seq is the exchange sequence counter. It is the sole source of order
	// priority; no wall-clock time is involved.
	seq uint64
	// nextOrderID allocates order IDs from a counter rather than from anything
	// the client supplies, so a replay of the same commands mints the same IDs
	// and later cancel commands still refer to the right orders.
	nextOrderID uint64

	trades []engine.Trade
	halted error
}

// Option configures a Market.
type Option func(*Market)

// WithJournal records every sequenced command to j.
func WithJournal(j Journal) Option { return func(m *Market) { m.journal = j } }

// WithSink publishes market data events to s.
func WithSink(s Sink) Option { return func(m *Market) { m.sink = s } }

// WithDepth sets how many price levels per side published snapshots carry.
func WithDepth(levels int) Option { return func(m *Market) { m.depth = levels } }

// New returns an empty market for a listed symbol.
func New(symbol string, opts ...Option) (*Market, error) {
	inst, err := instrument.Lookup(symbol)
	if err != nil {
		return nil, err
	}
	m := &Market{
		instrument: inst,
		book:       engine.NewBook(inst.Symbol),
		sink:       DiscardSink(),
		depth:      engine.DefaultDepth,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Symbol returns the market's symbol.
func (m *Market) Symbol() string { return m.instrument.Symbol }

// Instrument returns the security this market trades.
func (m *Market) Instrument() instrument.Instrument { return m.instrument }

// Sequence returns the highest sequence number assigned so far.
func (m *Market) Sequence() uint64 { return m.seq }

// Apply sequences, records and executes one command.
//
// Sequencing happens here and nowhere else. Whichever command reaches this method
// first gets the lower sequence number and therefore the better priority, which
// is why two requests racing over the network cannot produce an ambiguous
// outcome: the race is resolved by arrival at the market, not by a timestamp
// either side generated.
func (m *Market) Apply(cmd Command) Ack {
	if m.halted != nil {
		return Ack{Sequence: m.seq, Rejection: fmt.Sprintf("%v: %v", ErrHalted, m.halted)}
	}

	// Record before executing, and only consume the sequence number once the
	// record is durable enough to replay. If the write fails the market halts
	// without advancing, so the log that did get written still replays to exactly
	// the state the live market stopped in.
	next := m.seq + 1
	if m.journal != nil {
		if err := m.journal.Append(Record{Sequence: next, Command: cmd}); err != nil {
			m.halted = err
			return Ack{Sequence: m.seq, Rejection: fmt.Sprintf("%v: %v", ErrHalted, err)}
		}
	}
	m.seq = next

	if err := cmd.Validate(); err != nil {
		m.reject(next, err)
		return Ack{Sequence: next, Rejection: err.Error()}
	}

	if cmd.Kind == Cancel {
		return m.applyCancel(next, cmd)
	}
	return m.applySubmit(next, cmd)
}

func (m *Market) applySubmit(seq uint64, cmd Command) Ack {
	typ := engine.Limit
	if cmd.Kind == SubmitMarket {
		typ = engine.Market
	}

	m.nextOrderID++
	order := engine.NewOrder(
		engine.OrderID(m.nextOrderID), seq,
		cmd.ParticipantID, m.instrument.Symbol,
		cmd.Side, typ, cmd.LimitPrice, cmd.Quantity,
	)

	res := m.book.Submit(order)
	m.trades = append(m.trades, res.Trades...)

	accepted := res.Order
	m.publish(Event{Kind: OrderAccepted, Sequence: seq, Order: &accepted})
	for i := range res.Trades {
		trade := res.Trades[i]
		m.publish(Event{Kind: TradeExecuted, Sequence: seq, Trade: &trade})
	}
	switch {
	case res.Rested:
		m.publish(Event{Kind: OrderRested, Sequence: seq, Order: &accepted})
	case res.CanceledQty > 0:
		m.publish(Event{
			Kind: OrderCanceled, Sequence: seq, Order: &accepted,
			Reason: "market order exhausted the opposite side",
		})
	}
	m.publishBook(seq)

	return Ack{
		Sequence:    seq,
		Accepted:    true,
		OrderID:     res.Order.ID,
		Order:       &accepted,
		Trades:      res.Trades,
		Rested:      res.Rested,
		CanceledQty: res.CanceledQty,
	}
}

func (m *Market) applyCancel(seq uint64, cmd Command) Ack {
	// Ownership is checked before the book is touched: one participant must not
	// be able to pull another's quotes.
	if existing, ok := m.book.Order(cmd.OrderID); ok && existing.ParticipantID != cmd.ParticipantID {
		m.reject(seq, ErrNotOrderOwner)
		return Ack{Sequence: seq, Rejection: ErrNotOrderOwner.Error()}
	}

	view, err := m.book.Cancel(cmd.OrderID)
	if err != nil {
		m.reject(seq, err)
		return Ack{Sequence: seq, Rejection: err.Error()}
	}

	m.publish(Event{Kind: OrderCanceled, Sequence: seq, Order: &view, Reason: "canceled by participant"})
	m.publishBook(seq)

	return Ack{
		Sequence:    seq,
		Accepted:    true,
		OrderID:     view.ID,
		Order:       &view,
		CanceledQty: view.CanceledQty,
	}
}

func (m *Market) reject(seq uint64, err error) {
	m.publish(Event{Kind: OrderRejected, Sequence: seq, Reason: err.Error()})
}

func (m *Market) publish(e Event) {
	e.Symbol = m.instrument.Symbol
	m.sink.Publish(e)
}

func (m *Market) publishBook(seq uint64) {
	snap := m.book.Snapshot(m.depth)
	snap.Sequence = seq
	m.publish(Event{Kind: BookChanged, Sequence: seq, Book: &snap})
}

// Snapshot returns the published depth plus the derived quotes.
func (m *Market) Snapshot() engine.BookSnapshot {
	snap := m.book.Snapshot(m.depth)
	snap.Sequence = m.seq
	return snap
}

// OpenOrders returns the resting orders belonging to participant. An empty
// participant returns every resting order.
func (m *Market) OpenOrders(participant string) []engine.View {
	return m.book.OpenOrders(participant)
}

// RecentTrades returns the last n trades, oldest first. A n of 0 or less returns
// the whole session.
func (m *Market) RecentTrades(n int) []engine.Trade {
	if n > 0 && len(m.trades) > n {
		return append([]engine.Trade(nil), m.trades[len(m.trades)-n:]...)
	}
	return append([]engine.Trade(nil), m.trades...)
}

// TradeCount returns how many trades the session has printed.
func (m *Market) TradeCount() int { return len(m.trades) }

// Halted reports why the market stopped accepting commands, or nil.
func (m *Market) Halted() error { return m.halted }

// CheckInvariants verifies the book's structural invariants plus the market's own
// sequencing invariants.
func (m *Market) CheckInvariants() error {
	problems := []error{m.book.CheckInvariants()}

	// Trade sequences must be non-decreasing and must never run ahead of the
	// market, and trade IDs must be dense and strictly increasing.
	var prevSeq uint64
	for i, t := range m.trades {
		if t.Sequence < prevSeq {
			problems = append(problems, fmt.Errorf("trade %d has sequence %d after sequence %d", t.ID, t.Sequence, prevSeq))
		}
		if t.Sequence > m.seq {
			problems = append(problems, fmt.Errorf("trade %d has sequence %d beyond the market's %d", t.ID, t.Sequence, m.seq))
		}
		if t.ID != engine.TradeID(i+1) {
			problems = append(problems, fmt.Errorf("trade at position %d has id %d", i, t.ID))
		}
		prevSeq = t.Sequence
	}
	return errors.Join(problems...)
}

// CanonicalState renders the market's full state as stable text: the sequence
// counters, every price level, every resting order and every trade of the session.
func (m *Market) CanonicalState() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "market %s\n", m.instrument.Symbol)
	fmt.Fprintf(&sb, "sequence %d\n", m.seq)
	fmt.Fprintf(&sb, "orders-issued %d\n", m.nextOrderID)
	m.book.WriteCanonical(&sb)
	for _, t := range m.trades {
		engine.WriteCanonicalTrade(&sb, t)
	}
	return sb.String()
}

// Digest is a SHA-256 of CanonicalState.
//
// Comparing digests is how the replay test asserts that a fresh market fed the
// recorded command stream ended up in exactly the same state as the original,
// down to individual order priorities and the whole trade history.
func (m *Market) Digest() string {
	sum := sha256.Sum256([]byte(m.CanonicalState()))
	return hex.EncodeToString(sum[:])
}
