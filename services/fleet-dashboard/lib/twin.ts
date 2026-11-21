// Turns live telemetry and open alerts into the state of each part of the digital twin.
// Kept free of Three.js so that what the model shows can be unit tested. Thresholds match
// internal/diag where a rule exists. The motor has no diagnostic rule, so its limits are
// display-only.
import type { Alert, Level, LiveState } from "./types";

export type ComponentId = "battery" | "motor" | "cooling" | "charge_port" | "antenna" | "sensor" | "electronics";
export type Tone = "ok" | "warning" | "critical" | "inactive";

export const BATTERY_WARNING_C = 50;
export const BATTERY_CRITICAL_C = 58;
export const MOTOR_WARNING_C = 90;
export const MOTOR_CRITICAL_C = 110;

/** Where the model shows each diagnosis. */
const CATEGORY_COMPONENT: Record<string, ComponentId> = {
  thermal_management_issue: "battery",
  battery_cooling_failure: "cooling",
  charging_anomaly: "charge_port",
  excessive_energy_consumption: "motor",
  connectivity_instability: "antenna",
  sensor_drift: "sensor",
  abnormal_power_draw: "electronics",
};

export const COMPONENT_LABELS: Record<ComponentId, string> = {
  battery: "Battery pack",
  motor: "Drive motor",
  cooling: "Cooling loop",
  charge_port: "Charge port",
  antenna: "Connectivity",
  sensor: "Temperature sensor",
  electronics: "Vehicle electronics",
};

export const COMPONENT_ORDER: ComponentId[] = ["battery", "motor", "cooling", "charge_port", "antenna", "sensor", "electronics"];

export interface ComponentState {
  id: ComponentId;
  label: string;
  tone: Tone;
  summary: string;
  readings: { label: string; value: string }[];
  alerts: Alert[];
  /** The most severe open alert, which sets the marker on the model. */
  marker: Level | null;
}

export interface TwinState {
  stale: boolean;
  speedKph: number;
  socPct: number;
  /** 0 at normal temperature up to 1 at the critical limit. */
  batteryHeat: number;
  motorHeat: number;
  charging: { active: boolean; kind: string; powerKw: number };
  components: Record<ComponentId, ComponentState>;
}

function fixed(v: number, digits = 1): string {
  return Number.isFinite(v) ? v.toFixed(digits) : "–";
}

function heat(temp: number, from: number, to: number): number {
  return Math.min(Math.max((temp - from) / (to - from), 0), 1);
}

function worstLevel(alerts: Alert[]): Level | null {
  if (alerts.some((a) => a.level === "critical")) return "critical";
  if (alerts.some((a) => a.level === "warning")) return "warning";
  return alerts.length ? "info" : null;
}

function worstTone(...tones: Tone[]): Tone {
  for (const t of ["critical", "warning", "ok"] as const) if (tones.includes(t)) return t;
  return "inactive";
}

function levelTone(level: Level | null): Tone {
  return level === "critical" ? "critical" : level === "warning" ? "warning" : "ok";
}

function limitTone(temp: number, warning: number, critical: number): Tone {
  return temp >= critical ? "critical" : temp >= warning ? "warning" : "ok";
}

