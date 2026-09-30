import { isTerminal } from "./types";
import type {
  Level,
  OrderMessage,
  Side,
  SnapshotMessage,
  StreamMessage,
  Trade,
  UpdateMessage,
  WireOrder,
} from "./types";

/** How many levels each side of the ladder shows. The feed's snapshot depth matches this. */
export const DEPTH = 10;

/** Prints kept for the tape and the chart. Older ones scroll off; their ids stay seen. */
const MAX_TRADES = 180;

export interface TrackedOrder extends WireOrder {
  /** Market sequence of the lifecycle event that installed this view. */
  lastSeq: number;
}

export interface MarketState {
  symbol: string;
  synced: boolean;
  /** Sequence of the snapshot currently in force. Events at or below it are already included. */
  snapshotSeq: number;
  /** Highest update sequence applied since that snapshot. */
  lastUpdateSeq: number;
  /** Highest sequence applied, for display. */
  latestSeq: number;
  bids: Map<number, Level>;
  asks: Map<number, Level>;
  hasBestBid: boolean;
  bestBid: number;
  hasBestAsk: boolean;
  bestAsk: number;
  hasLast: boolean;
  last: number;
  /** Oldest first. */
  trades: Trade[];
  seenTradeIds: Set<number>;
  orders: Map<number, TrackedOrder>;
  /** Sequence at which an order became terminal, so a late event cannot bring it back. */
  closed: Map<number, number>;
}

export interface MarketView {
  synced: boolean;
  sequence: number;
  bids: Level[];
  asks: Level[];
  bestBid: number | null;
  bestAsk: number | null;
  spread: number | null;
  last: number | null;
  trades: Trade[];
  orders: TrackedOrder[];
}

export function emptyState(symbol: string): MarketState {
  return {
    symbol,
    synced: false,
    snapshotSeq: 0,
    lastUpdateSeq: 0,
    latestSeq: 0,
    bids: new Map(),
    asks: new Map(),
    hasBestBid: false,
    bestBid: 0,
    hasBestAsk: false,
    bestAsk: 0,
    hasLast: false,
    last: 0,
    trades: [],
    seenTradeIds: new Set(),
    orders: new Map(),
    closed: new Map(),
  };
}

/** Drop streamed book, tape, and orders. The next snapshot is the new authority. */
export function discardStreamingState(state: MarketState): MarketState {
  return emptyState(state.symbol);
}

export function reduceMarket(state: MarketState, message: StreamMessage): MarketState {
  // Error frames use an empty symbol. Any other frame for a different market is ignored.
  if (message.symbol && message.symbol !== state.symbol) return state;
  switch (message.type) {
    case "error":
      return message.resync ? discardStreamingState(state) : state;
    case "snapshot":
      return applySnapshot(message.symbol || state.symbol, message);
    case "update":
      return applyUpdate(state, message);
    case "order":
      return applyOrder(state, message);
    default:
      return state;
  }
}

export function parseStreamMessage(raw: string): StreamMessage | null {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!value || typeof value !== "object") return null;
  const type = (value as { type?: unknown }).type;
  if (type !== "snapshot" && type !== "update" && type !== "order" && type !== "error") {
    return null;
  }
  return value as StreamMessage;
}

function applySnapshot(symbol: string, message: SnapshotMessage): MarketState {
  const book = message.book;
  if (!book) return emptyState(symbol);
  const next = emptyState(symbol);
  next.synced = true;
  next.snapshotSeq = message.seq;
  next.lastUpdateSeq = message.seq;
  next.latestSeq = message.seq;
  next.bids = levelsFrom(book.bids);
  next.asks = levelsFrom(book.asks);
  next.hasBestBid = book.has_best_bid === true;
  next.bestBid = next.hasBestBid ? (book.best_bid ?? 0) : 0;
  next.hasBestAsk = book.has_best_ask === true;
  next.bestAsk = next.hasBestAsk ? (book.best_ask ?? 0) : 0;
  next.hasLast = book.has_last_trade === true;
  next.last = next.hasLast ? (book.last_trade_price ?? 0) : 0;
  for (const order of message.orders ?? []) {
    if (isTerminal(order.status)) continue;
    next.orders.set(order.id, { ...order, lastSeq: message.seq });
  }
  return next;
}

