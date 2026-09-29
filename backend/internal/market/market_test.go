package market

import (
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
)

func newTestMarket(t *testing.T, opts ...Option) *Market {
	t.Helper()
	m, err := New("NOVA", opts...)
	if err != nil {
		t.Fatalf("creating market: %v", err)
	}
	return m
}

func TestNewRejectsUnlistedSymbol(t *testing.T) {
	if _, err := New("TSLA"); !errors.Is(err, instrument.ErrUnknownSymbol) {
		t.Fatalf("New returned %v, want ErrUnknownSymbol", err)
	}
}

func TestMalformedCommandsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		cmd  Command
		want error
	}{
		{"zero quantity", Command{Kind: SubmitLimit, ParticipantID: "h", Side: engine.Buy, LimitPrice: 10000}, ErrInvalidQuantity},
		{"negative quantity", Command{Kind: SubmitLimit, ParticipantID: "h", Side: engine.Buy, LimitPrice: 10000, Quantity: -5}, ErrInvalidQuantity},
		{"zero price", Command{Kind: SubmitLimit, ParticipantID: "h", Side: engine.Buy, Quantity: 10}, ErrInvalidPrice},
		{"negative price", Command{Kind: SubmitLimit, ParticipantID: "h", Side: engine.Buy, LimitPrice: -1, Quantity: 10}, ErrInvalidPrice},
		{"missing side", Command{Kind: SubmitLimit, ParticipantID: "h", LimitPrice: 10000, Quantity: 10}, ErrInvalidSide},
		{"invalid side", Command{Kind: SubmitLimit, ParticipantID: "h", Side: engine.Side(9), LimitPrice: 10000, Quantity: 10}, ErrInvalidSide},
		{"missing participant", Command{Kind: SubmitLimit, Side: engine.Buy, LimitPrice: 10000, Quantity: 10}, ErrMissingParticipant},
		{"priced market order", Command{Kind: SubmitMarket, ParticipantID: "h", Side: engine.Buy, Quantity: 10, LimitPrice: 10000}, ErrPricedMarketOrder},
		{"market zero quantity", Command{Kind: SubmitMarket, ParticipantID: "h", Side: engine.Buy}, ErrInvalidQuantity},
		{"submit with order id", Command{Kind: SubmitLimit, ParticipantID: "h", Side: engine.Buy, LimitPrice: 10000, Quantity: 10, OrderID: 3}, ErrUnexpectedOrderID},
		{"cancel without order id", Command{Kind: Cancel, ParticipantID: "h"}, ErrMissingOrderID},
		{"cancel with quantity", Command{Kind: Cancel, ParticipantID: "h", OrderID: 1, Quantity: 10}, ErrUnexpectedField},
		{"unknown kind", Command{Kind: "submit_stop", ParticipantID: "h"}, ErrUnknownKind},
		{"empty kind", Command{ParticipantID: "h"}, ErrUnknownKind},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cmd.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate returned %v, want %v", err, tc.want)
			}

			rec := NewRecorder()
			m := newTestMarket(t, WithSink(rec))
			ack := m.Apply(tc.cmd)

			if ack.Accepted {
				t.Fatal("market accepted a malformed command")
			}
			if ack.Rejection == "" {
				t.Fatal("rejection carries no reason")
			}
			// A rejected command still occupies a sequence number, so that the
			// session log and the sequence stream stay in step.
			if ack.Sequence != 1 {
				t.Fatalf("rejected command got sequence %d, want 1", ack.Sequence)
			}
			if rec.Count(OrderRejected) != 1 {
				t.Fatalf("got %d rejection events, want 1", rec.Count(OrderRejected))
			}
			if rec.Count(BookChanged) != 0 {
				t.Fatal("a rejected command published a book change")
			}
		})
	}
}

