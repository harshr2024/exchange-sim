package market

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
)

// Config configures an Exchange.
type Config struct {
	// Symbols to list. Empty means every listed instrument.
	Symbols []string
	// SessionDir is where per-market session logs are written. Empty disables
	// recording, which is useful for benchmarks and throwaway runs.
	SessionDir string
	// QueueSize is the depth of each market's inbound command queue.
	QueueSize int
	// Depth is how many price levels per side published snapshots carry.
	Depth int
	// NewSink builds the market data sink for a symbol. Nil discards events.
	NewSink func(symbol string) Sink
}

// Exchange is the set of markets and the routing from symbol to market.
//
// Routing is the only thing that happens before sequencing. Everything after it
// belongs to exactly one market goroutine, so two symbols never share mutable
// state and a busy market cannot slow down a quiet one.
type Exchange struct {
	markets  map[string]*Actor
	symbols  []string
	journals []Journal

	mu      sync.Mutex
	started bool
}

// Open builds the configured markets. It does not start them; call Run.
func Open(cfg Config) (*Exchange, error) {
	symbols := cfg.Symbols
	if len(symbols) == 0 {
		symbols = instrument.Symbols()
	}

	ex := &Exchange{markets: make(map[string]*Actor, len(symbols))}
	for _, symbol := range symbols {
		inst, err := instrument.Lookup(symbol)
		if err != nil {
			ex.closeJournals()
			return nil, err
		}

		opts := []Option{}
		if cfg.Depth > 0 {
			opts = append(opts, WithDepth(cfg.Depth))
		}
		if cfg.SessionDir != "" {
			journal, err := OpenFileJournal(cfg.SessionDir, inst.Symbol)
			if err != nil {
				ex.closeJournals()
				return nil, err
			}
			ex.journals = append(ex.journals, journal)
			opts = append(opts, WithJournal(journal))
		}
		if cfg.NewSink != nil {
			opts = append(opts, WithSink(cfg.NewSink(inst.Symbol)))
		}

		m, err := New(inst.Symbol, opts...)
		if err != nil {
			ex.closeJournals()
			return nil, err
		}
		ex.markets[inst.Symbol] = NewActor(m, cfg.QueueSize)
		ex.symbols = append(ex.symbols, inst.Symbol)
	}
	return ex, nil
}

// Symbols returns the listed symbols, ordered.
func (e *Exchange) Symbols() []string { return append([]string(nil), e.symbols...) }

// Market routes a symbol to its market.
func (e *Exchange) Market(symbol string) (*Actor, error) {
	inst, err := instrument.Lookup(symbol)
	if err != nil {
		return nil, err
	}
	actor, ok := e.markets[inst.Symbol]
	if !ok {
		return nil, fmt.Errorf("%w: %s is not listed on this exchange", instrument.ErrUnknownSymbol, inst.Symbol)
	}
	return actor, nil
}

// Submit routes and submits one command.
func (e *Exchange) Submit(ctx context.Context, symbol string, cmd Command) (Ack, error) {
	actor, err := e.Market(symbol)
	if err != nil {
		return Ack{}, err
	}
	return actor.Submit(ctx, cmd)
}

// Snapshot returns one market's current book.
func (e *Exchange) Snapshot(ctx context.Context, symbol string) (engine.BookSnapshot, error) {
	actor, err := e.Market(symbol)
	if err != nil {
		return engine.BookSnapshot{}, err
	}
	return actor.Snapshot(ctx)
}

// Run starts every market goroutine and blocks until ctx is canceled and all of
// them have stopped.
func (e *Exchange) Run(ctx context.Context) {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	var wg sync.WaitGroup
	for _, actor := range e.markets {
		wg.Add(1)
		go func(a *Actor) {
			defer wg.Done()
			a.Run(ctx)
		}(actor)
	}
	wg.Wait()
}

// Close flushes and closes the session logs. Call it after Run has returned.
func (e *Exchange) Close() error { return e.closeJournals() }

func (e *Exchange) closeJournals() error {
	var problems []error
	for _, j := range e.journals {
		if err := j.Close(); err != nil {
			problems = append(problems, err)
		}
	}
	e.journals = nil
	return errors.Join(problems...)
}
