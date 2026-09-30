import { useEffect, useState } from "react";
import { FALLBACK_MARKETS, fetchMarkets } from "./api";
import { OrderBook } from "./components/OrderBook";
import { OpenOrders } from "./components/OpenOrders";
import { OrderEntry } from "./components/OrderEntry";
import { PriceChart } from "./components/PriceChart";
import { TradeTape } from "./components/TradeTape";
import { formatTicks } from "./format";
import { project } from "./marketState";
import type { Instrument } from "./types";
import { useMarketStream } from "./useMarketStream";

export function App() {
  const [markets, setMarkets] = useState<Instrument[]>(FALLBACK_MARKETS);
  const [marketError, setMarketError] = useState("");
  const [symbol, setSymbol] = useState("NOVA");
  const { state, status, detail } = useMarketStream(symbol);
  const view = project(state);
  const instrument = markets.find((market) => market.symbol === symbol) ?? markets[0];
  const tick = instrument?.tick_size_cents ?? 1;

  useEffect(() => {
    let cancel = false;
    fetchMarkets()
      .then((list) => {
        if (cancel) return;
        setMarkets(list);
        setMarketError("");
        setSymbol((current) => (list.some((market) => market.symbol === current) ? current : list[0].symbol));
      })
      .catch((err: unknown) => {
        if (!cancel) setMarketError(err instanceof Error ? err.message : "Could not load markets.");
      });
    return () => {
      cancel = true;
    };
  }, []);

  const hold = view.synced
    ? null
    : status === "resyncing"
      ? "Reconnecting. The book and working orders stay blank until the next snapshot."
      : "Waiting for a synchronised snapshot.";

  const statusLabel = status === "live" ? "Live" : status === "resyncing" ? "Resyncing" : "Connecting";

  return (
    <div className="app">
      <div className="shell">
        <header className="nav">
          <div className="brand">Exchange</div>
          <div className="markets" role="group" aria-label="Market">
            {markets.map((market) => (
              <button
                key={market.symbol}
                type="button"
                aria-pressed={market.symbol === symbol}
                onClick={() => setSymbol(market.symbol)}
              >
                {market.symbol}
              </button>
            ))}
          </div>
          <div className={`conn ${status}`} role="status">
            <span className="dot" />
            <span className="conn-label">{statusLabel}</span>
            {view.synced ? <span className="seq">{view.sequence}</span> : null}
          </div>
        </header>
        <div className="summary">
          <div className="sym">
            <strong>{symbol}</strong>
            <span>{instrument?.display_name ?? ""}</span>
          </div>
          <div className="quotes">
            <Quote label="Last" value={view.last} tick={tick} />
            <Quote label="Bid" value={view.bestBid} tick={tick} tone="bid" />
            <Quote label="Ask" value={view.bestAsk} tick={tick} tone="ask" />
            <Quote label="Spread" value={view.spread} tick={tick} />
          </div>
          {!view.synced && detail ? <span className="conn-detail">{detail}</span> : null}
        </div>
        {marketError ? <p className="banner" role="alert">{marketError}</p> : null}
        <main className="workspace">
          <section className="pane chart" aria-label="Price chart">
            <header className="pane-h"><h2>Price</h2><span>Exchange prints</span></header>
            <div className="pane-b">
              <PriceChart trades={view.trades} tickSizeCents={tick} hold={hold} />
            </div>
          </section>
          <section className="pane book" aria-label="Order book">
            <header className="pane-h"><h2>Book</h2><span>Top 10</span></header>
            <div className="pane-b">
              <OrderBook view={view} tickSizeCents={tick} hold={hold} />
            </div>
          </section>
          <section className="pane tape-pane" aria-label="Recent trades">
            <header className="pane-h"><h2>Trades</h2><span>Since snapshot</span></header>
            <div className="pane-b">
              <TradeTape trades={view.trades} tickSizeCents={tick} hold={hold} />
            </div>
          </section>
          <section className="pane entry" aria-label="Order entry">
            <header className="pane-h"><h2>Order</h2><span>{symbol}</span></header>
            <div className="pane-b">
              <OrderEntry symbol={symbol} tickSizeCents={tick} bestBid={view.bestBid} bestAsk={view.bestAsk} />
            </div>
          </section>
          <section className="pane orders-pane" aria-label="Open orders">
            <header className="pane-h"><h2>Open orders</h2><span>You</span></header>
            <div className="pane-b">
              <OpenOrders symbol={symbol} orders={view.orders} tickSizeCents={tick} hold={hold} />
            </div>
          </section>
        </main>
      </div>
    </div>
  );
}

function Quote({
  label,
  value,
  tick,
  tone,
}: {
  label: string;
  value: number | null;
  tick: number;
  tone?: "bid" | "ask";
}) {
  return (
    <div className={`quote ${tone ?? ""}`}>
      <span>{label}</span>
      <strong>{value == null ? "—" : formatTicks(value, tick)}</strong>
    </div>
  );
}