func TestRejectedCommandDoesNotConsumeAnOrderID(t *testing.T) {
	m := newTestMarket(t)

	m.Apply(Command{Kind: SubmitLimit, ParticipantID: "human", Side: engine.Buy, Quantity: 0, LimitPrice: 10000})
	ack := m.Apply(LimitOrder("human", engine.Buy, 10000, 10))

	if ack.Sequence != 2 {
		t.Fatalf("second command got sequence %d, want 2", ack.Sequence)
	}
	// Order IDs are only minted for commands that pass validation, so the first
	// live order is always ID 1 regardless of how much junk preceded it.
	if ack.OrderID != 1 {
		t.Fatalf("first accepted order got id %d, want 1", ack.OrderID)
	}
}

func TestSequenceStrictlyIncreasesAcrossEveryCommand(t *testing.T) {
	m := newTestMarket(t)

	cmds := []Command{
		LimitOrder("human", engine.Buy, 9900, 10),
		Command{Kind: SubmitLimit, ParticipantID: "human", Side: engine.Buy}, // rejected
		MarketOrder("human", engine.Sell, 5),
		CancelOrder("human", 99), // rejected: no such order
		LimitOrder("human", engine.Sell, 10100, 10),
	}

	var prev uint64
	for i, cmd := range cmds {
		ack := m.Apply(cmd)
		if ack.Sequence != prev+1 {
			t.Fatalf("command %d got sequence %d, want %d", i, ack.Sequence, prev+1)
		}
		prev = ack.Sequence
	}
	if m.Sequence() != uint64(len(cmds)) {
		t.Fatalf("market sequence is %d after %d commands", m.Sequence(), len(cmds))
	}
}

func TestSubmitPublishesEventsInOrder(t *testing.T) {
	rec := NewRecorder()
	m := newTestMarket(t, WithSink(rec))

	m.Apply(LimitOrder("maker", engine.Sell, 10000, 40))
	before := len(rec.Events())

	// Fills 40 and rests 60.
	m.Apply(LimitOrder("taker", engine.Buy, 10000, 100))

	got := rec.Events()[before:]
	want := []EventKind{OrderAccepted, TradeExecuted, OrderRested, BookChanged}
	if len(got) != len(want) {
		t.Fatalf("published %d events, want %d: %v", len(got), len(want), kinds(got))
	}
	for i := range want {
		if got[i].Kind != want[i] {
			t.Fatalf("event %d is %s, want %s (all: %v)", i, got[i].Kind, want[i], kinds(got))
		}
		if got[i].Sequence != 2 {
			t.Fatalf("event %d carries sequence %d, want 2", i, got[i].Sequence)
		}
		if got[i].Symbol != "NOVA" {
			t.Fatalf("event %d carries symbol %q, want NOVA", i, got[i].Symbol)
		}
	}
	// book_changed carries an incremental update, not a full snapshot: the ask level
	// drained and a new bid level appeared for the remainder.
	update := got[3].Update
	if update == nil {
		t.Fatal("book_changed carries no update")
	}
	if update.Sequence != 2 || update.Symbol != "NOVA" {
		t.Fatalf("update is for %s at sequence %d, want NOVA at 2", update.Symbol, update.Sequence)
	}
	wantLevels := []engine.LevelChange{
		{Side: engine.Buy, Price: 10000, Quantity: 60, Orders: 1},
		{Side: engine.Sell, Price: 10000, Quantity: 0, Orders: 0},
	}
	if !reflect.DeepEqual(update.Levels, wantLevels) {
		t.Fatalf("update levels are %+v, want %+v", update.Levels, wantLevels)
	}
	if len(update.Trades) != 1 || update.Trades[0].Quantity != 40 {
		t.Fatalf("update carries trades %+v, want one fill of 40", update.Trades)
	}
	if !update.HasLastTrade || update.LastTradePrice != 10000 {
		t.Fatalf("update reports last trade (%d, %v), want (10000, true)", update.LastTradePrice, update.HasLastTrade)
	}
}

// TestUpdatesAreNotBuiltWhenNobodyIsListening keeps replay from paying for market data
// it discards. A long session log is replayed command by command, and building a delta
// per command would be pure overhead.
func TestUpdatesAreNotBuiltWhenNobodyIsListening(t *testing.T) {
	m := newTestMarket(t) // no sink
	m.Apply(LimitOrder("maker", engine.Sell, 10000, 40))

	// The engine's change record must have been dropped rather than left to grow.
	if got := m.book.TakeLevelChanges(); len(got) != 0 {
		t.Fatalf("level changes accumulated with no sink attached: %+v", got)
	}
}

