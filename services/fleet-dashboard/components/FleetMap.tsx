"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { num } from "@/lib/format";
import { batteryBand, fitView, nearest, pan, panelLayout, project, scaleBar, zoomAt, type Rect, type View } from "@/lib/map";
import type { Position } from "@/lib/types";

export type ColorBy = "health" | "battery";

const COLORS = { good: "#0ca30c", warning: "#fab219", critical: "#d03b3b", offline: "#898781" };
const SURFACE = "#0d0d0d";
const TWEEN_MS = 800;

type Shape = "circle" | "triangle" | "diamond" | "ring";

/** Colour plus shape, so state can still be read without relying on colour. */
export function markStyle(p: Position, colorBy: ColorBy): { color: string; shape: Shape; rank: number } {
  if (!p.online) return { color: COLORS.offline, shape: "ring", rank: 0 };
  const band = colorBy === "battery" ? batteryBand(p.soc) : p.health;
  if (band === "critical" || band === "low") return { color: COLORS.critical, shape: "diamond", rank: 3 };
  if (band === "warning" || band === "reduced") return { color: COLORS.warning, shape: "triangle", rank: 2 };
  return { color: COLORS.good, shape: "circle", rank: 1 };
}

function drawMark(ctx: CanvasRenderingContext2D, x: number, y: number, shape: Shape, r: number) {
  ctx.beginPath();
  if (shape === "triangle") {
    ctx.moveTo(x, y - r - 1);
    ctx.lineTo(x + r + 1, y + r);
    ctx.lineTo(x - r - 1, y + r);
    ctx.closePath();
  } else if (shape === "diamond") {
    ctx.moveTo(x, y - r - 2);
    ctx.lineTo(x + r + 2, y);
    ctx.lineTo(x, y + r + 2);
    ctx.lineTo(x - r - 2, y);
    ctx.closePath();
  } else {
    ctx.arc(x, y, r, 0, Math.PI * 2);
  }
}

interface Screen {
  x: number;
  y: number;
  p: Position;
}

function gridStep(scale: number): number {
  const rough = 90 / scale;
  const pow = 10 ** Math.floor(Math.log10(rough));
  return ([1, 2, 5, 10].map((m) => m * pow).find((c) => c >= rough) ?? 10 * pow);
}

/**
 * A panel per region, because operating regions sit thousands of kilometres apart and a
 * single world view would collapse each into one dot. There are no map tiles: the map
 * only needs relative position, and this avoids a third-party tile dependency.
 */
