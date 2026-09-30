import { useState } from "react";
import { cancelOrder } from "../api";
import { formatQty, formatTicks } from "../format";
import type { TrackedOrder } from "../marketState";

const STATUS_LABEL: Record<string, string> = {
  open: "Open",
  partially_filled: "Partial",
  filled: "Filled",
  canceled: "Canceled",
};

export function OpenOrders({
  symbol,
  orders,
  tickSizeCents,
  hold,
}: {
  symbol: string;
  orders: TrackedOrder[];
  tickSizeCents: number;
  hold: string | null;
}) {
  const [pending, setPending] = useState<number | null>(null);
  const [error, setError] = useState("");

  async function onCancel(id: number) {
    setError("");
    setPending(id);
    try {
      await cancelOrder(symbol, id);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Cancel failed.");
    } finally {
      setPending(null);
    }
  }

  if (hold) return <p className="hold">{hold}</p>;

  return (
    <div>
      {error ? <p className="alert" role="alert">{error}</p> : null}
      <table className="orders">
        <colgroup>
          <col className="c-side" />
          <col className="c-type" />
          <col className="c-px" />
          <col className="c-qty" />
          <col className="c-qty" />
          <col className="c-qty" />
          <col className="c-status" />
          <col className="c-act" />
        </colgroup>
        <thead>
          <tr>
            <th>Side</th>
            <th>Type</th>
            <th className="num">Price</th>
            <th className="num">Original</th>
            <th className="num">Filled</th>
            <th className="num">Remaining</th>
            <th>Status</th>
            <th className="num" />
          </tr>
        </thead>
        <tbody>
          {orders.length === 0 ? (
            <tr>
              <td className="empty" colSpan={8}>No working orders.</td>
            </tr>
          ) : orders.map((order) => {
            const active = order.status === "open" || order.status === "partially_filled";
            const price = order.type === "limit" && order.limit_price
              ? formatTicks(order.limit_price, tickSizeCents)
              : "—";
            return (
              <tr key={order.id} data-order-id={order.id}>
                <td className={order.side === "buy" ? "side bid" : "side ask"}>
                  {order.side === "buy" ? "Buy" : "Sell"}
                </td>
                <td className="type">{order.type === "limit" ? "Limit" : "Market"}</td>
                <td className="num">{price}</td>
                <td className="num">{formatQty(order.original_qty)}</td>
                <td className="num">{formatQty(order.filled_qty)}</td>
                <td className="num">{formatQty(order.remaining_qty)}</td>
                <td className="status">{STATUS_LABEL[order.status] ?? order.status}</td>
                <td className="action">
                  {active ? (
                    <button
                      type="button"
                      className="cancel"
                      disabled={pending === order.id}
                      onClick={() => void onCancel(order.id)}
                    >
                      {pending === order.id ? "Canceling" : "Cancel"}
                    </button>
                  ) : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
