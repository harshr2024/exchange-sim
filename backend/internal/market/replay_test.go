package market

import (
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
)

// TestReplayReproducesTheSessionExactly is the project's central claim: given the
// same sequenced command stream, a fresh engine produces the same trades and the
// same final book.
//
// The comparison is a digest of the market's full canonical state, not of the
// published snapshot. That covers every price level rather than the top ten, and
// every resting order's ID, owner, priority sequence and quantities rather than
// just the level totals, plus the entire trade history. Two markets can agree on
// a snapshot while disagreeing about which resting order is next in line; they
// cannot agree on this digest without being genuinely identical.
func TestReplayReproducesTheSessionExactly(t *testing.T) {
	for _, seed := range []int64{3, 11, 2024} {
		t.Run("", func(t *testing.T) {
			journal := NewMemJournal()
			live := newTestMarket(t, WithJournal(journal), WithSink(NewRecorder()))

			for _, cmd := range randomCommandStream(seed, 3000) {
				live.Apply(cmd)
			}
			if live.TradeCount() == 0 {
				t.Fatalf("seed %d produced no trades, so the comparison is vacuous", seed)
			}

			replayed, err := Replay("NOVA", journal.Records())
			if err != nil {
				t.Fatalf("replay: %v", err)
			}

			if replayed.Digest != live.Digest() {
				t.Fatalf("replay diverged:\n%s", diffCanonical(live.CanonicalState(), replayed.Market.CanonicalState()))
			}
			if replayed.Sequence != live.Sequence() {
				t.Fatalf("replay ended at sequence %d, live market at %d", replayed.Sequence, live.Sequence())
			}
			if replayed.Trades != live.TradeCount() {
				t.Fatalf("replay printed %d trades, live market printed %d", replayed.Trades, live.TradeCount())
			}

			// Spelled out as well as digested, so a failure says which part drifted.
			assertSameTrades(t, live.RecentTrades(0), replayed.Market.RecentTrades(0))
			assertSameOpenOrders(t, live.OpenOrders(""), replayed.Market.OpenOrders(""))
			if !reflect.DeepEqual(live.Snapshot(), replayed.Market.Snapshot()) {
				t.Fatalf("snapshots differ:\nlive:     %+v\nreplayed: %+v", live.Snapshot(), replayed.Market.Snapshot())
			}
			if err := replayed.Market.CheckInvariants(); err != nil {
				t.Fatalf("replayed market violates its invariants: %v", err)
			}

			t.Logf("seed %d: %d records, %d trades, %d orders resting, digest %s",
				seed, replayed.Records, replayed.Trades, len(replayed.Market.OpenOrders("")), replayed.Digest[:16])
		})
	}
}

func TestReplayFromFileJournalReproducesTheSession(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenFileJournal(dir, "NOVA")
	if err != nil {
		t.Fatalf("opening session log: %v", err)
	}

	live := newTestMarket(t, WithJournal(journal))
	for _, cmd := range randomCommandStream(5, 1500) {
		live.Apply(cmd)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("closing session log: %v", err)
	}

	path := filepath.Join(dir, SessionFileName("NOVA"))
	if want := journal.Path(); path != want {
		t.Fatalf("session log path is %q, want %q", want, path)
	}

	replayed, err := ReplayFile("NOVA", path)
	if err != nil {
		t.Fatalf("replaying %s: %v", path, err)
	}
	if replayed.Digest != live.Digest() {
		t.Fatalf("replay from disk diverged:\n%s",
			diffCanonical(live.CanonicalState(), replayed.Market.CanonicalState()))
	}
	if replayed.Records != int(live.Sequence()) {
		t.Fatalf("log holds %d records for %d sequenced commands", replayed.Records, live.Sequence())
	}
}

// TestReplayReproducesRejections checks that the log is a faithful record of what
// the market was asked to do, not just of what it agreed to do. Rejected commands
// consume sequence numbers, so a replay that silently skipped them would mint
// different order IDs from that point on.
func TestReplayReproducesRejections(t *testing.T) {
	journal := NewMemJournal()
	live := newTestMarket(t, WithJournal(journal))

	live.Apply(Command{Kind: SubmitLimit, ParticipantID: "human", Side: engine.Buy, Quantity: 0}) // rejected
	live.Apply(LimitOrder("human", engine.Sell, 10000, 10))
	live.Apply(Command{Kind: "submit_stop", ParticipantID: "human"}) // rejected
	live.Apply(LimitOrder("human", engine.Buy, 10000, 4))

	records := journal.Records()
	if len(records) != 4 {
		t.Fatalf("log holds %d records, want all 4 commands including the rejected ones", len(records))
	}

	replayed, err := Replay("NOVA", records)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Digest != live.Digest() {
		t.Fatalf("replay diverged:\n%s", diffCanonical(live.CanonicalState(), replayed.Market.CanonicalState()))
	}
	if replayed.Sequence != 4 {
		t.Fatalf("replay ended at sequence %d, want 4", replayed.Sequence)
	}
	if open := replayed.Market.OpenOrders("human"); len(open) != 1 || open[0].ID != 1 {
		t.Fatalf("replayed market holds %+v, want only order 1 resting", open)
	}
}

