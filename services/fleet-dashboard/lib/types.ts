export type Health = "healthy" | "warning" | "critical";
export type Level = "info" | "warning" | "critical";
export type AlertStatus = "active" | "acknowledged" | "resolved";

export interface Overview {
  total_vehicles: number;
  online: number;
  offline: number;
  healthy: number;
  warning: number;
  critical: number;
  driving: number;
  charging: number;
  parked: number;
  avg_soc_pct: number;
  avg_battery_temp_c: number;
  max_battery_temp_c: number;
  active_alerts: number;
  alerts_critical: number;
  alerts_warning: number;
  alerts_info: number;
  alerts_unacknowledged: number;
  events_per_sec: number;
  queue_depth: number;
  software_versions: { version: string; vehicles: number }[] | null;
}

export interface Position {
  id: string;
  lat: number;
  lon: number;
  heading: number;
  soc: number;
  battery_temp_c: number;
  speed_kph: number;
  mode: string;
  health: Health;
  online: boolean;
  region: string;
}

export interface LiveState {
  vehicle_id: string;
  timestamp_ms: number;
  model: string;
  region: string;
  software_version: string;
  soc_pct: number;
  battery_temp_c: number;
  motor_temp_c: number;
  coolant_temp_c: number;
  ambient_temp_c: number;
  cooling_demand_pct: number;
  speed_kph: number;
  latitude: number;
  longitude: number;
  heading_deg: number;
  odometer_km: number;
  charging_state: string;
  charge_power_kw: number;
  pack_power_kw: number;
  connectivity: string;
  signal_dbm: number;
  mode: string;
  health: Health;
  deviation_score: number;
  battery_residual: number;
  wh_per_km: number;
  rise_rate_c_min: number;
  online?: boolean;
}

export interface Alert {
  id: number;
  subject: string;
  category: string;
  title: string;
  level: Level;
  status: AlertStatus;
  reason: string;
  likely_cause: string;
  confidence: number;
  recommended_actions: string[] | null;
  occurrences: number;
  created_at: string;
  updated_at: string;
  acknowledged_at: string | null;
  acknowledged_by: string | null;
  resolved_at: string | null;
  resolved_by: string | null;
}

export interface Evidence {
  signal: string;
  detail: string;
  weight: number;
}

export interface Diagnostic {
  id: number;
  subject: string;
  category: string;
  transition: string;
  severity: string;
  confidence: number;
  deviation_score: number;
  reason: string;
  likely_cause: string;
  evidence: Evidence[] | null;
  recommended_actions: string[] | null;
  software_version: string;
  observed_at: string;
}

export interface VehicleDetail {
  vehicle: {
    vehicle_id: string;
    vin: string;
    model: string;
    region: string;
    pack_capacity_kwh: number;
    software_version: string;
    first_seen_at: string;
  };
  live: LiveState | null;
  active_alerts: Alert[];
  diagnostics: Diagnostic[];
  recommended_actions: string[];
}

export interface TimelineSample {
  t: number;
  b: number;
  m: number;
  a: number;
  s: number;
  v: number;
  p: number;
}

export interface Trends {
  bucket_minutes: number;
  points: { bucket: string; level: Level; count: number }[] | null;
  categories: { category: string; count: number; open: number; mean_minutes_to_resolve: number | null }[] | null;
}

export interface OpsEvent {
  type: string;
  at: string;
  alert?: Alert;
  diagnostic?: Diagnostic;
  actor?: string;
}

export interface AlertEvent {
  event_type: string;
  actor: string;
  note: string;
  created_at: string;
}
