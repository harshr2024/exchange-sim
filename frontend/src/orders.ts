import { dollarsToTicks } from "./format";
import type { OrderType, Side } from "./types";

/** Body of POST /api/markets/{symbol}/orders. No participant field exists. */
export interface OrderBody {
  side: Side;
  type: OrderType;
  quantity: number;
  price?: number;
}

export type OrderDraft =
  | { ok: true; body: OrderBody }
  | { ok: false; error: string };

const QTY_ERROR = "Quantity must be a positive whole number.";
const PRICE_ERROR = "Enter a positive limit price in dollars and cents.";

function parseQuantity(raw: string): number | null {
  const text = raw.trim();
  if (!/^[1-9][0-9]*$/.test(text)) return null;
  const qty = Number(text);
  if (!Number.isSafeInteger(qty) || qty <= 0) return null;
  return qty;
}

/**
 * Builds the JSON the exchange accepts.
 * Limit orders need a positive tick price. Market orders send side and quantity only.
 */
export function buildOrderPayload(input: {
  side: Side;
  type: OrderType;
  quantity: string;
  price: string;
  tickSizeCents: number;
}): OrderDraft {
  const quantity = parseQuantity(input.quantity);
  if (quantity == null) return { ok: false, error: QTY_ERROR };
  if (input.type === "market") {
    return { ok: true, body: { side: input.side, type: "market", quantity } };
  }
  const price = dollarsToTicks(input.price, input.tickSizeCents);
  if (price == null) return { ok: false, error: PRICE_ERROR };
  return { ok: true, body: { side: input.side, type: "limit", quantity, price } };
}
