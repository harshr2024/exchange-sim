// Command exchanged runs the exchange.
//
// At this stage it boots one market goroutine per listed instrument and serves a
// health endpoint and the instrument listing. The order entry, snapshot and
// WebSocket endpoints belong to the network-layer phase and are not here yet; use
// cmd/session to drive a market from the terminal in the meantime.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func main() {
	addr := flag.String("addr", ":8080", "address to listen on")
	sessionDir := flag.String("session-dir", "sessions", "directory for append-only session logs; empty disables recording")
	flag.Parse()

	if err := run(*addr, *sessionDir); err != nil {
		log.Fatalf("exchanged: %v", err)
	}
}

func run(addr, sessionDir string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	exchange, err := market.Open(market.Config{SessionDir: sessionDir})
	if err != nil {
		return err
	}

	markets := make(chan struct{})
	go func() {
		defer close(markets)
		exchange.Run(ctx)
	}()

	log.Printf("listed markets: %v", exchange.Symbols())
	if sessionDir != "" {
		log.Printf("recording session logs to %s/", sessionDir)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(exchange),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		stop()
		<-markets
		return errors.Join(err, exchange.Close())
	case <-ctx.Done():
		log.Print("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)

	// The markets stop with the signal context, so waiting here guarantees every
	// accepted command was applied and recorded before the logs are closed.
	<-markets
	return errors.Join(shutdownErr, exchange.Close())
}

func routes(exchange *market.Exchange) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"markets": exchange.Symbols(),
		})
	})

	mux.HandleFunc("GET /api/markets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, instrument.All())
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		fmt.Fprintf(os.Stderr, "exchanged: writing response: %v\n", err)
	}
}
