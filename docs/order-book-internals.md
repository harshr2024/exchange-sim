# Order book internals

This is the part of the design most worth being able to explain without notes: the
hybrid map-plus-heap book, and specifically how drained price levels are handled. The
scheme is defensible, but it is clever in a way that invites attack, and clever
things you cannot explain are liabilities.

---

## The structure

Each side of the book is three things:

```go
type bookSide struct {
    side     Side
    levels   map[PriceTicks]*PriceLevel // price -> level
    prices   priceHeap                  // max-heap for bids, min-heap for asks
    nonEmpty int                        // levels currently holding orders
    resting  int                        // orders currently resting on this side
}
```

Plus one book-wide index:

```go
index map[OrderID]*locator   // locator = {side, level, node}
```

Four operations pull in four different directions, which is why it is a hybrid rather
than one container:

| Operation | Cost | Served by |
| --- | --- | --- |
| Best price | O(1) read | heap root |
| Insert at a price | O(1) amortised, O(log L) if the price is new | level map, heap push |
| Consume oldest at a price | O(1) | level's FIFO head |
| Cancel by order ID | O(1) | index → locator → unlink |

`L` is the number of distinct price levels on a side, not the number of orders.

---

## Why the levels exist at all

Two reasons, and it is worth giving both.

A price level is the unit of **time priority**: orders at the same price form one FIFO
queue, so "oldest first" is just "front of the queue" rather than a comparison.

A price level is also the unit of **aggregation**: market data publishes total
quantity per price, and the level maintains that as a running sum, so a depth snapshot
never walks the orders.

A heap of individual orders would handle best-price lookup fine and would handle the
other three badly: cancelling would mean finding an arbitrary element, same-price FIFO
would need a tiebreaker comparison on every sift, and aggregation would mean a scan.

---

## The five questions about drained levels

When a level's last order is filled or cancelled, it is **not** immediately removed
from the heap. It is left in place, empty, and discarded later. Here is why that is
safe.

### 1. Why can a stale empty level never become executable liquidity?

Because matching never reads a price from the heap and acts on it. It asks for a
*level*, and the only way to get one is `bestLevel()`, which will not return an empty
level:

```go
func (s *bookSide) bestLevel() *PriceLevel {
    for s.prices.Len() > 0 {
        price := s.prices.prices[0]
        if lvl, ok := s.levels[price]; ok && !lvl.Empty() {
            return lvl
        }
        heap.Pop(&s.prices)
        delete(s.levels, price)
    }
    return nil
}
```

An empty level has `count == 0`, `head == nil` and `TotalQty == 0`. There is nothing
in it to match against, and no code path constructs an order out of a price. The
stale entry is a note saying "this price once had a level", not liquidity.

The same reasoning covers market data: `sortedLevels` filters on `!lvl.Empty()`, so a
drained level is never published either.

### 2. How do `bestBid()` and `bestAsk()` discard them?

`bestPrice()` calls `bestLevel()`, so both go through the loop above. It reads the
heap root and then does one of two things:

- **The root's level has orders.** Return it. This is the overwhelmingly common case
  and costs one map lookup — no heap mutation at all.
- **The root's level is empty (or missing).** Pop it from the heap *and* delete it from
  the map, then look at the new root. Repeat.

Two things matter about that second branch. First, popping and deleting together is
what preserves the central invariant: **the set of prices in the heap is always
exactly the set of keys in the level map, with no duplicates.** That invariant is why
no separate "is this price in the heap?" set is needed, and `CheckImplementationInvariants`
asserts it after every command in the tests.

Second, the work is amortised. A level can only be drained once per time it is
created, so each stale entry is popped at most once. Over a run of *n* commands the
total discard work is O(n log L), the same as if levels were removed eagerly — it is
just deferred to the point where it is free to notice.

### 3. When does compaction fire?

Lazy discarding only reclaims levels that reach the root. A price far from the touch
can be drained and then sit in the heap indefinitely, because the root never gets
there. Compaction is what bounds that:

```go
const compactThreshold = 32

func (s *bookSide) maybeCompact() {
    if len(s.levels) < compactThreshold || s.nonEmpty*2 > len(s.levels) {
        return
    }
    // rebuild from the non-empty levels only
}
```

It is checked only from `remove()`, when a level has just become empty — the only
moment the ratio can worsen. Both conditions must hold:

- **at least 32 levels**, so a small book never pays for a rebuild it does not need;
- **at most half the levels are live**, so a rebuild is only triggered when stale
  entries have come to dominate.

