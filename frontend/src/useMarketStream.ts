import { useEffect, useState } from "react";
import { discardStreamingState, emptyState, parseStreamMessage, reduceMarket } from "./marketState";
import type { MarketState } from "./marketState";
import type { ConnectionStatus } from "./types";

export function marketStreamURL(symbol: string, loc: { protocol: string; host: string } = window.location): string {
  const proto = loc.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${loc.host}/ws/markets/${encodeURIComponent(symbol)}`;
}

/**
 * Subscribes to one market. The snapshot that opens the stream replaces local
 * state. A close or a resync frame throws that state away and connects again;
 * nothing is replayed from the client.
 */
export function useMarketStream(symbol: string): {
  state: MarketState;
  status: ConnectionStatus;
  detail: string;
} {
  const [state, setState] = useState(() => emptyState(symbol));
  const [status, setStatus] = useState<ConnectionStatus>("connecting");
  const [detail, setDetail] = useState("");
  const [symbolSeen, setSymbolSeen] = useState(symbol);
  if (symbol !== symbolSeen) {
    setSymbolSeen(symbol);
    setState(emptyState(symbol));
    setStatus("connecting");
    setDetail("");
  }

  useEffect(() => {
    let stopped = false;
    let socket: WebSocket | null = null;
    let timer = 0;
    let attempt = 0;
    let retrying = false;

    const connect = () => {
      if (stopped) return;
      setStatus(retrying ? "resyncing" : "connecting");
      const ws = new WebSocket(marketStreamURL(symbol));
      socket = ws;

      ws.onmessage = (event) => {
        if (stopped || socket !== ws) return;
        const message = parseStreamMessage(String(event.data));
        if (!message) return;
        if (message.type === "error" && message.resync) {
          retrying = true;
          setStatus("resyncing");
          setDetail(message.reason || "The exchange asked for a resync.");
        } else if (message.type === "snapshot") {
          attempt = 0;
          setStatus("live");
          setDetail("");
        }
        setState((prev) => reduceMarket(prev, message));
      };

      ws.onclose = () => {
        if (stopped || socket !== ws) return;
        retrying = true;
        setStatus("resyncing");
        setDetail((current) => current || "Reconnecting");
        setState((prev) => discardStreamingState(prev));
        const delay = Math.min(2000, 250 * 2 ** attempt);
        attempt += 1;
        timer = window.setTimeout(connect, delay);
      };
    };

    setState(emptyState(symbol));
    setDetail("");
    connect();

    return () => {
      stopped = true;
      window.clearTimeout(timer);
      socket?.close();
    };
  }, [symbol]);

  return { state, status, detail };
}
