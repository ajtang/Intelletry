"use client";

import Link from "next/link";
import { useMemo } from "react";
import { SegBar, TrendBars } from "@/components/charts";
import { ErrorBanner, Freshness, LevelBadge, Panel, State, SubjectLink, Tile } from "@/components/ui";
import { trendBuckets } from "@/lib/chart";
import { humanize, modeLabel, num } from "@/lib/format";
import { usePoll } from "@/lib/hooks";
import type { Alert, LiveState, Overview, Trends } from "@/lib/types";

const TREND_HOURS = 6;

function SoftwareRow({ version, vehicles, max }: { version: string; vehicles: number; max: number }) {
  const { data } = usePoll<{ alerts: Alert[] }>(`/alerts?status=open&limit=1&subject=${encodeURIComponent(`software:${version}`)}`, 15000);
  const finding = data?.alerts[0];
  return (
    <div className="hbar">
      <span className="mono">{version}</span>
      <div className="hbar-track">
        <div className="hbar-fill" style={{ width: `${(vehicles / Math.max(max, 1)) * 100}%` }} />
      </div>
      <span className="num">{num(vehicles)}</span>
      {finding ? (
        <div style={{ gridColumn: "1 / -1" }}>
          <LevelBadge level={finding.level} /> <Link href={`/operations?alert=${finding.id}`}>{finding.title}</Link>
        </div>
      ) : null}
    </div>
  );
}

