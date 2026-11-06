"use client";

import { useRef, useState, type PointerEvent, type ReactNode } from "react";
import { linePath, niceTicks, spreadLabels, timeTicks, type TrendBucket } from "@/lib/chart";
import { clock, clockShort, num } from "@/lib/format";
import { useWidth } from "@/lib/hooks";

export interface Series {
  label: string;
  color: string;
  values: number[];
  dash?: boolean;
}

function Tooltip({ x, width, children }: { x: number; width: number; children: ReactNode }) {
  const onRight = x < width / 2;
  return (
    <div className="tooltip" style={onRight ? { left: x + 12, top: 4 } : { right: width - x + 12, top: 4 }}>
      {children}
    </div>
  );
}

/** One y-axis, a crosshair tooltip, and direct labels when lines need telling apart. */
export function TimeChart({
  title,
  unit,
  times,
  series,
  from,
  to,
  gapMs,
  digits = 1,
  height = 150,
  fromZero = false,
  showLatest = true,
}: {
  title: string;
  unit: string;
  times: number[];
  series: Series[];
  from: number;
  to: number;
  gapMs: number;
  digits?: number;
  height?: number;
  /** For quantities that cannot be negative, such as speed. */
  fromZero?: boolean;
  /** Hide the latest value when samples are averages rather than current readings. */
  showLatest?: boolean;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const width = useWidth(ref);
  const [hover, setHover] = useState<number | null>(null);

  const multi = series.length > 1;
  const m = { top: 8, right: multi ? 62 : 10, bottom: 20, left: 40 };
  const plotW = Math.max(width - m.left - m.right, 10);
  const plotH = height - m.top - m.bottom;

  const all = series.flatMap((s) => s.values).filter(Number.isFinite);
  const low = all.length ? Math.min(...all) : 0;
  const high = all.length ? Math.max(...all) : 1;
  const y = fromZero ? niceTicks(0, Math.max(high, 1), 3) : niceTicks(low, high, 3);
  const span = Math.max(to - from, 1);
  const px = (t: number) => m.left + ((t - from) / span) * plotW;
  const py = (v: number) => m.top + plotH - ((v - y.min) / Math.max(y.max - y.min, 1e-9)) * plotH;

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    if (times.length === 0) return;
    const rect = e.currentTarget.getBoundingClientRect();
    const t = from + ((e.clientX - rect.left - m.left) / plotW) * span;
    let best = 0;
    for (let i = 1; i < times.length; i++) {
      if (Math.abs(times[i]! - t) < Math.abs(times[best]! - t)) best = i;
    }
    setHover(best);
  };

  const latest = times.length - 1;
  const hoverIndex = hover !== null && hover <= latest ? hover : null;
  const endYs = multi && latest >= 0 ? spreadLabels(series.map((s) => py(s.values[latest]!)), 13, m.top + 5, m.top + plotH) : [];

  return (
    <div className="chart" ref={ref}>
      <div className="chart-title">
        <strong>{title}</strong>
        {!multi && showLatest && latest >= 0 ? (
          <span className="note">
            {num(series[0]?.values[latest], digits)} {unit}
          </span>
        ) : null}
      </div>
      {width > 0 ? (
        <svg
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          onPointerMove={onMove}
          onPointerLeave={() => setHover(null)}
          role="img"
          aria-label={`${title} in ${unit}`}
        >
          {y.ticks.map((tick) => (
            <g key={tick}>
              <line x1={m.left} x2={m.left + plotW} y1={py(tick)} y2={py(tick)} stroke="var(--grid)" />
              <text x={m.left - 6} y={py(tick) + 4} textAnchor="end" className="axis-text">
                {num(tick, Math.abs(y.ticks[1]! - y.ticks[0]!) < 1 ? 1 : 0)}
              </text>
            </g>
          ))}
          {timeTicks(from, to, Math.max(2, Math.floor(plotW / 90))).map((t) => (
            <text key={t} x={px(t)} y={height - 5} textAnchor="middle" className="axis-text">
              {clockShort(t)}
            </text>
          ))}
          {series.map((s) => (
            <path
              key={s.label}
              d={linePath(times, s.values, px, py, gapMs)}
              fill="none"
              stroke={s.color}
              strokeWidth={2}
              strokeLinejoin="round"
              strokeDasharray={s.dash ? "4 3" : undefined}
            />
          ))}
          {multi && latest >= 0
            ? series.map((s, i) => (
                <text key={s.label} x={m.left + plotW + 6} y={endYs[i]! + 4} className="end-label">
                  {s.label}
                </text>
              ))
            : null}
          {hoverIndex !== null ? (
            <>
              <line x1={px(times[hoverIndex]!)} x2={px(times[hoverIndex]!)} y1={m.top} y2={m.top + plotH} stroke="var(--muted)" />
              {series.map((s) => (
                <circle key={s.label} cx={px(times[hoverIndex]!)} cy={py(s.values[hoverIndex]!)} r={4} fill={s.color} stroke="var(--surface)" strokeWidth={2} />
              ))}
            </>
          ) : null}
          {times.length === 0 ? (
            <text x={m.left + plotW / 2} y={m.top + plotH / 2} textAnchor="middle" className="axis-text">
              No samples in this window
            </text>
          ) : null}
        </svg>
      ) : (
        <div style={{ height }} />
      )}
      {hoverIndex !== null ? (
        <Tooltip x={px(times[hoverIndex]!)} width={width}>
          <div className="tt-time">{clock(times[hoverIndex]!)}</div>
          {series.map((s) => (
            <div key={s.label}>
              <span>{s.label}</span>
              <b>
                {num(s.values[hoverIndex], digits)} {unit}
              </b>
            </div>
          ))}
        </Tooltip>
      ) : null}
      {multi ? (
        <div className="legend">
          {series.map((s) => (
            <span key={s.label}>
              <i style={{ background: s.color }} />
              {s.label} ({unit})
            </span>
          ))}
        </div>
      ) : null}
    </div>
  );
}

