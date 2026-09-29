package engine

import (
	"math/rand"
	"testing"
)

// These benchmarks measure the matching engine on its own: no channels, no
// journal, no serialisation. Report whatever they actually produce on the machine
// they ran on; the numbers are not a target.

// builder mints orders with the sequence and ID allocation a market would do,
// without any of the market's other work.
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

// BenchmarkSubmitPassiveLimit measures inserting an order that never matches, so
// the cost is the level lookup, the FIFO append and the index write.
func BenchmarkSubmitPassiveLimit(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder

	for i := 0; b.Loop(); i++ {
		// Spread across 100 price levels so level lookup is not always a hit on
		// the same level.
		book.Submit(bld.limit(Buy, PriceTicks(10000-i%100), 10))
	}
	b.ReportMetric(float64(book.RestingCount()), "resting")
}

// BenchmarkMatchSingleLevel measures the common case: an aggressive order that
// fully fills against one resting order at the top of book.
func BenchmarkMatchSingleLevel(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder

	for b.Loop() {
		book.Submit(bld.limit(Sell, 10000, 10))
		book.Submit(bld.limit(Buy, 10000, 10))
	}
	if book.RestingCount() != 0 {
		b.Fatalf("%d orders left resting", book.RestingCount())
	}
}

// BenchmarkSweepTenLevels measures an aggressive order walking ten price levels,
// which is the expensive path: ten best-price lookups and ten level removals.
func BenchmarkSweepTenLevels(b *testing.B) {
	const levels = 10
	book := NewBook("BENCH")
	var bld builder

	for b.Loop() {
		b.StopTimer()
		for i := 0; i < levels; i++ {
			book.Submit(bld.limit(Sell, PriceTicks(10000+i), 10))
		}
		b.StartTimer()

		res := book.Submit(bld.market(Buy, levels*10))
		if len(res.Trades) != levels {
			b.Fatalf("swept %d levels, want %d", len(res.Trades), levels)
		}
	}
}

// BenchmarkCancel measures cancellation, which goes straight through the order
// index and never scans the book.
func BenchmarkCancel(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder

	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		o := bld.limit(Buy, PriceTicks(10000-i%100), 10)
		book.Submit(o)
		b.StartTimer()

		if _, err := book.Cancel(o.ID); err != nil {
			b.Fatalf("cancel: %v", err)
		}
	}
}

// BenchmarkMixedFlow measures a realistic blend of passive orders, aggressive
// orders and cancellations against a book that stays populated.
func BenchmarkMixedFlow(b *testing.B) {
	book := NewBook("BENCH")
	var bld builder
	rng := rand.New(rand.NewSource(1))

	// Seed a book with depth on both sides so aggressive orders have something
	// to hit from the first iteration.
	resting := make([]OrderID, 0, 1024)
	for i := 0; i < 500; i++ {
		bid := bld.limit(Buy, PriceTicks(9990-i%20), 25)
		ask := bld.limit(Sell, PriceTicks(10010+i%20), 25)
		book.Submit(bid)
		book.Submit(ask)
		resting = append(resting, bid.ID, ask.ID)
	}

	var trades int
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
			book.Cancel(id) //nolint:errcheck // a filled order is a legitimate miss here
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
}

// BenchmarkLargeWorkload runs one generated stream of 100k commands end to end,
// so the reported time is for the whole workload rather than per command.
func BenchmarkLargeWorkload(b *testing.B) {
	const commands = 100_000

	for b.Loop() {
		b.StopTimer()
		book := NewBook("BENCH")
		var bld builder
		rng := rand.New(rand.NewSource(7))
		orders := make([]*Order, 0, commands)
		for i := 0; i < commands; i++ {
			side := Buy
			if rng.Intn(2) == 0 {
				side = Sell
			}
			if rng.Intn(100) < 15 {
				orders = append(orders, bld.market(side, int64(1+rng.Intn(40))))
			} else {
				orders = append(orders, bld.limit(side, PriceTicks(10000+rng.Intn(41)-20), int64(1+rng.Intn(40))))
			}
		}
		b.StartTimer()

		trades := 0
		for _, o := range orders {
			trades += len(book.Submit(o).Trades)
		}

		b.StopTimer()
		b.ReportMetric(float64(commands), "commands/op")
		b.ReportMetric(float64(trades), "trades/op")
		b.StartTimer()
	}
}
