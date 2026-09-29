package api

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/feed"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// orderMessages reads until n order messages have arrived, ignoring everything else.
func (s *stream) orderMessages(n int) []feed.Message {
	s.t.Helper()
	var out []feed.Message
	for range 400 {
		msg := s.next()
		if msg.Type != feed.TypeOrder {
			continue
		}
		out = append(out, msg)
		if len(out) == n {
			return out
		}
	}
	s.t.Fatalf("only %d of %d order messages arrived", len(out), n)
	return nil
}

// TestPassiveHumanOrderPartiallyFilledByABot is the correction's whole point: the human is
// the passive side, so nothing it submitted caused this execution, and without a lifecycle
// event its only evidence would be a trade carrying its order ID.
func TestPassiveHumanOrderPartiallyFilledByABot(t *testing.T) {
	r := newDefaultRig(t)

	mine := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":100}`)

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	// A bot hits 40 of the human's resting bid.
	r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Sell, 40))

	msg := s.orderMessages(1)[0]
	if msg.Event != string(market.OrderFilled) {
		t.Fatalf("event is %q, want %q", msg.Event, market.OrderFilled)
	}

	order := msg.Order
	if order.ID != mine.OrderID {
		t.Fatalf("event is for order %d, want %d", order.ID, mine.OrderID)
	}
	// Everything needed to render the order, with no reference to the trade tape.
	if order.Symbol != "NOVA" || order.Side != engine.Buy || order.Type != engine.Limit {
		t.Fatalf("order lacks its identifying fields: %+v", order)
	}
	if order.LimitPrice != 9990 || order.OriginalQty != 100 {
		t.Fatalf("order is %+v, want limit 9990 for 100", order)
	}
	if order.FilledQty != 40 || order.RemainingQty != 60 {
		t.Fatalf("order reports filled=%d leaves=%d, want 40 and 60", order.FilledQty, order.RemainingQty)
	}
	if order.Status != engine.StatusPartiallyFilled {
		t.Fatalf("order status is %q, want %q", order.Status, engine.StatusPartiallyFilled)
	}
	if msg.Sequence == 0 {
		t.Fatal("the lifecycle event carries no market sequence")
	}

	// And it agrees with the authoritative REST view.
	open := r.openOrders("NOVA")
	if len(open) != 1 || open[0].FilledQty != 40 || open[0].RemainingQty != 60 {
		t.Fatalf("REST open orders are %+v, want the streamed state", open)
	}
}

func TestPassiveHumanOrderFullyFilledByABot(t *testing.T) {
	r := newDefaultRig(t)

	mine := r.submit("NOVA", `{"side":"sell","type":"limit","price":10010,"quantity":50}`)

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Buy, 50))

	order := s.orderMessages(1)[0].Order
	if order.ID != mine.OrderID {
		t.Fatalf("event is for order %d, want %d", order.ID, mine.OrderID)
	}
	if order.FilledQty != 50 || order.RemainingQty != 0 {
		t.Fatalf("order reports filled=%d leaves=%d, want 50 and 0", order.FilledQty, order.RemainingQty)
	}
	if order.Status != engine.StatusFilled {
		t.Fatalf("order status is %q, want %q", order.Status, engine.StatusFilled)
	}
	if !order.Status.Terminal() {
		t.Fatal("a fully filled order is not reported as terminal")
	}
	if open := r.openOrders("NOVA"); len(open) != 0 {
		t.Fatalf("a fully filled order is still open: %+v", open)
	}
}

