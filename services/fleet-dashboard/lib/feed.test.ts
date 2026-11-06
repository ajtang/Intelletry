import { describe, expect, it } from "vitest";
import { diagnosticEntry, entriesFromAlert, entryFromEvent, matchesFilter, mergeFeed } from "./feed";
import type { Alert, Diagnostic } from "./types";

const alert: Alert = {
  id: 7,
  subject: "EV-00001",
  category: "sensor_drift",
  title: "Sensor drift",
  level: "warning",
  status: "acknowledged",
  reason: "r",
  likely_cause: "c",
  confidence: 0.9,
  recommended_actions: [],
  occurrences: 1,
  created_at: "2026-01-01T10:00:00Z",
  updated_at: "2026-01-01T10:05:00Z",
  acknowledged_at: "2026-01-01T10:05:00Z",
  acknowledged_by: "sam",
  resolved_at: null,
  resolved_by: null,
};

const diagnostic: Diagnostic = {
  id: 3,
  subject: "EV-00002",
  category: "charging_anomaly",
  transition: "opened",
  severity: "critical",
  confidence: 0.8,
  deviation_score: 2,
  reason: "r",
  likely_cause: "c",
  evidence: [],
  recommended_actions: [],
  software_version: "1",
  observed_at: "2026-01-01T11:00:00Z",
};

describe("feed", () => {
  it("does not duplicate an event when history replays after a reconnect", () => {
    const live = entryFromEvent({ type: "alert.acknowledged", at: "2026-01-01T10:05:00Z", alert, actor: "sam" })!;
    const merged = mergeFeed([live], entriesFromAlert(alert));
    expect(merged.filter((e) => e.type === "alert.acknowledged")).toHaveLength(1);
    expect(merged).toHaveLength(2);
  });

  it("orders newest first and caps its length", () => {
    const merged = mergeFeed(entriesFromAlert(alert), [diagnosticEntry(diagnostic)], 2);
    expect(merged).toHaveLength(2);
    expect(merged[0]!.source).toBe("diagnostic");
    expect(merged[1]!.type).toBe("alert.acknowledged");
  });

  it("records who acted on an alert", () => {
    expect(entriesFromAlert(alert).find((e) => e.type === "alert.acknowledged")?.actor).toBe("sam");
  });

  it("filters by source, level and text", () => {
    const e = diagnosticEntry(diagnostic);
    expect(matchesFilter(e, { source: "", level: "", q: "" })).toBe(true);
    expect(matchesFilter(e, { source: "alert", level: "", q: "" })).toBe(false);
    expect(matchesFilter(e, { source: "", level: "warning", q: "" })).toBe(false);
    expect(matchesFilter(e, { source: "", level: "", q: "ev-00002" })).toBe(true);
    expect(matchesFilter(e, { source: "", level: "", q: "CHARGING" })).toBe(true);
    expect(matchesFilter(e, { source: "", level: "", q: "other" })).toBe(false);
  });

  it("ignores events with no payload", () => {
    expect(entryFromEvent({ type: "x", at: "2026-01-01T10:00:00Z" })).toBeNull();
  });
});
