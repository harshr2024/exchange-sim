package engine

import (
	"math/rand"
	"testing"
)

// These benchmarks measure the matching engine alone: no channels, no journal, no
// serialisation.
//
// None of them pause the timer to set up a book. Stopping and starting the timer
// around a few hundred nanoseconds of work costs more than the work itself and
// distorts what is being measured. Instead each benchmark times a self-contained
// unit of work, and its comment says exactly what that unit includes.
//
// That means these figures are not subtractable from one another: they differ in
// how large the book is while the work happens, and book size is the main thing
// the engine's cost depends on. Compare a benchmark against itself across changes,
// not against its neighbours.
//
// The numbers these produce are a description, not a target. Report what the
// machine actually did.

// builder mints orders with the ID and sequence allocation a market would do.
type builder struct {
	seq uint64
	id  uint64
}

func (b *builder) limit(side Side, price PriceTicks, qty int64) *Order {
	b.seq++
	b.id++
	return NewOrder(OrderID(b.id), b.seq, "bench", "BENCH", side, Limit, price, qty)
}

func (b *builder) market(side Side, qty int64) *Order {
	b.seq++
	b.id++
	return NewOrder(OrderID(b.id), b.seq, "bench", "BENCH", side, Market, 0, qty)
}

// BenchmarkSubmitPassiveLimit times one limit order that cannot match, spread over
// 100 price levels: the level lookup, the FIFO append and the order index write.
//
// Nothing ever leaves this book, so it grows for the whole run and the figure
// includes the order index rehashing and the garbage collector's view of millions
// of live orders. That is the pessimistic end of insertion cost, not the typical
// one; the "resting" metric reports how large the book got.
func BenchmarkSubmitPassiveLimit(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder

	i := 0
	for b.Loop() {
		book.Submit(bld.limit(Buy, PriceTicks(10000-i%100), 10))
		i++
	}
	b.ReportMetric(float64(book.RestingCount()), "resting")
}

// BenchmarkSubmitAndCancel times one passive insert plus the cancellation of that
// same order, which exercises the order index lookup and the FIFO unlink.
//
// Because every order is withdrawn immediately, the book stays near-empty
// throughout, so this comes out faster per command than inserting alone does in
// BenchmarkSubmitPassiveLimit.
func BenchmarkSubmitAndCancel(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder

	i := 0
	for b.Loop() {
		o := bld.limit(Buy, PriceTicks(10000-i%100), 10)
		book.Submit(o)
		if _, err := book.Cancel(o.ID); err != nil {
			b.Fatalf("cancel: %v", err)
		}
		i++
	}
	if book.RestingCount() != 0 {
		b.Fatalf("%d orders left resting", book.RestingCount())
	}
}

// BenchmarkMatchOneLevel times a resting order plus the aggressive order that
// fully consumes it: the common case of a single fill at the touch.
func BenchmarkMatchOneLevel(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder

	trades := 0
	for b.Loop() {
		book.Submit(bld.limit(Sell, 10000, 10))
		trades += len(book.Submit(bld.limit(Buy, 10000, 10)).Trades)
	}
	if book.RestingCount() != 0 {
		b.Fatalf("%d orders left resting", book.RestingCount())
	}
	b.ReportMetric(float64(trades)/float64(b.N), "trades/op")
}

// BenchmarkSweepTenLevels times ten passive inserts at ten distinct prices plus the
// market order that sweeps all ten, so one reported operation is 21 commands' worth
// of work: the expensive matching path, where a single order drains ten levels.
func BenchmarkSweepTenLevels(b *testing.B) {
	const levels = 10
	book := NewBook("BENCH")
	var bld builder

	for b.Loop() {
		for i := 0; i < levels; i++ {
			book.Submit(bld.limit(Sell, PriceTicks(10000+i), 10))
		}
		res := book.Submit(bld.market(Buy, levels*10))
		if len(res.Trades) != levels {
			b.Fatalf("swept %d levels, want %d", len(res.Trades), levels)
		}
	}
	b.ReportMetric(levels, "levels/op")
}

// BenchmarkMixedFlow times a blend of passive orders, aggressive orders and
// cancellations against a book that stays populated on both sides, which is the
// closest of these to what the simulated traders will generate.
func BenchmarkMixedFlow(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder
	rng := rand.New(rand.NewSource(1))

	// Seed depth on both sides so aggressive orders have something to hit from the
	// first iteration. This happens before the timer starts.
	resting := make([]OrderID, 0, 2048)
	for i := 0; i < 500; i++ {
		bid := bld.limit(Buy, PriceTicks(9990-i%20), 25)
		ask := bld.limit(Sell, PriceTicks(10010+i%20), 25)
		book.Submit(bid)
		book.Submit(ask)
		resting = append(resting, bid.ID, ask.ID)
	}

	var trades, cancels int
	b.ResetTimer()
	for b.Loop() {
		side := Buy
		if rng.Intn(2) == 0 {
			side = Sell
		}
		switch n := rng.Intn(100); {
		case n < 20 && len(resting) > 0:
			pick := rng.Intn(len(resting))
			id := resting[pick]
			resting[pick] = resting[len(resting)-1]
			resting = resting[:len(resting)-1]
			// A miss is legitimate: the order may already have been filled.
			if _, err := book.Cancel(id); err == nil {
				cancels++
			}
		case n < 35:
			trades += len(book.Submit(bld.market(side, int64(1+rng.Intn(40)))).Trades)
		default:
			o := bld.limit(side, PriceTicks(10000+rng.Intn(41)-20), int64(1+rng.Intn(40)))
			res := book.Submit(o)
			trades += len(res.Trades)
			if res.Rested {
				resting = append(resting, o.ID)
			}
		}
	}
	b.StopTimer()

	b.ReportMetric(float64(trades)/float64(b.N), "trades/op")
	b.ReportMetric(float64(cancels)/float64(b.N), "cancels/op")
	b.ReportMetric(float64(book.RestingCount()), "resting")
}

// BenchmarkLargeWorkload runs a generated stream of 100k commands through a fresh
// book each iteration, so the reported figure is for the whole workload rather than
// one command. The stream is generated once, before the timer, and each iteration
// builds its own orders from it, because allocating an order per command is part of
// what a real market does.
func BenchmarkLargeWorkload(b *testing.B) {
	const commands = 100_000

	type spec struct {
		side  Side
		typ   OrderType
		price PriceTicks
		qty   int64
	}

	rng := rand.New(rand.NewSource(7))
	stream := make([]spec, commands)
	for i := range stream {
		s := spec{side: Buy, typ: Limit, qty: int64(1 + rng.Intn(40))}
		if rng.Intn(2) == 0 {
			s.side = Sell
		}
		if rng.Intn(100) < 15 {
			s.typ = Market
		} else {
			s.price = PriceTicks(10000 + rng.Intn(41) - 20)
		}
		stream[i] = s
	}

	var trades int
	b.ResetTimer()
	for b.Loop() {
		book := NewBook("BENCH")
		var bld builder
		trades = 0
		for _, s := range stream {
			var o *Order
			if s.typ == Market {
				o = bld.market(s.side, s.qty)
			} else {
				o = bld.limit(s.side, s.price, s.qty)
			}
			trades += len(book.Submit(o).Trades)
		}
	}
	b.StopTimer()

	b.ReportMetric(commands, "commands/op")
	b.ReportMetric(float64(trades), "trades/op")
	// Commands per second for the whole workload, which is the figure worth quoting.
	if elapsed := b.Elapsed().Seconds(); elapsed > 0 {
		b.ReportMetric(float64(b.N)*commands/elapsed, "commands/sec")
	}
}
