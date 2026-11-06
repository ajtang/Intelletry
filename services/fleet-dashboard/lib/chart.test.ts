import { describe, expect, it } from "vitest";
import { linePath, niceTicks, spreadLabels, timeTicks, trendBuckets } from "./chart";

describe("niceTicks", () => {
  it("spans the data with round steps", () => {
    const { min, max, ticks } = niceTicks(21.3, 47.8, 4);
    expect(min).toBeLessThanOrEqual(21.3);
    expect(max).toBeGreaterThanOrEqual(47.8);
    expect(ticks).toEqual([20, 30, 40, 50]);
  });

  it("gives a flat series a usable range", () => {
    const { min, max } = niceTicks(30, 30);
    expect(max).toBeGreaterThan(min);
  });

  it("uses whole steps for counts", () => {
    expect(niceTicks(0, 1, 3, true).ticks).toEqual([0, 1]);
  });

  it("survives having no data", () => {
    expect(niceTicks(Infinity, -Infinity).ticks).toEqual([0, 1]);
  });
});

describe("timeTicks", () => {
  it("aligns ticks to the clock", () => {
    const from = Date.UTC(2026, 0, 1, 10, 3);
    const ticks = timeTicks(from, from + 60 * 60000, 4);
    expect(ticks.length).toBeGreaterThan(1);
    for (const t of ticks) expect(t % (15 * 60000)).toBe(0);
  });
});

describe("linePath", () => {
  const id = (v: number) => v;

  it("breaks the line across a gap instead of joining it", () => {
    expect(linePath([0, 10, 100], [1, 2, 3], id, id, 50)).toBe("M0.0 1.0L10.0 2.0M100.0 3.0");
  });

  it("skips values that are not numbers", () => {
    expect(linePath([0, 1, 2], [1, NaN, 3], id, id, 50)).toBe("M0.0 1.0M2.0 3.0");
  });
});

describe("spreadLabels", () => {
  it("separates overlapping labels and stays in bounds", () => {
    const out = spreadLabels([50, 51, 52], 13, 0, 100);
    const sorted = [...out].sort((a, b) => a - b);
    expect(sorted[1]! - sorted[0]!).toBeGreaterThanOrEqual(13);
    expect(sorted[2]! - sorted[1]!).toBeGreaterThanOrEqual(13);
    expect(Math.max(...spreadLabels([99, 100, 100], 13, 0, 100))).toBeLessThanOrEqual(100);
  });
});

describe("trendBuckets", () => {
  it("keeps empty buckets and sums levels", () => {
    const now = Date.UTC(2026, 0, 1, 12, 0);
    const buckets = trendBuckets(
      {
        bucket_minutes: 30,
        categories: [],
        points: [
          { bucket: new Date(now - 60 * 60000).toISOString(), level: "warning", count: 2 },
          { bucket: new Date(now - 60 * 60000).toISOString(), level: "critical", count: 1 },
        ],
      },
      2,
      now,
    );
    expect(buckets).toHaveLength(5);
    expect(buckets[2]).toMatchObject({ warning: 2, critical: 1, info: 0 });
    expect(buckets[0]).toMatchObject({ warning: 0, critical: 0, info: 0 });
  });
});