func TestReplayRejectsDamagedLogs(t *testing.T) {
	journal := NewMemJournal()
	live := newTestMarket(t, WithJournal(journal))
	for _, cmd := range randomCommandStream(13, 200) {
		live.Apply(cmd)
	}
	records := journal.Records()

	t.Run("gap", func(t *testing.T) {
		damaged := append([]Record(nil), records...)
		damaged = append(damaged[:50], damaged[51:]...)
		if _, err := Replay("NOVA", damaged); err == nil {
			t.Fatal("replay accepted a log with a missing record")
		}
	})

	t.Run("reordered", func(t *testing.T) {
		damaged := append([]Record(nil), records...)
		damaged[10], damaged[11] = damaged[11], damaged[10]
		if _, err := Replay("NOVA", damaged); err == nil {
			t.Fatal("replay accepted a reordered log")
		}
	})

	t.Run("duplicated", func(t *testing.T) {
		damaged := append([]Record(nil), records[:20]...)
		damaged = append(damaged, records[19])
		if _, err := Replay("NOVA", damaged); err == nil {
			t.Fatal("replay accepted a log with a duplicated record")
		}
	})

	t.Run("truncated is a valid prefix", func(t *testing.T) {
		// Truncation is not corruption: a prefix of the log is a legitimate
		// earlier point in the session and must replay cleanly.
		res, err := Replay("NOVA", records[:100])
		if err != nil {
			t.Fatalf("replaying a log prefix: %v", err)
		}
		if res.Sequence != 100 {
			t.Fatalf("prefix replay ended at sequence %d, want 100", res.Sequence)
		}
	})
}

// TestDigestDetectsADifferentOrdering guards the replay test itself: if the digest
// were insensitive to what actually happened, the replay assertion would pass for
// the wrong reason.
func TestDigestDetectsADifferentOrdering(t *testing.T) {
	stream := randomCommandStream(21, 400)

	forward := newTestMarket(t)
	for _, cmd := range stream {
		forward.Apply(cmd)
	}

	reversed := newTestMarket(t)
	for i := len(stream) - 1; i >= 0; i-- {
		reversed.Apply(stream[i])
	}

	if forward.Digest() == reversed.Digest() {
		t.Fatal("the same commands in reverse order produced the same digest")
	}
}

// TestFIFOPrioritySurvivesReplay pins down the subtle half of determinism: not
// just that the same levels come back, but that orders come back in the same
// queue positions, so the next incoming order fills the same one.
func TestFIFOPrioritySurvivesReplay(t *testing.T) {
	journal := NewMemJournal()
	live := newTestMarket(t, WithJournal(journal))

	for i := 0; i < 5; i++ {
		live.Apply(LimitOrder("maker", engine.Sell, 10000, 10))
	}
	live.Apply(CancelOrder("maker", 3)) // punch a hole in the middle of the queue

	replayed, err := Replay("NOVA", journal.Records())
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	liveNext := live.Apply(MarketOrder("taker", engine.Buy, 25))
	replayedNext := replayed.Market.Apply(MarketOrder("taker", engine.Buy, 25))

	assertSameTrades(t, liveNext.Trades, replayedNext.Trades)
	if live.Digest() != replayed.Market.Digest() {
		t.Fatalf("markets diverged after an identical follow-up order:\n%s",
			diffCanonical(live.CanonicalState(), replayed.Market.CanonicalState()))
	}
}

func TestReadRecordsRejectsMalformedLines(t *testing.T) {
	if _, err := ReadRecords(strings.NewReader("{\"seq\":1}\nnot json\n")); err == nil {
		t.Fatal("ReadRecords accepted a malformed line")
	}
}

func assertSameTrades(t *testing.T, want, got []engine.Trade) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d trades, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trade %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

func assertSameOpenOrders(t *testing.T, want, got []engine.View) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d resting orders, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resting order %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

// diffCanonical returns the first differing line of two canonical states, which is
// far more useful in a failure message than two multi-thousand-line dumps.
func diffCanonical(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")

	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return "line " + strconv.Itoa(i+1) + " of the canonical state:\n  live:     " + w + "\n  replayed: " + g
		}
	}
	return "states are identical but digests differed"
}