func TestUnfilledMarketOrderPublishesCancellation(t *testing.T) {
	rec := NewRecorder()
	m := newTestMarket(t, WithSink(rec))

	ack := m.Apply(MarketOrder("taker", engine.Buy, 25))

	if !ack.Accepted {
		t.Fatalf("market order against an empty book was rejected: %s", ack.Rejection)
	}
	if ack.CanceledQty != 25 || ack.Rested {
		t.Fatalf("ack reports canceled=%d rested=%v, want 25 and false", ack.CanceledQty, ack.Rested)
	}
	if rec.Count(OrderCanceled) != 1 {
		t.Fatalf("got %d cancellation events, want 1", rec.Count(OrderCanceled))
	}
	if rec.Count(OrderRested) != 0 {
		t.Fatal("market order published a rested event")
	}
}

func TestCancelByAnotherParticipantIsRejected(t *testing.T) {
	m := newTestMarket(t)

	ack := m.Apply(LimitOrder("bot-mm", engine.Buy, 9900, 10))
	if !ack.Accepted {
		t.Fatalf("submitting the bot quote: %s", ack.Rejection)
	}

	stolen := m.Apply(CancelOrder("human", ack.OrderID))
	if stolen.Accepted {
		t.Fatal("one participant canceled another participant's order")
	}
	if !strings.Contains(stolen.Rejection, "another participant") {
		t.Fatalf("rejection reads %q, want it to mention ownership", stolen.Rejection)
	}
	if len(m.OpenOrders("bot-mm")) != 1 {
		t.Fatal("the bot's order left the book")
	}

	owned := m.Apply(CancelOrder("bot-mm", ack.OrderID))
	if !owned.Accepted {
		t.Fatalf("the owner could not cancel its own order: %s", owned.Rejection)
	}
}

func TestMarketHaltsWhenItCannotRecordACommand(t *testing.T) {
	boom := errors.New("disk is on fire")
	m := newTestMarket(t, WithJournal(failingJournal{err: boom}))

	ack := m.Apply(LimitOrder("human", engine.Buy, 9900, 10))

	if ack.Accepted {
		t.Fatal("market executed a command it could not record")
	}
	// The sequence must not advance: the log that was written has to remain an
	// exact description of the state the market reached.
	if ack.Sequence != 0 || m.Sequence() != 0 {
		t.Fatalf("sequence advanced to %d despite the failed write", m.Sequence())
	}
	if !errors.Is(m.Halted(), boom) {
		t.Fatalf("market reports halted=%v, want the write error", m.Halted())
	}
	if next := m.Apply(LimitOrder("human", engine.Buy, 9900, 10)); next.Accepted {
		t.Fatal("halted market accepted a later command")
	}
}

func TestSnapshotAndTradesReflectActivity(t *testing.T) {
	m := newTestMarket(t)

	m.Apply(LimitOrder("maker", engine.Sell, 10010, 30))
	m.Apply(LimitOrder("maker", engine.Sell, 10020, 50))
	m.Apply(LimitOrder("maker", engine.Buy, 9990, 40))
	m.Apply(MarketOrder("taker", engine.Buy, 30))

	snap := m.Snapshot()
	if snap.Symbol != "NOVA" || snap.Sequence != 4 {
		t.Fatalf("snapshot is for %s at sequence %d, want NOVA at 4", snap.Symbol, snap.Sequence)
	}
	if !snap.HasLastTrade || snap.LastTradePrice != 10010 {
		t.Fatalf("last traded price is (%d, %v), want (10010, true)", snap.LastTradePrice, snap.HasLastTrade)
	}
	if snap.BestBid != 9990 || snap.BestAsk != 10020 || snap.Spread != 30 {
		t.Fatalf("top of book is %d / %d spread %d, want 9990 / 10020 spread 30",
			snap.BestBid, snap.BestAsk, snap.Spread)
	}
	if got := m.RecentTrades(0); len(got) != 1 || got[0].Quantity != 30 {
		t.Fatalf("recent trades are %+v, want one trade of 30", got)
	}
	if err := m.CheckInvariants(); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
}

