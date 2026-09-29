package sim

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func TestDriverRunProducesTradesAndMovement(t *testing.T) {
	r := newRig(t)
	maker, noise := defaultBots(7)
	driver := NewDriver(r.actor, r.inst, maker, noise)

	if err := driver.Run(r.ctx, 400); err != nil {
		t.Fatalf("running the simulation: %v", err)
	}

	stats := driver.Stats()
	if stats.Trades == 0 {
		t.Fatal("400 ticks of bot activity produced no trades")
	}
	if stats.Filled == 0 {
		t.Fatal("no shares changed hands")
	}

	snap, err := r.actor.Snapshot(r.ctx)
	if err != nil {
		t.Fatalf("reading the book: %v", err)
	}
	if !snap.HasLastTrade {
		t.Fatal("no last traded price after the simulation ran")
	}
	// The maker is supposed to keep both sides populated.
	if !snap.HasBestBid || !snap.HasBestAsk {
		t.Fatalf("book is one-sided: bid %v, ask %v", snap.HasBestBid, snap.HasBestAsk)
	}
	if len(snap.Bids) == 0 || len(snap.Asks) == 0 {
		t.Fatalf("book has %d bid and %d ask levels", len(snap.Bids), len(snap.Asks))
	}

	r.checkInvariants()

	t.Logf("%d ticks, %d commands (%d accepted, %d rejected), %d trades, %d shares filled",
		stats.Ticks, stats.Commands, stats.Accepted, stats.Rejected, stats.Trades, stats.Filled)
}

// TestDriverRunsAreReproducibleFromSeeds is the guarantee the deterministic
// scheduler exists to provide: no recorded log needed, just the seeds.
func TestDriverRunsAreReproducibleFromSeeds(t *testing.T) {
	run := func(seed int64) (string, Stats) {
		t.Helper()
		r := newRig(t)
		maker, noise := defaultBots(seed)
		driver := NewDriver(r.actor, r.inst, maker, noise)
		if err := driver.Run(r.ctx, 300); err != nil {
			t.Fatalf("running the simulation: %v", err)
		}
		return r.digest(), driver.Stats()
	}

	firstDigest, firstStats := run(42)
	secondDigest, secondStats := run(42)

	if firstDigest != secondDigest {
		t.Fatalf("two runs with the same seeds diverged:\n  %s\n  %s", firstDigest, secondDigest)
	}
	if !reflect.DeepEqual(firstStats, secondStats) {
		t.Fatalf("two runs with the same seeds produced different stats:\n  %+v\n  %+v", firstStats, secondStats)
	}

	// And the digest is actually sensitive to the seed, so the check above is not
	// passing for the wrong reason.
	otherDigest, _ := run(43)
	if otherDigest == firstDigest {
		t.Fatal("a different seed produced the same session")
	}
}

// TestDriverRunReplaysIdentically checks that the simulation is still just another
// source of commands: its session replays like any other.
func TestDriverRunReplaysIdentically(t *testing.T) {
	r := newRig(t)
	maker, noise := defaultBots(11)
	driver := NewDriver(r.actor, r.inst, maker, noise)

	if err := driver.Run(r.ctx, 300); err != nil {
		t.Fatalf("running the simulation: %v", err)
	}

	if live, replayed := r.digest(), r.replayDigest(); live != replayed {
		t.Fatalf("replay of a bot session diverged:\n  live   %s\n  replay %s", live, replayed)
	}
}

// TestBotsOnlyCancelTheirOwnOrders reconstructs ownership from the recorded command
// stream and checks every cancel against it.
//
// This is the end-to-end version of the claim. The structural argument is stronger:
// a bot's View carries only its own resting orders and an aggregated book with no
// order identities in it, so it has no way to name another participant's order. This
// test confirms that nothing in practice works around that.
func TestBotsOnlyCancelTheirOwnOrders(t *testing.T) {
	r := newRig(t)
	maker, noise := defaultBots(3)
	driver := NewDriver(r.actor, r.inst, maker, noise)

	if err := driver.Run(r.ctx, 400); err != nil {
		t.Fatalf("running the simulation: %v", err)
	}

	// Replay the log ourselves, tracking who owns which order ID.
	owner := make(map[engine.OrderID]string)
	var nextID engine.OrderID
	cancels := 0

	for _, rec := range r.journal.Records() {
		cmd := rec.Command
		switch cmd.Kind {
		case market.SubmitLimit, market.SubmitMarket:
			if err := cmd.Validate(); err != nil {
				t.Fatalf("sequence %d: a bot emitted an invalid command: %v", rec.Sequence, err)
			}
			nextID++
			owner[nextID] = cmd.ParticipantID
		case market.Cancel:
			cancels++
			got, known := owner[cmd.OrderID]
			if !known {
				t.Fatalf("sequence %d: %s canceled order %d, which was never submitted",
					rec.Sequence, cmd.ParticipantID, cmd.OrderID)
			}
			if got != cmd.ParticipantID {
				t.Fatalf("sequence %d: %s canceled order %d owned by %s",
					rec.Sequence, cmd.ParticipantID, cmd.OrderID, got)
			}
		}
	}

	if cancels == 0 {
		t.Fatal("the run produced no cancellations, so the check is vacuous")
	}

	// The market must never have had to refuse a cancel on ownership grounds.
	for _, e := range r.recorder.Events() {
		if e.Kind == market.OrderRejected && strings.Contains(e.Reason, "another participant") {
			t.Fatalf("sequence %d was rejected for ownership: %s", e.Sequence, e.Reason)
		}
	}
	t.Logf("%d cancellations, all owner-matched", cancels)
}

