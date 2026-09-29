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
| 4 | Simulated traders (market maker, noise trader) | not started |
| 5 | HTTP order entry and WebSocket market data | not started |
| 6 | React + TypeScript trading screen | not started |
| 7 | Polish and documentation | not started |

The backend is complete and tested through phase 3. There is no frontend yet; use
`cmd/session` to drive a market from a terminal.

---

## Try it

Requires Go 1.24 or later.

```sh
cd backend

# Run a scripted session and prove it replays.
go run ./cmd/session -session-dir /tmp/sessions

# Rebuild that market from its log.
go run ./cmd/replay -file /tmp/sessions/session-NOVA.ndjson

# Boot the exchange (health endpoint and instrument listing only, so far).
go run ./cmd/exchanged
```

`cmd/session` seeds a book, submits a limit order that partially fills, cancels the
remainder, then sends a market order that sweeps several levels — printing the book,
the tape and the open orders after each step. It finishes by replaying its own
session log into a fresh engine and comparing digests:

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

-- replay -------------------------------------------------------
  live digest    562ed7e43b3b310b500fffc368bed985239cb452867b7462c0294e660bc4bd60
  replay digest  562ed7e43b3b310b500fffc368bed985239cb452867b7462c0294e660bc4bd60
  result         identical: same trades, same book, same order priorities
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
| `internal/instrument` | The listed fictional securities and tick-to-decimal formatting. |
| `internal/display` | Plain-text rendering for the command-line tools. |
| `cmd/exchanged` | The server. |
| `cmd/session` | Drives a scripted session and verifies it replays. |
| `cmd/replay` | Rebuilds a market from a session log. |

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

---

## Testing

```sh
cd backend
go test ./...              # 51 test functions, 77 cases including subtests
go test -race ./...        # the same suite under the race detector
go test ./internal/engine/ -run '^$' -bench . -benchmem
```

### What the tests are built around

**Invariants, checked after every single command.** The engine test harness re-runs
the full invariant check after each operation rather than at the end of a test. A bug
that corrupts the order index or lets a level total drift from its queue is usually
invisible in the result of the command that caused it and only surfaces several
commands later, by which point the cause is gone.

`Book.CheckInvariants` verifies, and reports all violations together:

- the resting book is never crossed (`bestBid < bestAsk` whenever both exist)
- every resting order has `remainingQty > 0`
- every order conserves quantity: `original == filled + remaining + canceled`
- every `PriceLevel.TotalQty` equals the sum of its orders' remaining quantities
- the FIFO queue's links are consistent in both directions and its count is right
- within a level, priority sequences strictly increase
- every resting order has exactly one index entry pointing at its actual node and
  level; filled and canceled orders have none
- the heap's price set equals the level map's key set, with no duplicates

Trades are validated as they happen: positive quantity, never more than either
order's pre-trade remaining quantity, and always at the resting order's limit price.

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

### Methodology

Benchmarks cover the matching engine alone: no channels, no journal, no
serialisation. None of them pause the timer to set up a book, because stopping and
starting the timer around a few hundred nanoseconds of work costs more than the work
itself. Instead each one times a self-contained unit and documents what that unit
includes.

That means **these figures are not subtractable from one another.** They differ in
how large the book is while the work happens, and book size is the main thing the
engine's cost depends on. `BenchmarkSubmitAndCancel` comes out *faster* per command
than `BenchmarkSubmitPassiveLimit` for exactly that reason: it withdraws every order
it places, so its book stays near-empty, while the other's grows to millions of
resting orders. Compare a benchmark against itself across changes, not against its
neighbours.

No throughput target was set. These are measurements.

### Measured

Apple M3, 8 cores, macOS 26.6, Go 1.27.1, `-benchtime 1s`:

```
BenchmarkSubmitPassiveLimit-8    5158771     246.2 ns/op    202 B/op     3 allocs/op   5158771 resting
BenchmarkSubmitAndCancel-8       7967174     147.6 ns/op    215 B/op     5 allocs/op
BenchmarkMatchOneLevel-8         9143617     133.1 ns/op    320 B/op     5 allocs/op   1.000 trades/op
BenchmarkSweepTenLevels-8         607994    1916.0 ns/op   4544 B/op    63 allocs/op   10.00 levels/op
BenchmarkMixedFlow-8             6471408     208.0 ns/op    218 B/op     3 allocs/op   0.730 trades/op
BenchmarkLargeWorkload-8              69   16764572 ns/op                             100000 commands/op
```

Reading these:

- A passive insert into a book that has grown to five million resting orders costs
  about 246 ns, and that includes the order index rehashing and the collector's view
  of every live order. This is the pessimistic end of insertion cost.
- One resting order plus the aggressive order that fully consumes it is about 133 ns
  for the pair.
- Ten inserts plus the market order that sweeps all ten levels is about 1.9 µs, so
  roughly 91 ns per command on the expensive matching path.
- The 100,000-command mixed workload runs in about 16.8 ms end to end, which is
  around 6.0 million commands per second, producing about 88,500 trades.

Allocation counts are reported but not yet optimised. Each command allocates an
`Order`, and matching allocates the trade slice it returns. The obvious next step
would be to profile before changing anything, which has not been done.

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
- **`book_changed` carries a full snapshot**, not a delta. Deltas are an optimisation
  for later.

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
