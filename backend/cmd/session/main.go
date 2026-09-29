// Command session drives one market from the terminal and then proves the session
// replays.
//
// It runs a scripted scenario through a real market actor: seed liquidity, a
// partial fill, a cancellation, and a market order that sweeps several price
// levels. The book, the tape and the human's open orders are printed after each
// step. At the end it closes the session log, replays it into a fresh engine, and
// compares digests.
//
// The scripted "seed" participant stands in for the liquidity bot until the
// simulation phase exists.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/display"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

const human = "human"

func main() {
	symbol := flag.String("symbol", "NOVA", "market to run")
	sessionDir := flag.String("session-dir", "sessions", "directory for the append-only session log")
	flag.Parse()

	if err := run(*symbol, *sessionDir); err != nil {
		log.Fatalf("session: %v", err)
	}
}

func run(symbol, sessionDir string) error {
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

	// One goroutine owns this market for the rest of the run.
	actor := market.NewActor(m, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go actor.Run(ctx)

	out := os.Stdout
	runner := &scenario{actor: actor, inst: inst, out: out, ctx: ctx}

	runner.step("seed the book", func() {
		// Asks, then bids, away from the touch.
		runner.submit(market.LimitOrder("seed", engine.Sell, 10010, 30))
		runner.submit(market.LimitOrder("seed", engine.Sell, 10020, 50))
		runner.submit(market.LimitOrder("seed", engine.Sell, 10030, 40))
		runner.submit(market.LimitOrder("seed", engine.Sell, 10040, 60))
		runner.submit(market.LimitOrder("seed", engine.Buy, 9990, 40))
		runner.submit(market.LimitOrder("seed", engine.Buy, 9980, 70))
	})

	runner.step("human buys 50 at 100.10: fills 30, rests 20", func() {
		runner.submit(market.LimitOrder(human, engine.Buy, 10010, 50))
	})

	var resting engine.OrderID
	for _, o := range runner.openOrders() {
		resting = o.ID
	}

	runner.step(fmt.Sprintf("human cancels the resting order #%d", resting), func() {
		runner.submit(market.CancelOrder(human, resting))
	})

	runner.step("human sends a market buy for 100: sweeps several levels", func() {
		runner.submit(market.MarketOrder(human, engine.Buy, 100))
	})

	liveDigest, err := actor.Digest(ctx)
	if err != nil {
		return err
	}
	if err := actor.CheckInvariants(ctx); err != nil {
		return fmt.Errorf("live market violates its invariants: %w", err)
	}

	// Stop the market before closing the log, so every accepted command is
	// recorded before anything reads the file back.
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

	display.Rule(out, "replay")
	fmt.Fprintf(out, "  session log    %s (%d commands)\n", path, replayed.Records)
	fmt.Fprintf(out, "  live digest    %s\n", liveDigest)
	fmt.Fprintf(out, "  replay digest  %s\n", replayed.Digest)
	if replayed.Digest != liveDigest {
		return fmt.Errorf("replay diverged from the live session")
	}
	fmt.Fprintf(out, "  result         identical: same trades, same book, same order priorities\n")
	fmt.Fprintf(out, "  events         %d published (%d trades)\n",
		recorder.Count(), recorder.Count(market.TradeExecuted))
	return nil
}

type scenario struct {
	actor *market.Actor
	inst  instrument.Instrument
	out   *os.File
	ctx   context.Context
}

// step runs a labelled group of commands and then prints the resulting state.
func (s *scenario) step(label string, body func()) {
	display.Rule(s.out, label)
	body()

	snap, err := s.actor.Snapshot(s.ctx)
	if err != nil {
		log.Fatalf("session: reading the book: %v", err)
	}
	display.Book(s.out, s.inst, snap)

	trades, err := s.actor.RecentTrades(s.ctx, 5)
	if err != nil {
		log.Fatalf("session: reading the tape: %v", err)
	}
	fmt.Fprint(s.out, "recent trades:\n")
	display.Tape(s.out, s.inst, trades)

	fmt.Fprintf(s.out, "%s open orders:\n", human)
	display.OpenOrders(s.out, s.inst, s.openOrders())
	fmt.Fprint(s.out, "\n")
}

func (s *scenario) submit(cmd market.Command) {
	ack, err := s.actor.Submit(s.ctx, cmd)
	if err != nil {
		log.Fatalf("session: submitting %s: %v", cmd.Kind, err)
	}
	if !ack.Accepted {
		log.Fatalf("session: %s was rejected: %s", cmd.Kind, ack.Rejection)
	}
	for _, t := range ack.Trades {
		fmt.Fprintf(s.out, "  filled %d @ %s\n", t.Quantity, s.inst.Format(t.Price))
	}
	if ack.CanceledQty > 0 && cmd.Kind != market.Cancel {
		fmt.Fprintf(s.out, "  %d unfilled shares canceled (market orders never rest)\n", ack.CanceledQty)
	}
}

func (s *scenario) openOrders() []engine.View {
	orders, err := s.actor.OpenOrders(s.ctx, human)
	if err != nil {
		log.Fatalf("session: reading open orders: %v", err)
	}
	return orders
}