export function FleetMap({
  positions,
  colorBy,
  focus,
  selectedId,
  onSelect,
}: {
  positions: Position[];
  colorBy: ColorBy;
  focus: string;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
}) {
  const wrap = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const [size, setSize] = useState({ w: 0, h: 0 });
  const [hover, setHover] = useState<{ x: number; y: number; p: Position } | null>(null);
  const views = useRef(new Map<string, View>());
  const screen = useRef<Screen[]>([]);
  const previous = useRef(new Map<string, { lat: number; lon: number }>());
  const shown = useRef(new Map<string, { lat: number; lon: number }>());
  const tween = useRef({ start: 0, frame: 0 });
  const redraw = useRef<(t?: number) => void>(() => {});
  const [resetCount, setResetCount] = useState(0);

  const regions = useMemo(() => {
    const names = [...new Set(positions.map((p) => p.region))].sort();
    return focus && names.includes(focus) ? [focus] : names;
  }, [positions, focus]);
  const rects = useMemo(() => panelLayout(regions.length, size.w, size.h), [regions.length, size.w, size.h]);

  useEffect(() => {
    const el = wrap.current;
    if (!el) return;
    const observer = new ResizeObserver(([entry]) => {
      if (entry) setSize({ w: entry.contentRect.width, h: entry.contentRect.height });
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // Views refit when the layout changes, and otherwise stay put so that a vehicle moving
  // near the edge cannot make the whole map jump.
  const layoutKey = `${regions.join(",")}|${Math.round(size.w)}|${Math.round(size.h)}|${resetCount}`;
  useEffect(() => {
    views.current.clear();
  }, [layoutKey]);

  useEffect(() => {
    const draw = (time = performance.now()) => {
      const el = canvas.current;
      if (!el || size.w === 0) return;
      const dpr = window.devicePixelRatio || 1;
      if (el.width !== Math.round(size.w * dpr) || el.height !== Math.round(size.h * dpr)) {
        el.width = Math.round(size.w * dpr);
        el.height = Math.round(size.h * dpr);
      }
      const ctx = el.getContext("2d");
      if (!ctx) return;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, size.w, size.h);

      const reduce = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
      const progress = reduce ? 1 : Math.min((time - tween.current.start) / TWEEN_MS, 1);
      const nextScreen: Screen[] = [];

      regions.forEach((region, i) => {
        const rect = rects[i];
        if (!rect) return;
        const members = positions.filter((p) => p.region === region);
        let view = views.current.get(region);
        if (!view) {
          view = fitView(members, { ...rect, h: rect.h - 24 });
          views.current.set(region, view);
        }
        drawPanel(ctx, region, rect, view, members);

        ctx.save();
        ctx.beginPath();
        ctx.rect(rect.x, rect.y, rect.w, rect.h);
        ctx.clip();
        const marks = members
          .map((p) => {
            const from = previous.current.get(p.id) ?? p;
            const lat = from.lat + (p.lat - from.lat) * progress;
            const lon = from.lon + (p.lon - from.lon) * progress;
            shown.current.set(p.id, { lat, lon });
            return { p, ...project(view!, rect, lat, lon), style: markStyle(p, colorBy) };
          })
          .sort((a, b) => a.style.rank - b.style.rank);
        const detailed = positions.length <= 2000;
        for (const m of marks) {
          nextScreen.push({ x: m.x, y: m.y, p: m.p });
          if (detailed && m.p.online && m.p.speed_kph > 1) {
            const a = (m.p.heading * Math.PI) / 180;
            ctx.strokeStyle = m.style.color;
            ctx.lineWidth = 1.5;
            ctx.beginPath();
            ctx.moveTo(m.x, m.y);
            ctx.lineTo(m.x + Math.sin(a) * 9, m.y - Math.cos(a) * 9);
            ctx.stroke();
          }
          drawMark(ctx, m.x, m.y, m.style.shape, 3.5);
          ctx.lineWidth = 2;
          ctx.strokeStyle = SURFACE;
          ctx.stroke();
          if (m.style.shape === "ring") {
            ctx.lineWidth = 1.5;
            ctx.strokeStyle = m.style.color;
            ctx.stroke();
          } else {
            ctx.fillStyle = m.style.color;
            ctx.fill();
          }
          if (m.p.id === selectedId) {
            ctx.beginPath();
            ctx.arc(m.x, m.y, 9, 0, Math.PI * 2);
            ctx.lineWidth = 2;
            ctx.strokeStyle = "#f0efec";
            ctx.stroke();
          }
        }
        ctx.restore();
      });
      screen.current = nextScreen;
      if (progress < 1) {
        tween.current.frame = requestAnimationFrame((t) => draw(t));
      }
    };
    redraw.current = draw;
    draw();
    const animation = tween.current;
    return () => cancelAnimationFrame(animation.frame);
  }, [positions, regions, rects, size, colorBy, selectedId, layoutKey]);

  // New positions animate from wherever each vehicle is currently drawn.
  useEffect(() => {
    previous.current = new Map(shown.current);
    tween.current.start = performance.now();
    cancelAnimationFrame(tween.current.frame);
    redraw.current();
  }, [positions]);

  const panelAt = (x: number, y: number): { region: string; rect: Rect } | null => {
    const i = rects.findIndex((r) => x >= r.x && x < r.x + r.w && y >= r.y && y < r.y + r.h);
    const region = regions[i];
    const rect = rects[i];
    return region && rect ? { region, rect } : null;
  };

  // React attaches wheel listeners as passive, so preventing page scroll needs a native one.
  const wheelState = useRef({ panelAt });
  useEffect(() => {
    wheelState.current = { panelAt };
  });
  useEffect(() => {
    const el = canvas.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      const box = el.getBoundingClientRect();
      const x = e.clientX - box.left;
      const y = e.clientY - box.top;
      const hit = wheelState.current.panelAt(x, y);
      const view = hit && views.current.get(hit.region);
      if (!hit || !view) return;
      e.preventDefault();
      views.current.set(hit.region, zoomAt(view, hit.rect, x, y, Math.exp(-e.deltaY * 0.0015)));
      redraw.current();
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, []);

  const drag = useRef<{ region: string; x: number; y: number; moved: boolean } | null>(null);

  return (
    <div className="map-wrap" ref={wrap} style={size.w > 0 && size.w < 640 ? { height: regions.length * 260 } : undefined}>
      <canvas
        ref={canvas}
        role="img"
        aria-label="Vehicle positions by region. The table below lists the same vehicles."
        onPointerDown={(e) => {
          const box = e.currentTarget.getBoundingClientRect();
          const hit = panelAt(e.clientX - box.left, e.clientY - box.top);
          if (hit) {
            drag.current = { region: hit.region, x: e.clientX, y: e.clientY, moved: false };
            e.currentTarget.setPointerCapture(e.pointerId);
          }
        }}
        onPointerMove={(e) => {
          const box = e.currentTarget.getBoundingClientRect();
          const d = drag.current;
          if (d) {
            const dx = e.clientX - d.x;
            const dy = e.clientY - d.y;
            if (d.moved || Math.abs(dx) + Math.abs(dy) > 3) {
              d.moved = true;
              d.x = e.clientX;
              d.y = e.clientY;
              const view = views.current.get(d.region);
              if (view) views.current.set(d.region, pan(view, dx, dy));
              setHover(null);
              redraw.current();
            }
            return;
          }
          const hit = nearest(screen.current, e.clientX - box.left, e.clientY - box.top, 12);
          setHover(hit ? { x: hit.x, y: hit.y, p: hit.p } : null);
        }}
        onPointerUp={(e) => {
          const d = drag.current;
          drag.current = null;
          if (d && !d.moved) {
            const box = e.currentTarget.getBoundingClientRect();
            const hit = nearest(screen.current, e.clientX - box.left, e.clientY - box.top, 12);
            onSelect(hit ? hit.p.id : null);
          }
        }}
        onPointerLeave={() => setHover(null)}
        onDoubleClick={() => setResetCount((c) => c + 1)}
      />
      {hover ? (
        <div className="tooltip" style={hover.x < size.w / 2 ? { left: hover.x + 14, top: hover.y - 10 } : { right: size.w - hover.x + 14, top: hover.y - 10 }}>
          <div>
            <b>{hover.p.id}</b>
            <span>{hover.p.online ? hover.p.health : "offline"}</span>
          </div>
          <div>
            <span>Charge</span>
            <b>{num(hover.p.soc)} %</b>
          </div>
          <div>
            <span>Battery</span>
            <b>{num(hover.p.battery_temp_c, 1)} °C</b>
          </div>
          <div>
            <span>Speed</span>
            <b>{num(hover.p.speed_kph)} km/h</b>
          </div>
        </div>
      ) : null}
    </div>
  );
}

function drawPanel(ctx: CanvasRenderingContext2D, region: string, rect: Rect, view: View, members: Position[]) {
  ctx.save();
  ctx.beginPath();
  ctx.rect(rect.x, rect.y, rect.w, rect.h);
  ctx.clip();
  ctx.fillStyle = "#141413";
  ctx.fillRect(rect.x + 1, rect.y + 1, rect.w - 2, rect.h - 2);

  // Graticule at round degree steps gives position and scale without a basemap.
  const step = gridStep(view.scale);
  const halfLat = rect.h / 2 / view.scale;
  const halfLon = rect.w / 2 / (view.k * view.scale);
  ctx.strokeStyle = "#2c2c2a";
  ctx.lineWidth = 1;
  ctx.fillStyle = "#898781";
  ctx.font = "10px system-ui, sans-serif";
  const digits = step < 1 ? Math.min(Math.ceil(-Math.log10(step)), 4) : 0;
  for (let lat = Math.ceil((view.lat - halfLat) / step) * step; lat <= view.lat + halfLat; lat += step) {
    const { y } = project(view, rect, lat, view.lon);
    ctx.beginPath();
    ctx.moveTo(rect.x, y);
    ctx.lineTo(rect.x + rect.w, y);
    ctx.stroke();
    if (y - rect.y > 34) ctx.fillText(`${lat.toFixed(digits)}°`, rect.x + 5, y - 3);
  }
  for (let lon = Math.ceil((view.lon - halfLon) / step) * step; lon <= view.lon + halfLon; lon += step) {
    const { x } = project(view, rect, view.lat, lon);
    ctx.beginPath();
    ctx.moveTo(x, rect.y);
    ctx.lineTo(x, rect.y + rect.h);
    ctx.stroke();
    ctx.fillText(`${lon.toFixed(digits)}°`, x + 3, rect.y + rect.h - 5);
  }

  ctx.fillStyle = "#f0efec";
  ctx.font = "600 12px system-ui, sans-serif";
  ctx.fillText(region, rect.x + 10, rect.y + 18);
  const nameWidth = ctx.measureText(region).width;
  ctx.fillStyle = "#c3c2b7";
  ctx.font = "12px system-ui, sans-serif";
  ctx.fillText(`${members.length} vehicles`, rect.x + 18 + nameWidth, rect.y + 18);

  const bar = scaleBar(view);
  const bx = rect.x + rect.w - bar.px - 12;
  const by = rect.y + rect.h - 22;
  ctx.strokeStyle = "#c3c2b7";
  ctx.lineWidth = 2;
  ctx.beginPath();
  ctx.moveTo(bx, by);
  ctx.lineTo(bx + bar.px, by);
  ctx.stroke();
  ctx.fillStyle = "#c3c2b7";
  ctx.font = "11px system-ui, sans-serif";
  ctx.fillText(`${bar.km} km`, bx, by - 5);

  ctx.strokeStyle = "#383835";
  ctx.lineWidth = 1;
  ctx.strokeRect(rect.x + 0.5, rect.y + 0.5, rect.w - 1, rect.h - 1);
  ctx.restore();
}