// TestRepeatedPartialFillsAccumulate checks that filled and leaves are cumulative across
// executions rather than per-execution, which is what lets a client treat each event as the
// order's whole current state.
func TestRepeatedPartialFillsAccumulate(t *testing.T) {
	r := newDefaultRig(t)

	mine := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":100}`)

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	// Four separate bot sells chew through it: 10, 20, 30, then 40 to finish.
	for _, qty := range []int64{10, 20, 30, 40} {
		r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Sell, qty))
	}

	msgs := s.orderMessages(4)

	wantFilled := []int64{10, 30, 60, 100}
	wantLeaves := []int64{90, 70, 40, 0}
	wantStatus := []engine.OrderStatus{
		engine.StatusPartiallyFilled,
		engine.StatusPartiallyFilled,
		engine.StatusPartiallyFilled,
		engine.StatusFilled,
	}

	var previousSeq uint64
	for i, msg := range msgs {
		order := msg.Order
		if order.ID != mine.OrderID {
			t.Fatalf("event %d is for order %d, want %d", i, order.ID, mine.OrderID)
		}
		if order.FilledQty != wantFilled[i] {
			t.Fatalf("event %d reports cumulative filled %d, want %d", i, order.FilledQty, wantFilled[i])
		}
		if order.RemainingQty != wantLeaves[i] {
			t.Fatalf("event %d reports leaves %d, want %d", i, order.RemainingQty, wantLeaves[i])
		}
		if order.Status != wantStatus[i] {
			t.Fatalf("event %d reports status %q, want %q", i, order.Status, wantStatus[i])
		}
		// Quantities must always balance, event by event.
		if order.FilledQty+order.RemainingQty+order.CanceledQty != order.OriginalQty {
			t.Fatalf("event %d does not conserve quantity: %+v", i, order)
		}
		if msg.Sequence <= previousSeq {
			t.Fatalf("event %d carries sequence %d, not after %d", i, msg.Sequence, previousSeq)
		}
		previousSeq = msg.Sequence
	}
}

// TestBotPassiveFillsStayOffTheHumanStream is the containment half. Adding passive lifecycle
// events must not start leaking the bots' order churn, which on a live market is most of
// the traffic and none of the client's business.
func TestBotPassiveFillsStayOffTheHumanStream(t *testing.T) {
	r := newDefaultRig(t)

	// Both participants rest on opposite sides.
	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 100))
	mine := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":100}`)

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	// A second bot fills the first bot's resting ask. The human is not involved.
	r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Buy, 30))
	// Then something that does involve the human, as a marker to read up to.
	r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Sell, 25))

	// The first order message must be the human's, never the bot's.
	msg := s.orderMessages(1)[0]
	if msg.Order.ParticipantID != DefaultParticipant {
		t.Fatalf("the stream carried %s's order lifecycle", msg.Order.ParticipantID)
	}
	if msg.Order.ID != mine.OrderID {
		t.Fatalf("event is for order %d, want the human's %d", msg.Order.ID, mine.OrderID)
	}
	if msg.Order.FilledQty != 25 {
		t.Fatalf("human order reports filled %d, want 25", msg.Order.FilledQty)
	}

	// The bot's fill did reach the book feed, so it was not simply lost.
	// (Both trades are public; only the order lifecycle is private.)
	trades := r.expectStatus(http.MethodGet, "/api/markets/NOVA/trades", "", http.StatusOK)
	var executed []engine.Trade
	decodeInto(t, trades, &executed)
	if len(executed) != 2 {
		t.Fatalf("got %d trades, want both the bot-to-bot and bot-to-human executions", len(executed))
	}
}

// TestCancellationAfterAPartialFill covers the state a client is most likely to get wrong:
// an order that traded and was then withdrawn is canceled, not filled, and its filled
// quantity survives.
func TestCancellationAfterAPartialFill(t *testing.T) {
	r := newDefaultRig(t)

	mine := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":100}`)

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Sell, 35))
	filled := s.orderMessages(1)[0].Order
	if filled.FilledQty != 35 || filled.Status != engine.StatusPartiallyFilled {
		t.Fatalf("after the partial fill the order is %+v", filled)
	}

	r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+itoa(uint64(mine.OrderID)), "", http.StatusOK)

	canceled := s.orderMessages(1)[0]
	if canceled.Event != string(market.OrderCanceled) {
		t.Fatalf("event is %q, want %q", canceled.Event, market.OrderCanceled)
	}
	order := canceled.Order
	if order.Status != engine.StatusCanceled {
		t.Fatalf("order status is %q, want %q", order.Status, engine.StatusCanceled)
	}
	// The executed quantity is not undone by the cancellation.
	if order.FilledQty != 35 {
		t.Fatalf("canceled order reports filled %d, want the 35 that had already traded", order.FilledQty)
	}
	if order.CanceledQty != 65 || order.RemainingQty != 0 {
		t.Fatalf("canceled order reports canceled=%d leaves=%d, want 65 and 0",
			order.CanceledQty, order.RemainingQty)
	}
	if order.FilledQty+order.CanceledQty != order.OriginalQty {
		t.Fatalf("canceled order does not conserve quantity: %+v", order)
	}
}

