# HTTP and WebSocket protocol

Commands go in over HTTP. Market data comes out over WebSockets. The split is deliberate:
order entry is request/response and benefits from ordinary HTTP status semantics, while
market data is server-driven, continuous and frequent, which is what WebSockets are for.

All prices are integer ticks. With a one-cent tick, `10010` reads as `$100.10`. The
conversion to a decimal string happens in the client.

---

## Identity

There is no authentication, and correspondingly **no client-supplied participant**. Every
command the API submits is attributed to a single server-assigned identity, `human`.

This is not merely "the field is ignored". The request schema has no participant field and
unknown fields are rejected, so a client that tries to name itself gets a `400` saying which
field was refused rather than having its request silently reinterpreted:

```
POST /api/markets/NOVA/orders
{"side":"buy","type":"limit","price":9950,"quantity":1,"participant":"bot-mm"}

400 {"error":"json: unknown field \"participant\""}
```

Cancellation ownership is enforced by the market, not by the API. A browser can cancel the
orders it created and nothing else; attempting a bot's quote returns `403`.

---

## HTTP

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/healthz` | Liveness, listed markets, the assigned participant |
| `GET` | `/api/markets` | Listed instruments with tick size and reference price |
| `GET` | `/api/markets/{symbol}/snapshot` | Full book: top 10 levels per side plus quotes |
| `GET` | `/api/markets/{symbol}/trades?limit=N` | Recent trades, oldest first |
| `GET` | `/api/markets/{symbol}/orders` | The participant's resting orders |
| `POST` | `/api/markets/{symbol}/orders` | Submit a limit or market order |
| `DELETE` | `/api/markets/{symbol}/orders/{id}` | Cancel a resting order |
| `GET` | `/ws/markets/{symbol}` | Market data stream |

Symbols are case-insensitive.

### Submitting an order

```
POST /api/markets/NOVA/orders
{"side":"buy","type":"limit","price":10010,"quantity":50}
{"side":"sell","type":"market","quantity":25}
```

`side` is `buy` or `sell`; `type` is `limit` or `market`. A market order must not carry
`price` — it is rejected rather than having the price quietly dropped, because the engine
would ignore it and the client would have been told something untrue.

`201` on success, carrying the sequence number that established the order's priority, the
server-allocated order ID, the fills, and whether a remainder rested:

```json
{
  "sequence": 37,
  "accepted": true,
  "order_id": 30,
  "order": { "id": 30, "participant": "human", "side": "buy", "type": "limit",
             "limit_price": 9950, "original_qty": 40, "remaining_qty": 40,
             "filled_qty": 0, "canceled_qty": 0, "priority_sequence": 37 },
  "rested": true
}
```

A market order reports `canceled_qty` for the part that found no liquidity, since a market
order never rests.

### Status codes

| Status | Meaning |
| --- | --- |
| `400` | Malformed command: bad side, type, quantity or price; a priced market order; an unknown field |
| `403` | The order belongs to another participant |
| `404` | Unknown symbol, or no resting order with that ID |
| `503` | The market is halted or shutting down |

A rejected command still consumed a sequence number, and the response reports it. That is
not an implementation leak: the sequence is what would have established priority, and it is
recorded in the session log either way.

Cancelling an order that never existed, has already filled, or has already been cancelled
all return the same `404`. The book only knows live resting orders, so it gives one
consistent answer rather than three shades of "nothing to do".

---

## WebSocket

`GET /ws/markets/{symbol}`. The server only sends; anything the client sends is discarded.
Every frame is JSON with a `type` and the market `seq` it belongs to.

### `snapshot`

Sent once when the stream opens, and again never — a second snapshot only arrives on a new
connection. Carries the same body as `GET /snapshot`.

```json
{"type":"snapshot","symbol":"NOVA","seq":37,"book":{ "...": "top 10 levels per side" }}
```

### `update`

Incremental market data for one command. This is the live path.

```json
{"type":"update","symbol":"NOVA","seq":38,
 "update":{"symbol":"NOVA","sequence":38,
   "levels":[{"side":"sell","price":9995,"quantity":135,"orders":1}],
   "trades":[{"id":10,"buy_order_id":31,"sell_order_id":20,"price":9995,
              "quantity":3,"aggressor_side":"buy","sequence":38}],
   "best_bid":9991,"has_best_bid":true,"best_ask":9995,"has_best_ask":true,
   "last_trade_price":9995,"has_last_trade":true}}
