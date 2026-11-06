import { describe, expect, it } from "vitest";
import { isStream, sameOrigin, upstreamPath } from "./proxy";

describe("upstreamPath", () => {
  it("maps versioned API paths", () => {
    expect(upstreamPath(["v1", "fleet", "overview"])).toBe("/api/v1/fleet/overview");
  });

  it("rejects anything outside the versioned API", () => {
    expect(upstreamPath(["metrics"])).toBeNull();
    expect(upstreamPath(["v1"])).toBeNull();
  });

  it("rejects path traversal", () => {
    expect(upstreamPath(["v1", "..", "..", "metrics"])).toBeNull();
    expect(upstreamPath(["v1", "."])).toBeNull();
  });

  it("encodes segments so a decoded slash cannot change the route", () => {
    expect(upstreamPath(["v1", "vehicles", "a/../../metrics"])).toBe("/api/v1/vehicles/a%2F..%2F..%2Fmetrics");
  });
});

describe("isStream", () => {
  it("identifies only the streaming routes", () => {
    expect(isStream("/api/v1/events")).toBe(true);
    expect(isStream("/api/v1/vehicles/EV-1/stream")).toBe(true);
    expect(isStream("/api/v1/alerts")).toBe(false);
  });
});

describe("sameOrigin", () => {
  it("accepts a matching host", () => {
    expect(sameOrigin("https://fleet.example.com", "fleet.example.com")).toBe(true);
    expect(sameOrigin("http://localhost:3000", "localhost:3000")).toBe(true);
  });

  it("rejects missing, foreign or malformed origins", () => {
    expect(sameOrigin(null, "fleet.example.com")).toBe(false);
    expect(sameOrigin("https://evil.example", "fleet.example.com")).toBe(false);
    expect(sameOrigin("not a url", "fleet.example.com")).toBe(false);
  });
});