// TestInitialSyncCarriesAuthoritativeOpenOrders covers reconnection: the opening message has
// to be enough on its own, so a client can throw away whatever it had.
func TestInitialSyncCarriesAuthoritativeOpenOrders(t *testing.T) {
	r := newDefaultRig(t)

	// The order meant to stay untouched sits at the worse price, because a seller takes
	// the best bid first: a bot selling into 9980 would hit a 9990 bid on the way.
	resting := r.submit("NOVA", `{"side":"buy","type":"limit","price":9980,"quantity":100}`)
	partial := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":80}`)
	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 9990, 30))

	// Noise that must not appear: a bot's resting order, and a human order that is gone.
	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Buy, 9970, 50))
	gone := r.submit("NOVA", `{"side":"buy","type":"limit","price":9960,"quantity":10}`)
	r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+itoa(uint64(gone.OrderID)), "", http.StatusOK)

	s := r.dial("NOVA")
	snap := s.nextOf(feed.TypeSnapshot)

	if snap.Book == nil {
		t.Fatal("the synchronisation message carries no book")
	}
	if len(snap.Orders) != 2 {
		t.Fatalf("synchronisation carries %d orders, want the human's 2: %+v", len(snap.Orders), snap.Orders)
	}

	byID := map[engine.OrderID]engine.View{}
	for _, o := range snap.Orders {
		if o.ParticipantID != DefaultParticipant {
			t.Fatalf("synchronisation leaked %s's order", o.ParticipantID)
		}
		byID[o.ID] = o
	}

	if got := byID[resting.OrderID]; got.Status != engine.StatusOpen || got.RemainingQty != 100 {
		t.Fatalf("untouched order is %+v, want open with 100 leaves", got)
	}
	if got := byID[partial.OrderID]; got.Status != engine.StatusPartiallyFilled || got.FilledQty != 30 || got.RemainingQty != 50 {
		t.Fatalf("partly filled order is %+v, want partially_filled with 30 done and 50 left", got)
	}
	if _, ok := byID[gone.OrderID]; ok {
		t.Fatal("synchronisation included an order that had been canceled")
	}

	// The synchronised state matches the REST view exactly, and is stamped with the same
	// sequence as the book it arrived with.
	if snap.Sequence != snap.Book.Sequence {
		t.Fatalf("synchronisation sequence is %d but the book's is %d", snap.Sequence, snap.Book.Sequence)
	}
	rest := r.openOrders("NOVA")
	if len(rest) != len(snap.Orders) {
		t.Fatalf("REST reports %d open orders, synchronisation carried %d", len(rest), len(snap.Orders))
	}
	for i := range rest {
		if rest[i] != snap.Orders[i] {
			t.Fatalf("order %d differs: REST %+v, sync %+v", i, rest[i], snap.Orders[i])
		}
	}
}

