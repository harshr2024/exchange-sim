package sim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// TestLiveRunReplaysIdentically is the only reproducibility guarantee a concurrent
// simulation can make, and it is the one that matters.
//
// Note what is deliberately absent: there is no assertion that two live runs with
// identical seeds produce identical sessions. They will not, and requiring it would
// be a conceptual error. Several bot goroutines race to reach the market, and which
// one arrives first is the Go scheduler's decision, not the seed's. What the design
// does guarantee is that the actor turns whatever race occurs into one specific total
// order, and that replaying the log of that order reproduces the run exactly.
//
// Reproducibility from seeds alone is what Driver is for; see
// TestDriverRunsAreReproducibleFromSeeds.
func TestLiveRunReplaysIdentically(t *testing.T) {
	r := newRig(t)
	maker, noise := defaultBots(23)

	live := NewLive(r.actor, r.inst,
		Schedule{Bot: maker, Interval: 2 * time.Millisecond},
		Schedule{Bot: noise, Interval: time.Millisecond},
	)

	ctx, cancel := context.WithTimeout(r.ctx, 400*time.Millisecond)
	defer cancel()
	live.Run(ctx)

	offered, dropped, wakeups := live.Stats()
	if wakeups == 0 {
		t.Fatal("no bot ever woke up")
	}
	if offered == 0 {
		t.Fatal("no commands were offered to the market")
	}

	// Let the actor finish draining anything still queued, then compare.
	if _, err := r.actor.Sequence(r.ctx); err != nil {
		t.Fatalf("draining the market: %v", err)
	}

	r.checkInvariants()

	trades, err := r.actor.RecentTrades(r.ctx, 0)
	if err != nil {
		t.Fatalf("reading the tape: %v", err)
	}
	if len(trades) == 0 {
		t.Fatal("a live run of two bots produced no trades")
	}

	if liveDigest, replayed := r.digest(), r.replayDigest(); liveDigest != replayed {
		t.Fatalf("replay of a live run diverged:\n  live   %s\n  replay %s", liveDigest, replayed)
	}

	t.Logf("%d wakeups, %d commands offered, %d dropped, %d trades",
		wakeups, offered, dropped, len(trades))
}

// TestLiveRunHoldsOwnershipAndValidity checks the same guarantees as the
// deterministic run, but under real concurrency.
func TestLiveRunHoldsOwnershipAndValidity(t *testing.T) {
	r := newRig(t)
	maker, noise := defaultBots(29)

	live := NewLive(r.actor, r.inst,
		Schedule{Bot: maker, Interval: 2 * time.Millisecond},
		Schedule{Bot: noise, Interval: time.Millisecond},
	)

	ctx, cancel := context.WithTimeout(r.ctx, 300*time.Millisecond)
	defer cancel()
	live.Run(ctx)

	if _, err := r.actor.Sequence(r.ctx); err != nil {
		t.Fatalf("draining the market: %v", err)
	}

	owner := make(map[engine.OrderID]string)
	var nextID engine.OrderID
	for _, rec := range r.journal.Records() {
		cmd := rec.Command
		if err := cmd.Validate(); err != nil {
			t.Fatalf("sequence %d: %+v is invalid: %v", rec.Sequence, cmd, err)
		}
		switch cmd.Kind {
		case market.SubmitLimit, market.SubmitMarket:
			nextID++
			owner[nextID] = cmd.ParticipantID
		case market.Cancel:
			if got, known := owner[cmd.OrderID]; known && got != cmd.ParticipantID {
				t.Fatalf("sequence %d: %s canceled order %d owned by %s",
					rec.Sequence, cmd.ParticipantID, cmd.OrderID, got)
			}
		}
	}

	for _, e := range r.recorder.Events() {
		if e.Kind != market.OrderRejected {
			continue
		}
		// A cancel losing a race with a fill is expected under concurrency. Any
		// other rejection means a bot emitted something it should not have.
		if !strings.Contains(e.Reason, engine.ErrUnknownOrder.Error()) {
			t.Fatalf("sequence %d rejected as %q", e.Sequence, e.Reason)
		}
	}
	r.checkInvariants()
}

// TestLiveRunnerDropsMisattributedCommands checks the live runner's equivalent of
// the Driver's misattribution guard. It has no error path to return on, so it must
// drop the command rather than submit it under an identity the bot does not own.
func TestLiveRunnerDropsMisattributedCommands(t *testing.T) {
	r := newRig(t)

	if ack, err := r.actor.Submit(r.ctx, market.LimitOrder("human", engine.Buy, 9990, 10)); err != nil || !ack.Accepted {
		t.Fatalf("seeding the human order: %v %s", err, ack.Rejection)
	}
	before := r.digest()

	live := NewLive(r.actor, r.inst, Schedule{Bot: impostor{}, Interval: time.Millisecond})
	ctx, cancel := context.WithTimeout(r.ctx, 100*time.Millisecond)
	defer cancel()
	live.Run(ctx)

	if _, err := r.actor.Sequence(r.ctx); err != nil {
		t.Fatalf("draining the market: %v", err)
	}

	_, dropped, wakeups := live.Stats()
	if wakeups == 0 {
		t.Fatal("the bot never woke up")
	}
	if dropped == 0 {
		t.Fatal("the misattributed commands were not dropped")
	}
	if after := r.digest(); after != before {
		t.Fatal("a misattributed command reached the market and changed its state")
	}
	if open, err := r.actor.OpenOrders(r.ctx, "human"); err != nil || len(open) != 1 {
		t.Fatalf("the human's order was disturbed: %v, %d remaining", err, len(open))
	}
}
