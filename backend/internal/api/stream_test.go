package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/feed"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// stream is a connected market data client.
type stream struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
}

// dial opens a market stream.
func (r *rig) dial(symbol string) *stream {
	r.t.Helper()

	url := "ws" + strings.TrimPrefix(r.http.URL, "http") + "/ws/markets/" + symbol
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	r.t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		r.t.Fatalf("dialling %s: %v", url, err)
	}
	r.t.Cleanup(func() { conn.CloseNow() })

	return &stream{t: r.t, conn: conn, ctx: ctx}
}

// next reads the next message, failing the test if none arrives.
func (s *stream) next() feed.Message {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	var msg feed.Message
	if err := wsjson.Read(ctx, s.conn, &msg); err != nil {
		s.t.Fatalf("reading from the stream: %v", err)
	}
	return msg
}

// nextOf reads until a message of the given type arrives.
func (s *stream) nextOf(want feed.MessageType) feed.Message {
	s.t.Helper()
	for range 200 {
		if msg := s.next(); msg.Type == want {
			return msg
		}
	}
	s.t.Fatalf("no %s message arrived within 200 messages", want)
	return feed.Message{}
}

// read attempts one read and returns the error, for cases where failure is the point.
func (s *stream) read() (feed.Message, error) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()

	var msg feed.Message
	err := wsjson.Read(ctx, s.conn, &msg)
	return msg, err
}

func TestStreamOpensWithASnapshot(t *testing.T) {
	r := newDefaultRig(t)

	// Some state to be reflected in the opening snapshot.
	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 30))
	r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":40}`)

	s := r.dial("NOVA")
	msg := s.next()

	if msg.Type != feed.TypeSnapshot {
		t.Fatalf("first message is %s, want a snapshot", msg.Type)
	}
	if msg.Book == nil {
		t.Fatal("the snapshot carries no book")
	}
	if msg.Book.Symbol != "NOVA" {
		t.Fatalf("snapshot is for %s", msg.Book.Symbol)
	}
	if msg.Sequence != msg.Book.Sequence || msg.Sequence < 2 {
		t.Fatalf("snapshot sequence is %d (book %d), want the market's current sequence",
			msg.Sequence, msg.Book.Sequence)
	}
	if !msg.Book.HasBestBid || msg.Book.BestBid != 9990 {
		t.Fatalf("snapshot best bid is (%d, %v), want (9990, true)", msg.Book.BestBid, msg.Book.HasBestBid)
	}
	if !msg.Book.HasBestAsk || msg.Book.BestAsk != 10010 {
		t.Fatalf("snapshot best ask is (%d, %v), want (10010, true)", msg.Book.BestAsk, msg.Book.HasBestAsk)
	}
}

// TestStreamDeliversIncrementalUpdates checks that live market data is a delta rather than
// a re-sent snapshot, which is the architectural requirement for this phase.
func TestStreamDeliversIncrementalUpdates(t *testing.T) {
	r := newDefaultRig(t)
	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":40}`)

	msg := s.nextOf(feed.TypeUpdate)
	if msg.Book != nil {
		t.Fatal("a live update carried a full snapshot instead of a delta")
	}
	if msg.Update == nil {
		t.Fatal("the update carries no payload")
	}
	want := []engine.LevelChange{{Side: engine.Buy, Price: 9990, Quantity: 40, Orders: 1}}
	if len(msg.Update.Levels) != 1 || msg.Update.Levels[0] != want[0] {
		t.Fatalf("update levels are %+v, want %+v", msg.Update.Levels, want)
	}
	if !msg.Update.HasBestBid || msg.Update.BestBid != 9990 {
		t.Fatalf("update reports best bid (%d, %v)", msg.Update.BestBid, msg.Update.HasBestBid)
	}
}