```

**Level quantities are absolute, not differences.** `quantity` is what rests at that price
now; `0` means the level is gone and should be dropped. Absolute values make an update
idempotent, so applying one twice is harmless and a client never has to track whether it has
already accounted for a fill.

Updates carrying no levels and no trades are not sent, because there would be nothing to
learn from one.

### `order`

A lifecycle change to one of *the subscriber's own* orders — `order_accepted`,
`order_rested`, `order_canceled`. The bots generate most of the order churn on a live market
and none of it is a client's business, so only the configured participant's orders reach the
wire.

```json
{"type":"order","symbol":"NOVA","seq":38,"event":"order_rested",
 "order":{"id":30,"participant":"human","remaining_qty":40, "...": "..."}}
```

Rejections are not streamed: they carry no order, so they cannot be attributed to a
participant. The HTTP response to the rejected command reports it instead.

### `error`

Terminal. `resync: true` means the client must refetch a snapshot and reconnect.

```json
{"type":"error","reason":"subscriber fell behind the market data feed","resync":true}
```

The connection then closes with `1013 Try Again Later`.

---

## Synchronisation

The obvious way to open a stream is to fetch a snapshot and then subscribe. That silently
loses every update that lands in between: the client believes it is current, the book has
already moved, and nothing will ever tell it otherwise. A small, silent gap is the worst
kind.

So the order is inverted:

```
1. subscribe                     the hub starts queueing immediately; nothing can be missed
2. snapshot at sequence S        taken on the market's goroutine, so it reflects exactly
                                 commands 1..S, and updates for those are already queued
3. send the snapshot
4. forward queued messages,      the snapshot already accounts for everything up to S
   discarding seq <= S
5. continue live
```

Step 4 needs no separate catch-up phase and no boundary marker. Messages leave the hub in
sequence order, because they are published from the market's own goroutine, so once one
passes the `seq > S` test every later one does too. The filter is applied uniformly and
stops mattering by itself.

**If the subscriber is evicted at any point**, including while the snapshot is being taken,
messages were lost and continuing would give the client a view with a hole in it. The server
sends `error` with `resync: true` and closes, rather than delivering whatever is left on the
far side of the gap.

### Recovery

On `resync`, or on any disconnect:

1. open a new WebSocket (which performs the sequence above), or
2. refetch `GET /snapshot` and `GET /orders`, then reconnect.

There is deliberately no gap-recovery protocol — no message replay, no sequence-range
requests, no incremental catch-up. For a single-user demo, reconnecting and taking a fresh
snapshot is simpler and cannot be subtly wrong. A real feed would want snapshot sequencing
and catch-up; that is listed as a future extension.

---

## Backpressure

The matching engine must never wait for a socket. If it could, one browser that stopped
reading would set the pace of the exchange for everyone.

```
market actor (one goroutine per symbol)
      │  publishes inline with matching, never blocks
      ▼
market data hub
      ├──► subscriber 1  bounded queue ──► writer goroutine ──► WebSocket
      ├──► subscriber 2  bounded queue ──► writer goroutine ──► WebSocket
      └──► subscriber 3  bounded queue ──► writer goroutine ──► WebSocket
```

- Every publish is a non-blocking send. A subscriber with a full queue is **evicted on the
  spot** rather than waited for.
- Eviction is per-subscriber. A stalled client costs the others nothing, and the market
  keeps sequencing at full speed.
- Each write carries a deadline, so a stalled TCP connection cannot pin its writer goroutine
  even though the hub itself never blocks.
- Queue depth defaults to 256 and is configurable with `-subscriber-queue`.

---

## Why snapshots are not the live path

`Snapshot` collects every level on the book and sorts them to publish the top ten, so it
costs O(L log L) in the number of distinct prices. Broadcasting one per command would make
market data dominate the matching underneath it:

| distinct price levels | full snapshot | incremental update |
| --- | --- | --- |
| 20 | 537 ns | 300 ns |
| 200 | 6.5 µs | 274 ns |
| 2,000 | 139 µs | 325 ns |

(Apple M3. The update figures include the two order submissions that produced the change, so
the delta collection itself is cheaper still.)

The incremental path is flat in the size of the book because the engine records which levels
each command touched and reports only those. Snapshots remain the right tool for initial
synchronisation and recovery, where they are paid once per connection rather than once per
command.

The order book's data structures were **not** changed for this. Top-K snapshot generation
could be optimised independently — select the top ten instead of sorting all of them, or
maintain the book in an ordered structure — if books with thousands of distinct price levels
ever become an actual workload. Today they are not: a running market holds a few dozen
levels.
