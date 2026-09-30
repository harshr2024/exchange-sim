export type Side = "buy" | "sell";
export type OrderType = "limit" | "market";
export type OrderStatus = "open" | "partially_filled" | "filled" | "canceled";

export interface Level {
  price: number;
  quantity: number;
  orders: number;
}

/** Order view as published by the exchange. Enough to render without the tape. */
export interface WireOrder {
  id: number;
  participant: string;
  symbol: string;
  side: Side;
  type: OrderType;
  limit_price?: number;
  original_qty: number;
  remaining_qty: number;
  filled_qty: number;
  canceled_qty: number;
  status: OrderStatus;
  priority_sequence: number;
}

export interface Trade {
  id: number;
  symbol: string;
  buy_order_id: number;
  sell_order_id: number;
  price: number;
  quantity: number;
  aggressor_side: Side;
  sequence: number;
}

export interface LevelChange {
  side: Side;
  price: number;
  quantity: number;
  orders: number;
}

export interface BookSnapshot {
  symbol: string;
  sequence: number;
  bids?: Level[] | null;
  asks?: Level[] | null;
  best_bid?: number;
  has_best_bid?: boolean;
  best_ask?: number;
  has_best_ask?: boolean;
  spread?: number;
  has_spread?: boolean;
  mid?: number;
  has_mid?: boolean;
  last_trade_price?: number;
  has_last_trade?: boolean;
}

export interface MarketUpdate {
  symbol: string;
  sequence: number;
  levels?: LevelChange[] | null;
  trades?: Trade[] | null;
  best_bid?: number;
  has_best_bid?: boolean;
  best_ask?: number;
  has_best_ask?: boolean;
  last_trade_price?: number;
  has_last_trade?: boolean;
}

export interface SnapshotMessage {
  type: "snapshot";
  symbol: string;
  seq: number;
  book?: BookSnapshot | null;
  orders?: WireOrder[] | null;
}

export interface UpdateMessage {
  type: "update";
  symbol: string;
  seq: number;
  update?: MarketUpdate | null;
}

export interface OrderMessage {
  type: "order";
  symbol: string;
  seq: number;
  event?: string;
  order?: WireOrder | null;
}

export interface ErrorMessage {
  type: "error";
  symbol?: string;
  seq?: number;
  reason?: string;
  resync?: boolean;
}

export type StreamMessage = SnapshotMessage | UpdateMessage | OrderMessage | ErrorMessage;

export interface Instrument {
  symbol: string;
  display_name: string;
  tick_size_cents: number;
  initial_reference_price: number;
}

export type ConnectionStatus = "connecting" | "live" | "resyncing";

export function isTerminal(status: OrderStatus): boolean {
  return status === "filled" || status === "canceled";
}