// TestBotCommandsAreAlwaysValid checks that bots never emit malformed commands.
//
// Rejections are not all equal. A cancel that misses because the order was filled a
// moment earlier is legitimate and expected; a malformed order is a bug in the bot.
// This distinguishes the two.
func TestBotCommandsAreAlwaysValid(t *testing.T) {
	r := newRig(t)
	maker, noise := defaultBots(5)
	driver := NewDriver(r.actor, r.inst, maker, noise)

	if err := driver.Run(r.ctx, 500); err != nil {
		t.Fatalf("running the simulation: %v", err)
	}

	for _, rec := range r.journal.Records() {
		if err := rec.Command.Validate(); err != nil {
			t.Fatalf("sequence %d: %+v is invalid: %v", rec.Sequence, rec.Command, err)
		}
	}

	// The only rejection a correct bot can provoke is a cancel that arrived too
	// late, which the engine reports as an unknown order.
	for reason, n := range driver.Stats().Rejections {
		if !strings.Contains(reason, engine.ErrUnknownOrder.Error()) {
			t.Fatalf("%d commands were rejected as %q, which no correct bot should cause", n, reason)
		}
	}
}

func TestDriverRefusesMisattributedCommands(t *testing.T) {
	r := newRig(t)
	driver := NewDriver(r.actor, r.inst, impostor{})

	err := driver.Run(r.ctx, 1)
	if !errors.Is(err, ErrMisattributedCommand) {
		t.Fatalf("driver returned %v, want ErrMisattributedCommand", err)
	}
}

// TestBotViewIsScopedToItsOwnOrders pins down the structural half of ownership: a
// bot is not shown anything it could use to cancel someone else's order.
func TestBotViewIsScopedToItsOwnOrders(t *testing.T) {
	r := newRig(t)

	// Two identities with resting orders, plus a human.
	for _, cmd := range []market.Command{
		market.LimitOrder("bot-mm", engine.Buy, 9990, 50),
		market.LimitOrder("bot-noise", engine.Buy, 9980, 50),
		market.LimitOrder("human", engine.Buy, 9970, 50),
	} {
		if ack, err := r.actor.Submit(r.ctx, cmd); err != nil || !ack.Accepted {
			t.Fatalf("seeding %s: %v %s", cmd.ParticipantID, err, ack.Rejection)
		}
	}

	spy := &spyBot{participant: "bot-mm"}
	driver := NewDriver(r.actor, r.inst, spy)
	if err := driver.Run(r.ctx, 1); err != nil {
		t.Fatalf("running the driver: %v", err)
	}

	if len(spy.seen.Open) != 1 {
		t.Fatalf("bot-mm was shown %d resting orders, want only its own 1", len(spy.seen.Open))
	}
	if spy.seen.Open[0].ParticipantID != "bot-mm" {
		t.Fatalf("bot-mm was shown an order owned by %s", spy.seen.Open[0].ParticipantID)
	}
	// The aggregated book must not leak order identities either.
	if len(spy.seen.Book.Bids) != 3 {
		t.Fatalf("book shows %d bid levels, want 3", len(spy.seen.Book.Bids))
	}
}

// impostor emits a command attributed to a different participant.
type impostor struct{}

func (impostor) Participant() string { return "bot-mm" }
func (impostor) Decide(View) []market.Command {
	return []market.Command{market.CancelOrder("human", 1)}
}

// spyBot records the view it was handed and does nothing.
type spyBot struct {
	participant string
	seen        View
}

func (s *spyBot) Participant() string { return s.participant }
func (s *spyBot) Decide(v View) []market.Command {
	s.seen = v
	return nil
}
