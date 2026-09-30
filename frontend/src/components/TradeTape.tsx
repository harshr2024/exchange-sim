import { formatQty, formatTicks } from "../format";
import type { Trade } from "../types";

const TAPE_ROWS = 18;

export function TradeTape({
  trades,
  tickSizeCents,
  hold,
}: {
  trades: Trade[];
  tickSizeCents: number;
  hold: string | null;
}) {
  if (hold) return <p className="hold">{hold}</p>;
  const rows = [...trades].reverse().slice(0, TAPE_ROWS);
  return (
    <table className="tape">
      <colgroup>
        <col className="c-price" />
        <col className="c-qty" />
        <col className="c-side" />
      </colgroup>
      <thead>
        <tr>
          <th>Price</th>
          <th className="num">Qty</th>
          <th>Side</th>
        </tr>
      </thead>
      <tbody>
        {rows.length === 0 ? (
          <tr>
            <td className="empty" colSpan={3}>No trades since the snapshot.</td>
          </tr>
        ) : rows.map((trade) => (
          <tr key={trade.id} data-trade-id={trade.id}>
            <td className={trade.aggressor_side === "buy" ? "bid" : "ask"}>
              {formatTicks(trade.price, tickSizeCents)}
            </td>
            <td className="num">{formatQty(trade.quantity)}</td>
            <td className={trade.aggressor_side === "buy" ? "bid" : "ask"}>
              {trade.aggressor_side === "buy" ? "Buy" : "Sell"}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