// TestSyncUnderConcurrentChangeLosesNothing is the race the combined query exists to close.
//
// A human order is chewed through by continuous bot fills while clients connect repeatedly.
// For each connection, the synchronised orders plus every lifecycle event after sequence S
// must reconstruct the order's true state. If the book and the orders were read at different
// sequences, or if events at or below S were discarded when they were not yet reflected in
// the synchronised state, a fill would vanish here.
func TestSyncUnderConcurrentChangeLosesNothing(t *testing.T) {
	r := newDefaultRig(t)

	const total = 2000
	mine := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":`+itoa(total)+`}`)

	// Grind the order down in small pieces, continuously.
	var wg sync.WaitGroup
	var stopOnce sync.Once
	stop := make(chan struct{})
	halt := func() {
		stopOnce.Do(func() { close(stop) })
		wg.Wait()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range total / 4 {
			select {
			case <-stop:
				return
			default:
			}
			r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Sell, 4))
			time.Sleep(200 * time.Microsecond)
		}
	}()
	t.Cleanup(halt)

	// Reconnect repeatedly while that runs.
	for attempt := range 12 {
		s := r.dial("NOVA")
		snap := s.nextOf(feed.TypeSnapshot)

		// Establish state from the synchronisation message alone, exactly as a
		// reconnecting client would.
		var filled, leaves int64
		var found bool
		for _, o := range snap.Orders {
			if o.ID == mine.OrderID {
				filled, leaves, found = o.FilledQty, o.RemainingQty, true
			}
		}
		if !found && filled == 0 {
			// The order may already be fully filled by now, which is a valid end state.
			if open := r.openOrders("NOVA"); len(open) == 0 {
				s.conn.CloseNow()
				break
			}
		}
		if found && filled+leaves != total {
			t.Fatalf("attempt %d: synchronised order does not conserve quantity: filled %d, leaves %d",
				attempt, filled, leaves)
		}

		// Apply lifecycle events newer than S, the way a client would.
		for range 20 {
			msg, err := s.read()
			if err != nil {
				break
			}
			if msg.Sequence <= snap.Sequence {
				t.Fatalf("attempt %d: message at sequence %d was not filtered against the synchronised %d",
					attempt, msg.Sequence, snap.Sequence)
			}
			if msg.Type != feed.TypeOrder || msg.Order.ID != mine.OrderID {
				continue
			}
			// Cumulative state must only ever move forwards.
			if msg.Order.FilledQty < filled {
				t.Fatalf("attempt %d: filled went backwards from %d to %d",
					attempt, filled, msg.Order.FilledQty)
			}
			filled, leaves = msg.Order.FilledQty, msg.Order.RemainingQty
			if filled+leaves != total {
				t.Fatalf("attempt %d: streamed order does not conserve quantity: filled %d, leaves %d",
					attempt, filled, leaves)
			}
		}
		s.conn.CloseNow()
	}

	// Let the grinder finish, then confirm no fill was lost overall.
	halt()

	actor, _ := r.server.Exchange().Market("NOVA")
	if err := actor.CheckInvariants(t.Context()); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
	trades, err := actor.RecentTrades(t.Context(), 0)
	if err != nil {
		t.Fatalf("reading the tape: %v", err)
	}
	var traded int64
	for _, tr := range trades {
		traded += tr.Quantity
	}
	open := r.openOrders("NOVA")
	if len(open) == 1 {
		if open[0].FilledQty != traded {
			t.Fatalf("order reports %d filled but %d shares traded", open[0].FilledQty, traded)
		}
		if open[0].FilledQty+open[0].RemainingQty != total {
			t.Fatalf("final order does not conserve quantity: %+v", open[0])
		}
	}
}

// TestSyncStateIsReadAtOneSequence pins down the actor-level guarantee the stream relies on,
// without any networking in the way.
func TestSyncStateIsReadAtOneSequence(t *testing.T) {
	r := newDefaultRig(t)
	actor, err := r.server.Exchange().Market("NOVA")
	if err != nil {
		t.Fatalf("looking up NOVA: %v", err)
	}

	r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":100}`)

	// Keep the market moving while sync states are taken.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.actorSubmit("NOVA", market.MarketOrder("bot-noise", engine.Sell, 1))
		}
	}()

	for range 200 {
		state, err := actor.SyncState(t.Context(), DefaultParticipant)
		if err != nil {
			t.Fatalf("reading sync state: %v", err)
		}
		// The book and the orders were read together, so they carry the same sequence.
		if state.Book.Sequence != state.Sequence {
			t.Fatalf("book is at sequence %d but the state claims %d", state.Book.Sequence, state.Sequence)
		}
		for _, o := range state.Orders {
			if o.FilledQty+o.RemainingQty+o.CanceledQty != o.OriginalQty {
				t.Fatalf("synchronised order does not conserve quantity: %+v", o)
			}
			if o.Status.Terminal() {
				t.Fatalf("a terminal order appeared in open orders: %+v", o)
			}
		}
	}

	close(stop)
	wg.Wait()
}
