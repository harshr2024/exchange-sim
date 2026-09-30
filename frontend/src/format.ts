/**
 * Prices travel as integer ticks. One tick is `tickSizeCents` cents.
 * Parsing stays in integer cents so a price never becomes a binary float.
 */
export function dollarsToTicks(raw: string, tickSizeCents: number): number | null {
  if (!Number.isInteger(tickSizeCents) || tickSizeCents <= 0) return null;
  const match = /^(\d+)(?:\.(\d{1,2}))?$/.exec(raw.trim());
  if (!match) return null;
  const whole = match[1];
  if (whole.length > 1 && whole.startsWith("0")) return null;
  const dollars = Number(whole);
  const centsPart = (match[2] ?? "").padEnd(2, "0");
  const cents = dollars * 100 + Number(centsPart);
  if (!Number.isSafeInteger(cents) || cents <= 0) return null;
  if (cents % tickSizeCents !== 0) return null;
  return cents / tickSizeCents;
}

export function formatTicks(ticks: number, tickSizeCents: number): string {
  if (!Number.isFinite(ticks) || !Number.isInteger(tickSizeCents) || tickSizeCents <= 0) {
    return "—";
  }
  const negative = ticks < 0;
  const cents = Math.abs(Math.trunc(ticks)) * tickSizeCents;
  if (!Number.isSafeInteger(cents)) return "—";
  const dollars = Math.floor(cents / 100);
  const rem = cents % 100;
  return `${negative ? "-" : ""}${dollars}.${String(rem).padStart(2, "0")}`;
}

export function formatQty(qty: number): string {
  if (!Number.isFinite(qty)) return "—";
  return String(Math.trunc(qty));
}
