import { formatQty, formatTicks } from "../format";
import type { MarketView } from "../marketState";

export function OrderBook({
  view,
  tickSizeCents,
  hold,
}: {
  view: MarketView;
  tickSizeCents: number;
  hold: string | null;
}) {
  if (hold) return <p className="hold">{hold}</p>;

  const maxQty = Math.max(
    1,
    ...view.bids.map((level) => level.quantity),
    ...view.asks.map((level) => level.quantity),
  );
  const asks = [...view.asks].reverse();

  return (
    <div className="ladder">
      <div className="book-row cols">
        <span>Price</span>
        <span className="qty">Qty</span>
        <span className="count">Orders</span>
      </div>
      <div className="asks">
        {asks.length === 0 ? <p className="empty">No asks</p> : asks.map((level) => (
          <LevelRow key={level.price} side="ask" price={level.price} quantity={level.quantity} orders={level.orders} maxQty={maxQty} tickSizeCents={tickSizeCents} />
        ))}
      </div>
      <div className="spread-row">
        <span>
          <span className="k">Last</span>
          <span className="v">{view.last == null ? "—" : formatTicks(view.last, tickSizeCents)}</span>
        </span>
        <span>
          <span className="k">Spread</span>
          <span className="v spread">{view.spread == null ? "—" : formatTicks(view.spread, tickSizeCents)}</span>
        </span>
      </div>
      <div className="bids">
        {view.bids.length === 0 ? <p className="empty">No bids</p> : view.bids.map((level) => (
          <LevelRow key={level.price} side="bid" price={level.price} quantity={level.quantity} orders={level.orders} maxQty={maxQty} tickSizeCents={tickSizeCents} />
        ))}
      </div>
    </div>
  );
}

function LevelRow({
  side,
  price,
  quantity,
  orders,
  maxQty,
  tickSizeCents,
}: {
  side: "bid" | "ask";
  price: number;
  quantity: number;
  orders: number;
  maxQty: number;
  tickSizeCents: number;
}) {
  const width = Math.max(8, Math.round((quantity / maxQty) * 64));
  return (
    <div
      className={`book-row level ${side}`}
      style={{ ["--w" as string]: `${width}%` }}
      data-side={side === "bid" ? "buy" : "sell"}
      data-price={price}
      data-qty={quantity}
    >
      <span className="px">{formatTicks(price, tickSizeCents)}</span>
      <span className="qty">{formatQty(quantity)}</span>
      <span className="count">{orders}</span>
    </div>
  );
}
