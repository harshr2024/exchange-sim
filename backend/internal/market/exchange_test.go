package market

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
)

func TestExchangeRoutesBySymbol(t *testing.T) {
	dir := t.TempDir()
	ex, err := Open(Config{SessionDir: dir})
	if err != nil {
		t.Fatalf("opening exchange: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ex.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("exchange did not stop within 5s")
		}
		if err := ex.Close(); err != nil {
			t.Errorf("closing session logs: %v", err)
		}
	})

	if got, want := ex.Symbols(), instrument.Symbols(); !reflect.DeepEqual(got, want) {
		t.Fatalf("exchange lists %v, want %v", got, want)
	}

	// Each market is independent: an order on one is invisible to the others, and
	// each keeps its own sequence.
	if _, err := ex.Submit(ctx, "NOVA", LimitOrder("human", engine.Buy, 9990, 10)); err != nil {
		t.Fatalf("submitting to NOVA: %v", err)
	}
	if _, err := ex.Submit(ctx, "ORBIT", LimitOrder("human", engine.Sell, 4260, 20)); err != nil {
		t.Fatalf("submitting to ORBIT: %v", err)
	}

	nova, err := ex.Snapshot(ctx, "NOVA")
	if err != nil {
		t.Fatalf("reading NOVA: %v", err)
	}
	if !nova.HasBestBid || nova.BestBid != 9990 || nova.HasBestAsk {
		t.Fatalf("NOVA shows bid (%d, %v) ask (%d, %v), want only the bid",
			nova.BestBid, nova.HasBestBid, nova.BestAsk, nova.HasBestAsk)
	}

	orbit, err := ex.Snapshot(ctx, "ORBIT")
	if err != nil {
		t.Fatalf("reading ORBIT: %v", err)
	}
	if !orbit.HasBestAsk || orbit.BestAsk != 4260 || orbit.HasBestBid {
		t.Fatalf("ORBIT shows bid (%d, %v) ask (%d, %v), want only the ask",
			orbit.BestBid, orbit.HasBestBid, orbit.BestAsk, orbit.HasBestAsk)
	}

	pulse, err := ex.Snapshot(ctx, "PULSE")
	if err != nil {
		t.Fatalf("reading PULSE: %v", err)
	}
	if pulse.Sequence != 0 || pulse.HasBestBid || pulse.HasBestAsk {
		t.Fatalf("PULSE is not empty: %+v", pulse)
	}

	// Symbol validation happens at routing, before anything is sequenced.
	if _, err := ex.Submit(ctx, "TSLA", LimitOrder("human", engine.Buy, 100, 1)); !errors.Is(err, instrument.ErrUnknownSymbol) {
		t.Fatalf("submitting to an unlisted symbol returned %v, want ErrUnknownSymbol", err)
	}
	if _, err := ex.Snapshot(ctx, "TSLA"); !errors.Is(err, instrument.ErrUnknownSymbol) {
		t.Fatalf("reading an unlisted symbol returned %v, want ErrUnknownSymbol", err)
	}

	// Symbols are accepted in any case and route to the canonical market.
	if _, err := ex.Snapshot(ctx, "nova"); err != nil {
		t.Fatalf("lower-case symbol did not route: %v", err)
	}
}

func TestExchangeWritesOneLogPerMarket(t *testing.T) {
	dir := t.TempDir()
	ex, err := Open(Config{Symbols: []string{"NOVA", "PULSE"}, SessionDir: dir})
	if err != nil {
		t.Fatalf("opening exchange: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ex.Run(ctx)
	}()

	if _, err := ex.Submit(ctx, "NOVA", LimitOrder("human", engine.Buy, 9990, 10)); err != nil {
		t.Fatalf("submitting to NOVA: %v", err)
	}

	cancel()
	wg.Wait()
	if err := ex.Close(); err != nil {
		t.Fatalf("closing session logs: %v", err)
	}

	for _, symbol := range []string{"NOVA", "PULSE"} {
		if _, err := os.Stat(filepath.Join(dir, SessionFileName(symbol))); err != nil {
			t.Fatalf("session log for %s: %v", symbol, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, SessionFileName("ORBIT"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a log was written for a market that was not listed")
	}

	res, err := ReplayFile("NOVA", filepath.Join(dir, SessionFileName("NOVA")))
	if err != nil {
		t.Fatalf("replaying NOVA: %v", err)
	}
	if res.Records != 1 || res.Sequence != 1 {
		t.Fatalf("NOVA log holds %d records ending at sequence %d, want 1 and 1", res.Records, res.Sequence)
	}
}

func TestOpenRejectsUnlistedSymbol(t *testing.T) {
	if _, err := Open(Config{Symbols: []string{"NOVA", "TSLA"}}); !errors.Is(err, instrument.ErrUnknownSymbol) {
		t.Fatalf("Open returned %v, want ErrUnknownSymbol", err)
	}
}
