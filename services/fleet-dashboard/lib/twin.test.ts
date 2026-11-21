import { describe, expect, it } from "vitest";
import { litModules, twinState, wheelRate } from "./twin";
import type { Alert, Level, LiveState } from "./types";

const live: LiveState = {
  vehicle_id: "EV-1",
  timestamp_ms: 0,
  model: "M",
  region: "r",
  software_version: "1",
  soc_pct: 50,
  battery_temp_c: 30,
  motor_temp_c: 45,
  coolant_temp_c: 25,
  ambient_temp_c: 20,
  cooling_demand_pct: 10,
  speed_kph: 80,
  latitude: 0,
  longitude: 0,
  heading_deg: 0,
  odometer_km: 0,
  charging_state: "NONE",
  charge_power_kw: 0,
  pack_power_kw: 20,
  connectivity: "ONLINE",
  signal_dbm: -70,
  mode: "city",
  health: "healthy",
  deviation_score: 0,
  battery_residual: 0,
  wh_per_km: 160,
  rise_rate_c_min: 0,
};

function alert(category: string, level: Level = "warning", status: Alert["status"] = "active"): Alert {
  return {
    id: 1,
    subject: "EV-1",
    category,
    title: category,
    level,
    status,
    reason: "r",
    likely_cause: "c",
    confidence: 0.9,
    recommended_actions: [],
    occurrences: 1,
    created_at: "",
    updated_at: "",
    acknowledged_at: null,
    acknowledged_by: null,
    resolved_at: null,
    resolved_by: null,
  };
}

describe("battery colour", () => {
  it("is green within limits", () => {
    expect(twinState(live, [], true).components.battery.tone).toBe("ok");
  });

  it("uses the same limits as the diagnostic rules", () => {
    expect(twinState({ ...live, battery_temp_c: 49.9 }, [], true).components.battery.tone).toBe("ok");
    expect(twinState({ ...live, battery_temp_c: 50 }, [], true).components.battery.tone).toBe("warning");
    expect(twinState({ ...live, battery_temp_c: 58 }, [], true).components.battery.tone).toBe("critical");
  });

  it("follows an open alert even at a normal temperature", () => {
    expect(twinState(live, [alert("thermal_management_issue", "critical")], true).components.battery.tone).toBe("critical");
    expect(twinState(live, [alert("battery_cooling_failure")], true).components.battery.tone).toBe("warning");
  });

  it("does not let an alert lower a temperature limit breach", () => {
    expect(twinState({ ...live, battery_temp_c: 60 }, [alert("thermal_management_issue")], true).components.battery.tone).toBe("critical");
  });
});

describe("diagnostics", () => {
  it("places every diagnosis on the part it concerns", () => {
    const cases = {
      battery_cooling_failure: "cooling",
      thermal_management_issue: "battery",
      charging_anomaly: "charge_port",
      excessive_energy_consumption: "motor",
      connectivity_instability: "antenna",
      sensor_drift: "sensor",
      abnormal_power_draw: "electronics",
    } as const;
    for (const [category, id] of Object.entries(cases)) {
      const c = twinState(live, [alert(category)], true).components[id];
      expect(c.marker).toBe("warning");
      expect(c.alerts).toHaveLength(1);
    }
  });

  it("keeps markers for acknowledged alerts and drops resolved ones", () => {
    expect(twinState(live, [alert("sensor_drift", "warning", "acknowledged")], true).components.sensor.marker).toBe("warning");
    expect(twinState(live, [alert("sensor_drift", "warning", "resolved")], true).components.sensor.marker).toBeNull();
  });

  it("shows the most severe of several alerts", () => {
    const state = twinState(live, [alert("sensor_drift"), alert("sensor_drift", "critical")], true);
    expect(state.components.sensor.marker).toBe("critical");
  });

  it("ignores categories with no part on the vehicle", () => {
    const state = twinState(live, [alert("software_version_regression")], true);
    expect(Object.values(state.components).every((c) => c.marker === null)).toBe(true);
  });
});

describe("live behaviour", () => {
  it("shows charging only while it is happening", () => {
    const charging = twinState({ ...live, charging_state: "DC", charge_power_kw: 150 }, [], true);
    expect(charging.charging).toEqual({ active: true, kind: "DC", powerKw: 150 });
    expect(charging.components.charge_port.tone).toBe("ok");
    expect(twinState(live, [], true).components.charge_port.tone).toBe("inactive");
  });

  it("stops the model when the vehicle has stopped reporting", () => {
    const state = twinState({ ...live, charging_state: "AC" }, [], false);
    expect(state.stale).toBe(true);
    expect(state.speedKph).toBe(0);
    expect(state.charging.active).toBe(false);
    expect(state.components.antenna.tone).toBe("critical");
  });

  it("does not call anything normal without recent data, but keeps open alerts", () => {
    const state = twinState(live, [alert("sensor_drift")], false);
    expect(state.components.battery.tone).toBe("inactive");
    expect(state.components.sensor.tone).toBe("warning");
    expect(state.components.sensor.marker).toBe("warning");
  });

  it("marks a degraded connection", () => {
    expect(twinState({ ...live, connectivity: "DEGRADED" }, [], true).components.antenna.tone).toBe("warning");
  });

  it("limits heat to the 0 to 1 range", () => {
    expect(twinState({ ...live, battery_temp_c: 10 }, [], true).batteryHeat).toBe(0);
    expect(twinState({ ...live, battery_temp_c: 90 }, [], true).batteryHeat).toBe(1);
  });

  it("lights modules in proportion to charge", () => {
    expect(litModules(0, 24)).toBe(0);
    expect(litModules(1, 24)).toBe(1);
    expect(litModules(50, 24)).toBe(12);
    expect(litModules(100, 24)).toBe(24);
    expect(litModules(120, 24)).toBe(24);
  });

  it("stops the wheels at rest and speeds them up with the vehicle", () => {
    expect(wheelRate(0)).toBe(0);
    expect(wheelRate(-5)).toBe(0);
    expect(wheelRate(100)).toBeGreaterThan(wheelRate(50));
  });
});
