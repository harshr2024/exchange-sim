package sim

import (
	"context"
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// rig is one market, its actor, its journal and its event recorder, wired together
// the way a running exchange would be.
type rig struct {
	t        *testing.T
	inst     instrument.Instrument
	actor    *market.Actor
	journal  *market.MemJournal
	recorder *market.Recorder
	ctx      context.Context
}

func newRig(t *testing.T) *rig {
	t.Helper()

	inst, err := instrument.Lookup("NOVA")
	if err != nil {
		t.Fatalf("looking up NOVA: %v", err)
	}
	journal := market.NewMemJournal()
	recorder := market.NewRecorder()

	m, err := market.New(inst.Symbol, market.WithJournal(journal), market.WithSink(recorder))
	if err != nil {
		t.Fatalf("creating market: %v", err)
	}

	actor := market.NewActor(m, 0)
	ctx, cancel := context.WithCancel(context.Background())
	go actor.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-actor.Done():
		case <-time.After(5 * time.Second):
			t.Error("market actor did not stop within 5s")
		}
	})

	return &rig{t: t, inst: inst, actor: actor, journal: journal, recorder: recorder, ctx: ctx}
}

// digest returns the live market's canonical state digest.
func (r *rig) digest() string {
	r.t.Helper()
	d, err := r.actor.Digest(r.ctx)
	if err != nil {
		r.t.Fatalf("reading digest: %v", err)
	}
	return d
}

// checkInvariants asserts every engine and market invariant still holds.
func (r *rig) checkInvariants() {
	r.t.Helper()
	if err := r.actor.CheckInvariants(r.ctx); err != nil {
		r.t.Fatalf("invariant violation: %v", err)
	}
}

// replayDigest rebuilds the session from its recorded log and returns the digest.
func (r *rig) replayDigest() string {
	r.t.Helper()
	res, err := market.Replay(r.inst.Symbol, r.journal.Records())
	if err != nil {
		r.t.Fatalf("replaying the session: %v", err)
	}
	return res.Digest
}

// defaultBots returns a maker and a noise trader seeded from seed.
func defaultBots(seed int64) (*MarketMaker, *NoiseTrader) {
	maker := NewMarketMaker(MakerConfig{
		Levels: 3, Size: 100, HalfSpread: 2, LevelStep: 1, SizeJitter: 20, Seed: seed,
	})
	noise := NewNoiseTrader(NoiseConfig{Seed: seed + 1, MaxQty: 25})
	return maker, noise
}