export function twinState(live: LiveState, alerts: Alert[], online: boolean): TwinState {
  const byComponent = new Map<ComponentId, Alert[]>(COMPONENT_ORDER.map((id) => [id, []]));
  for (const a of alerts) {
    const id = CATEGORY_COMPONENT[a.category];
    if (id && a.status !== "resolved") byComponent.get(id)!.push(a);
  }
  const part = (id: ComponentId, tone: Tone, summary: string, readings: ComponentState["readings"]): ComponentState => {
    const own = byComponent.get(id)!;
    return { id, label: COMPONENT_LABELS[id], tone, summary, readings, alerts: own, marker: worstLevel(own) };
  };
  const alertTone = (id: ComponentId) => levelTone(worstLevel(byComponent.get(id)!));

  // A cooling failure is a battery problem too, so it also colours the pack.
  const batteryTone = worstTone(limitTone(live.battery_temp_c, BATTERY_WARNING_C, BATTERY_CRITICAL_C), alertTone("battery"), alertTone("cooling"));
  const motorTone = worstTone(limitTone(live.motor_temp_c, MOTOR_WARNING_C, MOTOR_CRITICAL_C), alertTone("motor"));
  const chargingActive = live.charging_state !== "NONE" && live.charging_state !== "" && live.charging_state !== undefined;
  const connection = live.connectivity ?? "";
  const antennaTone = worstTone(connection === "OFFLINE" || !online ? "critical" : connection === "DEGRADED" ? "warning" : "ok", alertTone("antenna"));

  const components: Record<ComponentId, ComponentState> = {
    battery: part(
      "battery",
      batteryTone,
      batteryTone === "ok" ? "Within normal limits" : batteryTone === "critical" ? "Needs immediate attention" : "Outside normal behaviour",
      [
        { label: "Temperature", value: `${fixed(live.battery_temp_c)} °C` },
        { label: "State of charge", value: `${fixed(live.soc_pct)} %` },
        { label: "Pack power", value: `${fixed(live.pack_power_kw)} kW` },
        { label: "Rise rate", value: `${fixed(live.rise_rate_c_min, 2)} °C/min` },
      ],
    ),
    motor: part("motor", motorTone, live.speed_kph > 1 ? "Driving" : "Stationary", [
      { label: "Temperature", value: `${fixed(live.motor_temp_c)} °C` },
      { label: "Speed", value: `${fixed(live.speed_kph, 0)} km/h` },
      { label: "Consumption", value: `${fixed(live.wh_per_km, 0)} Wh/km` },
    ]),
    cooling: part("cooling", alertTone("cooling"), live.cooling_demand_pct > 0 ? "Cooling active" : "Cooling idle", [
      { label: "Coolant", value: `${fixed(live.coolant_temp_c)} °C` },
      { label: "Ambient", value: `${fixed(live.ambient_temp_c)} °C` },
      { label: "Cooling demand", value: `${fixed(live.cooling_demand_pct, 0)} %` },
    ]),
    charge_port: part(
      "charge_port",
      byComponent.get("charge_port")!.length ? alertTone("charge_port") : chargingActive ? "ok" : "inactive",
      chargingActive ? `${live.charging_state} charging` : "Not charging",
      [
        { label: "Charge power", value: `${fixed(live.charge_power_kw)} kW` },
        { label: "State of charge", value: `${fixed(live.soc_pct)} %` },
      ],
    ),
    antenna: part("antenna", antennaTone, online ? (connection === "DEGRADED" ? "Degraded signal" : "Connected") : "No recent reports", [
      { label: "Signal", value: `${fixed(live.signal_dbm, 0)} dBm` },
      { label: "Status", value: connection || "–" },
    ]),
    sensor: part("sensor", alertTone("sensor"), byComponent.get("sensor")!.length ? "Reading looks unreliable" : "Reading agrees with coolant", [
      { label: "Battery reading", value: `${fixed(live.battery_temp_c)} °C` },
      { label: "Coolant reading", value: `${fixed(live.coolant_temp_c)} °C` },
      { label: "Residual", value: `${fixed(live.battery_residual)} °C` },
    ]),
    electronics: part("electronics", alertTone("electronics"), live.speed_kph <= 1 && live.charging_state === "NONE" ? "Parked" : "Awake", [
      { label: "Software", value: live.software_version || "–" },
      { label: "Pack power", value: `${fixed(live.pack_power_kw)} kW` },
    ]),
  };

  // Without recent data nothing can be called normal. Open alerts still stand.
  if (!online) {
    for (const c of Object.values(components)) if (c.tone === "ok") c.tone = "inactive";
  }

  return {
    stale: !online,
    // A vehicle that has stopped reporting must not look as though it is still moving.
    speedKph: online ? Math.max(live.speed_kph, 0) : 0,
    socPct: Math.min(Math.max(live.soc_pct, 0), 100),
    batteryHeat: heat(live.battery_temp_c, 30, BATTERY_CRITICAL_C),
    motorHeat: heat(live.motor_temp_c, 40, MOTOR_CRITICAL_C),
    charging: { active: online && chargingActive, kind: live.charging_state, powerKw: Math.max(live.charge_power_kw, 0) },
    components,
  };
}

/** How many of the pack's modules are lit, so the model shows charge at a glance. */
export function litModules(socPct: number, total: number): number {
  if (socPct <= 0) return 0;
  return Math.min(Math.ceil((socPct / 100) * total), total);
}

export const WHEEL_SLOWDOWN = 0.25;

/**
 * Wheel rotation in radians per second. It is slowed well below true speed, because at
 * real rates the wheels would strobe and appear to spin backwards on screen.
 */
export function wheelRate(speedKph: number): number {
  return (Math.max(speedKph, 0) / 3.6 / 0.34) * WHEEL_SLOWDOWN;
}
