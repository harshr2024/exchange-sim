import { formatTicks } from "../format";
import type { Trade } from "../types";

const CHART_POINTS = 120;

export function PriceChart({
  trades,
  tickSizeCents,
  hold,
}: {
  trades: Trade[];
  tickSizeCents: number;
  hold: string | null;
}) {
  if (hold) return <p className="hold">{hold}</p>;
  const points = trades.slice(-CHART_POINTS);
  if (points.length === 0) {
    return <p className="empty">The chart plots prints from this exchange. None yet.</p>;
  }

  const width = 640;
  const height = 200;
  const padX = 8;
  const padY = 12;
  const prices = points.map((trade) => trade.price);
  const min = Math.min(...prices);
  const max = Math.max(...prices);
  const pad = Math.max(1, Math.round((max - min) * 0.08));
  const lo = min - pad;
  const hi = max + pad;
  const span = Math.max(1, hi - lo);
  const innerW = width - padX * 2;
  const innerH = height - padY * 2;
  const coords = points.map((trade, index) => {
    const x = points.length === 1 ? width / 2 : padX + (index / (points.length - 1)) * innerW;
    const y = padY + ((hi - trade.price) / span) * innerH;
    return { x, y };
  });

  let path = `M ${coords[0].x.toFixed(2)} ${coords[0].y.toFixed(2)}`;
  if (coords.length === 1) {
    path = `M ${padX} ${coords[0].y.toFixed(2)} H ${width - padX}`;
  } else {
    for (let i = 1; i < coords.length; i++) {
      path += ` H ${coords[i].x.toFixed(2)} V ${coords[i].y.toFixed(2)}`;
    }
  }
  const grid = [0, 0.5, 1].map((t) => (padY + t * innerH).toFixed(2));

  return (
    <div className="chart-wrap">
      <svg viewBox={`0 0 ${width} ${height}`} preserveAspectRatio="none" role="img" aria-label="Trade price chart">
        {grid.map((y) => (
          <line key={y} x1={padX} x2={width - padX} y1={y} y2={y} className="chart-grid" />
        ))}
        <path d={path} className="chart-line" />
      </svg>
      <div className="chart-meta">
        <span>{points.length} print{points.length === 1 ? "" : "s"}</span>
        <span>{formatTicks(min, tickSizeCents)} – {formatTicks(max, tickSizeCents)}</span>
      </div>
    </div>
  );
}
