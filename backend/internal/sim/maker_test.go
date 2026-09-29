package sim

import (
	"testing"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func TestMakerQuotesBothSidesOfAnEmptyBook(t *testing.T) {
	r := newRig(t)
	maker := NewMarketMaker(MakerConfig{Levels: 3, Size: 100, HalfSpread: 2, LevelStep: 1})
	driver := NewDriver(r.actor, r.inst, maker)

	if err := driver.Run(r.ctx, 1); err != nil {
		t.Fatalf("running one tick: %v", err)
	}

	snap, err := r.actor.Snapshot(r.ctx)
	if err != nil {
		t.Fatalf("reading the book: %v", err)
	}
	if len(snap.Bids) != 3 || len(snap.Asks) != 3 {
		t.Fatalf("book has %d bid and %d ask levels, want 3 each", len(snap.Bids), len(snap.Asks))
	}

	// Quoted around the instrument's bootstrap price, since the market had nothing
	// else to offer.
	ref := r.inst.InitialReferencePrice
	if snap.BestBid != ref-2 || snap.BestAsk != ref+2 {
		t.Fatalf("top of book is %d / %d, want %d / %d", snap.BestBid, snap.BestAsk, ref-2, ref+2)
	}
	if snap.Spread != 4 {
		t.Fatalf("quoted spread is %d ticks, want 4", snap.Spread)
	}

	// Quoting is not trading. The reference price must not have leaked into the
	// displayed last price.
	if snap.HasLastTrade {
		t.Fatalf("last traded price is set to %d although nothing traded", snap.LastTradePrice)
	}
	r.checkInvariants()
}

// TestMakerLeavesHealthyQuotesAlone checks the maker does not churn for its own
// sake: with an unchanged ladder and undisturbed quotes it should do nothing.
func TestMakerLeavesHealthyQuotesAlone(t *testing.T) {
	r := newRig(t)
	maker := NewMarketMaker(MakerConfig{Levels: 3, Size: 100, HalfSpread: 2, LevelStep: 1})
	driver := NewDriver(r.actor, r.inst, maker)

	if err := driver.Run(r.ctx, 1); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	after := driver.Stats().Commands
	if after != 6 {
		t.Fatalf("first tick sent %d commands, want 6", after)
	}

	if err := driver.Run(r.ctx, 5); err != nil {
		t.Fatalf("later ticks: %v", err)
	}
	if extra := driver.Stats().Commands - after; extra != 0 {
		t.Fatalf("five idle ticks sent %d further commands, want 0", extra)
	}
}

// TestMakerReplacesDepletedQuotes checks the cancel/replace path fires when a quote
// has been partly eaten, which is the maker's main reason to exist.
func TestMakerReplacesDepletedQuotes(t *testing.T) {
	r := newRig(t)
	maker := NewMarketMaker(MakerConfig{
		Levels: 1, Size: 100, HalfSpread: 2, LevelStep: 1, RefillBelow: 0.5,
	})
	driver := NewDriver(r.actor, r.inst, maker)

	if err := driver.Run(r.ctx, 1); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	open, err := r.actor.OpenOrders(r.ctx, "bot-mm")
	if err != nil {
		t.Fatalf("reading quotes: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("maker has %d quotes, want 2", len(open))
	}
	var askID engine.OrderID
	for _, o := range open {
		if o.Side == engine.Sell {
			askID = o.ID
		}
	}

	// Take 60 of the 100-share ask, leaving 40, which is below half of the target.
	if ack, err := r.actor.Submit(r.ctx, market.MarketOrder("human", engine.Buy, 60)); err != nil || !ack.Accepted {
		t.Fatalf("taking liquidity: %v %s", err, ack.Rejection)
	}

	before := driver.Stats().Commands
	if err := driver.Run(r.ctx, 1); err != nil {
		t.Fatalf("refresh tick: %v", err)
	}
	if sent := driver.Stats().Commands - before; sent != 2 {
		t.Fatalf("refresh tick sent %d commands, want a cancel and a replacement", sent)
	}

	open, err = r.actor.OpenOrders(r.ctx, "bot-mm")
	if err != nil {
		t.Fatalf("reading quotes: %v", err)
	}
	for _, o := range open {
		if o.ID == askID {
			t.Fatal("the depleted quote was not replaced")
		}
		if o.Side == engine.Sell && o.RemainingQty != 100 {
			t.Fatalf("replacement ask carries %d shares, want the full 100", o.RemainingQty)
		}
	}
	r.checkInvariants()
}

// TestMakerDoesNotTakeLiquidity runs the maker on its own against pre-existing
// one-sided liquidity. A liquidity provider that crossed would be taking, and with
// no self-trade prevention it could also match its own quote on the other side.
func TestMakerDoesNotTakeLiquidity(t *testing.T) {
	r := newRig(t)

	// A resting ask well inside where the maker will want to quote.
	if ack, err := r.actor.Submit(r.ctx, market.LimitOrder("human", engine.Sell, 9995, 500)); err != nil || !ack.Accepted {
		t.Fatalf("seeding the ask: %v %s", err, ack.Rejection)
	}

	maker := NewMarketMaker(MakerConfig{Levels: 3, Size: 100, HalfSpread: 2, LevelStep: 1, SizeJitter: 20})
	driver := NewDriver(r.actor, r.inst, maker)
	if err := driver.Run(r.ctx, 200); err != nil {
		t.Fatalf("running the simulation: %v", err)
	}

	if trades := driver.Stats().Trades; trades != 0 {
		t.Fatalf("the maker caused %d trades while running alone", trades)
	}
	r.checkInvariants()
}

// TestMakerLadderShiftsRatherThanCrossing exercises the case the integration test
// cannot reach easily: a reference price stranded above the current best ask, which
// happens when the market has traded and then one side emptied out.
func TestMakerLadderShiftsRatherThanCrossing(t *testing.T) {
	inst, err := instrument.Lookup("NOVA")
	if err != nil {
		t.Fatalf("looking up NOVA: %v", err)
	}
	maker := NewMarketMaker(MakerConfig{Levels: 3, Size: 100, HalfSpread: 2, LevelStep: 1})

	// No bid, so there is no mid and the reference falls back to the last trade at
	// 100.50 even though the best ask has dropped to 100.00.
	view := View{
		Instrument: inst,
		Book: engine.BookSnapshot{
			Symbol:         inst.Symbol,
			Asks:           []engine.LevelView{{Price: 10000, Quantity: 50, Orders: 1}},
			BestAsk:        10000,
			HasBestAsk:     true,
			LastTradePrice: 10050,
			HasLastTrade:   true,
		},
	}
	if got := view.Reference(); got != 10050 {
		t.Fatalf("reference is %d, want the last trade at 10050", got)
	}

	cmds := maker.Decide(view)
	if len(cmds) != 6 {
		t.Fatalf("maker emitted %d commands, want 6", len(cmds))
	}

	for _, cmd := range cmds {
		if err := cmd.Validate(); err != nil {
			t.Fatalf("%+v is invalid: %v", cmd, err)
		}
		if cmd.Side == engine.Buy && cmd.LimitPrice >= 10000 {
			t.Fatalf("maker bid at %d would cross the 10000 ask", cmd.LimitPrice)
		}
	}

	// The bid ladder is shifted to sit just under the touch, still three distinct
	// levels rather than three copies of the same clamped price.
	seen := map[engine.PriceTicks]bool{}
	for _, cmd := range cmds {
		if cmd.Side != engine.Buy {
			continue
		}
		if seen[cmd.LimitPrice] {
			t.Fatalf("maker quoted price %d twice", cmd.LimitPrice)
		}
		seen[cmd.LimitPrice] = true
	}
	for _, want := range []engine.PriceTicks{9999, 9998, 9997} {
		if !seen[want] {
			t.Fatalf("bid ladder is %v, want it to include %d", seen, want)
		}
	}
}

func TestMakerSizeJitterIsSeeded(t *testing.T) {
	inst, err := instrument.Lookup("NOVA")
	if err != nil {
		t.Fatalf("looking up NOVA: %v", err)
	}
	view := View{Instrument: inst}

	first := NewMarketMaker(MakerConfig{Levels: 2, Size: 100, SizeJitter: 50, Seed: 9}).Decide(view)
	second := NewMarketMaker(MakerConfig{Levels: 2, Size: 100, SizeJitter: 50, Seed: 9}).Decide(view)

	if len(first) == 0 {
		t.Fatal("maker emitted nothing against an empty book")
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("command %d differs between identically seeded makers:\n  %+v\n  %+v", i, first[i], second[i])
		}
	}

	// And the jitter is actually doing something.
	varied := false
	for _, cmd := range first {
		if cmd.Quantity != 100 {
			varied = true
		}
	}
	if !varied {
		t.Fatal("size jitter never varied the quoted size")
	}
}
