package sim

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
)

// Schedule pairs a bot with how often it wakes up in a live run.
type Schedule struct {
	Bot      Bot
	Interval time.Duration
}

// LiveStats counts what a live run offered to the market. It is approximate by
// construction, because commands are offered without waiting for a result.
type LiveStats struct {
	Offered atomic.Int64
	Dropped atomic.Int64
	Wakeups atomic.Int64
}

// Live runs each bot on its own goroutine and its own wall-clock interval.
//
// This is the realistic runner and the non-reproducible one. Several goroutines race
// to reach the market, and which of them arrives first is up to the Go scheduler, so
// two live runs with identical seeds will generally not produce identical sessions.
// That is not a defect to be fixed; it is what concurrent participants are. The
// market still converts whatever race occurs into one total order, and replaying the
// recorded log of a specific run reproduces that run exactly.
//
// If you want reproducibility from seeds alone, use Driver instead.
type Live struct {
	ex        Exchange
	inst      instrument.Instrument
	schedules []Schedule
	stats     LiveStats
}

// NewLive builds a live runner.
func NewLive(ex Exchange, inst instrument.Instrument, schedules ...Schedule) *Live {
	return &Live{ex: ex, inst: inst, schedules: schedules}
}

// Stats returns pointers-free snapshots of the counters.
func (l *Live) Stats() (offered, dropped, wakeups int64) {
	return l.stats.Offered.Load(), l.stats.Dropped.Load(), l.stats.Wakeups.Load()
}

// Run starts one goroutine per bot and blocks until ctx is canceled.
func (l *Live) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, s := range l.schedules {
		if s.Interval <= 0 {
			s.Interval = 50 * time.Millisecond
		}
		wg.Add(1)
		go func(s Schedule) {
			defer wg.Done()
			l.runBot(ctx, s)
		}(s)
	}
	wg.Wait()
}

func (l *Live) runBot(ctx context.Context, s Schedule) {
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		l.stats.Wakeups.Add(1)

		book, err := l.ex.Snapshot(ctx)
		if err != nil {
			return
		}
		open, err := l.ex.OpenOrders(ctx, s.Bot.Participant())
		if err != nil {
			return
		}

		for _, cmd := range s.Bot.Decide(View{Instrument: l.inst, Book: book, Open: open}) {
			if cmd.ParticipantID != s.Bot.Participant() {
				// Same defect as in Driver, but a live bot has no error path to
				// return on, so drop the command rather than submit it under an
				// identity the bot does not own.
				l.stats.Dropped.Add(1)
				continue
			}
			// Offered, not submitted: a bot that blocked waiting for room would
			// only accumulate quote updates that are already stale by the time
			// they run, and it must never apply backpressure to matching. A
			// dropped update is harmless because the maker reconstructs its whole
			// ladder from scratch on its next wakeup.
			if err := l.ex.TryEnqueue(cmd); err != nil {
				l.stats.Dropped.Add(1)
				continue
			}
			l.stats.Offered.Add(1)
		}
	}
}