const LEVELS = [
  { key: "info", label: "Info", color: "var(--info)" },
  { key: "warning", label: "Warning", color: "var(--warning)" },
  { key: "critical", label: "Critical", color: "var(--critical)" },
] as const;

/** New alerts per time bucket, stacked by level. */
export function TrendBars({ buckets, bucketMinutes, height = 150 }: { buckets: TrendBucket[]; bucketMinutes: number; height?: number }) {
  const ref = useRef<HTMLDivElement>(null);
  const width = useWidth(ref);
  const [hover, setHover] = useState<number | null>(null);
  const m = { top: 8, right: 8, bottom: 20, left: 36 };
  const plotW = Math.max(width - m.left - m.right, 10);
  const plotH = height - m.top - m.bottom;
  const peak = Math.max(1, ...buckets.map((b) => b.info + b.warning + b.critical));
  const y = niceTicks(0, peak, 3, true);
  const py = (v: number) => m.top + plotH - (v / y.max) * plotH;
  const slot = plotW / Math.max(buckets.length, 1);
  const barW = Math.max(Math.min(slot - 2, 14), 1);
  const from = buckets[0]?.t ?? 0;
  const to = (buckets[buckets.length - 1]?.t ?? 1) + bucketMinutes * 60000;
  const hovered = hover !== null ? buckets[hover] : undefined;

  return (
    <div className="chart" ref={ref}>
      {width > 0 ? (
        <svg height={height} viewBox={`0 0 ${width} ${height}`} onPointerLeave={() => setHover(null)} role="img" aria-label="New alerts over time by level">
          {y.ticks.map((tick) => (
            <g key={tick}>
              <line x1={m.left} x2={m.left + plotW} y1={py(tick)} y2={py(tick)} stroke={tick === 0 ? "var(--axis)" : "var(--grid)"} />
              <text x={m.left - 6} y={py(tick) + 4} textAnchor="end" className="axis-text">
                {num(tick)}
              </text>
            </g>
          ))}
          {timeTicks(from, to, Math.max(2, Math.floor(plotW / 90))).map((t) => (
            <text key={t} x={m.left + ((t - from) / (to - from)) * plotW} y={height - 5} textAnchor="middle" className="axis-text">
              {clockShort(t)}
            </text>
          ))}
          {buckets.map((b, i) => {
            const x = m.left + i * slot + (slot - barW) / 2;
            let base = 0;
            return (
              <g key={b.t}>
                {LEVELS.map((l) => {
                  const v = b[l.key];
                  if (!v) return null;
                  const top = py(base + v);
                  const h = Math.max(py(base) - top - (base > 0 ? 2 : 0), 1);
                  base += v;
                  return <rect key={l.key} x={x} y={top} width={barW} height={h} rx={Math.min(barW / 2, 2)} fill={l.color} />;
                })}
                <rect x={m.left + i * slot} y={m.top} width={slot} height={plotH} fill="transparent" onPointerEnter={() => setHover(i)} />
              </g>
            );
          })}
        </svg>
      ) : (
        <div style={{ height }} />
      )}
      {hovered ? (
        <Tooltip x={m.left + hover! * slot + slot / 2} width={width}>
          <div className="tt-time">{clock(hovered.t)}</div>
          {LEVELS.map((l) => (
            <div key={l.key}>
              <span>{l.label}</span>
              <b>{hovered[l.key]}</b>
            </div>
          ))}
        </Tooltip>
      ) : null}
      <div className="legend">
        {LEVELS.map((l) => (
          <span key={l.key}>
            <i className="block" style={{ background: l.color }} />
            {l.label}
          </span>
        ))}
      </div>
    </div>
  );
}

/** A part-to-whole bar whose legend carries the exact values. */
export function SegBar({ parts }: { parts: { label: string; value: number; color: string; glyph?: string }[] }) {
  const total = parts.reduce((sum, p) => sum + p.value, 0);
  return (
    <div>
      <div className="segbar" role="img" aria-label={parts.map((p) => `${p.label} ${p.value}`).join(", ")}>
        {parts
          .filter((p) => p.value > 0)
          .map((p) => (
            <span key={p.label} style={{ flexGrow: p.value, flexBasis: 0, background: p.color }} title={`${p.label}: ${p.value}`} />
          ))}
        {total === 0 ? <span style={{ flexGrow: 1, background: "var(--grid)" }} /> : null}
      </div>
      <div className="legend">
        {parts.map((p) => (
          <span key={p.label}>
            <i className="block" style={{ background: p.color }} />
            {p.label} {num(p.value)}
          </span>
        ))}
      </div>
    </div>
  );
}
