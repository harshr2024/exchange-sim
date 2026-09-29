package sim

import (
	"math/rand"
	"testing"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func novaInstrument(t *testing.T) instrument.Instrument {
	t.Helper()
	inst, err := instrument.Lookup("NOVA")
	if err != nil {
		t.Fatalf("looking up NOVA: %v", err)
	}
	return inst
}

// TestNoiseTraderIsReproducibleGivenTheSameViews isolates the bot from the runner:
// fed an identical sequence of views, two identically seeded traders must decide
// identically. This is what the deterministic Driver relies on.
func TestNoiseTraderIsReproducibleGivenTheSameViews(t *testing.T) {
	inst := novaInstrument(t)
	views := syntheticViews(inst, 500)

	first := NewNoiseTrader(NoiseConfig{Seed: 17, MaxQty: 25})
	second := NewNoiseTrader(NoiseConfig{Seed: 17, MaxQty: 25})

	decisions := 0
	for i, v := range views {
		a, b := first.Decide(v), second.Decide(v)
		if len(a) != len(b) {
			t.Fatalf("view %d: traders emitted %d and %d commands", i, len(a), len(b))
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("view %d command %d differs:\n  %+v\n  %+v", i, j, a[j], b[j])
			}
		}
		decisions += len(a)
	}
	if decisions == 0 {
		t.Fatal("the trader never acted, so the comparison is vacuous")
	}

	// A different seed must produce a different stream, or the check above proves
	// nothing about the seeding.
	other := NewNoiseTrader(NoiseConfig{Seed: 18, MaxQty: 25})
	differs := false
	replay := NewNoiseTrader(NoiseConfig{Seed: 17, MaxQty: 25})
	for _, v := range views {
		a, b := replay.Decide(v), other.Decide(v)
		if len(a) != len(b) || (len(a) == 1 && a[0] != b[0]) {
			differs = true
			break
		}
	}
	if !differs {
		t.Fatal("a different seed produced the same decisions")
	}
}

func TestNoiseTraderOnlyEmitsValidCommands(t *testing.T) {
	inst := novaInstrument(t)
	trader := NewNoiseTrader(NoiseConfig{Seed: 4, MaxQty: 25})

	kinds := map[market.Kind]int{}
	for i, v := range syntheticViews(inst, 4000) {
		for _, cmd := range trader.Decide(v) {
			if err := cmd.Validate(); err != nil {
				t.Fatalf("view %d: %+v is invalid: %v", i, cmd, err)
			}
			if cmd.ParticipantID != trader.Participant() {
				t.Fatalf("view %d: command attributed to %q", i, cmd.ParticipantID)
			}
			kinds[cmd.Kind]++
		}
	}

	// It has to produce a genuine mixture, otherwise it is not order flow.
	for _, kind := range []market.Kind{market.SubmitLimit, market.SubmitMarket, market.Cancel} {
		if kinds[kind] == 0 {
			t.Fatalf("the trader never emitted %s over 4000 decisions: %v", kind, kinds)
		}
	}
	t.Logf("command mix: %v", kinds)
}

// TestNoiseTraderDoesNotAggressAnEmptySide checks it falls back to providing
// liquidity rather than firing market orders into a void.
func TestNoiseTraderDoesNotAggressAnEmptySide(t *testing.T) {
	inst := novaInstrument(t)
	trader := NewNoiseTrader(NoiseConfig{Seed: 21, MaxQty: 25, ActPercent: 100})

	empty := View{Instrument: inst, Book: engine.BookSnapshot{Symbol: inst.Symbol}}
	markets := 0
	for range 500 {
		for _, cmd := range trader.Decide(empty) {
			if cmd.Kind == market.SubmitMarket {
				markets++
			}
			if err := cmd.Validate(); err != nil {
				t.Fatalf("%+v is invalid: %v", cmd, err)
			}
			// With nothing resting, a passive order must be priced off the
			// instrument's reference and must stay positive.
			if cmd.Kind == market.SubmitLimit && cmd.LimitPrice <= 0 {
				t.Fatalf("passive order priced at %d", cmd.LimitPrice)
			}
		}
	}
	if markets != 0 {
		t.Fatalf("the trader sent %d market orders into an empty book", markets)
	}
}

