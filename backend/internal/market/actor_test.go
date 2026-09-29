package market

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
)

func startActor(t *testing.T, m *Market, queueSize int) *Actor {
	t.Helper()
	actor := NewActor(m, queueSize)
	ctx, cancel := context.WithCancel(context.Background())
	go actor.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-actor.Done():
		case <-time.After(5 * time.Second):
			t.Error("actor did not stop within 5s")
		}
	})
	return actor
}

// TestConcurrentSubmissionsAreSequencedAndReproducible is the concurrency claim.
//
// Note what it deliberately does not assert: that two genuinely concurrent
// goroutines get any particular ordering relative to each other. That is not
// knowable and asserting it would be wrong. What must hold is that the commands
// get totally ordered somehow, that the order is recorded, and that the recorded
// order is enough to reproduce the run exactly. Whichever command reaches the
// market first wins priority; no participant's clock is consulted.
func TestConcurrentSubmissionsAreSequencedAndReproducible(t *testing.T) {
	const (
		workers   = 16
		perWorker = 250
		total     = workers * perWorker
	)

	journal := NewMemJournal()
	recorder := NewRecorder()
	actor := startActor(t, newTestMarket(t, WithJournal(journal), WithSink(recorder)), 64)

	ctx := context.Background()
	seen := make([][]uint64, workers)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w) + 1))
			for i := 0; i < perWorker; i++ {
				ack, err := actor.Submit(ctx, concurrentCommand(rng, w))
				if err != nil {
					t.Errorf("worker %d submit: %v", w, err)
					return
				}
				if ack.Sequence == 0 {
					t.Errorf("worker %d received an unsequenced ack: %+v", w, ack)
					return
				}
				seen[w] = append(seen[w], ack.Sequence)
			}
		}(w)
	}
	wg.Wait()

	// Every command got exactly one sequence number, and the numbers are the
	// dense range 1..total: nothing skipped and nothing handed out twice.
	assigned := make(map[uint64]int, total)
	for w, seqs := range seen {
		for _, seq := range seqs {
			if prev, dup := assigned[seq]; dup {
				t.Fatalf("sequence %d was given to worker %d and worker %d", seq, prev, w)
			}
			assigned[seq] = w
		}
	}
	if len(assigned) != total {
		t.Fatalf("%d sequence numbers were assigned for %d commands", len(assigned), total)
	}
	for seq := uint64(1); seq <= total; seq++ {
		if _, ok := assigned[seq]; !ok {
			t.Fatalf("sequence %d was never assigned", seq)
		}
	}

	// Each worker saw its own submissions in the order it made them.
	for w, seqs := range seen {
		for i := 1; i < len(seqs); i++ {
			if seqs[i] <= seqs[i-1] {
				t.Fatalf("worker %d got sequence %d after %d", w, seqs[i], seqs[i-1])
			}
		}
	}

	if err := actor.CheckInvariants(ctx); err != nil {
		t.Fatalf("invariant violation after concurrent flow: %v", err)
	}

	liveDigest, err := actor.Digest(ctx)
	if err != nil {
		t.Fatalf("reading digest: %v", err)
	}

	// This is the part that makes concurrency tractable: the interleaving was
	// arbitrary, but it was recorded, and replaying the recording reproduces this
	// specific run rather than some other valid one.
	records := journal.Records()
	if len(records) != total {
		t.Fatalf("log holds %d records for %d commands", len(records), total)
	}
	replayed, err := Replay("NOVA", records)
	if err != nil {
		t.Fatalf("replaying the concurrent run: %v", err)
	}
	if replayed.Digest != liveDigest {
		live, _ := actor.CanonicalState(ctx)
		t.Fatalf("replay of the concurrent run diverged:\n%s",
			diffCanonical(live, replayed.Market.CanonicalState()))
	}

	if replayed.Trades == 0 {
		t.Fatal("concurrent flow produced no trades")
	}
	t.Logf("%d commands from %d goroutines, %d trades, digest %s", total, workers, replayed.Trades, liveDigest[:16])
}

