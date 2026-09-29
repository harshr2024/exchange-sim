// Package api exposes the exchange over HTTP and WebSockets.
//
// Commands go in over HTTP, which keeps request/response semantics simple: a submission
// gets back the order ID, the fills and whether it rested. Market data comes out over
// WebSockets, because it is server-driven, continuous and frequent.
//
// There is no authentication. The browser is not asked who it is and is not allowed to
// say: every command the API submits is attributed to one server-assigned participant.
// That is why a participant ID is absent from the request schema rather than merely
// ignored.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/feed"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// DefaultParticipant is the identity assigned to every command arriving over HTTP.
const DefaultParticipant = "human"

// Config configures the server.
type Config struct {
	// Symbols to list. Empty means every listed instrument.
	Symbols []string
	// SessionDir is where per-market session logs are written. Empty disables recording.
	SessionDir string
	// Participant is the identity assigned to HTTP commands. Defaults to
	// DefaultParticipant. Clients cannot influence it.
	Participant string
	// Depth is how many price levels per side a snapshot carries.
	Depth int
	// QueueSize is the depth of each market's inbound command queue.
	QueueSize int
	// SubscriberQueue is the depth of each WebSocket subscriber's outgoing queue.
	SubscriberQueue int
	// AllowedOrigins are the Origin header patterns accepted for WebSocket upgrades, in
	// the form "host:port" with optional wildcards. Same-origin is always allowed.
	AllowedOrigins []string
	// WriteTimeout caps a single WebSocket write, so a stalled socket cannot pin its
	// writer goroutine forever.
	WriteTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Participant == "" {
		c.Participant = DefaultParticipant
	}
	if c.SubscriberQueue <= 0 {
		c.SubscriberQueue = feed.DefaultQueueSize
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if len(c.AllowedOrigins) == 0 {
		c.AllowedOrigins = []string{"localhost:*", "127.0.0.1:*", "[::1]:*"}
	}
	return c
}

// Server owns the exchange, one market data hub per market, and the HTTP routes.
type Server struct {
	cfg      Config
	exchange *market.Exchange
	hubs     map[string]*feed.Hub
}

// New builds the exchange and its market data hubs. It does not start the markets; call
// Run.
func New(cfg Config) (*Server, error) {
	cfg = cfg.withDefaults()

	srv := &Server{cfg: cfg, hubs: make(map[string]*feed.Hub)}

	// NewSink is called synchronously while the exchange is being built, before any
	// market goroutine exists, so populating the map here needs no synchronisation and
	// every market has a hub by the time anything can publish.
	exchange, err := market.Open(market.Config{
		Symbols:    cfg.Symbols,
		SessionDir: cfg.SessionDir,
		QueueSize:  cfg.QueueSize,
		Depth:      cfg.Depth,
		NewSink: func(symbol string) market.Sink {
			hub := feed.NewHub()
			srv.hubs[symbol] = hub
			return feed.NewBroadcaster(hub, cfg.Participant)
		},
	})
	if err != nil {
		return nil, err
	}
	srv.exchange = exchange
	return srv, nil
}

// Exchange exposes the underlying exchange, for tests and the simulation wiring.
func (s *Server) Exchange() *market.Exchange { return s.exchange }

// Hub returns the market data hub for a symbol.
func (s *Server) Hub(symbol string) (*feed.Hub, error) {
	inst, err := instrument.Lookup(symbol)
	if err != nil {
		return nil, err
	}
	hub, ok := s.hubs[inst.Symbol]
	if !ok {
		return nil, fmt.Errorf("%w: %s has no market data hub", instrument.ErrUnknownSymbol, inst.Symbol)
	}
	return hub, nil
}

// Participant is the identity assigned to HTTP commands.
func (s *Server) Participant() string { return s.cfg.Participant }

// Run starts every market and blocks until ctx is canceled, then closes the hubs and the
// session logs.
func (s *Server) Run(ctx context.Context) error {
	s.exchange.Run(ctx)
	for _, hub := range s.hubs {
		hub.Close()
	}
	return s.exchange.Close()
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/markets", s.handleMarkets)
	mux.HandleFunc("GET /api/markets/{symbol}/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/markets/{symbol}/trades", s.handleTrades)
	mux.HandleFunc("GET /api/markets/{symbol}/orders", s.handleOpenOrders)
	mux.HandleFunc("POST /api/markets/{symbol}/orders", s.handleSubmit)
	mux.HandleFunc("DELETE /api/markets/{symbol}/orders/{id}", s.handleCancel)
	mux.HandleFunc("GET /ws/markets/{symbol}", s.handleStream)

	return mux
}

// statusFor maps a rejection to an HTTP status.
//
// The market reports refusals as values rather than only as strings precisely so this can
// classify them without matching on message text.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, instrument.ErrUnknownSymbol):
		return http.StatusNotFound
	case errors.Is(err, engineUnknownOrder):
		return http.StatusNotFound
	case errors.Is(err, market.ErrNotOrderOwner):
		return http.StatusForbidden
	case errors.Is(err, market.ErrHalted), errors.Is(err, market.ErrStopped):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable
	default:
		// Everything else is a malformed command: bad quantity, bad price, a market
		// order carrying a limit, an unknown kind.
		return http.StatusBadRequest
	}
}
