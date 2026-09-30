import type { OrderBody } from "./orders";
import type { Instrument } from "./types";

export const FALLBACK_MARKETS: Instrument[] = [
  {
    symbol: "NOVA",
    display_name: "Nova Dynamics",
    tick_size_cents: 1,
    initial_reference_price: 10000,
  },
  {
    symbol: "ORBIT",
    display_name: "Orbit Logistics",
    tick_size_cents: 1,
    initial_reference_price: 4250,
  },
  {
    symbol: "PULSE",
    display_name: "Pulse Biotech",
    tick_size_cents: 1,
    initial_reference_price: 18775,
  },
];

export async function fetchMarkets(): Promise<Instrument[]> {
  const res = await fetch("/api/markets");
  if (!res.ok) throw new Error(await readError(res));
  const data: unknown = await res.json();
  if (!Array.isArray(data) || data.length === 0) {
    throw new Error("The exchange returned no markets.");
  }
  return data as Instrument[];
}

export async function submitOrder(symbol: string, body: OrderBody): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`/api/markets/${encodeURIComponent(symbol)}/orders`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    throw new Error("Could not reach the exchange.");
  }
  if (!res.ok) throw new Error(await readError(res));
}

export async function cancelOrder(symbol: string, id: number): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`/api/markets/${encodeURIComponent(symbol)}/orders/${id}`, {
      method: "DELETE",
    });
  } catch {
    throw new Error("Could not reach the exchange.");
  }
  if (!res.ok) throw new Error(await readError(res));
}

export async function readError(res: Response): Promise<string> {
  try {
    const data: unknown = await res.json();
    if (data && typeof data === "object" && "error" in data) {
      const message = (data as { error?: unknown }).error;
      if (typeof message === "string" && message.trim()) return message;
    }
  } catch {
    // A non-JSON body still needs a visible failure.
  }
  return `Request failed (${res.status}).`;
}
