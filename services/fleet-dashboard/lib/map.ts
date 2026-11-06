// Geometry for the fleet map. Positions use an equirectangular projection scaled by the
// cosine of the view's latitude, which is accurate at the regional distances shown.

export interface Rect {
  x: number;
  y: number;
  w: number;
  h: number;
}

/**
 * A view is the point at the panel's centre and the pixels per degree of latitude. The
 * longitude factor k is fixed when the view is fitted. Deriving it from the current
 * centre would change the projection during a pan and make the map drift under the cursor.
 */
export interface View {
  lat: number;
  lon: number;
  scale: number;
  k: number;
}

interface LatLon {
  lat: number;
  lon: number;
}

export function panelLayout(n: number, width: number, height: number): Rect[] {
  if (n <= 0) return [];
  let cols = Math.ceil(Math.sqrt(n));
  if (width < 640) cols = 1;
  const rows = Math.ceil(n / cols);
  const w = width / cols;
  const h = height / rows;
  return Array.from({ length: n }, (_, i) => ({ x: (i % cols) * w, y: Math.floor(i / cols) * h, w, h }));
}

export function fitView(points: LatLon[], rect: Rect, pad = 36): View {
  if (points.length === 0) return { lat: 0, lon: 0, scale: 1, k: 1 };
  let minLat = Infinity;
  let maxLat = -Infinity;
  let minLon = Infinity;
  let maxLon = -Infinity;
  for (const p of points) {
    minLat = Math.min(minLat, p.lat);
    maxLat = Math.max(maxLat, p.lat);
    minLon = Math.min(minLon, p.lon);
    maxLon = Math.max(maxLon, p.lon);
  }
  const lat = (minLat + maxLat) / 2;
  const lon = (minLon + maxLon) / 2;
  const k = Math.cos((lat * Math.PI) / 180);
  const spanY = Math.max(maxLat - minLat, 0.02);
  const spanX = Math.max((maxLon - minLon) * k, 0.02);
  const scale = Math.min(Math.max(rect.w - pad * 2, 40) / spanX, Math.max(rect.h - pad * 2, 40) / spanY);
  return { lat, lon, scale, k };
}

export function project(view: View, rect: Rect, lat: number, lon: number): { x: number; y: number } {
  return {
    x: rect.x + rect.w / 2 + (lon - view.lon) * view.k * view.scale,
    y: rect.y + rect.h / 2 - (lat - view.lat) * view.scale,
  };
}

export function unproject(view: View, rect: Rect, x: number, y: number): LatLon {
  return {
    lat: view.lat - (y - rect.y - rect.h / 2) / view.scale,
    lon: view.lon + (x - rect.x - rect.w / 2) / (view.k * view.scale),
  };
}

/** Zooms while keeping the point under the cursor fixed. */
export function zoomAt(view: View, rect: Rect, x: number, y: number, factor: number): View {
  const anchor = unproject(view, rect, x, y);
  const scale = Math.min(Math.max(view.scale * factor, 20), 200000);
  return {
    ...view,
    scale,
    lat: anchor.lat + (y - rect.y - rect.h / 2) / scale,
    lon: anchor.lon - (x - rect.x - rect.w / 2) / (view.k * scale),
  };
}

export function pan(view: View, dx: number, dy: number): View {
  return { ...view, lat: view.lat + dy / view.scale, lon: view.lon - dx / (view.k * view.scale) };
}

/** A round scale-bar length: kilometres and its width in pixels. */
export function scaleBar(view: View, maxPx = 110): { km: number; px: number } {
  const pxPerKm = view.scale / 111.32;
  const rough = maxPx / pxPerKm;
  const pow = 10 ** Math.floor(Math.log10(rough));
  const km = [5, 2, 1].map((m) => m * pow).find((c) => c <= rough) ?? pow;
  return { km, px: km * pxPerKm };
}

export function nearest<T extends { x: number; y: number }>(points: T[], x: number, y: number, radius: number): T | null {
  let best: T | null = null;
  let bestD = radius * radius;
  for (const p of points) {
    const d = (p.x - x) ** 2 + (p.y - y) ** 2;
    if (d <= bestD) {
      best = p;
      bestD = d;
    }
  }
  return best;
}

export type BatteryBand = "low" | "reduced" | "ok";

export function batteryBand(soc: number): BatteryBand {
  return soc < 15 ? "low" : soc < 30 ? "reduced" : "ok";
}