func TestStreamDeliversTrades(t *testing.T) {
	r := newDefaultRig(t)
	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 100))

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	r.submit("NOVA", `{"side":"buy","type":"market","quantity":25}`)

	msg := s.nextOf(feed.TypeUpdate)
	if len(msg.Update.Trades) != 1 {
		t.Fatalf("update carries %d trades, want 1: %+v", len(msg.Update.Trades), msg.Update.Trades)
	}
	trade := msg.Update.Trades[0]
	if trade.Quantity != 25 || trade.Price != 10010 || trade.AggressorSide != engine.Buy {
		t.Fatalf("trade is %+v, want 25 at 10010 aggressed by a buyer", trade)
	}
	if !msg.Update.HasLastTrade || msg.Update.LastTradePrice != 10010 {
		t.Fatalf("update reports last trade (%d, %v)", msg.Update.LastTradePrice, msg.Update.HasLastTrade)
	}
	// The level was only partly consumed, so it is still there with less on it.
	if len(msg.Update.Levels) != 1 || msg.Update.Levels[0].Quantity != 75 {
		t.Fatalf("update levels are %+v, want the ask down to 75", msg.Update.Levels)
	}
}

func TestStreamDeliversOwnOrderLifecycle(t *testing.T) {
	r := newDefaultRig(t)
	s := r.dial("NOVA")
	snap := s.nextOf(feed.TypeSnapshot)

	ack := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":40}`)

	var kinds []string
	for range 10 {
		msg := s.next()
		if msg.Type != feed.TypeOrder {
			continue
		}
		if msg.Order.ID != ack.OrderID {
			t.Fatalf("order message is for order %d, want %d", msg.Order.ID, ack.OrderID)
		}
		if msg.Order.ParticipantID != DefaultParticipant {
			t.Fatalf("order message is for participant %q", msg.Order.ParticipantID)
		}
		if msg.Sequence <= snap.Sequence {
			t.Fatalf("order message carries sequence %d, not newer than the snapshot's %d",
				msg.Sequence, snap.Sequence)
		}
		kinds = append(kinds, msg.Event)
		if msg.Event == string(market.OrderRested) {
			break
		}
	}
	if len(kinds) == 0 {
		t.Fatal("no order lifecycle messages arrived")
	}
	if kinds[len(kinds)-1] != string(market.OrderRested) {
		t.Fatalf("order lifecycle was %v, want it to end with rested", kinds)
	}
}

// TestStreamDoesNotLeakOtherParticipantsOrders keeps the bots' order churn off the wire.
func TestStreamDoesNotLeakOtherParticipantsOrders(t *testing.T) {
	r := newDefaultRig(t)
	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 30))

	// The book change is published; the bot's order lifecycle is not.
	msg := s.nextOf(feed.TypeUpdate)
	if msg.Update.Levels[0].Price != 10010 {
		t.Fatalf("update is %+v, want the bot's new ask level", msg.Update.Levels)
	}

	// Nothing further should arrive for the bot. Provoke one message we do expect and
	// check no order message slipped in before it.
	r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":10}`)
	for range 10 {
		next := s.next()
		if next.Type == feed.TypeOrder {
			if next.Order.ParticipantID != DefaultParticipant {
				t.Fatalf("the stream carried %s's order", next.Order.ParticipantID)
			}
			return
		}
	}
}

// TestStreamOrderingAroundSynchronisation is the race this design exists to close.
//
// The market is kept busy throughout, so commands land continuously while the stream is
// being set up. Whatever the interleaving, the client must see a snapshot at some sequence
// S followed only by messages strictly newer than S, with no duplicate and no gap in the
// ordering it observes.
func TestStreamOrderingAroundSynchronisation(t *testing.T) {
	r := newDefaultRig(t)

	// Continuous traffic on NOVA for the duration.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Buy, 9990, 5))
			r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 5))
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})

	// Let the market get ahead so the snapshot is taken at a non-trivial sequence.
	time.Sleep(50 * time.Millisecond)

	s := r.dial("NOVA")
	first := s.next()
	if first.Type != feed.TypeSnapshot {
		t.Fatalf("first message is %s, want a snapshot", first.Type)
	}
	snapshotSeq := first.Sequence
	if snapshotSeq == 0 {
		t.Fatal("the snapshot was taken before any command was sequenced")
	}

	// Everything after the snapshot must be strictly newer than it, and monotonic.
	previous := snapshotSeq
	for range 100 {
		msg, err := s.read()
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if msg.Type == feed.TypeError {
			t.Fatalf("the stream failed mid-sync: %s", msg.Reason)
		}
		if msg.Type == feed.TypeSnapshot {
			t.Fatalf("a second snapshot arrived at sequence %d", msg.Sequence)
		}
		if msg.Sequence <= snapshotSeq {
			t.Fatalf("message at sequence %d is already covered by the snapshot at %d",
				msg.Sequence, snapshotSeq)
		}
		if msg.Sequence < previous {
			t.Fatalf("message at sequence %d arrived after %d, out of order",
				msg.Sequence, previous)
		}
		previous = msg.Sequence
	}
	if previous <= snapshotSeq {
		t.Fatal("no post-snapshot messages arrived, so the ordering check is vacuous")
	}
}