// TestNoiseTraderCancelsOnlyItsOwnOrders checks it never names an order it was not
// shown, given that its view only ever contains its own.
func TestNoiseTraderCancelsOnlyItsOwnOrders(t *testing.T) {
	inst := novaInstrument(t)
	trader := NewNoiseTrader(NoiseConfig{Seed: 33, MaxQty: 25, ActPercent: 100, CancelPercent: 60})

	own := map[engine.OrderID]bool{11: true, 12: true, 13: true}
	view := View{
		Instrument: inst,
		Book: engine.BookSnapshot{
			Symbol:  inst.Symbol,
			Bids:    []engine.LevelView{{Price: 9990, Quantity: 100, Orders: 1}},
			Asks:    []engine.LevelView{{Price: 10010, Quantity: 100, Orders: 1}},
			BestBid: 9990, HasBestBid: true,
			BestAsk: 10010, HasBestAsk: true,
		},
		Open: []engine.View{
			{ID: 11, ParticipantID: "bot-noise", Side: engine.Buy, LimitPrice: 9990, RemainingQty: 10},
			{ID: 12, ParticipantID: "bot-noise", Side: engine.Sell, LimitPrice: 10010, RemainingQty: 10},
			{ID: 13, ParticipantID: "bot-noise", Side: engine.Buy, LimitPrice: 9985, RemainingQty: 10},
		},
	}

	cancels := 0
	for range 500 {
		for _, cmd := range trader.Decide(view) {
			if cmd.Kind != market.Cancel {
				continue
			}
			cancels++
			if !own[cmd.OrderID] {
				t.Fatalf("the trader tried to cancel order %d, which it does not own", cmd.OrderID)
			}
		}
	}
	if cancels == 0 {
		t.Fatal("the trader never cancelled anything, so the check is vacuous")
	}
}

// syntheticViews builds a deterministic sequence of plausible market views, so bot
// decisions can be tested without running a market.
func syntheticViews(inst instrument.Instrument, n int) []View {
	rng := rand.New(rand.NewSource(1))
	out := make([]View, 0, n)

	for i := range n {
		mid := engine.PriceTicks(10000 + rng.Intn(41) - 20)
		book := engine.BookSnapshot{Symbol: inst.Symbol, Sequence: uint64(i)}

		// Every twentieth view is a one-sided or empty book, so the bots' fallback
		// paths get exercised too.
		switch i % 20 {
		case 0:
			// Empty book.
		case 1:
			book.BestAsk, book.HasBestAsk = mid+2, true
			book.Asks = []engine.LevelView{{Price: mid + 2, Quantity: 100, Orders: 1}}
		case 2:
			book.BestBid, book.HasBestBid = mid-2, true
			book.Bids = []engine.LevelView{{Price: mid - 2, Quantity: 100, Orders: 1}}
		default:
			book.BestBid, book.HasBestBid = mid-2, true
			book.BestAsk, book.HasBestAsk = mid+2, true
			book.Bids = []engine.LevelView{{Price: mid - 2, Quantity: 100, Orders: 1}}
			book.Asks = []engine.LevelView{{Price: mid + 2, Quantity: 100, Orders: 1}}
			book.Mid, book.HasMid = mid, true
			book.Spread, book.HasSpread = 4, true
			book.LastTradePrice, book.HasLastTrade = mid, true
		}

		var open []engine.View
		for j := range rng.Intn(4) {
			open = append(open, engine.View{
				ID:            engine.OrderID(1000 + i*10 + j),
				ParticipantID: "bot-noise",
				Side:          engine.Buy,
				LimitPrice:    mid - 5,
				RemainingQty:  10,
			})
		}
		out = append(out, View{Instrument: inst, Book: book, Open: open})
	}
	return out
}