func TestReferencePriceNeverBecomesTheLastPrice(t *testing.T) {
	m := newTestMarket(t)

	if ref := m.Instrument().InitialReferencePrice; ref == 0 {
		t.Fatal("NOVA has no reference price to check against")
	}
	// Quotes around the reference price are not trades, so nothing may print.
	m.Apply(LimitOrder("bot-mm", engine.Buy, 9998, 100))
	m.Apply(LimitOrder("bot-mm", engine.Sell, 10002, 100))

	if snap := m.Snapshot(); snap.HasLastTrade {
		t.Fatalf("last price is %d before any trade happened", snap.LastTradePrice)
	}
	// It only appears once liquidity is actually consumed.
	m.Apply(MarketOrder("taker", engine.Buy, 10))
	if snap := m.Snapshot(); !snap.HasLastTrade || snap.LastTradePrice != 10002 {
		t.Fatalf("last price is (%d, %v) after a trade at 10002", snap.LastTradePrice, snap.HasLastTrade)
	}
}

func TestInvariantsHoldAcrossRandomisedFlow(t *testing.T) {
	m := newTestMarket(t)
	rng := rand.New(rand.NewSource(99))

	for i := 0; i < 3000; i++ {
		ack := m.Apply(randomCommand(rng, m))
		if err := m.CheckInvariants(); err != nil {
			t.Fatalf("invariant violation after sequence %d: %v", ack.Sequence, err)
		}
	}
	if m.TradeCount() == 0 {
		t.Fatal("randomised flow produced no trades")
	}
}

// randomCommand builds a deterministic pseudo-random command. It reads the
// market's resting orders to pick cancel targets, so it must only be used on the
// market's own goroutine.
func randomCommand(rng *rand.Rand, m *Market) Command {
	side := engine.Buy
	if rng.Intn(2) == 0 {
		side = engine.Sell
	}
	qty := int64(1 + rng.Intn(50))

	switch n := rng.Intn(100); {
	case n < 20:
		// Cancel something, sometimes deliberately aiming at an ID that is gone.
		open := m.OpenOrders("noise")
		if len(open) == 0 {
			return CancelOrder("noise", engine.OrderID(1+rng.Intn(50)))
		}
		return CancelOrder("noise", open[rng.Intn(len(open))].ID)
	case n < 35:
		return MarketOrder("noise", side, qty)
	default:
		return LimitOrder("noise", side, engine.PriceTicks(10000+rng.Intn(41)-20), qty)
	}
}

// randomCommandStream builds a command stream without consulting any market, so
// the same stream can be fed to several markets.
func randomCommandStream(seed int64, n int) []Command {
	rng := rand.New(rand.NewSource(seed))
	out := make([]Command, 0, n)
	var issued int

	for i := 0; i < n; i++ {
		side := engine.Buy
		if rng.Intn(2) == 0 {
			side = engine.Sell
		}
		qty := int64(1 + rng.Intn(50))

		switch k := rng.Intn(100); {
		case k < 20 && issued > 0:
			// Order IDs are minted in order, so guessing within the issued range
			// hits live orders often and already-gone ones the rest of the time.
			out = append(out, CancelOrder("noise", engine.OrderID(1+rng.Intn(issued))))
		case k < 35:
			out = append(out, MarketOrder("noise", side, qty))
			issued++
		case k < 40:
			// Some deliberate junk, so replay has to reproduce rejections too.
			out = append(out, Command{Kind: SubmitLimit, ParticipantID: "noise", Side: side, Quantity: -1})
		default:
			out = append(out, LimitOrder("noise", side, engine.PriceTicks(10000+rng.Intn(41)-20), qty))
			issued++
		}
	}
	return out
}

type failingJournal struct{ err error }

func (f failingJournal) Append(Record) error { return f.err }
func (f failingJournal) Close() error        { return nil }

func kinds(events []Event) []EventKind {
	out := make([]EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}
