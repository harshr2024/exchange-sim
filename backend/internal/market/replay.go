package market

import (
	"fmt"
)

// ReplayResult reports what a replay produced.
type ReplayResult struct {
	Market   *Market
	Records  int
	Sequence uint64
	Trades   int
	Digest   string
}

// Replay rebuilds a market by feeding a recorded command stream into a fresh
// engine.
//
// This works because nothing in the write path depends on anything outside the
// command stream. Order IDs come from a counter, priority comes from the sequence
// number, rejections are a pure function of the command, and no wall-clock time
// is read. So replaying the same commands in the same order reproduces the same
// trades, the same resting orders in the same FIFO positions, and the same final
// sequence.
//
// It is deliberately not a general event-sourcing framework: there are no
// snapshots, no schema migration and no compaction. It replays commands from the
// beginning of the session.
func Replay(symbol string, records []Record, opts ...Option) (ReplayResult, error) {
	// The replayed market records nothing by default: re-journalling the same
	// commands would only duplicate the log it was read from.
	m, err := New(symbol, append([]Option{WithSink(DiscardSink())}, opts...)...)
	if err != nil {
		return ReplayResult{}, err
	}

	for i, rec := range records {
		// The recorded sequence must match the one the fresh market derives.
		// A mismatch means the log was truncated, reordered or interleaved with
		// another market's, and continuing would produce a plausible-looking but
		// wrong book.
		if want := m.Sequence() + 1; rec.Sequence != want {
			return ReplayResult{}, fmt.Errorf(
				"market: session log record %d has sequence %d, expected %d", i, rec.Sequence, want)
		}
		if ack := m.Apply(rec.Command); ack.Sequence != rec.Sequence {
			return ReplayResult{}, fmt.Errorf(
				"market: replaying record %d assigned sequence %d, expected %d", i, ack.Sequence, rec.Sequence)
		}
		if err := m.Halted(); err != nil {
			return ReplayResult{}, fmt.Errorf("market: replay halted at record %d: %w", i, err)
		}
	}

	return ReplayResult{
		Market:   m,
		Records:  len(records),
		Sequence: m.Sequence(),
		Trades:   m.TradeCount(),
		Digest:   m.Digest(),
	}, nil
}

// ReplayFile replays the session log at path.
func ReplayFile(symbol, path string, opts ...Option) (ReplayResult, error) {
	records, err := ReadRecordsFile(path)
	if err != nil {
		return ReplayResult{}, err
	}
	return Replay(symbol, records, opts...)
}