// TestSlowSubscriberIsDisconnectedNotBackpressured covers the isolation requirement
// end to end: a client that stops reading is told to resync and dropped, while the market
// keeps running at full speed.
func TestSlowSubscriberIsDisconnectedNotBackpressured(t *testing.T) {
	// A queue of one makes falling behind easy to provoke deterministically.
	r := newRig(t, Config{Symbols: []string{"NOVA"}, SubscriberQueue: 1})

	s := r.dial("NOVA")
	s.nextOf(feed.TypeSnapshot)

	// Flood the market without reading anything from the socket.
	for i := range 400 {
		side := engine.Buy
		price := engine.PriceTicks(9990 - i%20)
		if i%2 == 1 {
			side = engine.Sell
			price = engine.PriceTicks(10010 + i%20)
		}
		r.actorSubmit("NOVA", market.LimitOrder("bot-mm", side, price, 5))
	}

	// The market itself must be entirely healthy: nothing about a stalled client may have
	// slowed it down or corrupted it.
	actor, _ := r.server.Exchange().Market("NOVA")
	if err := actor.CheckInvariants(t.Context()); err != nil {
		t.Fatalf("the market was damaged by a slow subscriber: %v", err)
	}
	seq, err := actor.Sequence(t.Context())
	if err != nil {
		t.Fatalf("reading the sequence: %v", err)
	}
	if seq < 400 {
		t.Fatalf("market reached sequence %d, want at least 400: the slow client applied backpressure", seq)
	}

	hub, err := r.server.Hub("NOVA")
	if err != nil {
		t.Fatalf("looking up the hub: %v", err)
	}
	if _, evicted := hub.Stats(); evicted == 0 {
		t.Fatal("the stalled subscriber was never evicted")
	}

	// The client eventually reads a resync instruction and then a closed connection.
	sawResync := false
	for range 500 {
		msg, err := s.read()
		if err != nil {
			break
		}
		if msg.Type == feed.TypeError {
			if !msg.Resync {
				t.Fatalf("stream ended with %q but did not ask for a resync", msg.Reason)
			}
			sawResync = true
			break
		}
	}
	if !sawResync {
		t.Fatal("the evicted client was never told to resync")
	}

	// And a fresh stream works immediately: eviction is per-subscriber.
	fresh := r.dial("NOVA")
	if msg := fresh.next(); msg.Type != feed.TypeSnapshot {
		t.Fatalf("a reconnecting client got %s, want a snapshot", msg.Type)
	}
}

func TestStreamsAreIndependent(t *testing.T) {
	r := newDefaultRig(t)

	a := r.dial("NOVA")
	b := r.dial("NOVA")
	a.nextOf(feed.TypeSnapshot)
	b.nextOf(feed.TypeSnapshot)

	r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":10}`)

	for name, s := range map[string]*stream{"a": a, "b": b} {
		msg := s.nextOf(feed.TypeUpdate)
		if msg.Update.Levels[0].Quantity != 10 {
			t.Fatalf("%s received %+v", name, msg.Update.Levels)
		}
	}

	// One client leaving must not disturb the other.
	a.conn.Close(websocket.StatusNormalClosure, "")
	r.submit("NOVA", `{"side":"buy","type":"limit","price":9980,"quantity":20}`)

	msg := b.nextOf(feed.TypeUpdate)
	if msg.Update.Levels[0].Price != 9980 {
		t.Fatalf("the surviving client received %+v", msg.Update.Levels)
	}
}

