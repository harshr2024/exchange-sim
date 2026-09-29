// Command exchanged runs the exchange: HTTP order entry, WebSocket market data, and
// optionally the simulated traders so the markets are alive to look at.
//
// Commands arrive over HTTP and are attributed to one server-assigned participant. Market
// data leaves over WebSockets through a per-market hub with bounded subscriber queues, so
// no client can slow the matching engine down.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/api"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/sim"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	sessionDir := flag.String("session-dir", "sessions", "directory for append-only session logs; empty disables recording")
	bots := flag.Bool("bots", true, "run the simulated traders on every market")
	seed := flag.Int64("seed", 1, "base seed for the bots")
	subscriberQueue := flag.Int("subscriber-queue", 256, "outgoing queue depth per WebSocket subscriber")
	origins := flag.String("allowed-origins", "localhost:*,127.0.0.1:*,[::1]:*", "comma-separated Origin patterns accepted for WebSocket upgrades")
	flag.Parse()

	if err := run(*addr, *sessionDir, *bots, *seed, *subscriberQueue, *origins); err != nil {
		log.Fatalf("exchanged: %v", err)
	}
}

func run(addr, sessionDir string, bots bool, seed int64, subscriberQueue int, origins string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server, err := api.New(api.Config{
		SessionDir:      sessionDir,
		SubscriberQueue: subscriberQueue,
		AllowedOrigins:  splitAndTrim(origins),
	})
	if err != nil {
		return err
	}

	// The markets run until ctx is canceled; everything else shuts down around them.
	markets := make(chan error, 1)
	go func() { markets <- server.Run(ctx) }()

	log.Printf("listed markets: %v", server.Exchange().Symbols())
	log.Printf("http commands are attributed to participant %q", server.Participant())
	if sessionDir != "" {
		log.Printf("recording session logs to %s/", sessionDir)
	}

	var traders sync.WaitGroup
	if bots {
		startBots(ctx, &traders, server, seed)
		log.Printf("simulated traders running on every market")
	}

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No write timeout: it would cut off WebSocket streams, which are long-lived by
		// design. Individual stream writes carry their own deadline instead.
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		err := httpServer.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	var listenErr error
	select {
	case listenErr = <-serveErr:
		stop()
	case <-ctx.Done():
		log.Print("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)

	// Bots stop with the signal context; wait for them before the markets so no command
	// is still in flight when the session logs close.
	traders.Wait()
	return errors.Join(listenErr, shutdownErr, <-markets)
}

// startBots runs a market maker and a noise trader on every listed market.
//
// Each market gets its own seeds so the markets do not move in lockstep. This is the live
// runner, so the resulting sessions are reproducible from their logs rather than from these
// seeds; see internal/sim for why those are different guarantees.
func startBots(ctx context.Context, wg *sync.WaitGroup, server *api.Server, seed int64) {
	for i, symbol := range server.Exchange().Symbols() {
		actor, err := server.Exchange().Market(symbol)
		if err != nil {
			log.Printf("skipping bots for %s: %v", symbol, err)
			continue
		}
		inst, err := instrument.Lookup(symbol)
		if err != nil {
			log.Printf("skipping bots for %s: %v", symbol, err)
			continue
		}

		offset := seed + int64(i)*1000
		live := sim.NewLive(actor, inst,
			sim.Schedule{
				Bot: sim.NewMarketMaker(sim.MakerConfig{
					Levels: 4, Size: 120, HalfSpread: 2, LevelStep: 1, SizeJitter: 40, Seed: offset,
				}),
				Interval: 250 * time.Millisecond,
			},
			sim.Schedule{
				Bot:      sim.NewNoiseTrader(sim.NoiseConfig{Seed: offset + 1, MaxQty: 30}),
				Interval: 120 * time.Millisecond,
			},
		)

		wg.Add(1)
		go func() {
			defer wg.Done()
			live.Run(ctx)
		}()
	}
}

func splitAndTrim(csv string) []string {
	var out []string
	for _, part := range strings.Split(csv, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
