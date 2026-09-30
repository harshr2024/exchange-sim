import { useState, type FormEvent } from "react";
import { submitOrder } from "../api";
import { formatTicks } from "../format";
import { buildOrderPayload } from "../orders";
import type { OrderType, Side } from "../types";

export function OrderEntry({
  symbol,
  tickSizeCents,
  bestBid,
  bestAsk,
}: {
  symbol: string;
  tickSizeCents: number;
  bestBid: number | null;
  bestAsk: number | null;
}) {
  const [side, setSide] = useState<Side>("buy");
  const [type, setType] = useState<OrderType>("limit");
  const [quantity, setQuantity] = useState("50");
  const [price, setPrice] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);

  const touch = side === "buy" ? bestAsk : bestBid;

  function edit(next: () => void) {
    setError("");
    setNotice("");
    next();
  }

  function useTouch() {
    if (touch == null) return;
    edit(() => setPrice(formatTicks(touch, tickSizeCents)));
  }

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    const draft = buildOrderPayload({ side, type, quantity, price, tickSizeCents });
    if (!draft.ok) {
      setNotice("");
      setError(draft.error);
      return;
    }
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await submitOrder(symbol, draft.body);
      setNotice("Submitted.");
    } catch (err) {
      setError(err instanceof Error ? err.message : "Order was rejected.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="entry" onSubmit={(event) => void onSubmit(event)}>
      <div className="seg" role="group" aria-label="Side">
        <button type="button" className="buy" aria-pressed={side === "buy"} onClick={() => edit(() => setSide("buy"))}>Buy</button>
        <button type="button" className="sell" aria-pressed={side === "sell"} onClick={() => edit(() => setSide("sell"))}>Sell</button>
      </div>
      <div className="seg" role="group" aria-label="Order type">
        <button type="button" aria-pressed={type === "limit"} onClick={() => edit(() => setType("limit"))}>Limit</button>
        <button type="button" aria-pressed={type === "market"} onClick={() => edit(() => setType("market"))}>Market</button>
      </div>
      <div className="field">
        <label htmlFor="order-qty">Quantity</label>
        <input
          id="order-qty"
          inputMode="numeric"
          autoComplete="off"
          value={quantity}
          onChange={(event) => edit(() => setQuantity(event.target.value))}
        />
      </div>
      {type === "limit" ? (
        <div className="field">
          <label htmlFor="order-price">Price</label>
          <span className="price-line">
            <input
              id="order-price"
              inputMode="decimal"
              autoComplete="off"
              placeholder="100.10"
              value={price}
              onChange={(event) => edit(() => setPrice(event.target.value))}
            />
            <button type="button" className="touch" onClick={useTouch} disabled={touch == null}>
              Touch
            </button>
          </span>
        </div>
      ) : (
        <p className="hint">Market orders need a side and a quantity. Unfilled size is canceled.</p>
      )}
      <button type="submit" className={`submit ${side}`} disabled={busy}>
        {busy ? "Sending" : side === "buy" ? `Buy ${symbol}` : `Sell ${symbol}`}
      </button>
      {error ? <p className="alert" role="alert">{error}</p> : null}
      {notice ? <p className="notice">{notice}</p> : null}
    </form>
  );
}
