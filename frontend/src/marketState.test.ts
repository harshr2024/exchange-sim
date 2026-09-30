import { describe, expect, it } from "vitest";
import {
  activeOrders,
  discardStreamingState,
  emptyState,
  project,
  reduceMarket,
} from "./marketState";
import type { Level, StreamMessage, Trade, WireOrder } from "./types";

function level(price: number, quantity: number, orders = 1): Level {
  return { price, quantity, orders };
}

function order(over: Partial<WireOrder> = {}): WireOrder {
  return {
    id: 7,
    participant: "human",
    symbol: "NOVA",
    side: "buy",
    type: "limit",
    limit_price: 10000,
    original_qty: 100,
    remaining_qty: 100,
    filled_qty: 0,
    canceled_qty: 0,
    status: "open",
    priority_sequence: 4,
    ...over,
  };
}

function trade(id: number, price = 10010, quantity = 10): Trade {
  return {
    id,
    symbol: "NOVA",
    buy_order_id: 7,
    sell_order_id: 3,
    price,
    quantity,
    aggressor_side: "buy",
    sequence: 6,
  };
}

function snapshot(seq: number, opts?: {
  bids?: Level[];
  asks?: Level[];
  orders?: WireOrder[];
  last?: number;
}): StreamMessage {
  const bids = opts?.bids ?? [level(9990, 40), level(9980, 25)];
  const asks = opts?.asks ?? [level(10010, 30), level(10020, 50)];
  return {
    type: "snapshot",
    symbol: "NOVA",
    seq,
    book: {
      symbol: "NOVA",
      sequence: seq,
      bids,
      asks,
      best_bid: bids[0]?.price,
      has_best_bid: bids.length > 0,
      best_ask: asks[0]?.price,
      has_best_ask: asks.length > 0,
      has_last_trade: opts?.last != null,
      last_trade_price: opts?.last,
    },
    orders: opts?.orders ?? [],
  };
}

describe("initial snapshot", () => {
  it("replaces book, quotes, and human orders and starts sequence tracking", () => {
    const dirty = reduceMarket(emptyState("NOVA"), snapshot(3, {
      bids: [level(1, 9)],
      orders: [order({ id: 1 })],
      last: 1,
    }));
    const withTape = reduceMarket(dirty, {
      type: "update",
      symbol: "NOVA",
      seq: 4,
      update: {
        symbol: "NOVA",
        sequence: 4,
        has_best_bid: true,
        best_bid: 1,
        has_best_ask: false,
        has_last_trade: true,
        last_trade_price: 1,
        trades: [trade(99, 1, 1)],
      },
    });

    const next = reduceMarket(withTape, snapshot(12, {
      bids: [level(9990, 40), level(9980, 25)],
      asks: [level(10010, 30)],
      orders: [order({ id: 7, filled_qty: 15, remaining_qty: 85, status: "partially_filled" })],
      last: 10010,
    }));
    const view = project(next);

    expect(next.synced).toBe(true);
    expect(next.snapshotSeq).toBe(12);
    expect(next.lastUpdateSeq).toBe(12);
    expect(view.sequence).toBe(12);
    expect(view.bids.map((row) => row.price)).toEqual([9990, 9980]);
    expect(view.asks.map((row) => [row.price, row.quantity])).toEqual([[10010, 30]]);
    expect(view.bestBid).toBe(9990);
    expect(view.bestAsk).toBe(10010);
    expect(view.spread).toBe(20);
    expect(view.last).toBe(10010);
    expect(view.trades).toEqual([]);
    expect(view.orders.map((row) => row.id)).toEqual([7]);
    expect(view.orders[0]?.filled_qty).toBe(15);
    expect(next.bids.has(1)).toBe(false);
    expect(next.orders.has(1)).toBe(false);
  });

  it("ignores incremental events until a snapshot arrives", () => {
    const state = reduceMarket(emptyState("NOVA"), {
      type: "update",
      symbol: "NOVA",
      seq: 1,
      update: {
        symbol: "NOVA",
        sequence: 1,
        levels: [{ side: "buy", price: 100, quantity: 5, orders: 1 }],
        has_best_bid: true,
        best_bid: 100,
        has_best_ask: false,
        has_last_trade: false,
      },
    });
    expect(state.synced).toBe(false);
    expect(state.bids.size).toBe(0);
  });
});

