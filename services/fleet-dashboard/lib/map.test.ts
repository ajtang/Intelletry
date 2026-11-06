import { describe, expect, it } from "vitest";
import { batteryBand, fitView, nearest, pan, panelLayout, project, scaleBar, unproject, zoomAt } from "./map";

const rect = { x: 100, y: 50, w: 400, h: 300 };
const points = [
  { lat: 48.0, lon: 11.0 },
  { lat: 48.4, lon: 12.0 },
];

describe("map geometry", () => {
  it("fits every point inside the panel", () => {
    const view = fitView(points, rect);
    for (const p of points) {
      const { x, y } = project(view, rect, p.lat, p.lon);
      expect(x).toBeGreaterThanOrEqual(rect.x);
      expect(x).toBeLessThanOrEqual(rect.x + rect.w);
      expect(y).toBeGreaterThanOrEqual(rect.y);
      expect(y).toBeLessThanOrEqual(rect.y + rect.h);
    }
  });

  it("round-trips through project and unproject", () => {
    const view = fitView(points, rect);
    const { x, y } = project(view, rect, 48.2, 11.3);
    const back = unproject(view, rect, x, y);
    expect(back.lat).toBeCloseTo(48.2, 8);
    expect(back.lon).toBeCloseTo(11.3, 8);
  });

  it("keeps the point under the cursor fixed while zooming", () => {
    const view = fitView(points, rect);
    const before = unproject(view, rect, 220, 140);
    const after = unproject(zoomAt(view, rect, 220, 140, 2), rect, 220, 140);
    expect(after.lat).toBeCloseTo(before.lat, 8);
    expect(after.lon).toBeCloseTo(before.lon, 8);
  });

  it("moves the map with the drag", () => {
    const view = fitView(points, rect);
    const start = project(view, rect, 48.2, 11.5);
    const moved = project(pan(view, 30, -20), rect, 48.2, 11.5);
    expect(moved.x - start.x).toBeCloseTo(30, 6);
    expect(moved.y - start.y).toBeCloseTo(-20, 6);
  });

  it("lays out panels without overlap, and in one column when narrow", () => {
    const panels = panelLayout(4, 800, 600);
    expect(panels).toHaveLength(4);
    expect(panels[3]).toEqual({ x: 400, y: 300, w: 400, h: 300 });
    expect(new Set(panelLayout(4, 400, 800).map((p) => p.x)).size).toBe(1);
    expect(panelLayout(0, 800, 600)).toEqual([]);
  });

  it("uses a round scale bar that fits", () => {
    const bar = scaleBar({ lat: 48, lon: 11, scale: 400, k: 0.67 });
    expect([1, 2, 5, 10, 20, 50]).toContain(bar.km);
    expect(bar.px).toBeLessThanOrEqual(110);
  });

  it("finds the nearest point in range only", () => {
    const pts = [
      { x: 0, y: 0, id: "a" },
      { x: 10, y: 0, id: "b" },
    ];
    expect(nearest(pts, 8, 0, 5)?.id).toBe("b");
    expect(nearest(pts, 50, 50, 5)).toBeNull();
  });

  it("bands battery state", () => {
    expect([batteryBand(10), batteryBand(20), batteryBand(80)]).toEqual(["low", "reduced", "ok"]);
  });
});