export default function OverviewPage() {
  const overview = usePoll<Overview>("/fleet/overview", 3000);
  const trends = usePoll<Trends>(`/alerts/trends?hours=${TREND_HOURS}`, 15000);
  const attention = usePoll<{ total: number; vehicles: (LiveState & { online: boolean })[] }>("/vehicles?limit=10", 5000);
  const alerts = usePoll<{ alerts: Alert[] }>("/alerts?status=active&limit=8", 5000);
  const o = overview.data;

  const buckets = useMemo(
    () => (trends.data ? trendBuckets(trends.data, TREND_HOURS, trends.updatedAt ?? 0) : []),
    [trends.data, trends.updatedAt],
  );
  const versions = o?.software_versions ?? [];
  const maxVersion = Math.max(0, ...versions.map((v) => v.vehicles));

  return (
    <>
      <div className="page-head">
        <h1>Fleet Overview</h1>
        <Freshness updatedAt={overview.updatedAt} />
      </div>
      <ErrorBanner error={overview.error} />

      <div className="grid stack">
        <div className="grid tiles">
          <Tile label="Vehicles online" value={<>{num(o?.online)} <small>/ {num(o?.total_vehicles)}</small></>} sub={`${num(o?.offline)} offline`} href="/map" />
          <Tile label="Healthy" tone="healthy" value={num(o?.healthy)} sub="online vehicles" href="/vehicles?health=healthy" />
          <Tile label="Warning" tone="warning" value={num(o?.warning)} sub="online vehicles" href="/vehicles?health=warning" />
          <Tile label="Critical" tone="critical" value={num(o?.critical)} sub="online vehicles" href="/vehicles?health=critical" />
          <Tile label="Open alerts" value={num(o?.active_alerts)} sub={`${num(o?.alerts_unacknowledged)} unacknowledged`} href="/operations" />
          <Tile label="Avg state of charge" value={<>{num(o?.avg_soc_pct)}<small> %</small></>} sub="online vehicles" />
          <Tile label="Battery temperature" value={<>{num(o?.avg_battery_temp_c, 1)}<small> °C avg</small></>} sub={`Max ${num(o?.max_battery_temp_c, 1)} °C`} />
          <Tile label="Pipeline throughput" value={<>{num(o?.events_per_sec)}<small> events/s</small></>} sub={`Queue depth ${num(o?.queue_depth)}`} />
        </div>

        <div className="grid cols-2">
          <Panel title="Fleet activity" note="Online vehicles">
            <SegBar
              parts={[
                { label: "Driving", value: o?.driving ?? 0, color: "var(--series-1)" },
                { label: "Charging", value: o?.charging ?? 0, color: "var(--series-2)" },
                { label: "Parked", value: o?.parked ?? 0, color: "var(--series-3)" },
              ]}
            />
          </Panel>
          <Panel title="Open alerts by level">
            <SegBar
              parts={[
                { label: "Critical", value: o?.alerts_critical ?? 0, color: "var(--critical)" },
                { label: "Warning", value: o?.alerts_warning ?? 0, color: "var(--warning)" },
                { label: "Info", value: o?.alerts_info ?? 0, color: "var(--info)" },
              ]}
            />
          </Panel>
        </div>

        <div className="grid cols-2">
          <Panel title="New alerts" note={`Last ${TREND_HOURS} hours, ${trends.data?.bucket_minutes ?? "–"} minute buckets`}>
            <TrendBars buckets={buckets} bucketMinutes={trends.data?.bucket_minutes ?? 1} />
          </Panel>
          <Panel title="Alert categories" note={`Last ${TREND_HOURS} hours`}>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Category</th>
                    <th className="num">Raised</th>
                    <th className="num">Still open</th>
                    <th className="num">Mean time to resolve</th>
                  </tr>
                </thead>
                <tbody>
                  {(trends.data?.categories ?? []).map((c) => (
                    <tr key={c.category}>
                      <td>{humanize(c.category)}</td>
                      <td className="num">{num(c.count)}</td>
                      <td className="num">{num(c.open)}</td>
                      <td className="num">{c.mean_minutes_to_resolve === null ? "–" : `${num(c.mean_minutes_to_resolve, 1)} min`}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {trends.data && !trends.data.categories?.length ? <p className="empty">No alerts in this period.</p> : null}
            </div>
          </Panel>
        </div>

        <div className="grid main-side">
          <Panel title="Vehicles needing attention" note="Worst health, then largest deviation" actions={<Link href="/vehicles">All vehicles</Link>}>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Vehicle</th>
                    <th>Health</th>
                    <th>Activity</th>
                    <th className="num">SoC %</th>
                    <th className="num">Battery °C</th>
                    <th className="num">Deviation</th>
                    <th>Software</th>
                  </tr>
                </thead>
                <tbody>
                  {(attention.data?.vehicles ?? []).map((v) => (
                    <tr key={v.vehicle_id}>
                      <td>
                        <Link href={`/vehicles/${v.vehicle_id}`}>{v.vehicle_id}</Link>
                      </td>
                      <td>{v.online ? <State tone={v.health} /> : <State tone="offline" />}</td>
                      <td>{modeLabel(v.mode)}</td>
                      <td className="num">{num(v.soc_pct)}</td>
                      <td className="num">{num(v.battery_temp_c, 1)}</td>
                      <td className="num">{num(v.deviation_score)}</td>
                      <td className="mono">{v.software_version}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </Panel>
          <div className="stack">
            <Panel title="Software versions" note="Online vehicles">
              {versions.map((v) => (
                <SoftwareRow key={v.version} version={v.version} vehicles={v.vehicles} max={maxVersion} />
              ))}
            </Panel>
            <Panel title="Latest unacknowledged alerts" actions={<Link href="/operations">Operations Feed</Link>}>
              {(alerts.data?.alerts ?? []).map((a) => (
                <div key={a.id} className="alert">
                  <div className="alert-head">
                    <LevelBadge level={a.level} />
                    <Link href={`/operations?alert=${a.id}`}>{a.title}</Link>
                    <SubjectLink subject={a.subject} />
                  </div>
                </div>
              ))}
              {alerts.data && alerts.data.alerts.length === 0 ? <p className="empty">Nothing is waiting for acknowledgement.</p> : null}
            </Panel>
          </div>
        </div>
      </div>
    </>
  );
}