// TestClientDisconnectReleasesItsSubscription checks the handler cleans up rather than
// leaking a subscriber per connection.
func TestClientDisconnectReleasesItsSubscription(t *testing.T) {
	r := newDefaultRig(t)
	hub, err := r.server.Hub("NOVA")
	if err != nil {
		t.Fatalf("looking up the hub: %v", err)
	}

	for range 3 {
		s := r.dial("NOVA")
		s.nextOf(feed.TypeSnapshot)
		if got := hub.Subscribers(); got != 1 {
			t.Fatalf("hub holds %d subscribers while one client is connected", got)
		}

		s.conn.Close(websocket.StatusNormalClosure, "done")

		// The handler notices asynchronously.
		deadline := time.Now().Add(5 * time.Second)
		for hub.Subscribers() != 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := hub.Subscribers(); got != 0 {
			t.Fatalf("hub still holds %d subscribers after the client disconnected", got)
		}
	}
}

func TestStreamRejectsUnknownSymbol(t *testing.T) {
	r := newDefaultRig(t)

	url := "ws" + strings.TrimPrefix(r.http.URL, "http") + "/ws/markets/TSLA"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("dialling an unlisted symbol succeeded")
	}
	if resp == nil {
		t.Fatalf("dial failed without a response: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dialling an unlisted symbol returned %d, want 404", resp.StatusCode)
	}
}

// TestSnapshotAndStreamAgree ties the two halves together: a client that takes the
// snapshot from the stream and applies every subsequent update must end up with the same
// book the REST snapshot endpoint reports.
func TestSnapshotAndStreamAgree(t *testing.T) {
	r := newDefaultRig(t)
	s := r.dial("NOVA")

	opening := s.nextOf(feed.TypeSnapshot)

	view := map[engine.Side]map[engine.PriceTicks]int64{
		engine.Buy:  {},
		engine.Sell: {},
	}
	for _, lvl := range opening.Book.Bids {
		view[engine.Buy][lvl.Price] = lvl.Quantity
	}
	for _, lvl := range opening.Book.Asks {
		view[engine.Sell][lvl.Price] = lvl.Quantity
	}

	// A mixture of resting orders, fills and cancellations.
	var mine []engine.OrderID
	for i := range 12 {
		price := engine.PriceTicks(9990 - i)
		ack := r.submit("NOVA", `{"side":"buy","type":"limit","price":`+itoa(uint64(price))+`,"quantity":10}`)
		mine = append(mine, ack.OrderID)
	}
	for _, price := range []engine.PriceTicks{10010, 10020} {
		r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, price, 25))
	}
	r.submit("NOVA", `{"side":"buy","type":"market","quantity":30}`)
	r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+itoa(uint64(mine[0])), "", http.StatusOK)

	// Read updates until the stream has caught up with the market.
	target := r.snapshot("NOVA")
	for previous := opening.Sequence; previous < target.Sequence; {
		msg, err := s.read()
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
		if msg.Type != feed.TypeUpdate {
			continue
		}
		for _, c := range msg.Update.Levels {
			if c.Quantity == 0 {
				delete(view[c.Side], c.Price)
				continue
			}
			view[c.Side][c.Price] = c.Quantity
		}
		previous = msg.Sequence
	}

	// Compare within the published depth: the snapshot is truncated to the top ten, so
	// only levels inside that window are guaranteed to be comparable.
	for _, side := range []struct {
		side   engine.Side
		levels []engine.LevelView
	}{
		{engine.Buy, target.Bids},
		{engine.Sell, target.Asks},
	} {
		for _, lvl := range side.levels {
			got, ok := view[side.side][lvl.Price]
			if !ok {
				t.Fatalf("%s: the streamed view is missing price %d", side.side, lvl.Price)
			}
			if got != lvl.Quantity {
				t.Fatalf("%s %d: streamed quantity %d, snapshot says %d",
					side.side, lvl.Price, got, lvl.Quantity)
			}
		}
	}
	if len(target.Bids) == 0 || len(target.Asks) == 0 {
		t.Fatal("the comparison is vacuous: the book is one-sided")
	}
}
