"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useState } from "react";
import { ErrorBanner, Freshness, Panel, Segmented, State } from "@/components/ui";
import { modeLabel, num } from "@/lib/format";
import { usePoll } from "@/lib/hooks";
import type { LiveState } from "@/lib/types";

const LIMIT = 200;
type HealthFilter = "" | "healthy" | "warning" | "critical";

function VehicleList() {
  const router = useRouter();
  const params = useSearchParams();
  const health = (params.get("health") ?? "") as HealthFilter;
  const [q, setQ] = useState(params.get("q") ?? "");

  const query = new URLSearchParams({ limit: String(LIMIT) });
  if (health) query.set("health", health);
  if (q.trim()) query.set("q", q.trim());
  const { data, error, updatedAt } = usePoll<{ total: number; vehicles: (LiveState & { online: boolean })[] }>(`/vehicles?${query}`, 5000);

  return (
    <>
      <div className="page-head">
        <h1>Vehicles</h1>
        <Freshness updatedAt={updatedAt} />
      </div>
      <ErrorBanner error={error} />
      <Panel
        title={data ? `${num(data.total)} vehicles` : "Vehicles"}
        note={data && data.total > LIMIT ? `Showing the ${LIMIT} that most need attention` : "Worst health first"}
        actions={
          <div className="filters">
            <input type="search" aria-label="Search by vehicle ID" placeholder="Search vehicle ID" value={q} onChange={(e) => setQ(e.target.value)} />
            <Segmented<HealthFilter>
              label="Health"
              value={health}
              onChange={(v) => router.replace(v ? `/vehicles?health=${v}` : "/vehicles")}
              options={[
                { value: "", label: "All" },
                { value: "critical", label: "Critical" },
                { value: "warning", label: "Warning" },
                { value: "healthy", label: "Healthy" },
              ]}
            />
          </div>
        }
      >
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Vehicle</th>
                <th>Health</th>
                <th>Connection</th>
                <th>Activity</th>
                <th className="num">SoC %</th>
                <th className="num">Battery °C</th>
                <th className="num">Motor °C</th>
                <th className="num">Speed km/h</th>
                <th className="num">Deviation</th>
                <th>Model</th>
                <th>Region</th>
                <th>Software</th>
              </tr>
            </thead>
            <tbody>
              {(data?.vehicles ?? []).map((v) => (
                <tr key={v.vehicle_id}>
                  <td>
                    <Link href={`/vehicles/${v.vehicle_id}`}>{v.vehicle_id}</Link>
                  </td>
                  <td>
                    <State tone={v.health} />
                  </td>
                  <td>{v.online ? <State tone="ok" label="Online" /> : <State tone="offline" />}</td>
                  <td>{modeLabel(v.mode)}</td>
                  <td className="num">{num(v.soc_pct)}</td>
                  <td className="num">{num(v.battery_temp_c, 1)}</td>
                  <td className="num">{num(v.motor_temp_c, 1)}</td>
                  <td className="num">{num(v.speed_kph)}</td>
                  <td className="num">{num(v.deviation_score)}</td>
                  <td>{v.model}</td>
                  <td>{v.region}</td>
                  <td className="mono">{v.software_version}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {data && data.vehicles.length === 0 ? <p className="empty">No vehicles match these filters.</p> : null}
        </div>
      </Panel>
    </>
  );
}

export default function VehiclesPage() {
  return (
    <Suspense>
      <VehicleList />
    </Suspense>
  );
}
