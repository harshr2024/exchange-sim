package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

// Steady-state benchmarks: book depth is held constant so the question "how does
// this operation behave as the book gets deeper?" actually has an answer.
//
// The benchmarks in bench_test.go each time a self-contained unit of work, which
// makes them honest but leaves book size varying between them — and book size is the
// main thing cost depends on. These fix the book at a known number of resting orders
// and keep it there for the whole run, so the same operation can be compared across
// 1k, 10k and 100k resting orders.
//
// Holding depth constant means every benchmark below pairs each operation with its
// inverse: an insert is matched by a cancel, and a fill is matched by a replacement.
// The reported ns/op is therefore the cost of the pair, and the figure worth reading
// is how it changes across depths rather than its absolute value.

var benchDepths = []int{1_000, 10_000, 100_000}

// seedBook fills a book with `orders` resting orders spread over `levels` prices on
// both sides, leaving a gap at the touch so nothing matches. It returns the resting
// order IDs in insertion order.
func seedBook(book *Book, bld *builder, orders, levels int) []OrderID {
	ids := make([]OrderID, 0, orders)
	for i := range orders {
		side := Buy
		price := PriceTicks(10000 - 10 - i%levels)
		if i%2 == 1 {
			side = Sell
			price = PriceTicks(10000 + 10 + i%levels)
		}
		o := bld.limit(side, price, 10)
		book.Submit(o)
		ids = append(ids, o.ID)
	}
	return ids
}

// BenchmarkSteadyStateInsertCancel holds depth constant by cancelling one resting
// order for every order inserted, so the book neither grows nor shrinks.
//
// This is the benchmark that answers how the order index and the level map behave as
// the book deepens.
func BenchmarkSteadyStateInsertCancel(b *testing.B) {
	for _, depth := range benchDepths {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			const levels = 200
			book := NewBook("BENCH")
			var bld builder
			resting := seedBook(book, &bld, depth, levels)

			rng := rand.New(rand.NewSource(1))
			next := 0

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				// Insert one, then cancel one already resting. Depth is unchanged.
				o := bld.limit(Buy, PriceTicks(10000-10-rng.Intn(levels)), 10)
				book.Submit(o)

				victim := resting[next]
				resting[next] = o.ID
				next = (next + 1) % len(resting)

				if _, err := book.Cancel(victim); err != nil {
					b.Fatalf("cancel: %v", err)
				}
			}
			b.StopTimer()

			if got := book.RestingCount(); got != depth {
				b.Fatalf("book holds %d orders, want a steady %d", got, depth)
			}
		})
	}
}

// BenchmarkSteadyStateMatch holds depth constant by replacing every order a fill
// consumes, so aggressive matching can be compared across book depths.
func BenchmarkSteadyStateMatch(b *testing.B) {
	for _, depth := range benchDepths {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			const levels = 200
			book := NewBook("BENCH")
			var bld builder
			seedBook(book, &bld, depth, levels)

			// A dedicated ask at the touch to trade against, replaced each time it
			// is consumed so the surrounding book never changes size.
			const touch = PriceTicks(10000)

			var trades int
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				book.Submit(bld.limit(Sell, touch, 10))
				trades += len(book.Submit(bld.limit(Buy, touch, 10)).Trades)
			}
			b.StopTimer()

			if got := book.RestingCount(); got != depth {
				b.Fatalf("book holds %d orders, want a steady %d", got, depth)
			}
			b.ReportMetric(float64(trades)/float64(b.N), "trades/op")
		})
	}
}

// BenchmarkSteadyStateBestPrice isolates the price heap: reading the best price on
// both sides of a book of known depth, with no mutation at all.
//
// This is the operation the lazy stale-level scheme is built around, so its cost
// should be flat in depth. If it ever is not, the heap is accumulating drained
// levels faster than they are being reclaimed.
func BenchmarkSteadyStateBestPrice(b *testing.B) {
	for _, depth := range benchDepths {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			const levels = 200
			book := NewBook("BENCH")
			var bld builder
			seedBook(book, &bld, depth, levels)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, ok := book.BestBid(); !ok {
					b.Fatal("no best bid")
				}
				if _, ok := book.BestAsk(); !ok {
					b.Fatal("no best ask")
				}
			}
		})
	}
}

// BenchmarkSteadyStateLevelChanges measures the incremental alternative to a snapshot:
// collecting the levels one command actually touched.
//
// Compare against BenchmarkSteadyStateSnapshot at the same level counts. A snapshot has to
// gather and sort every level on the book to publish the top ten, so it grows with the size
// of the book; this grows with the size of the change, which is why the live feed publishes
// deltas and keeps snapshots for synchronisation only.
func BenchmarkSteadyStateLevelChanges(b *testing.B) {
	for _, levels := range []int{20, 200, 2_000} {
		b.Run(fmt.Sprintf("levels=%d", levels), func(b *testing.B) {
			book := NewBook("BENCH")
			var bld builder
			seedBook(book, &bld, levels*5, levels)
			book.DiscardLevelChanges()

			const touch = PriceTicks(10000)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				// One resting order plus the fill that takes it: two touched levels,
				// whatever the size of the surrounding book.
				book.Submit(bld.limit(Sell, touch, 10))
				book.Submit(bld.limit(Buy, touch, 10))
				if got := len(book.TakeLevelChanges()); got == 0 {
					b.Fatal("no level changes recorded")
				}
			}
		})
	}
}

// BenchmarkSteadyStateSnapshot measures publishing the top ten levels per side,
// which is what every market data update costs. It sorts the live levels rather than
// reading the heap's internal order, so its cost tracks the number of distinct
// prices rather than the number of resting orders.
func BenchmarkSteadyStateSnapshot(b *testing.B) {
	for _, levels := range []int{20, 200, 2_000} {
		b.Run(fmt.Sprintf("levels=%d", levels), func(b *testing.B) {
			book := NewBook("BENCH")
			var bld builder
			seedBook(book, &bld, levels*5, levels)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				snap := book.Snapshot(DefaultDepth)
				if len(snap.Bids) == 0 || len(snap.Asks) == 0 {
					b.Fatal("empty snapshot")
				}
			}
		})
	}
}
