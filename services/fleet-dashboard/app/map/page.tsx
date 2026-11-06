"use client";

import Link from "next/link";
import { useMemo, useState } from "react";
import { FleetMap, type ColorBy } from "@/components/FleetMap";
import { ErrorBanner, Freshness, Panel, Segmented, State } from "@/components/ui";
import { modeLabel, num } from "@/lib/format";
import { usePoll } from "@/lib/hooks";
import { batteryBand } from "@/lib/map";
import type { Position } from "@/lib/types";

type Scope = "all" | "attention" | "offline";

const HEALTH_RANK = { critical: 2, warning: 1, healthy: 0 } as const;

function batteryState(soc: number) {
  const band = batteryBand(soc);
  if (band === "low") return <State tone="critical" label="Low" />;
  if (band === "reduced") return <State tone="warning" label="Reduced" />;
  return <State tone="ok" label="OK" />;
}

function Legend({ colorBy }: { colorBy: ColorBy }) {
  const parts =
    colorBy === "health"
      ? [
          <State key="h" tone="healthy" />,
          <State key="w" tone="warning" />,
          <State key="c" tone="critical" />,
        ]
      : [
          <State key="h" tone="ok" label="30% charge or more" />,
          <State key="w" tone="warning" label="Under 30%" />,
          <State key="c" tone="critical" label="Under 15%" />,
        ];
  return (
    <div className="map-legend">
      {parts}
      <State tone="offline" />
      <span className="note">Lines show heading. Scroll to zoom, drag to pan, double-click to reset.</span>
    </div>
  );
}

export default function MapPage() {
  const { data, error, updatedAt } = usePoll<Position[]>("/fleet/positions", 3000);
  const [colorBy, setColorBy] = useState<ColorBy>("health");
  const [scope, setScope] = useState<Scope>("all");
  const [region, setRegion] = useState("");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const all = useMemo(() => data ?? [], [data]);
  const regions = useMemo(() => [...new Set(all.map((p) => p.region))].sort(), [all]);
  const visible = useMemo(
    () =>
      all.filter((p) => {
        if (region && p.region !== region) return false;
        if (scope === "offline") return !p.online;
        if (scope === "attention") return p.online && p.health !== "healthy";
        return true;
      }),
    [all, region, scope],
  );
  const listed = useMemo(
    () =>
      [...visible]
        .sort((a, b) => Number(b.online) - Number(a.online) || HEALTH_RANK[b.health] - HEALTH_RANK[a.health] || a.id.localeCompare(b.id))
        .slice(0, 50),
    [visible],
  );
  const selected = all.find((p) => p.id === selectedId) ?? null;
  const online = all.filter((p) => p.online).length;

  return (
    <>
      <div className="page-head">
        <h1>Fleet Map</h1>
        <span className="note">
          {num(online)} online, {num(all.length - online)} offline
        </span>
        <Freshness updatedAt={updatedAt} />
      </div>
      <ErrorBanner error={error} />

      <div className="grid main-side">
        <Panel
          title="Positions"
          actions={
            <div className="filters">
              <Segmented<ColorBy>
                label="Colour by"
                value={colorBy}
                onChange={setColorBy}
                options={[
                  { value: "health", label: "Health" },
                  { value: "battery", label: "Battery" },
                ]}
              />
              <Segmented<Scope>
                label="Vehicles shown"
                value={scope}
                onChange={setScope}
                options={[
                  { value: "all", label: "All" },
                  { value: "attention", label: "Needs attention" },
                  { value: "offline", label: "Offline" },
                ]}
              />
              <select aria-label="Region" value={region} onChange={(e) => setRegion(e.target.value)}>
                <option value="">All regions</option>
                {regions.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            </div>
          }
        >
          <FleetMap positions={visible} colorBy={colorBy} focus={region} selectedId={selectedId} onSelect={setSelectedId} />
          <Legend colorBy={colorBy} />
        </Panel>

        <div className="stack">
          <Panel title="Selected vehicle">
            {selected ? (
              <dl className="meta">
                <dt>Vehicle</dt>
                <dd>
                  <Link href={`/vehicles/${selected.id}`}>{selected.id}</Link>
                </dd>
                <dt>Connection</dt>
                <dd>{selected.online ? <State tone="ok" label="Online" /> : <State tone="offline" />}</dd>
                <dt>Health</dt>
                <dd>
                  <State tone={selected.health} />
                </dd>
                <dt>Battery</dt>
                <dd>
                  {batteryState(selected.soc)} {num(selected.soc)} %, {num(selected.battery_temp_c, 1)} °C
                </dd>
                <dt>Activity</dt>
                <dd>
                  {modeLabel(selected.mode)}, {num(selected.speed_kph)} km/h
                </dd>
                <dt>Region</dt>
                <dd>{selected.region}</dd>
                <dt>Position</dt>
                <dd className="mono">
                  {selected.lat.toFixed(4)}, {selected.lon.toFixed(4)}
                </dd>
              </dl>
            ) : (
              <p className="empty">Select a vehicle on the map or in the list.</p>
            )}
          </Panel>
          <Panel title="Vehicles shown" note={visible.length > listed.length ? `First ${listed.length} of ${num(visible.length)}` : `${num(visible.length)} vehicles`}>
            <div className="table-wrap" style={{ maxHeight: "44vh", overflowY: "auto" }}>
              <table>
                <thead>
                  <tr>
                    <th>Vehicle</th>
                    <th>State</th>
                    <th className="num">SoC %</th>
                  </tr>
                </thead>
                <tbody>
                  {listed.map((p) => (
                    <tr key={p.id} className={p.id === selectedId ? "row-selected" : undefined}>
                      <td>
                        <button type="button" className="linklike" onClick={() => setSelectedId(p.id)}>
                          {p.id}
                        </button>
                      </td>
                      <td>{p.online ? <State tone={p.health} /> : <State tone="offline" />}</td>
                      <td className="num">{num(p.soc)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {data && listed.length === 0 ? <p className="empty">No vehicles match these filters.</p> : null}
            </div>
          </Panel>
        </div>
      </div>
    </>
  );
}