describe("absolute level updates", () => {
  it("writes the server quantity and deletes a level at zero", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(5, {
      bids: [level(9990, 100, 2)],
      asks: [level(10010, 30)],
    }));

    state = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 6,
      update: {
        symbol: "NOVA",
        sequence: 6,
        levels: [
          { side: "buy", price: 9990, quantity: 40, orders: 1 },
          { side: "sell", price: 10030, quantity: 12, orders: 1 },
        ],
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10010,
        has_last_trade: false,
      },
    });
    expect(state.bids.get(9990)?.quantity).toBe(40);
    expect(state.asks.get(10030)?.quantity).toBe(12);

    state = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 7,
      update: {
        symbol: "NOVA",
        sequence: 7,
        levels: [{ side: "sell", price: 10010, quantity: 0, orders: 0 }],
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10030,
        has_last_trade: true,
        last_trade_price: 10010,
        trades: [trade(1, 10010, 30)],
      },
    });
    expect(state.asks.has(10010)).toBe(false);
    expect(project(state).bestAsk).toBe(10030);
    expect(project(state).last).toBe(10010);
    expect(state.trades).toHaveLength(1);

    const again = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 7,
      update: {
        symbol: "NOVA",
        sequence: 7,
        levels: [{ side: "buy", price: 9990, quantity: 40, orders: 1 }],
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10030,
        has_last_trade: true,
        last_trade_price: 10010,
        trades: [trade(1, 10010, 30)],
      },
    });
    expect(again).toBe(state);
    expect(again.trades).toHaveLength(1);
    expect(again.bids.get(9990)?.quantity).toBe(40);
  });

  it("does not append a trade id that was already printed", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(5));
    const update = {
      type: "update" as const,
      symbol: "NOVA",
      seq: 8,
      update: {
        symbol: "NOVA",
        sequence: 8,
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10010,
        has_last_trade: true,
        last_trade_price: 10010,
        trades: [trade(1), trade(1)],
      },
    };
    state = reduceMarket(state, update);
    expect(state.trades.map((row) => row.id)).toEqual([1]);
  });

  it("rejects an update older than one already applied", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(5, { bids: [level(9990, 100)] }));
    state = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 9,
      update: {
        symbol: "NOVA",
        sequence: 9,
        levels: [{ side: "buy", price: 9990, quantity: 10, orders: 1 }],
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10010,
        has_last_trade: false,
      },
    });
    const stale = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 6,
      update: {
        symbol: "NOVA",
        sequence: 6,
        levels: [{ side: "buy", price: 9990, quantity: 100, orders: 2 }],
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: false,
        has_last_trade: false,
        trades: [trade(50)],
      },
    });
    expect(stale).toBe(state);
    expect(stale.bids.get(9990)?.quantity).toBe(10);
    expect(stale.hasBestAsk).toBe(true);
    expect(stale.trades).toHaveLength(0);
  });

  it("keeps only the top of a deeper book", () => {
    const bids = Array.from({ length: 12 }, (_, i) => level(10000 - i, 1));
    const state = reduceMarket(emptyState("NOVA"), snapshot(1, { bids, asks: [] }));
    expect(project(state).bids).toHaveLength(10);
    expect(project(state).bids[0]?.price).toBe(10000);
    expect(project(state).bids[9]?.price).toBe(9991);
  });
});