A rebuild is O(L): it walks the map, deletes the empty levels, collects the live
prices and calls `heap.Init`. Because it needs stale entries to be at least half of
the map before it fires, the amortised cost per drained level is constant.

`TestDrainedLevelsAreReclaimed` pins this down by creating and draining 5,000 levels
across 500 distinct prices and asserting the ask side ends with at most
`compactThreshold` levels and a heap that still matches the map.

### 4. What is the worst case before compaction?

The quantity that is bounded is the **stale level count**, and the bound is:

```
stale <= max(compactThreshold, live)
```

The argument runs on what can change `stale = len(levels) - nonEmpty`:

- **A push never increases it.** A brand-new price adds one to both counts; a refill of
  an already-present empty level adds one to `nonEmpty` only, which *decreases* stale.
- **`bestLevel()` never increases it.** It deletes from the heap and the map together,
  which lowers `len(levels)` for a level that was already empty.
- **Only a level emptying increases it**, and every such event calls `maybeCompact`,
  which either rebuilds (leaving stale at 0) or finds `nonEmpty * 2 > len(levels)`,
  which is exactly `stale < live`. Below the threshold no rebuild happens at all, so
  stale can reach `compactThreshold - 1` instead.

So the most recent emptying event caps stale, and the side can then grow arbitrarily
without loosening it.

Note what this does **not** say. It does not bound the *ratio* of held to live levels.
A side that drains almost completely can sit at 31 stale levels beside a single live
one, and then grow past the threshold still carrying them. `TestStaleLevelsStayBounded`
measures ratios of 6× to 8× on randomised flow, and that is within the bound rather
than a violation of it.

That test originally asserted the tidier-sounding `len(levels) <= 2 * live` and failed
on its first run at 33 levels against 15 live ones — which is how the bound above came
to be stated correctly. Worth remembering as the reason to assert this kind of claim
rather than only write it down.

The other thing to be precise about is what is being counted. It bounds **levels**, not
orders. A drained `PriceLevel` is a price, a zero total, a zero count and two nil
pointers — tens of bytes. The orders it used to hold are gone from it and gone from the
order index, so this scheme retains no order. Tens of bytes times a few dozen levels is
kilobytes, which is why the loose bound is acceptable in practice even though it is
looser than it first appears.

The cost that is *not* bounded by this is `bestLevel()`'s inner loop in a single call:
if many consecutive best prices have all drained, one call pops all of them. That is
still amortised O(1) per stale entry across the run, but a single unlucky call can be
O(k log L). For this simulator that is irrelevant; a venue that cared about tail
latency rather than throughput might prefer eager removal for exactly this reason,
paying a little more on the common path to avoid the spike.

### 5. Why does rebuilding the heap not change price priority?

Because nothing depends on the heap's internal array order.

`heap.Init` may permute the backing array — the same multiset of prices has many valid
heap layouts. But the root of a heap is the **unique minimum** (for asks) or **unique
maximum** (for bids) of its contents, and that is determined by the contents alone,
not the layout. `bestLevel()` reads only the root, so it returns the same price before
and after a rebuild.

Every place that needs more than the best price sorts explicitly instead of reading
the heap:

```go
func (s *bookSide) sortedLevels(limit int) []*PriceLevel {
    // collect non-empty levels from the map, then sort by price
}
```

So depth snapshots and the canonical state dump are ordered by an explicit comparison,
never by heap position. This is the reason the replay digest is stable: if a snapshot
had iterated the heap's array, two engines in identical logical states could serialise
their levels in different orders and produce different digests.

And within a price, priority is the FIFO queue's order, which compaction does not
touch at all — it only ever removes whole empty levels and rebuilds the array of
prices.

---

## What would make a balanced tree better

Worth being able to argue the other side.

An ordered tree (red-black, B-tree, skip list) keyed on price would give O(log L)
best-price access — slightly worse than the heap's O(1) — but in exchange:

- **ordered traversal is free**, so depth snapshots would be O(depth) instead of
  O(L log L). Given the measurement that a 2,000-level snapshot costs ~133 µs while
  matching costs ~200 ns, that is the strongest argument against the current design,
  and it only shows up once a book has many distinct prices.
- **no stale-entry scheme is needed**, because deletion is a first-class operation. All
  of section 3 and 4 above simply would not exist.
- **neighbouring-price queries** ("what is the level just below the touch?") become
  natural, which matters for order types this project does not have.

The heap was chosen because matching genuinely only needs the best price, Go's standard
library has a heap and no ordered tree, and hand-rolling a red-black tree would have
added a body of subtle code without improving the thing the project is about. That is a
defensible trade, not an obviously correct one.
