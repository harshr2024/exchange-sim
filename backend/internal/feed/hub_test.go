package feed

import (
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func updateMsg(seq uint64) Message {
	return UpdateMessage(market.Update{
		Symbol:   "NOVA",
		Sequence: seq,
		Levels:   []engine.LevelChange{{Side: engine.Buy, Price: 9990, Quantity: 10, Orders: 1}},
	})
}

func TestHubFansOutToEverySubscriber(t *testing.T) {
	hub := NewHub()
	a := hub.Subscribe(4)
	b := hub.Subscribe(4)

	hub.Publish(updateMsg(1))
	hub.Publish(updateMsg(2))

	for name, sub := range map[string]*Subscriber{"a": a, "b": b} {
		for want := uint64(1); want <= 2; want++ {
			select {
			case msg := <-sub.Messages():
				if msg.Sequence != want {
					t.Fatalf("%s received sequence %d, want %d", name, msg.Sequence, want)
				}
			default:
				t.Fatalf("%s did not receive sequence %d", name, want)
			}
		}
	}
	if got := hub.Subscribers(); got != 2 {
		t.Fatalf("hub holds %d subscribers, want 2", got)
	}
}

// TestPublishNeverBlocksOnAFullSubscriber is the property the exchange depends on. The
// market goroutine calls Publish inline with matching, so if it could block, one stalled
// browser would set the pace of the whole market.
func TestPublishNeverBlocksOnAFullSubscriber(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe(2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the queue can hold, and nobody is reading.
		for seq := uint64(1); seq <= 1000; seq++ {
			hub.Publish(updateMsg(seq))
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}

	if sub.Alive() {
		t.Fatal("a subscriber that fell behind was not evicted")
	}
	select {
	case <-sub.Dead():
	default:
		t.Fatal("the evicted subscriber's death was not signalled")
	}
	if sub.Reason() == "" {
		t.Fatal("the evicted subscriber has no reason recorded")
	}
	if _, evicted := hub.Stats(); evicted != 1 {
		t.Fatalf("hub evicted %d subscribers, want 1", evicted)
	}
	if got := hub.Subscribers(); got != 0 {
		t.Fatalf("hub still holds %d subscribers after eviction", got)
	}
}

// TestSlowSubscriberDoesNotAffectOthers covers the isolation requirement: one client
// falling over must not cost the others any data.
func TestSlowSubscriberDoesNotAffectOthers(t *testing.T) {
	hub := NewHub()
	slow := hub.Subscribe(2)
	fast := hub.Subscribe(64)

	const messages = 40
	for seq := uint64(1); seq <= messages; seq++ {
		hub.Publish(updateMsg(seq))
		// The healthy subscriber keeps up; the slow one never reads.
		select {
		case msg := <-fast.Messages():
			if msg.Sequence != seq {
				t.Fatalf("fast subscriber received sequence %d, want %d", msg.Sequence, seq)
			}
		default:
			t.Fatalf("fast subscriber missed sequence %d", seq)
		}
	}

	if slow.Alive() {
		t.Fatal("the slow subscriber was not evicted")
	}
	if !fast.Alive() {
		t.Fatal("the healthy subscriber was evicted alongside the slow one")
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe(8)

	hub.Unsubscribe(sub)
	if sub.Alive() {
		t.Fatal("an unsubscribed subscriber is still alive")
	}

	hub.Publish(updateMsg(1))
	select {
	case msg := <-sub.Messages():
		t.Fatalf("an unsubscribed subscriber received %+v", msg)
	default:
	}
	if got := hub.Subscribers(); got != 0 {
		t.Fatalf("hub holds %d subscribers after unsubscribe", got)
	}
}

func TestCloseKillsEverySubscriber(t *testing.T) {
	hub := NewHub()
	a, b := hub.Subscribe(4), hub.Subscribe(4)

	hub.Close()

	for name, sub := range map[string]*Subscriber{"a": a, "b": b} {
		if sub.Alive() {
			t.Fatalf("%s survived hub close", name)
		}
	}
	// Subscribing to a closed hub yields an already-dead subscriber rather than one that
	// waits forever for data that will never come.
	if hub.Subscribe(4).Alive() {
		t.Fatal("subscribing to a closed hub produced a live subscriber")
	}
}

// TestBroadcasterPublishesUpdatesAndOwnOrders checks the market-event-to-wire translation,
// including that other participants' order churn stays off the wire.
func TestBroadcasterPublishesUpdatesAndOwnOrders(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe(16)
	b := NewBroadcaster(hub, "human")

	human := engine.View{ID: 1, ParticipantID: "human", Side: engine.Buy, LimitPrice: 9990, RemainingQty: 10}
	bot := engine.View{ID: 2, ParticipantID: "bot-mm", Side: engine.Buy, LimitPrice: 9980, RemainingQty: 10}

	b.Publish(market.Event{Kind: market.OrderRested, Symbol: "NOVA", Sequence: 1, Order: &human})
	b.Publish(market.Event{Kind: market.OrderRested, Symbol: "NOVA", Sequence: 2, Order: &bot})
	b.Publish(market.Event{Kind: market.OrderRejected, Symbol: "NOVA", Sequence: 3, Reason: "bad"})
	b.Publish(market.Event{Kind: market.BookChanged, Symbol: "NOVA", Sequence: 4, Update: &market.Update{
		Symbol: "NOVA", Sequence: 4,
		Levels: []engine.LevelChange{{Side: engine.Buy, Price: 9990, Quantity: 10, Orders: 1}},
	}})
	// An update with nothing in it teaches a subscriber nothing, because level quantities
	// are absolute and the quotes follow from them.
	b.Publish(market.Event{Kind: market.BookChanged, Symbol: "NOVA", Sequence: 5, Update: &market.Update{
		Symbol: "NOVA", Sequence: 5,
	}})

	var got []Message
	for {
		select {
		case msg := <-sub.Messages():
			got = append(got, msg)
			continue
		default:
		}
		break
	}

	if len(got) != 2 {
		t.Fatalf("broadcast %d messages, want 2: %+v", len(got), got)
	}
	if got[0].Type != TypeOrder || got[0].Order.ParticipantID != "human" {
		t.Fatalf("first message is %+v, want the human's order", got[0])
	}
	if got[1].Type != TypeUpdate || got[1].Sequence != 4 {
		t.Fatalf("second message is %+v, want the update at sequence 4", got[1])
	}
}

func TestBroadcasterWithNoParticipantSuppressesOrderMessages(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe(8)
	b := NewBroadcaster(hub, "")

	order := engine.View{ID: 1, ParticipantID: "human"}
	b.Publish(market.Event{Kind: market.OrderRested, Symbol: "NOVA", Sequence: 1, Order: &order})

	select {
	case msg := <-sub.Messages():
		t.Fatalf("an order message was broadcast with no participant configured: %+v", msg)
	default:
	}
}