// TestConcurrentReadersDoNotRaceWithMatching runs snapshot readers, event
// consumers and command submitters at the same time. Under -race this is what
// catches state accidentally escaping the market's goroutine, which is the easiest
// way to break the single-writer design without breaking any test assertion.
func TestConcurrentReadersDoNotRaceWithMatching(t *testing.T) {
	recorder := NewRecorder()
	actor := startActor(t, newTestMarket(t, WithSink(recorder)), 64)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stop := make(chan struct{})
	var writers, readers sync.WaitGroup

	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			rng := rand.New(rand.NewSource(int64(100 + w)))
			for i := 0; i < 400; i++ {
				if _, err := actor.Submit(ctx, concurrentCommand(rng, w)); err != nil {
					return
				}
			}
		}(w)
	}

	// Readers, hammering every read path the API layer will use.
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := actor.Snapshot(ctx)
				if err != nil {
					return
				}
				// A snapshot must never show a crossed book to a client.
				if snap.HasBestBid && snap.HasBestAsk && snap.BestBid >= snap.BestAsk {
					t.Errorf("snapshot at sequence %d is crossed: bid %d, ask %d",
						snap.Sequence, snap.BestBid, snap.BestAsk)
					return
				}
				if _, err := actor.OpenOrders(ctx, "worker-0"); err != nil {
					return
				}
				if _, err := actor.RecentTrades(ctx, 20); err != nil {
					return
				}
				recorder.Events()
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	if err := actor.CheckInvariants(ctx); err != nil {
		t.Fatalf("invariant violation: %v", err)
	}
	if recorder.Count(TradeExecuted) == 0 {
		t.Fatal("no trades were published")
	}
}

func TestTryEnqueueReportsAFullQueueInsteadOfBlocking(t *testing.T) {
	// Deliberately not started, so nothing drains the queue.
	actor := NewActor(newTestMarket(t), 1)

	if err := actor.TryEnqueue(LimitOrder("bot", engine.Buy, 9900, 10)); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- actor.TryEnqueue(LimitOrder("bot", engine.Buy, 9900, 10)) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueFull) {
			t.Fatalf("enqueue on a full queue returned %v, want ErrQueueFull", err)
		}
	case <-time.After(time.Second):
		t.Fatal("TryEnqueue blocked on a full queue")
	}
}

func TestSubmitAfterShutdownFails(t *testing.T) {
	actor := NewActor(newTestMarket(t), 4)
	ctx, cancel := context.WithCancel(context.Background())
	go actor.Run(ctx)

	if _, err := actor.Submit(ctx, LimitOrder("human", engine.Buy, 9900, 10)); err != nil {
		t.Fatalf("submit while running: %v", err)
	}

	cancel()
	select {
	case <-actor.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("actor did not stop")
	}

	if _, err := actor.Submit(context.Background(), LimitOrder("human", engine.Buy, 9900, 10)); !errors.Is(err, ErrStopped) {
		t.Fatalf("submit after shutdown returned %v, want ErrStopped", err)
	}
	if _, err := actor.Snapshot(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("snapshot after shutdown returned %v, want ErrStopped", err)
	}
}

func TestSubmitHonoursContextCancellation(t *testing.T) {
	// Not started, and the queue is full, so the submit has nowhere to go.
	actor := NewActor(newTestMarket(t), 1)
	if err := actor.TryEnqueue(LimitOrder("bot", engine.Buy, 9900, 10)); err != nil {
		t.Fatalf("filling the queue: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := actor.Submit(ctx, LimitOrder("human", engine.Buy, 9900, 10)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("submit on a full queue returned %v, want a deadline error", err)
	}
}

// concurrentCommand builds a command without reading market state, so it is safe
// to call from a submitting goroutine.
func concurrentCommand(rng *rand.Rand, worker int) Command {
	participant := "worker-" + string(rune('0'+worker%10))

	side := engine.Buy
	if rng.Intn(2) == 0 {
		side = engine.Sell
	}
	qty := int64(1 + rng.Intn(40))

	switch n := rng.Intn(100); {
	case n < 10:
		// A guessed ID: usually a miss, which exercises the rejection path.
		return CancelOrder(participant, engine.OrderID(1+rng.Intn(200)))
	case n < 25:
		return MarketOrder(participant, side, qty)
	default:
		return LimitOrder(participant, side, engine.PriceTicks(10000+rng.Intn(41)-20), qty)
	}
}
