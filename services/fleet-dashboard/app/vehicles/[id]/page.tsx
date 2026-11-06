"use client";

import Link from "next/link";
import { use, useCallback, useMemo, useState } from "react";
import { AlertCard } from "@/components/AlertCard";
import { TimeChart } from "@/components/charts";
import { ErrorBanner, LevelBadge, Panel, Segmented, State, StreamBadge, Tile } from "@/components/ui";
import { ago, dateTime, humanize, modeLabel, num } from "@/lib/format";
import { useEventStream, useNow, usePoll } from "@/lib/hooks";
import type { Diagnostic, Level, LiveState, TimelineSample, VehicleDetail } from "@/lib/types";

const RANGES = [
  { value: "10", label: "10 min" },
  { value: "60", label: "1 hour" },
  { value: "360", label: "6 hours" },
  { value: "1440", label: "24 hours" },
] as const;
type Range = (typeof RANGES)[number]["value"];

function asSample(s: LiveState): TimelineSample {
  return {
    t: s.timestamp_ms,
    b: s.battery_temp_c,
    m: s.motor_temp_c,
    a: s.ambient_temp_c,
    s: s.soc_pct,
    v: s.speed_kph,
    p: s.pack_power_kw,
  };
}

function DiagnosticRow({ d }: { d: Diagnostic }) {
  const level: Level = d.severity === "critical" || d.severity === "warning" ? d.severity : "info";
  return (
    <details className="alert">
      <summary>
        <span className="alert-head" style={{ display: "inline-flex" }}>
          <LevelBadge level={level} />
          <span className="alert-title">{humanize(d.category)}</span>
          <span className="badge">{humanize(d.transition)}</span>
          <span className="note">{dateTime(d.observed_at)}</span>
        </span>
      </summary>
      <p className="alert-body">
        <span className="field-label">Why: </span>
        {d.reason}
      </p>
      <p className="alert-body">
        <span className="field-label">Likely cause: </span>
        {d.likely_cause}
      </p>
      <p className="alert-meta">
        Confidence {num(d.confidence * 100)}% · Deviation {num(d.deviation_score)} · Software {d.software_version || "–"}
      </p>
      {d.evidence?.length ? (
        <div className="evidence">
          <span className="field-label">Evidence (weight in favour of this diagnosis)</span>
          {d.evidence.map((e) => (
            <div className="evidence-row" key={e.signal + e.detail}>
              <b>
                {e.weight > 0 ? "+" : ""}
                {num(e.weight, 1)}
              </b>
              <span>{e.detail}</span>
            </div>
          ))}
        </div>
      ) : null}
    </details>
  );
}

