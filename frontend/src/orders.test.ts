import { describe, expect, it } from "vitest";
import { dollarsToTicks, formatTicks } from "./format";
import { buildOrderPayload } from "./orders";

describe("order entry payloads", () => {
  it("sends a limit order as side, type, integer quantity, and tick price", () => {
    const draft = buildOrderPayload({
      side: "buy",
      type: "limit",
      quantity: "50",
      price: "100.10",
      tickSizeCents: 1,
    });
    expect(draft.ok).toBe(true);
    if (!draft.ok) return;
    expect(draft.body).toEqual({ side: "buy", type: "limit", quantity: 50, price: 10010 });
    expect(JSON.parse(JSON.stringify(draft.body))).not.toHaveProperty("participant");
    expect(formatTicks(10010, 1)).toBe("100.10");
  });

  it("sends a market order with side and quantity only", () => {
    const draft = buildOrderPayload({
      side: "sell",
      type: "market",
      quantity: "80",
      price: "100.10",
      tickSizeCents: 1,
    });
    expect(draft.ok).toBe(true);
    if (!draft.ok) return;
    expect(draft.body).toEqual({ side: "sell", type: "market", quantity: 80 });
    expect(JSON.parse(JSON.stringify(draft.body))).toEqual({
      side: "sell",
      type: "market",
      quantity: 80,
    });
    expect(draft.body).not.toHaveProperty("price");
    expect(draft.body).not.toHaveProperty("participant");
  });

  it("rejects a missing quantity or an invalid limit price before any request", () => {
    expect(buildOrderPayload({
      side: "buy",
      type: "limit",
      quantity: "0",
      price: "100.00",
      tickSizeCents: 1,
    }).ok).toBe(false);
    expect(buildOrderPayload({
      side: "buy",
      type: "market",
      quantity: "1.5",
      price: "",
      tickSizeCents: 1,
    }).ok).toBe(false);
    expect(buildOrderPayload({
      side: "buy",
      type: "limit",
      quantity: "10",
      price: "",
      tickSizeCents: 1,
    }).ok).toBe(false);
    expect(dollarsToTicks("100.001", 1)).toBeNull();
    expect(dollarsToTicks("0.01", 5)).toBeNull();
    expect(dollarsToTicks("100.05", 5)).toBe(2001);
    expect(formatTicks(2001, 5)).toBe("100.05");
  });
});
