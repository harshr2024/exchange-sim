package feed

import (
	"sync"
	"sync/atomic"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// DefaultQueueSize is the default depth of a subscriber's outgoing queue.
//
// It needs to absorb a burst of updates while a client is briefly busy, without being so
// large that a client which has genuinely stopped reading sits on megabytes of stale
// market data before anyone notices.
const DefaultQueueSize = 256

// Subscriber is one consumer's bounded queue of market data.
//
// A subscriber is either live or dead. Dead is terminal: it means messages were lost, so
// whatever the subscriber has is incomplete and the only correct recovery is a fresh
// snapshot. The writer learns about it through Dead and is expected to tell the client to
// resync rather than quietly continuing from a gap.
type Subscriber struct {
	messages chan Message

	dead     chan struct{}
	deadOnce sync.Once
	reason   atomic.Pointer[string]
}

// Messages is the queue to read from.
func (s *Subscriber) Messages() <-chan Message { return s.messages }

// Dead is closed when the subscription can no longer be trusted, either because it fell
// behind or because the hub shut down.
func (s *Subscriber) Dead() <-chan struct{} { return s.dead }

// Reason explains why the subscription died, or is empty while it is live.
func (s *Subscriber) Reason() string {
	if r := s.reason.Load(); r != nil {
		return *r
	}
	return ""
}

// kill marks the subscription unusable. It is safe to call repeatedly and from any
// goroutine, and it never closes the message channel — the hub may still be inside a
// publish, and closing a channel it is about to send on would panic.
func (s *Subscriber) kill(reason string) {
	s.deadOnce.Do(func() {
		s.reason.Store(&reason)
		close(s.dead)
	})
}

// Alive reports whether the subscription is still usable.
func (s *Subscriber) Alive() bool {
	select {
	case <-s.dead:
		return false
	default:
		return true
	}
}

// Hub fans market data out to subscribers without ever blocking the publisher.
type Hub struct {
	mu     sync.Mutex
	subs   map[*Subscriber]struct{}
	closed bool

	published atomic.Int64
	evicted   atomic.Int64
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[*Subscriber]struct{})}
}

// Subscribe registers a new subscriber with a queue of the given depth. A depth of 0 or
// less uses DefaultQueueSize.
//
// Subscribing before reading a snapshot is what closes the gap between the two: the hub
// starts buffering immediately, so updates that land while the snapshot is being taken
// are queued rather than missed.
func (h *Hub) Subscribe(queueSize int) *Subscriber {
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	sub := &Subscriber{
		messages: make(chan Message, queueSize),
		dead:     make(chan struct{}),
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		sub.kill("market data hub is closed")
		return sub
	}
	h.subs[sub] = struct{}{}
	return sub
}

// Unsubscribe removes a subscriber and marks it dead, which is what a WebSocket handler
// calls when its client goes away.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	delete(h.subs, sub)
	h.mu.Unlock()
	sub.kill("unsubscribed")
}

// Publish offers a message to every live subscriber.
//
// This runs on the market's own goroutine, inline with matching, so it must not block for
// any reason. Every send is non-blocking: a subscriber with no room is evicted on the
// spot rather than waited for. Dropping one consumer is always preferable to stalling the
// exchange for everyone.
func (h *Hub) Publish(m Message) {
	h.published.Add(1)

	h.mu.Lock()
	defer h.mu.Unlock()

	for sub := range h.subs {
		if !sub.Alive() {
			delete(h.subs, sub)
			continue
		}
		select {
		case sub.messages <- m:
		default:
			// The queue is full, so this subscriber has already missed nothing but is
			// about to. Evict it now: its view would otherwise develop a silent gap.
			sub.kill("subscriber fell behind the market data feed")
			delete(h.subs, sub)
			h.evicted.Add(1)
		}
	}
}

// Close marks the hub closed and kills every subscriber.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for sub := range h.subs {
		sub.kill("market data hub is closed")
		delete(h.subs, sub)
	}
}

// Subscribers returns how many live subscribers the hub holds.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Stats returns the number of messages published and subscribers evicted for falling
// behind.
func (h *Hub) Stats() (published, evicted int64) {
	return h.published.Load(), h.evicted.Load()
}

// Broadcaster adapts a market's event stream onto a Hub. It implements market.Sink.
type Broadcaster struct {
	hub *Hub
	// participant is the only identity whose order lifecycle reaches the wire. The
	// bots generate most of the order churn and none of it is any client's business,
	// so broadcasting all of it would be volume without value.
	participant string
}

// NewBroadcaster returns a sink that publishes to hub. Order lifecycle messages are
// limited to participant; an empty participant suppresses them entirely.
func NewBroadcaster(hub *Hub, participant string) *Broadcaster {
	return &Broadcaster{hub: hub, participant: participant}
}

// Publish converts one market event into wire messages. It never blocks.
func (b *Broadcaster) Publish(e market.Event) {
	switch e.Kind {
	case market.BookChanged:
		// An update with no level changes and no trades tells a subscriber nothing:
		// level quantities are absolute and the quotes follow from them, so there is
		// nothing to learn from an empty one.
		if e.Update == nil || e.Update.Empty() {
			return
		}
		b.hub.Publish(UpdateMessage(*e.Update))

	case market.OrderAccepted, market.OrderRested, market.OrderCanceled:
		if b.participant == "" || e.Order == nil || e.Order.ParticipantID != b.participant {
			return
		}
		b.hub.Publish(OrderMessage(e.Symbol, e.Sequence, e.Kind, *e.Order))

	case market.OrderRejected:
		// Rejections carry no order, so they cannot be attributed to a participant.
		// The HTTP response to the command that was rejected reports it instead.
	}
}