describe("order lifecycle", () => {
  it("applies a passive partial fill as the complete order state", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(10, {
      orders: [order({ id: 7, original_qty: 100, remaining_qty: 100, filled_qty: 0, status: "open" })],
    }));
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 11,
      event: "order_filled",
      order: order({
        id: 7,
        original_qty: 100,
        filled_qty: 40,
        remaining_qty: 60,
        status: "partially_filled",
      }),
    });
    const working = activeOrders(state);
    expect(working).toHaveLength(1);
    expect(working[0]).toMatchObject({
      id: 7,
      filled_qty: 40,
      remaining_qty: 60,
      original_qty: 100,
      status: "partially_filled",
      limit_price: 10000,
    });
  });

  it("does not infer that fill from a trade on the tape", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(10, {
      orders: [order()],
    }));
    state = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 11,
      update: {
        symbol: "NOVA",
        sequence: 11,
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10010,
        has_last_trade: true,
        last_trade_price: 10000,
        trades: [trade(4, 10000, 40)],
      },
    });
    expect(state.orders.get(7)?.filled_qty).toBe(0);
    expect(state.trades).toHaveLength(1);
  });

  it("drops a filled order and a canceled order from the working set", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(10, {
      orders: [order({ id: 7 }), order({ id: 8, side: "sell", priority_sequence: 5 })],
    }));
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 12,
      event: "order_filled",
      order: order({ id: 7, filled_qty: 100, remaining_qty: 0, status: "filled" }),
    });
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 13,
      event: "order_canceled",
      order: order({
        id: 8,
        side: "sell",
        filled_qty: 20,
        remaining_qty: 0,
        canceled_qty: 80,
        status: "canceled",
      }),
    });
    expect(activeOrders(state)).toEqual([]);
    expect(state.orders.size).toBe(0);
  });

  it("ignores a lifecycle event older than the one already applied", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(10, { orders: [order()] }));
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 15,
      event: "order_filled",
      order: order({ filled_qty: 40, remaining_qty: 60, status: "partially_filled" }),
    });
    const stale = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 14,
      event: "order_accepted",
      order: order({ filled_qty: 0, remaining_qty: 100, status: "open" }),
    });
    expect(stale).toBe(state);
    expect(stale.orders.get(7)?.filled_qty).toBe(40);
    expect(stale.orders.get(7)?.status).toBe("partially_filled");
  });

  it("does not resurrect a terminal order from an older event", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(10, { orders: [order()] }));
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 16,
      event: "order_canceled",
      order: order({ remaining_qty: 0, canceled_qty: 100, status: "canceled" }),
    });
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 15,
      event: "order_filled",
      order: order({ filled_qty: 10, remaining_qty: 90, status: "partially_filled" }),
    });
    expect(state.orders.has(7)).toBe(false);
  });

  it("applies an order event that shares a sequence with a book update", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(10));
    state = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 18,
      update: {
        symbol: "NOVA",
        sequence: 18,
        levels: [{ side: "buy", price: 10000, quantity: 60, orders: 1 }],
        has_best_bid: true,
        best_bid: 10000,
        has_best_ask: true,
        best_ask: 10010,
        has_last_trade: true,
        last_trade_price: 10010,
        trades: [trade(9, 10010, 40)],
      },
    });
    state = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 18,
      event: "order_rested",
      order: order({
        filled_qty: 40,
        remaining_qty: 60,
        status: "partially_filled",
        limit_price: 10010,
      }),
    });
    expect(state.trades).toHaveLength(1);
    expect(state.orders.get(7)?.status).toBe("partially_filled");
    expect(state.bids.get(10000)?.quantity).toBe(60);
  });

  it("drops events already covered by the snapshot sequence", () => {
    const state = reduceMarket(emptyState("NOVA"), snapshot(20, {
      orders: [order({ filled_qty: 5, remaining_qty: 95, status: "partially_filled" })],
    }));
    const echoed = reduceMarket(state, {
      type: "order",
      symbol: "NOVA",
      seq: 20,
      event: "order_accepted",
      order: order({ filled_qty: 0, remaining_qty: 100, status: "open" }),
    });
    expect(echoed).toBe(state);
    expect(echoed.orders.get(7)?.filled_qty).toBe(5);
  });
});

describe("reconnect and resync", () => {
  it("discards stale streaming state and installs the next snapshot", () => {
    let state = reduceMarket(emptyState("NOVA"), snapshot(4, {
      bids: [level(9990, 80)],
      orders: [order({ id: 1, filled_qty: 10, remaining_qty: 90, status: "partially_filled" })],
      last: 10000,
    }));
    state = reduceMarket(state, {
      type: "update",
      symbol: "NOVA",
      seq: 5,
      update: {
        symbol: "NOVA",
        sequence: 5,
        has_best_bid: true,
        best_bid: 9990,
        has_best_ask: true,
        best_ask: 10010,
        has_last_trade: true,
        last_trade_price: 10000,
        trades: [trade(3, 10000, 10)],
      },
    });

    const dropped = reduceMarket(state, {
      type: "error",
      symbol: "",
      reason: "subscriber fell behind the market data feed",
      resync: true,
    });
    expect(dropped.synced).toBe(false);
    expect(dropped.bids.size).toBe(0);
    expect(dropped.orders.size).toBe(0);
    expect(dropped.trades).toEqual([]);
    expect(dropped.symbol).toBe("NOVA");
    expect(discardStreamingState(state).bids.size).toBe(0);

    const live = reduceMarket(dropped, snapshot(30, {
      bids: [level(10100, 5)],
      asks: [level(10120, 8)],
      orders: [order({ id: 44, limit_price: 10100, priority_sequence: 30 })],
      last: 10110,
    }));
    expect(project(live).bids.map((row) => row.price)).toEqual([10100]);
    expect(project(live).orders.map((row) => row.id)).toEqual([44]);
    expect(project(live).last).toBe(10110);
    expect(live.snapshotSeq).toBe(30);

    const leftover = reduceMarket(live, {
      type: "order",
      symbol: "NOVA",
      seq: 8,
      event: "order_filled",
      order: order({ id: 1, filled_qty: 50, remaining_qty: 50, status: "partially_filled" }),
    });
    expect(leftover.orders.has(1)).toBe(false);
    expect(leftover.orders.has(44)).toBe(true);

    const kept = reduceMarket(live, {
      type: "error",
      symbol: "NOVA",
      reason: "notice",
      resync: false,
    });
    expect(kept).toBe(live);
  });
});