function applyUpdate(state: MarketState, message: UpdateMessage): MarketState {
  // The snapshot already accounts for this sequence, and an older update would
  // roll the book backwards. A repeated sequence is not applied again: trades
  // are executions, and the level quantities being absolute does not make the
  // trade list safe to replay.
  if (!state.synced || message.seq <= state.snapshotSeq || message.seq <= state.lastUpdateSeq) {
    return state;
  }
  const update = message.update;
  if (!update) return state;

  const next = cloneState(state);
  next.lastUpdateSeq = message.seq;
  next.latestSeq = Math.max(next.latestSeq, message.seq);

  for (const level of update.levels ?? []) {
    const book = level.side === "buy" ? next.bids : next.asks;
    if (level.quantity <= 0) book.delete(level.price);
    else book.set(level.price, { price: level.price, quantity: level.quantity, orders: level.orders });
  }

  next.hasBestBid = update.has_best_bid === true;
  next.bestBid = next.hasBestBid ? (update.best_bid ?? 0) : 0;
  next.hasBestAsk = update.has_best_ask === true;
  next.bestAsk = next.hasBestAsk ? (update.best_ask ?? 0) : 0;
  if (update.has_last_trade === true) {
    next.hasLast = true;
    next.last = update.last_trade_price ?? next.last;
  }

  for (const trade of update.trades ?? []) {
    if (next.seenTradeIds.has(trade.id)) continue;
    next.seenTradeIds.add(trade.id);
    next.trades.push(trade);
  }
  if (next.trades.length > MAX_TRADES) {
    next.trades.splice(0, next.trades.length - MAX_TRADES);
  }
  return next;
}

function applyOrder(state: MarketState, message: OrderMessage): MarketState {
  if (!state.synced || message.seq <= state.snapshotSeq) return state;
  const order = message.order;
  if (!order) return state;

  const closedAt = state.closed.get(order.id);
  if (closedAt !== undefined && message.seq <= closedAt) return state;

  const prev = state.orders.get(order.id);
  if (prev && message.seq < prev.lastSeq) return state;

  const next = cloneState(state);
  next.latestSeq = Math.max(next.latestSeq, message.seq);
  if (isTerminal(order.status)) {
    next.orders.delete(order.id);
    next.closed.set(order.id, message.seq);
    return next;
  }
  next.closed.delete(order.id);
  next.orders.set(order.id, { ...order, lastSeq: message.seq });
  return next;
}

function levelsFrom(rows: Level[] | null | undefined): Map<number, Level> {
  const map = new Map<number, Level>();
  for (const row of rows ?? []) {
    if (row.quantity <= 0) continue;
    map.set(row.price, { price: row.price, quantity: row.quantity, orders: row.orders });
  }
  return map;
}

function cloneState(state: MarketState): MarketState {
  return {
    ...state,
    bids: new Map(state.bids),
    asks: new Map(state.asks),
    trades: state.trades.slice(),
    seenTradeIds: new Set(state.seenTradeIds),
    orders: new Map(state.orders),
    closed: new Map(state.closed),
  };
}

/** Best price first. Bids descend, asks ascend. */
export function topLevels(levels: Map<number, Level>, side: Side, depth = DEPTH): Level[] {
  const rows = [...levels.values()];
  rows.sort((a, b) => (side === "buy" ? b.price - a.price : a.price - b.price));
  return rows.slice(0, depth);
}

export function spreadTicks(state: MarketState): number | null {
  if (!state.hasBestBid || !state.hasBestAsk) return null;
  return state.bestAsk - state.bestBid;
}

export function activeOrders(state: MarketState): TrackedOrder[] {
  return [...state.orders.values()]
    .filter((order) => !isTerminal(order.status))
    .sort((a, b) => a.priority_sequence - b.priority_sequence || a.id - b.id);
}

export function project(state: MarketState): MarketView {
  return {
    synced: state.synced,
    sequence: state.latestSeq,
    bids: topLevels(state.bids, "buy"),
    asks: topLevels(state.asks, "sell"),
    bestBid: state.hasBestBid ? state.bestBid : null,
    bestAsk: state.hasBestAsk ? state.bestAsk : null,
    spread: spreadTicks(state),
    last: state.hasLast ? state.last : null,
    trades: state.trades,
    orders: activeOrders(state),
  };
}
