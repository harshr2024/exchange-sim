package sim

import (
	"context"
	"errors"
	"fmt"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// Exchange is the slice of the market actor a runner needs.
//
// Only runners hold one of these. Bots receive a View value and never see this
// interface, so no bot can submit under another identity, read another
// participant's orders, or reach the order book.
type Exchange interface {
	Submit(context.Context, market.Command) (market.Ack, error)
	TryEnqueue(market.Command) error
	Snapshot(context.Context) (engine.BookSnapshot, error)
	OpenOrders(context.Context, string) ([]engine.View, error)
}

// ErrMisattributedCommand is returned when a bot emits a command under an identity
// that is not its own.
var ErrMisattributedCommand = errors.New("sim: bot emitted a command for another participant")

// Stats summarises what a run produced.
type Stats struct {
	Ticks      int
	Commands   int
	Accepted   int
	Rejected   int
	Trades     int
	Filled     int64
	Canceled   int64
	Rejections map[string]int
}

func (s *Stats) note(ack market.Ack) {
	s.Commands++
	if !ack.Accepted {
		s.Rejected++
		if s.Rejections == nil {
			s.Rejections = make(map[string]int)
		}
		s.Rejections[ack.Rejection]++
		return
	}
	s.Accepted++
	s.Trades += len(ack.Trades)
	for _, t := range ack.Trades {
		s.Filled += t.Quantity
	}
	s.Canceled += ack.CanceledQty
}

// Driver polls bots from a single goroutine in a fixed order.
//
// This is the reproducible runner. Each tick consults every bot once, in the order
// they were registered, and submits each returned command synchronously before
// moving on. Nothing about the resulting command stream depends on the Go
// scheduler, so the same bots with the same seeds over the same number of ticks
// produce the same session every time.
//
// That is a stronger guarantee than the Live runner gives, and it is the reason
// this exists separately: reproducibility here comes from the seeds, whereas
// reproducing a concurrent run requires its recorded log.
type Driver struct {
	ex    Exchange
	inst  instrument.Instrument
	bots  []Bot
	stats Stats
}

// NewDriver registers bots in polling order.
func NewDriver(ex Exchange, inst instrument.Instrument, bots ...Bot) *Driver {
	return &Driver{ex: ex, inst: inst, bots: bots}
}

// Stats returns the running totals.
func (d *Driver) Stats() Stats { return d.stats }

// Tick consults every bot once.
func (d *Driver) Tick(ctx context.Context) error {
	for _, bot := range d.bots {
		view, err := d.view(ctx, bot.Participant())
		if err != nil {
			return err
		}

		for _, cmd := range bot.Decide(view) {
			// A bot that attributed a command to someone else would be a defect,
			// not a rejection to absorb: the market would refuse the cancel, but
			// the bot would have revealed it is reasoning about orders it does not
			// own. Fail loudly instead.
			if cmd.ParticipantID != bot.Participant() {
				return fmt.Errorf("%w: %q emitted a command for %q",
					ErrMisattributedCommand, bot.Participant(), cmd.ParticipantID)
			}

			ack, err := d.ex.Submit(ctx, cmd)
			if err != nil {
				return err
			}
			d.stats.note(ack)
		}
	}
	d.stats.Ticks++
	return nil
}

// Run advances the simulation by ticks.
func (d *Driver) Run(ctx context.Context, ticks int) error {
	for range ticks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.Tick(ctx); err != nil {
			return err
		}
	}
	return nil
}

// RunUntilCommands advances the simulation until at least n commands have been
// submitted, which makes a run comparable across bot configurations that differ in
// how often they act.
func (d *Driver) RunUntilCommands(ctx context.Context, n int) error {
	for d.stats.Commands < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := d.stats.Commands
		if err := d.Tick(ctx); err != nil {
			return err
		}
		// Bots that never act would otherwise spin here forever.
		if d.stats.Commands == before && d.stats.Ticks > n*1000 {
			return fmt.Errorf("sim: %d ticks produced only %d commands", d.stats.Ticks, d.stats.Commands)
		}
	}
	return nil
}

// view builds the read-only state one bot is allowed to see.
func (d *Driver) view(ctx context.Context, participant string) (View, error) {
	book, err := d.ex.Snapshot(ctx)
	if err != nil {
		return View{}, err
	}
	// Scoped to the bot: it cannot learn another participant's order IDs, and the
	// aggregated book carries no order identities at all.
	open, err := d.ex.OpenOrders(ctx, participant)
	if err != nil {
		return View{}, err
	}
	return View{Instrument: d.inst, Book: book, Open: open}, nil
}
