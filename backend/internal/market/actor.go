package market

import (
	"context"
	"errors"
	"sync"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
)

// ErrStopped is returned when a market actor is no longer running.
var ErrStopped = errors.New("market actor is stopped")

// ErrQueueFull is returned by TryEnqueue when the command queue has no room.
var ErrQueueFull = errors.New("market command queue is full")

// DefaultQueueSize is the default depth of a market's inbound command queue.
const DefaultQueueSize = 1024

// Actor is the single goroutine that owns one Market.
//
// Everything about this exchange that benefits from concurrency (HTTP handlers,
// WebSocket writers, simulated traders) stays concurrent. The one thing that does
// not benefit is mutating an order book from several goroutines at once: it would
// need locks on the matching path, and it would make the resulting trades depend
// on how the scheduler happened to interleave them. Instead every command for a
// symbol funnels into one channel, and one goroutine drains it. That is what makes
// the command log a complete description of the session.
type Actor struct {
	market  *Market
	cmds    chan envelope
	queries chan func(*Market)

	startOnce sync.Once
	stopped   chan struct{}
}

type envelope struct {
	cmd Command
	// reply is buffered so the actor can hand back an Ack without waiting for a
	// caller that may have already given up.
	reply chan Ack
}

// NewActor wraps m. queueSize of 0 or less uses DefaultQueueSize.
func NewActor(m *Market, queueSize int) *Actor {
	if queueSize <= 0 {
		queueSize = DefaultQueueSize
	}
	return &Actor{
		market:  m,
		cmds:    make(chan envelope, queueSize),
		queries: make(chan func(*Market), queueSize),
		stopped: make(chan struct{}),
	}
}

// Symbol returns the market's symbol. It reads immutable state and is safe from
// any goroutine.
func (a *Actor) Symbol() string { return a.market.Symbol() }

// Run drains the command and query queues until ctx is canceled. It blocks, and
// must be called exactly once.
func (a *Actor) Run(ctx context.Context) {
	a.startOnce.Do(func() {
		defer close(a.stopped)
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-a.cmds:
				ack := a.market.Apply(env.cmd)
				if env.reply != nil {
					env.reply <- ack
				}
			case fn := <-a.queries:
				fn(a.market)
			}
		}
	})
}

// Done returns a channel closed when the actor stops.
func (a *Actor) Done() <-chan struct{} { return a.stopped }

// Submit sequences a command and waits for its acknowledgement.
func (a *Actor) Submit(ctx context.Context, cmd Command) (Ack, error) {
	reply := make(chan Ack, 1)
	select {
	case a.cmds <- envelope{cmd: cmd, reply: reply}:
	case <-ctx.Done():
		return Ack{}, ctx.Err()
	case <-a.stopped:
		return Ack{}, ErrStopped
	}

	select {
	case ack := <-reply:
		return ack, nil
	case <-ctx.Done():
		return Ack{}, ctx.Err()
	case <-a.stopped:
		return Ack{}, ErrStopped
	}
}

// TryEnqueue offers a command without waiting for room or for a result.
//
// The simulated traders use this: a bot that blocked on a full queue would keep
// piling up quote updates that are already stale by the time they are processed.
func (a *Actor) TryEnqueue(cmd Command) error {
	select {
	case a.cmds <- envelope{cmd: cmd}:
		return nil
	case <-a.stopped:
		return ErrStopped
	default:
		return ErrQueueFull
	}
}

// query runs fn on the market's own goroutine and returns its result, which is how
// read access stays race-free without putting a lock on the matching path.
//
// The result comes back through a buffered channel rather than a variable the
// closure captures. That matters for abandonment: if the caller gives up on a
// canceled context after the closure is already queued, the actor will still run it,
// and a captured variable would then be written by the actor while the caller was
// reading it. Handing the value back over a channel with room for it means the actor
// never blocks and an abandoned result is simply never read.
//
// It is a free function rather than a method because Go does not allow methods to
// introduce their own type parameters.
func query[T any](ctx context.Context, a *Actor, fn func(*Market) T) (T, error) {
	var zero T
	reply := make(chan T, 1)

	select {
	case a.queries <- func(m *Market) { reply <- fn(m) }:
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-a.stopped:
		return zero, ErrStopped
	}

	select {
	case v := <-reply:
		return v, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-a.stopped:
		return zero, ErrStopped
	}
}

// Snapshot returns the current published book.
func (a *Actor) Snapshot(ctx context.Context) (engine.BookSnapshot, error) {
	return query(ctx, a, func(m *Market) engine.BookSnapshot { return m.Snapshot() })
}

// OpenOrders returns participant's resting orders.
func (a *Actor) OpenOrders(ctx context.Context, participant string) ([]engine.View, error) {
	return query(ctx, a, func(m *Market) []engine.View { return m.OpenOrders(participant) })
}

// RecentTrades returns the last n trades, oldest first.
func (a *Actor) RecentTrades(ctx context.Context, n int) ([]engine.Trade, error) {
	return query(ctx, a, func(m *Market) []engine.Trade { return m.RecentTrades(n) })
}

// Sequence returns the market's current sequence number.
func (a *Actor) Sequence(ctx context.Context) (uint64, error) {
	return query(ctx, a, func(m *Market) uint64 { return m.Sequence() })
}

// Digest returns a digest of the market's full canonical state.
func (a *Actor) Digest(ctx context.Context) (string, error) {
	return query(ctx, a, func(m *Market) string { return m.Digest() })
}

// CanonicalState returns the market's full canonical state as text.
func (a *Actor) CanonicalState(ctx context.Context) (string, error) {
	return query(ctx, a, func(m *Market) string { return m.CanonicalState() })
}

// CheckInvariants runs the market's invariant checks on the owning goroutine.
func (a *Actor) CheckInvariants(ctx context.Context) error {
	invErr, err := query(ctx, a, func(m *Market) error { return m.CheckInvariants() })
	if err != nil {
		return err
	}
	return invErr
}
