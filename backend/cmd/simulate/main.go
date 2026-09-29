// Command simulate runs a market autonomously with the simulated traders.
//
// Two modes, with deliberately different guarantees:
//
//	-mode deterministic  One goroutine polls the bots in a fixed order. The same
//	                     seed and tick count always produce the same session, so
//	                     the run is reproducible from its seed alone.
//
//	-mode live           Each bot runs on its own goroutine and wall-clock
//	                     interval. Arrival order is then up to the Go scheduler,
//	                     so the same seed does NOT imply the same session.
//	                     Reproducing a live run means replaying its log.
//
// Both modes finish by replaying the recorded session into a fresh engine and
// comparing digests.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/display"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/sim"
)

func main() {
	symbol := flag.String("symbol", "NOVA", "market to run")
	mode := flag.String("mode", "deterministic", "deterministic or live")
	ticks := flag.Int("ticks", 500, "decision rounds to run (deterministic mode)")
	duration := flag.Duration("duration", 3*time.Second, "how long to run (live mode)")
	seed := flag.Int64("seed", 1, "seed for both bots")
	sessionDir := flag.String("session-dir", "sessions", "directory for the append-only session log")
	printEvery := flag.Int("print-every", 0, "print the book every N ticks (deterministic mode); 0 prints only the final state")
	flag.Parse()

	if err := run(*symbol, *mode, *ticks, *duration, *seed, *sessionDir, *printEvery); err != nil {
		log.Fatalf("simulate: %v", err)
	}
}

func run(symbol, mode string, ticks int, duration time.Duration, seed int64, sessionDir string, printEvery int) error {
	inst, err := instrument.Lookup(symbol)
	if err != nil {
		return err
	}

	journal, err := market.OpenFileJournal(sessionDir, inst.Symbol)
	if err != nil {
		return err
	}
	recorder := market.NewRecorder()

	m, err := market.New(inst.Symbol, market.WithJournal(journal), market.WithSink(recorder))
	if err != nil {
		return err
	}

	actor := market.NewActor(m, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go actor.Run(ctx)

	maker := sim.NewMarketMaker(sim.MakerConfig{
		Levels: 3, Size: 100, HalfSpread: 2, LevelStep: 1, SizeJitter: 25, Seed: seed,
	})
	noise := sim.NewNoiseTrader(sim.NoiseConfig{Seed: seed + 1, MaxQty: 25})

	out := os.Stdout
	fmt.Fprintf(out, "%s (%s)  mode=%s  seed=%d\n", inst.Symbol, inst.DisplayName, mode, seed)

	var summary func()
	switch mode {
	case "deterministic":
		driver := sim.NewDriver(actor, inst, maker, noise)
		for tick := 1; tick <= ticks; tick++ {
			if err := driver.Tick(ctx); err != nil {
				return err
			}
			if printEvery > 0 && tick%printEvery == 0 {
				if err := report(out, actor, inst, ctx, fmt.Sprintf("tick %d", tick)); err != nil {
					return err
				}
			}
		}
		summary = func() { printStats(out, driver.Stats()) }

	case "live":
		live := sim.NewLive(actor, inst,
			sim.Schedule{Bot: maker, Interval: 25 * time.Millisecond},
			sim.Schedule{Bot: noise, Interval: 10 * time.Millisecond},
		)
		runCtx, stop := context.WithTimeout(ctx, duration)
		defer stop()
		live.Run(runCtx)
		// One query drains everything the bots already queued.
		if _, err := actor.Sequence(ctx); err != nil {
			return err
		}
		summary = func() {
			offered, dropped, wakeups := live.Stats()
			fmt.Fprintf(out, "  wakeups        %d\n", wakeups)
			fmt.Fprintf(out, "  offered        %d\n", offered)
			fmt.Fprintf(out, "  dropped        %d (queue full or misattributed)\n", dropped)
		}

	default:
		return fmt.Errorf("unknown mode %q; want deterministic or live", mode)
	}

	if err := report(out, actor, inst, ctx, "final state"); err != nil {
		return err
	}

	if err := actor.CheckInvariants(ctx); err != nil {
		return fmt.Errorf("live market violates its invariants: %w", err)
	}
	liveDigest, err := actor.Digest(ctx)
	if err != nil {
		return err
	}

	// Stop the market before closing the log, so every accepted command is recorded
	// before anything reads the file back.
	cancel()
	<-actor.Done()
	if err := journal.Close(); err != nil {
		return err
	}

	path := filepath.Join(sessionDir, market.SessionFileName(inst.Symbol))
	replayed, err := market.ReplayFile(inst.Symbol, path)
	if err != nil {
		return err
	}

	display.Rule(out, "summary")
	summary()
	fmt.Fprintf(out, "  events         %d (%d trades)\n", recorder.Count(), recorder.Count(market.TradeExecuted))

	display.Rule(out, "replay")
	fmt.Fprintf(out, "  session log    %s (%d commands)\n", path, replayed.Records)
	fmt.Fprintf(out, "  live digest    %s\n", liveDigest)
	fmt.Fprintf(out, "  replay digest  %s\n", replayed.Digest)
	if replayed.Digest != liveDigest {
		return fmt.Errorf("replay diverged from the live session")
	}
	fmt.Fprintf(out, "  result         identical: same trades, same book, same order priorities\n")

	if mode == "deterministic" {
		fmt.Fprintf(out, "  note           this mode also reproduces from -seed %d alone\n", seed)
	} else {
		fmt.Fprintf(out, "  note           a live run reproduces from its log, not from its seed:\n")
		fmt.Fprintf(out, "                 goroutine scheduling decides which command arrives first\n")
	}
	return nil
}

func report(out *os.File, actor *market.Actor, inst instrument.Instrument, ctx context.Context, label string) error {
	snap, err := actor.Snapshot(ctx)
	if err != nil {
		return err
	}
	trades, err := actor.RecentTrades(ctx, 5)
	if err != nil {
		return err
	}

	display.Rule(out, label)
	display.Book(out, inst, snap)
	fmt.Fprint(out, "recent trades:\n")
	display.Tape(out, inst, trades)
	fmt.Fprint(out, "\n")
	return nil
}

func printStats(out *os.File, s sim.Stats) {
	fmt.Fprintf(out, "  ticks          %d\n", s.Ticks)
	fmt.Fprintf(out, "  commands       %d (%d accepted, %d rejected)\n", s.Commands, s.Accepted, s.Rejected)
	fmt.Fprintf(out, "  trades         %d (%d shares)\n", s.Trades, s.Filled)
	for reason, n := range s.Rejections {
		fmt.Fprintf(out, "  rejected       %d x %s\n", n, reason)
	}
}
