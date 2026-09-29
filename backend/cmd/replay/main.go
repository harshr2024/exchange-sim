// Command replay rebuilds a market from a recorded session log.
//
// It replays the log into a fresh engine, prints the resulting book, tape and
// digest, and then replays it a second time to confirm both runs agree. Two
// independent replays producing the same digest is the check that the log is a
// complete description of the session: if anything outside the command stream
// influenced matching, the digests would drift.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/display"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func main() {
	file := flag.String("file", "", "session log to replay (required)")
	symbol := flag.String("symbol", "", "market symbol; inferred from the file name when omitted")
	tape := flag.Int("tape", 10, "how many recent trades to print")
	state := flag.Bool("state", false, "print the full canonical state instead of a summary")
	flag.Parse()

	if *file == "" {
		flag.Usage()
		os.Exit(2)
	}

	sym := *symbol
	if sym == "" {
		var err error
		if sym, err = symbolFromPath(*file); err != nil {
			log.Fatalf("replay: %v", err)
		}
	}

	if err := run(sym, *file, *tape, *state); err != nil {
		log.Fatalf("replay: %v", err)
	}
}

func run(symbol, path string, tapeLines int, printState bool) error {
	inst, err := instrument.Lookup(symbol)
	if err != nil {
		return err
	}

	records, err := market.ReadRecordsFile(path)
	if err != nil {
		return err
	}

	first, err := market.Replay(inst.Symbol, records)
	if err != nil {
		return err
	}
	second, err := market.Replay(inst.Symbol, records)
	if err != nil {
		return fmt.Errorf("second replay: %w", err)
	}

	out := os.Stdout
	if printState {
		fmt.Fprint(out, first.Market.CanonicalState())
		return verify(first, second)
	}

	display.Rule(out, "replayed book")
	display.Book(out, inst, first.Market.Snapshot())

	display.Rule(out, "recent trades")
	display.Tape(out, inst, first.Market.RecentTrades(tapeLines))

	display.Rule(out, "resting orders")
	display.OpenOrders(out, inst, first.Market.OpenOrders(""))

	display.Rule(out, "summary")
	fmt.Fprintf(out, "  log            %s\n", path)
	fmt.Fprintf(out, "  commands       %d\n", first.Records)
	fmt.Fprintf(out, "  final sequence %d\n", first.Sequence)
	fmt.Fprintf(out, "  trades         %d\n", first.Trades)
	fmt.Fprintf(out, "  digest         %s\n", first.Digest)

	return verify(first, second)
}

func verify(first, second market.ReplayResult) error {
	if first.Digest != second.Digest {
		return fmt.Errorf("two replays of the same log disagree:\n  %s\n  %s", first.Digest, second.Digest)
	}
	if err := first.Market.CheckInvariants(); err != nil {
		return fmt.Errorf("replayed market violates its invariants: %w", err)
	}
	fmt.Fprintf(os.Stdout, "  determinism    ok (two independent replays agree)\n")
	return nil
}

// symbolFromPath recovers the symbol from a "session-NOVA.ndjson" file name.
func symbolFromPath(path string) (string, error) {
	name := strings.TrimSuffix(filepath.Base(path), ".ndjson")
	sym, ok := strings.CutPrefix(name, "session-")
	if !ok || sym == "" {
		return "", fmt.Errorf("cannot infer a symbol from %q; pass -symbol", filepath.Base(path))
	}
	return sym, nil
}