export default function VehiclePage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = use(params);
  const encoded = encodeURIComponent(id);
  const detail = usePoll<VehicleDetail>(`/vehicles/${encoded}`, 5000);
  const [range, setRange] = useState<Range>("10");
  const minutes = Number(range);
  const history = usePoll<{ source: string; points: TimelineSample[] }>(`/vehicles/${encoded}/timeline?minutes=${minutes}`, minutes <= 15 ? 30000 : 60000);

  // The live stream is kept apart from polled data, so a poll in flight cannot overwrite
  // a newer telemetry sample with an older one.
  const [stream, setStream] = useState<{ id: string; live: LiveState | null; samples: TimelineSample[] }>({ id, live: null, samples: [] });
  const onMessage = useCallback(
    (msg: LiveState) => {
      setStream((s) => {
        const prev = s.id === id ? s : { id, live: null, samples: [] };
        return { id, live: msg, samples: [...prev.samples, asSample(msg)].slice(-2000) };
      });
    },
    [id],
  );
  const status = useEventStream<LiveState>(`/vehicles/${encoded}/stream`, onMessage);
  const now = useNow();

  const d = detail.data;
  const streamed = stream.id === id ? stream : null;
  const polled = d?.live ?? null;
  const live = streamed?.live && (!polled || streamed.live.timestamp_ms >= polled.timestamp_ms) ? streamed.live : polled;
  const online = polled?.online ?? false;

  const end = now || live?.timestamp_ms || 0;
  const from = end - minutes * 60000;
  const samples = useMemo(() => {
    const byT = new Map<number, TimelineSample>();
    for (const p of history.data?.points ?? []) byT.set(p.t, p);
    // Rollup points are minute averages, so only raw history is extended with live samples.
    if (history.data?.source === "raw") for (const p of streamed?.samples ?? []) byT.set(p.t, p);
    return [...byT.values()].sort((a, b) => a.t - b.t);
  }, [history.data, streamed?.samples]);
  const windowed = useMemo(() => samples.filter((p) => p.t >= from), [samples, from]);
  const times = windowed.map((p) => p.t);
  const isRollup = history.data?.source === "rollup";
  const gapMs = isRollup ? 5 * 60000 : 30000;

  if (detail.error && !d) {
    const missing = detail.error === "not found";
    return (
      <div className="panel">
        <p>{missing ? `Vehicle ${id} is not registered.` : `Could not load ${id}: ${detail.error}.`}</p>
        <p style={{ marginTop: 8 }}>
          <Link href="/vehicles">Back to vehicles</Link>
        </p>
      </div>
    );
  }

  return (
    <>
      <div className="page-head page-head-left">
        <h1>{id}</h1>
        {live ? (online ? <State tone={live.health} /> : <State tone="offline" />) : null}
        {live ? <span className="note">{modeLabel(live.mode)}</span> : null}
        <span style={{ marginRight: "auto" }} />
        {live && now ? <span className="fresh">Last report {ago(live.timestamp_ms, Math.max(now, live.timestamp_ms))}</span> : null}
        <StreamBadge status={status} />
      </div>
      <ErrorBanner error={detail.error} />

      <div className="stack">
        <div className="grid tiles">
          <Tile label="State of charge" value={<>{num(live?.soc_pct, 1)}<small> %</small></>} sub={live?.charging_state && live.charging_state !== "NONE" ? `${live.charging_state} charging at ${num(live.charge_power_kw)} kW` : "Not charging"} />
          <Tile label="Battery temperature" value={<>{num(live?.battery_temp_c, 1)}<small> °C</small></>} sub={`Coolant ${num(live?.coolant_temp_c, 1)} °C, ambient ${num(live?.ambient_temp_c, 1)} °C`} />
          <Tile label="Motor temperature" value={<>{num(live?.motor_temp_c, 1)}<small> °C</small></>} sub={`Cooling demand ${num(live?.cooling_demand_pct)} %`} />
          <Tile label="Speed" value={<>{num(live?.speed_kph)}<small> km/h</small></>} sub={`Odometer ${num(live?.odometer_km)} km`} />
          <Tile label="Pack power" value={<>{num(live?.pack_power_kw, 1)}<small> kW</small></>} sub={`${num(live?.wh_per_km)} Wh/km`} />
          <Tile label="Connectivity" value={live ? humanize(live.connectivity.toLowerCase()) : "–"} sub={`Signal ${num(live?.signal_dbm)} dBm`} />
          <Tile label="Battery deviation from fleet" value={<>{num(live?.deviation_score)}<small> / 100</small></>} sub={`Battery residual ${num(live?.battery_residual, 1)} °C`} />
        </div>

        <div className="grid main-side">
          <div className="stack">
            <Panel
              title="Telemetry timeline"
              note={history.data?.source === "rollup" ? "One-minute averages" : "Raw samples, live"}
              actions={<Segmented<Range> label="Time range" value={range} onChange={setRange} options={[...RANGES]} />}
            >
              <div className="grid chart-grid">
                <TimeChart
                  title="Temperatures"
                  unit="°C"
                  times={times}
                  from={from}
                  to={end}
                  gapMs={gapMs}
                  series={[
                    { label: "Battery", color: "var(--series-1)", values: windowed.map((p) => p.b) },
                    { label: "Motor", color: "var(--series-2)", values: windowed.map((p) => p.m) },
                    { label: "Ambient", color: "var(--series-3)", values: windowed.map((p) => p.a) },
                  ]}
                />
                <TimeChart showLatest={!isRollup} title="State of charge" unit="%" times={times} from={from} to={end} gapMs={gapMs} series={[{ label: "State of charge", color: "var(--series-1)", values: windowed.map((p) => p.s) }]} />
                <TimeChart showLatest={!isRollup} title="Speed" unit="km/h" digits={0} fromZero times={times} from={from} to={end} gapMs={gapMs} series={[{ label: "Speed", color: "var(--series-1)", values: windowed.map((p) => p.v) }]} />
                <TimeChart showLatest={!isRollup} title="Pack power" unit="kW" times={times} from={from} to={end} gapMs={gapMs} series={[{ label: "Pack power", color: "var(--series-1)", values: windowed.map((p) => p.p) }]} />
              </div>
            </Panel>
            <Panel title="Diagnostics" note="Latest 20. Open one to see its evidence.">
              {d?.diagnostics.map((x) => <DiagnosticRow key={x.id} d={x} />)}
              {d && d.diagnostics.length === 0 ? <p className="empty">The engine has not recorded a diagnosis for this vehicle.</p> : null}
            </Panel>
          </div>

          <div className="stack">
            <Panel title="Active alerts" note={d ? String(d.active_alerts.length) : undefined}>
              {d?.active_alerts.map((a) => <AlertCard key={a.id} alert={a} onChanged={detail.refresh} />)}
              {d && d.active_alerts.length === 0 ? <p className="empty">No open alerts.</p> : null}
            </Panel>
            <Panel title="Recommended actions">
              {d && d.recommended_actions.length > 0 ? (
                <ul className="actions">
                  {d.recommended_actions.map((a) => (
                    <li key={a}>{a}</li>
                  ))}
                </ul>
              ) : (
                <p className="empty">Nothing is recommended right now.</p>
              )}
            </Panel>
            <Panel title="Vehicle metadata">
              {d ? (
                <dl className="meta">
                  <dt>VIN</dt>
                  <dd className="mono">{d.vehicle.vin}</dd>
                  <dt>Model</dt>
                  <dd>{d.vehicle.model}</dd>
                  <dt>Region</dt>
                  <dd>{d.vehicle.region}</dd>
                  <dt>Pack capacity</dt>
                  <dd>{num(d.vehicle.pack_capacity_kwh, 1)} kWh</dd>
                  <dt>Software</dt>
                  <dd className="mono">{d.vehicle.software_version}</dd>
                  <dt>First seen</dt>
                  <dd>{dateTime(d.vehicle.first_seen_at)}</dd>
                </dl>
              ) : null}
            </Panel>
          </div>
        </div>
      </div>
    </>
  );
}
