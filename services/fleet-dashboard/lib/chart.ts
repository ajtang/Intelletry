import type { Level, Trends } from "./types";

/** Round axis ticks spanning the data. A flat series still gets a usable range. */
export function niceTicks(min: number, max: number, count = 4, integer = false): { min: number; max: number; ticks: number[] } {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return { min: 0, max: 1, ticks: [0, 1] };
  if (min === max) {
    const pad = Math.abs(min) > 1 ? Math.abs(min) * 0.05 : 1;
    min -= pad;
    max += pad;
  }
  const rough = (max - min) / Math.max(1, count);
  const pow = 10 ** Math.floor(Math.log10(rough));
  const f = rough / pow;
  let step = (f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10) * pow;
  if (integer) step = Math.max(step, 1);
  const lo = Math.floor(min / step) * step;
  const hi = Math.ceil(max / step) * step;
  const ticks: number[] = [];
  for (let i = 0; lo + i * step <= hi + step / 1e6; i++) ticks.push(Number((lo + i * step).toFixed(10)));
  return { min: lo, max: hi, ticks };
}

const TIME_STEPS_MIN = [1, 2, 5, 10, 15, 30, 60, 120, 180, 360, 720, 1440];

/** Clock-aligned time ticks. */
export function timeTicks(from: number, to: number, count = 5): number[] {
  const wanted = (to - from) / Math.max(1, count) / 60000;
  const stepMin = TIME_STEPS_MIN.find((s) => s >= wanted) ?? 1440;
  const step = stepMin * 60000;
  const ticks: number[] = [];
  for (let t = Math.ceil(from / step) * step; t <= to; t += step) ticks.push(t);
  return ticks;
}

/** SVG path for a line. A gap longer than gapMs breaks the line instead of inventing data. */
export function linePath(ts: number[], ys: number[], x: (t: number) => number, y: (v: number) => number, gapMs: number): string {
  let d = "";
  for (let i = 0; i < ts.length; i++) {
    const t = ts[i]!;
    const v = ys[i]!;
    if (!Number.isFinite(v)) continue;
    const start = i === 0 || !Number.isFinite(ys[i - 1]!) || t - ts[i - 1]! > gapMs;
    d += `${start ? "M" : "L"}${x(t).toFixed(1)} ${y(v).toFixed(1)}`;
  }
  return d;
}

/** Spreads end-of-line labels vertically so they do not overlap, staying inside [lo, hi]. */
export function spreadLabels(ys: number[], gap: number, lo: number, hi: number): number[] {
  const order = ys.map((y, i) => ({ y, i })).sort((a, b) => a.y - b.y);
  const out = [...ys];
  let prev = lo - gap;
  for (const item of order) {
    prev = Math.max(item.y, prev + gap);
    out[item.i] = prev;
  }
  let next = hi + gap;
  for (const item of order.reverse()) {
    next = Math.min(out[item.i]!, next - gap);
    out[item.i] = next;
  }
  return out;
}

export type TrendBucket = { t: number } & Record<Level, number>;

/** Fills every bucket in the range, so empty periods show as empty rather than vanishing. */
export function trendBuckets(trends: Trends, hours: number, now: number): TrendBucket[] {
  const step = trends.bucket_minutes * 60000;
  const end = Math.floor(now / step) * step;
  const start = Math.floor((now - hours * 3600000) / step) * step;
  const byT = new Map<number, TrendBucket>();
  for (let t = start; t <= end; t += step) byT.set(t, { t, info: 0, warning: 0, critical: 0 });
  for (const p of trends.points ?? []) {
    const b = byT.get(new Date(p.bucket).getTime());
    if (b) b[p.level] += p.count;
  }
  return [...byT.values()];
}
