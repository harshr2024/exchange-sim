# exchange-sim

A simulated electronic exchange. Prices are not fetched from anywhere — they are
produced by orders matching against a real in-memory limit order book.

The centre of the project is a deterministic price-time-priority matching engine.
The engineering claim it is built to support is:

> Given the same sequenced command stream, the engine always produces the same
> trades and the same final order book.

Everything else — the sequencing, the session log, the invariant tests, the
single-writer concurrency model — exists to make that claim checkable rather than
aspirational.

This is a deliberately limited **continuous central limit order book** using FIFO
price-time priority. That is a real matching model, but real venues also have
additional allocation algorithms, auctions, hidden order types, risk controls,
market-data protocols and regulatory rules that are not here. See
[Limitations](#limitations).

---

## Status

| Phase | Scope | State |
| --- | --- | --- |
| 0 | Module skeleton, domain types, health endpoint | done |
| 1 | Limit order book, FIFO levels, matching, sweeps | done |
| 2 | Market orders, cancellation, snapshots, invariants, benchmarks | done |
| 3 | Market actor, sequencing, session log, deterministic replay | done |
| 4 | Simulated traders and terminal simulation | done |
| 5 | HTTP order entry and WebSocket market data | not started |
| 6 | React + TypeScript trading screen | not started |
| 7 | Polish and documentation | not started |

The backend is complete and tested through phase 4. There is no frontend and no
network layer yet; the exchange runs from a terminal.

---

## The claim, demonstrated

This is the part of the project worth looking at first.

```
concurrent submissions          (HTTP clients, bots, several goroutines)
        │
        ▼
one sequenced command stream    (the market actor assigns the total order)
        │
        ▼
market execution                (price-time priority matching)
        │
        ▼
recorded session                (append-only NDJSON command log)
        │
        ▼
fresh engine replay             (a new book, fed only the log)
        │
        ▼
identical canonical SHA-256 digest
```

The digest covers every price level, every resting order's ID, owner, priority
sequence and quantities, and the entire trade history — so two markets cannot match
on it without being genuinely identical, down to which resting order is next in line.

```sh
cd backend
go run ./cmd/simulate -ticks 400 -seed 7 -session-dir /tmp/sessions
```

```
-- summary ------------------------------------------------------
  ticks          400
  commands       310 (310 accepted, 0 rejected)
  trades         102 (1262 shares)
-- replay -------------------------------------------------------
  session log    /tmp/sessions/session-NOVA.ndjson (310 commands)
  live digest    3c7c1e9733231580a4ce36f28f9eb374c0f1c922ca72d1face6245d6a7922369
  replay digest  3c7c1e9733231580a4ce36f28f9eb374c0f1c922ca72d1face6245d6a7922369
  result         identical: same trades, same book, same order priorities
```

### Two different guarantees

The distinction between these matters more than it looks:

| Runner | Reproducible from | Why |
| --- | --- | --- |
| `sim.Driver` (`-mode deterministic`) | **its seeds** | One goroutine polls bots in a fixed order, so no scheduling decision enters the command stream. |
| `sim.Live` (`-mode live`) | **its recorded log** | Bot goroutines race to reach the market; which arrives first is the Go scheduler's choice, not the seed's. |

Empirically, running each mode twice with the same seed:

```
deterministic, seed 7, twice:
  live digest    7a503843c544dea4714a2596f44cb3b27aba84ed2f8a6ea3d6eaa6f9deeb6fa3
  live digest    7a503843c544dea4714a2596f44cb3b27aba84ed2f8a6ea3d6eaa6f9deeb6fa3   <- same

live, seed 7, twice:
  live digest    94590dbe3ea1adb15087de28c6b675d6c1a60e96faf05096e3052a753fccd7ea
  replay digest  94590dbe3ea1adb15087de28c6b675d6c1a60e96faf05096e3052a753fccd7ea   <- replays
  live digest    7247330ccb094d3d83bb2b6ec0899775248a8351e7f3313aab550c03c0317977   <- differs
  replay digest  7247330ccb094d3d83bb2b6ec0899775248a8351e7f3313aab550c03c0317977   <- replays
```

Demanding that two concurrent runs match on seed alone would be asserting something
untrue about goroutine scheduling. What the design actually guarantees is that the
actor converts a nondeterministic arrival race into one specific total order, and that
once that order exists, execution is deterministic.

---

## Try it

Requires Go 1.24 or later.

```sh
cd backend

# Autonomous market: bots trade, then the session is replayed and verified.
go run ./cmd/simulate -ticks 400 -seed 7 -print-every 200
go run ./cmd/simulate -mode live -duration 3s

# A scripted human session: partial fill, cancel, multi-level sweep.
go run ./cmd/session -session-dir /tmp/sessions

# Rebuild any market from its log.
go run ./cmd/replay -file /tmp/sessions/session-NOVA.ndjson

# Boot the exchange (health endpoint and instrument listing only, so far).
go run ./cmd/exchanged
```

`cmd/session` prints the book, tape and open orders after each step:

```
-- human buys 50 at 100.10: fills 30, rests 20 ------------------
  filled 30 @ 100.10
NOVA  Nova Dynamics   seq 7   last 100.10   spread 0.10
       PRICE      QTY  ORDERS
      100.40       60       1   ask
      100.30       40       1   ask
      100.20       50       1   ask
      100.10                    <- last
      100.10       20       1   bid
       99.90       40       1   bid
       99.80       70       1   bid
```

---

## Architecture

```
                  ┌─────────────────┐
                  │ React / TS UI   │   (phase 6)
                  └────────┬────────┘
                           │ HTTP commands
                           v
                ┌─────────────────────┐
                │ Go HTTP API         │   (phase 5)
                └─────────┬───────────┘
                          │ command
                          v
        ┌────────────────────────────────────┐
        │ Market Actor: NOVA                 │
        │                                    │
        │ 1. assign sequence number          │
        │ 2. append to session log           │
        │ 3. validate                        │
        │ 4. match against the book          │
        │ 5. publish immutable events        │
        └────────────────┬───────────────────┘
                         │ immutable events
                         v
                ┌───────────────────┐
                │ Market Data Hub   │   (phase 5)
                └─────────┬─────────┘
                          │ WebSocket
                          v
                   React clients

Simulated traders ──────► same market actor input channel   (phase 4)
```

One goroutine per symbol owns that symbol's matching state. Nothing else touches
it, including reads: snapshots and digests are served by running a closure on the
market's own goroutine rather than by locking the book.

### Packages

| Package | Responsibility |
| --- | --- |
| `internal/engine` | The order book and matching. Generic over symbol, no concurrency, no I/O. |
| `internal/market` | Sequencing, validation, the session log, event publication, the actor, replay. |
| `internal/sim` | The simulated traders and the two runners that schedule them. |
| `internal/instrument` | The listed fictional securities and tick-to-decimal formatting. |
| `internal/display` | Plain-text rendering for the command-line tools. |
| `cmd/exchanged` | The server. |
| `cmd/simulate` | Runs a market autonomously with the bots, then verifies replay. |
| `cmd/session` | Drives a scripted human session and verifies it replays. |
| `cmd/replay` | Rebuilds a market from a session log. |

Further reading: [`docs/order-book-internals.md`](docs/order-book-internals.md) covers
the hybrid map-plus-heap design and the lazy stale-level scheme in detail.

---

## Matching engine

### Prices are integers

Prices are `PriceTicks int64`, a whole number of ticks. With a one-cent tick,
`$100.10` is `10010`. Decimal money is not exactly representable in binary floating
point, so using `float64` as a matching key would make price equality and
price-level lookup unreliable — and price equality is exactly what a level map is
built on. Ticks become a decimal string only in `internal/display` and
`instrument.Format`.

### Data structures

Each side of the book is:

- `map[PriceTicks]*PriceLevel` — O(1) lookup of the level for a price.
- a price heap — max-heap for bids, min-heap for asks, so the best price is the root.
- each `PriceLevel` holds a doubly-linked FIFO queue of resting orders and a running
  `TotalQty`.

Plus one book-wide `map[OrderID]*locator`, where a locator holds the side, the level
and the queue node, which is what makes cancellation a direct unlink rather than a
search.

The structure is a hybrid because the four operations pull in different directions:

| Operation | Served by |
| --- | --- |
| Find the best price | heap root |
| Insert at a price | level map |
| Consume the oldest order at a price | level's FIFO head |
| Cancel an arbitrary order by ID | order index → locator → unlink |

A single heap of individual orders handles the first well and the last badly. A map
alone gives no ordering. A sorted slice is simple but expensive to mutate as the
book grows.

A balanced ordered tree would also be defensible and would additionally give cheap
ordered traversal. It is not used here because matching only ever needs the *best*
price, Go's standard library has no ordered tree, and snapshots can sort the active
levels separately — which they do.

**Drained levels.** A level that empties is kept in both the map and the heap until
it surfaces at the heap root, at which point it is discarded from both. That keeps
the heap's price set exactly equal to the map's key set, with no duplicates, without
maintaining a separate membership set. Once drained levels outnumber live ones the
heap is rebuilt to bound memory. Rebuilding can permute the heap's backing array,
but the root of a heap is the unique extreme of its contents, so it cannot change
which level matching picks — and every ordered traversal sorts explicitly instead of
reading the heap's internal order. `TestDrainedLevelsAreReclaimed` pins this down.

### Price-time priority

Better price first. Within one price, lower priority sequence first.

Priority comes from the exchange sequence number assigned when the market accepted
the order. `Order` deliberately has no timestamp field, so nothing about matching
can come to depend on a clock — not the client's and not the server's.

### Order behaviour

- A **limit order** matches while the opposite best price is acceptable to it, then
  rests at its limit price with whatever is left.
- A **market order** matches until it is filled or the opposite side is exhausted.
  Any remainder is **canceled**, never rested: a resting order with no price would
  have no defined position in the book.
- Every trade prints at the **resting order's** price. The resting order published
  that price first and is entitled to it, so the aggressor takes any price
  improvement.
- **Cancelling** a nonexistent, already-filled or already-canceled order all return
  the same `ErrUnknownOrder`. The book only knows live resting orders, so it gives
  one consistent answer rather than three shades of "nothing to do".

---

## Determinism and replay

Every command that reaches a market gets the next sequence number and is appended
to `session-<SYMBOL>.ndjson` before it executes:

```
{"seq":7,"cmd":{"kind":"submit_limit","participant":"human","side":"buy","qty":50,"price":10010}}
{"seq":8,"cmd":{"kind":"cancel","participant":"human","order_id":7}}
{"seq":9,"cmd":{"kind":"submit_market","participant":"human","side":"buy","qty":100}}
```

Replaying that log into a fresh engine reproduces the trades, the resting orders in
the same FIFO positions, and the final sequence. Four decisions are what make this
work:

1. **Order IDs come from a counter**, not from the client. A replay mints the same
   IDs, so a recorded cancel still refers to the right order.
2. **Rejected commands are recorded and consume a sequence number.** A replay that
   skipped them would mint different IDs from that point onwards.
3. **Validation is a pure function of the command**, so a replay rejects exactly
   what the live session rejected.
4. **No wall-clock time is read on the write path.**

`Replay` also checks each record's recorded sequence against the one it derives, so
a truncated, reordered or duplicated log is an error rather than a plausible-looking
but wrong book. A *prefix* of a log is not corruption — it is an earlier point in
the session — and replays cleanly.

If a market cannot append a command to its log, it halts without consuming the
sequence number, so the log that was written still describes exactly the state the
market stopped in.

This is not an event-sourcing platform. It records the sequenced command stream and
rebuilds state by replaying it from the beginning. There are no snapshots, no
checkpointing, no compaction and no schema migration.

---

## Concurrency

Go's concurrency is used everywhere it helps and nowhere it does not.

Concurrent HTTP handlers, concurrent WebSocket writers and concurrent simulated
traders are all useful. Concurrently *mutating one order book* is not: it would put
locks on the matching path, and it would make the resulting trades depend on how the
scheduler happened to interleave them. So every command for a symbol funnels into
one channel and one goroutine drains it.

Consequences worth being explicit about:

- **Two simultaneous requests.** Whichever command the market actor accepts first
  gets the lower sequence number and the better priority. No participant's timestamp
  is consulted.
- **Reads.** `Snapshot`, `OpenOrders`, `RecentTrades` and `Digest` are closures run
  on the market's own goroutine. Callers get values and copies, never pointers into
  the book.
- **Events.** A `Sink.Publish` call happens inline on the matching path and must not
  block. A sink that waited on a slow consumer would push backpressure straight into
  matching and stall the market. Phase 5's hub will give each WebSocket connection a
  bounded queue and disconnect clients that fall behind; a reconnecting client
  refetches a REST snapshot rather than replaying a gap.
- **Bots** use `TryEnqueue`, which reports a full queue instead of waiting. A quoting
  bot that blocked would only pile up updates that are stale by the time they run.
- **Abandoned reads.** A query whose caller gives up on a canceled context is still run
  by the actor, so its result comes back over a buffered channel rather than a variable
  the closure captured. An earlier version captured, which the race detector caught once
  bots began reading the book on an expiring context; `TestAbandonedQueriesDoNotRace` is
  the regression test.

---

## Simulated traders

Two bots, in `internal/sim`. They exist to make the exchange demonstrable, not to study
market dynamics.

**Bots are decision functions, not goroutines.** A `Bot` receives an immutable `View`
and returns the commands it wants to submit. It holds no reference to the book, the
market or the actor, so it submits through exactly the same command interface as a human
and cannot mutate matching state even by accident. Choosing *when* to consult a bot is
the runner's job, which is what makes the same bot usable in both the reproducible and
the concurrent runner.

**Ownership is unexpressible, not just refused.** A bot's `View` carries only its own
resting orders, and the aggregated book carries no order identities at all — so a bot
has no way to *name* another participant's order. The market enforces ownership too, and
both runners refuse to submit a command a bot attributed to someone else, but the
structural argument is the one that matters. `TestBotsOnlyCancelTheirOwnOrders`
reconstructs ownership from the recorded command stream and checks every cancel against
it.

**The market maker** maintains a ladder of quotes around the reference price, cancelling
and replacing a quote once it leaves the ladder or falls below half its target size.
That is the whole strategy: no inventory model, no skew, no learning. It shifts its
whole ladder rather than crossing, so it never takes liquidity — and since there is no
self-trade prevention yet, that also stops it matching its own quote.
`TestMakerDoesNotTakeLiquidity` runs it alone against resting liquidity and asserts zero
trades.

**The noise trader** produces flow from a seeded PRNG: mostly market and marketable
limit orders, sometimes passive orders spread over a few ticks behind the touch,
occasionally a cancel. It has no view of value and no opinion about direction.

**The reference price is read, never written.** Both bots derive it from market state —
mid, then last trade, then whichever side exists — falling back to the instrument's
bootstrap price only for a market that has never traded and has nothing resting. It
never becomes the displayed price: the last price moves when a trade prints and at no
other time, which is what keeps the quoted price a product of activity in this market.
`TestMakerQuotesBothSidesOfAnEmptyBook` asserts the book has a spread while the last
price is still unset.

**What counts as a legitimate rejection.** A cancel that misses because the order was
filled a moment earlier is expected, especially under the live runner. A malformed
command is a bug. `TestBotCommandsAreAlwaysValid` distinguishes them: every recorded
command must pass validation, and the only rejection reason permitted is the engine's
unknown-order error.

---

## Testing

```sh
cd backend
go test ./...              # 74 test functions, 107 cases including subtests
go test -race ./...        # the same suite under the race detector
go test ./internal/engine/ -run '^$' -bench . -benchmem
```

### What the tests are built around

**Invariants, checked after every single command.** The engine test harness re-runs
the full invariant check after each operation rather than at the end of a test. A bug
that corrupts the order index or lets a level total drift from its queue is usually
invisible in the result of the command that caused it and only surfaces several
commands later, by which point the cause is gone.

The invariants come in **two classes**, and the distinction is worth keeping straight
because only one of them is a statement about exchanges.

`CheckSemanticInvariants` — properties any correct continuous limit order book must
hold, whatever it is built out of. These would still be required if the whole data
structure were replaced with a balanced tree or a sorted slice:

- the resting book is never crossed, and no bid level sits at or above any ask level
- every resting order has `remainingQty > 0`
- every order conserves quantity: `original == filled + remaining + canceled`
- every level's advertised `TotalQty` is exactly what rests there, so published depth
  is truthful
- within a price, priority sequences strictly increase, oldest at the front
- order IDs are unique among live orders, and every live order is addressable by ID
  while nothing else is
- an order rests on the side and at the price it asked for, and a market order never
  rests at all

`CheckImplementationInvariants` — properties of *this* design, which are how the code
keeps the promises above. A different design would have entirely different ones:

- the heap's price set equals the level map's key set, with no duplicates
- each order index locator points at the exact node and level holding that order
- the FIFO queue's links agree in both directions and its cached count is right
- the cached `nonEmpty` and `resting` counters match reality, since compaction
  depends on them

`TestInvariantClassesCatchDifferentFaults` keeps the split honest by corrupting state
deliberately and checking that each class detects faults the other misses: a bad cached
counter trips only the implementation check, a quantity that does not conserve or a
crossed book trips only the semantic one.

Trades are validated as they happen — positive quantity, never more than either order's
pre-trade remaining quantity, always at the resting order's limit price — which is a
semantic check.

**Worked examples.** Empty book, exact fill, partial passive fill, partial aggressive
fill, multi-level sweep, FIFO within a level, price priority beating time priority,
cancelling the middle of a queue, cancelling an unknown/filled/twice-cancelled order,
market sweeps, market remainders, the limit boundary (`$100.00` must not trade against
`$100.01`), price improvement, and symmetry between the buy and sell paths.

**Malformed commands.** Zero and negative quantity, zero and negative price, missing
and invalid side, missing participant, a market order carrying a limit price, a submit
carrying an order ID, a cancel without one, unknown and empty command kinds, and an
unlisted symbol. Each is checked to be rejected *and* to still consume a sequence
number without consuming an order ID.

**Randomised property tests.** Long seeded command streams with the invariant check
after every command, which covers the interleavings nobody thought to write by hand:
partially filled orders being cancelled, levels drained and recreated, sweeps that
stop mid-level, and the heap compaction path. Seeds are fixed, so failures reproduce.
These also assert global conservation — total filled quantity is exactly twice total
traded quantity, and every submitted share ends up filled, resting or cancelled.

**Replay determinism.** A seeded stream runs through a live market, then the recorded
log runs through a fresh one, and the two are compared by a SHA-256 digest of the
full canonical state: every price level (not just the published depth), every resting
order's ID, owner, priority sequence and quantities, and the entire trade history.
Two markets can agree on a snapshot while disagreeing about which resting order is
next in line; they cannot agree on this digest without being identical.
`TestDigestDetectsADifferentOrdering` guards the guard, by checking that the same
commands in a different order produce a different digest.

**Bot behaviour.** That bots cannot name another participant's order, that every command
they emit validates, that the market maker never takes liquidity and shifts its ladder
rather than crossing, that it replaces depleted quotes but leaves healthy ones alone,
that the noise trader never fires a market order into an empty book, and that identically
seeded bots fed identical views decide identically.

**Concurrency.** Sixteen goroutines submit 4,000 commands into one market actor. The
test asserts that sequence numbers form the dense range `1..N` with no gaps and no
duplicates, that each goroutine's own submissions are monotonic, that all invariants
hold, and that replaying the captured log reproduces *that specific run's* digest.

It deliberately does **not** assert that two genuinely concurrent goroutines get any
particular ordering relative to each other. That is not knowable, and asserting it
would misunderstand the problem. What must hold is that the commands get totally
ordered somehow, that the order is recorded, and that the recording is enough to
reproduce the run.

A second test runs snapshot readers, event consumers and submitters simultaneously,
which is what catches state accidentally escaping the market's goroutine — the
easiest way to break the single-writer design without breaking any assertion.

**What `-race` does and does not prove.** The race detector finds races that actually
occur on the code paths a run executes. A clean run is evidence, not proof, that the
program is race-free.

---

## Benchmarks

Benchmarks cover the matching engine alone: no channels, no journal, no serialisation.
No throughput target was set; these are measurements, and none of them is a headline.

All figures: Apple M3, 8 cores, macOS 26.6, Go 1.27.1.

### Scaling with book depth

These are the ones worth reading. Book depth is held constant for the whole run, so
"how does this behave as the book gets deeper?" has an actual answer. Each pairs an
operation with its inverse — an insert with a cancel, a fill with a replacement — so
depth never drifts, which means the reported `ns/op` is the cost of the pair and the
interesting figure is how it moves across depths.

```
                                    1k resting   10k resting   100k resting
insert + cancel                       116 ns       144 ns        153 ns
resting order + fill that takes it     167 ns       208 ns        212 ns
read best bid and best ask             9.9 ns       9.9 ns        9.8 ns
```

- **Mutation is close to flat**: 100× the depth costs about 30% more per operation, and
  that residue is cache and allocator behaviour rather than algorithmic. This is what
  the order index and the level map are for.
- **Best-price reads are flat at ~10 ns and allocation-free**, which is the property the
  whole lazy stale-level scheme exists to protect. If this ever grew with depth, it
  would mean drained levels were accumulating faster than they are reclaimed.

### The one that does not scale

Publishing depth costs more than matching does, and superlinearly in the number of
distinct prices:

```
                        20 levels   200 levels   2000 levels
snapshot top 10/side      507 ns      6.4 µs       133 µs
```

`Snapshot` collects every live level, sorts them, then keeps ten. That is O(L log L)
per call, and a `book_changed` event fires on **every command**, so at 2,000 distinct
prices market data would cost roughly 600× what the matching underneath it costs.

It is not a current problem — the running simulation holds a few dozen levels, where a
snapshot is around 1 µs — but it is a real scaling cliff and the strongest argument
against the map-plus-heap choice. The fixes, in increasing order of effort: select the
top ten instead of sorting all of them; publish deltas rather than full snapshots;
or maintain the book in an ordered structure where traversal is already cheap. This is
work for the network-layer phase, not something to guess at now.

### Self-contained units

These each time one unit of work and document what that unit includes. Because they
differ in how large the book is while the work happens, **they are not subtractable
from one another** — `SubmitAndCancel` comes out faster per command than
`SubmitPassiveLimit` purely because it withdraws every order it places while the other
grows to five million resting orders. Compare each against itself across changes.

```
BenchmarkSubmitPassiveLimit-8    5158771     246.2 ns/op    202 B/op     3 allocs/op   5158771 resting
BenchmarkSubmitAndCancel-8       7967174     147.6 ns/op    215 B/op     5 allocs/op
BenchmarkMatchOneLevel-8         9143617     133.1 ns/op    320 B/op     5 allocs/op   1.000 trades/op
BenchmarkSweepTenLevels-8         607994    1916.0 ns/op   4544 B/op    63 allocs/op   10.00 levels/op
BenchmarkMixedFlow-8             6471408     208.0 ns/op    218 B/op     3 allocs/op   0.730 trades/op
BenchmarkLargeWorkload-8              69   16764572 ns/op                             100000 commands/op
```

`BenchmarkLargeWorkload` runs 100,000 generated commands through a fresh book in about
16.8 ms, which works out to roughly 6 million commands per second. That number needs a
caveat attached whenever it is quoted: it is one synthetic stream, on one core, with no
sequencing, no journal write, no event publication and no network, against a book of a
few dozen price levels. It is a useful floor on what the matching core can do and not a
claim about the exchange.

Allocation counts are reported but not optimised: every command allocates an `Order`,
and matching allocates the trade slice it returns. Profiling comes before changing any
of that, and has not been done.

---

## Deliberately excluded

Not oversights — decisions:

- **No authentication.** One predefined `human` participant.
- **No database.** The live book belongs in memory. Putting a relational database on
  the matching path would make the architecture worse. Persistence is the
  append-only session log.
- **No portfolio, cash or positions.** Correct accounts require reserving cash and
  inventory when an order rests, releasing reserves on cancellation, and releasing
  price improvement correctly on partial fills. That is a real feature deserving its
  own design phase, and it is unrelated to proving the matching engine works.
- **No stop, stop-limit, iceberg, IOC or FOK orders; no order amendment.**
- **No options, margin, short selling, market hours, auctions, fees or dividends.**
- **No real market data, brokerage APIs, FIX or exchange multicast protocols.**
- **No Kafka, Redis, microservices or Kubernetes.**
- **No machine learning or AI.**

## Limitations

- **This is not how NASDAQ works.** It implements a simplified continuous limit order
  book with price-time priority. That is a real matching model, but real venues add
  many more order types, routing, risk controls, auctions, market-data protocols,
  regulatory rules and sometimes different allocation algorithms entirely.
- **The session log is not crash-safe.** Records are flushed to the OS on every
  append so a concurrent reader sees them, but there is no `fsync`, no checkpointing
  and no torn-write detection. It is a replay log. Calling it a write-ahead log would
  mean implementing and verifying durability semantics that are not there.
- **No latency claims.** Nothing here is "HFT" or "ultra-low latency". The
  benchmarks above are what they are.
- **Trade history is unbounded.** A market keeps every trade of its session in memory
  so replay can be verified against the whole history. Fine for a demo, not for a
  long-running process.
- **`book_changed` carries a full snapshot**, not a delta, and building one is
  superlinear in the number of distinct prices. See the benchmark section: this is the
  clearest scaling weakness in the current design.
- **No self-trade prevention.** The market maker avoids matching its own quotes by never
  crossing, but nothing in the engine would stop it.
- **The bots are not a market model.** They generate plausible-looking activity. Nothing
  about the resulting price series should be read as meaningful.

---

## Planned extensions

Roughly in order of value, and only after the MVP is polished:

1. **Accounts and risk checks** — virtual cash, positions, reserved cash and
   inventory, pre-trade buying-power validation, no naked shorting. Brings its own
   invariants: cash and shares are conserved, available balances never go negative.
2. **Stronger persistence** — snapshots, checkpointing, log rotation, restart
   recovery, integrity checks. Then kill the process mid-session and prove recovery.
3. **Cancel/replace** — forces a decision about when a modified order loses its
   priority.
4. **IOC and FOK** — FOK is interesting because it has to determine whether enough
   eligible liquidity exists *before* mutating the book.
5. **Self-trade prevention.**
6. **Market-data recovery** — snapshot sequencing, incremental deltas, gap detection,
   catch-up on reconnect.
7. **More sophisticated traders** — momentum, mean reversion, inventory-aware
   quoting. Still no ML.
8. **Latency simulation** — model participant, gateway and exchange latency, then
   investigate whether sequencing changes outcomes.
