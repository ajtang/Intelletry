"use client";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useCallback, useEffect, useMemo, useState } from "react";
import { AlertActions, AlertMeta, AlertWhy } from "@/components/AlertCard";
import { ErrorBanner, LevelBadge, Panel, Segmented, StreamBadge, SubjectLink } from "@/components/ui";
import { apiGet } from "@/lib/api";
import { entriesFromAlert, diagnosticEntry, entryFromEvent, matchesFilter, mergeFeed, type FeedEntry } from "@/lib/feed";
import { clock, dateTime, humanize, num } from "@/lib/format";
import { useEventStream, usePoll } from "@/lib/hooks";
import type { Alert, AlertEvent, Diagnostic, OpsEvent } from "@/lib/types";

const EVENT_LABELS: Record<string, string> = {
  "alert.created": "Alert opened",
  "alert.updated": "Alert updated",
  "alert.escalated": "Alert escalated",
  "alert.acknowledged": "Acknowledged",
  "alert.resolved": "Resolved",
};

function eventLabel(type: string): string {
  if (type.startsWith("diagnostic.")) return `Diagnostic ${type.slice("diagnostic.".length)}`;
  return EVENT_LABELS[type] ?? humanize(type);
}

type Selection = { kind: "alert"; id: number } | { kind: "diagnostic"; d: Diagnostic } | null;

function AlertDetail({ id, onChanged, version }: { id: number; onChanged: () => void; version: number }) {
  const { data, error, refresh } = usePoll<{ alert: Alert; history: AlertEvent[] | null }>(`/alerts/${id}`, 5000);
  // Refreshes when the live feed reports a change to any alert.
  useEffect(refresh, [version, refresh]);
  if (error && !data) return <p className="empty">Could not load this alert: {error}.</p>;
  if (!data) return <p className="empty">Loading…</p>;
  const a = data.alert;
  return (
    <>
      <div className="alert-head">
        <LevelBadge level={a.level} />
        <span className="alert-title">{a.title}</span>
        <span className="badge">{humanize(a.status)}</span>
      </div>
      <p className="alert-meta">
        <SubjectLink subject={a.subject} />
      </p>
      <AlertWhy alert={a} />
      <AlertMeta alert={a} />
      {a.recommended_actions?.length ? (
        <>
          <p className="alert-body field-label">Recommended actions</p>
          <ul className="actions">
            {a.recommended_actions.map((x) => (
              <li key={x}>{x}</li>
            ))}
          </ul>
        </>
      ) : null}
      <AlertActions
        alert={a}
        onChanged={() => {
          refresh();
          onChanged();
        }}
      />
      <p className="alert-body field-label" style={{ marginTop: 10 }}>
        Audit trail
      </p>
      <div className="evidence">
        {(data.history ?? []).map((h, i) => (
          <div key={i} className="note">
            {dateTime(h.created_at)}: {humanize(h.event_type)}
            {h.actor ? ` by ${h.actor}` : ""}
            {h.note ? `: ${h.note}` : ""}
          </div>
        ))}
      </div>
      {a.subject.startsWith("software:") ? null : (
        <p className="alert-meta">
          <Link href={`/vehicles/${encodeURIComponent(a.subject)}`}>See telemetry and evidence for {a.subject}</Link>
        </p>
      )}
    </>
  );
}

function DiagnosticDetail({ d }: { d: Diagnostic }) {
  return (
    <>
      <div className="alert-head">
        <span className="alert-title">{humanize(d.category)}</span>
        <span className="badge">{humanize(d.transition)}</span>
      </div>
      <p className="alert-meta">
        <SubjectLink subject={d.subject} /> · {dateTime(d.observed_at)}
      </p>
      <p className="alert-body">
        <span className="field-label">Why: </span>
        {d.reason}
      </p>
      <p className="alert-body">
        <span className="field-label">Likely cause: </span>
        {d.likely_cause}
      </p>
      <p className="alert-meta">Confidence {num(d.confidence * 100)}%</p>
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
    </>
  );
}

function Operations() {
  const router = useRouter();
  const params = useSearchParams();
  const linked = Number(params.get("alert")) || null;

  const [entries, setEntries] = useState<FeedEntry[]>([]);
  const [held, setHeld] = useState<FeedEntry[]>([]);
  const [paused, setPaused] = useState(false);
  const [source, setSource] = useState("");
  const [level, setLevel] = useState("");
  const [q, setQ] = useState("");
  const [picked, setPicked] = useState<Selection>(null);
  const [queueLevel, setQueueLevel] = useState("");
  const [version, setVersion] = useState(0);
  const [backfillError, setBackfillError] = useState<string | null>(null);

  const queuePath = `/alerts?status=open&limit=100${queueLevel ? `&level=${queueLevel}` : ""}`;
  const queue = usePoll<{ alerts: Alert[] }>(queuePath, 5000);
  const refreshQueue = queue.refresh;

  // The stream has no replay, so history is reloaded on every connect to fill any gap.
  const backfill = useCallback(async () => {
    try {
      const [alerts, diagnostics] = await Promise.all([apiGet<{ alerts: Alert[] }>("/alerts?limit=150"), apiGet<{ diagnostics: Diagnostic[] }>("/diagnostics?limit=150")]);
      setEntries((cur) => mergeFeed(cur, [...alerts.alerts.flatMap(entriesFromAlert), ...diagnostics.diagnostics.map(diagnosticEntry)]));
      setBackfillError(null);
    } catch (err) {
      setBackfillError(err instanceof Error ? err.message : "request failed");
    }
  }, []);

  const onEvent = useCallback(
    (ev: OpsEvent) => {
      const entry = entryFromEvent(ev);
      if (!entry) return;
      if (paused) setHeld((h) => [entry, ...h].slice(0, 400));
      else setEntries((cur) => mergeFeed(cur, [entry]));
      if (ev.alert) {
        setVersion((v) => v + 1);
        refreshQueue();
      }
    },
    [paused, refreshQueue],
  );
  const status = useEventStream<OpsEvent>("/events", onEvent, backfill);

  const resume = () => {
    setEntries((cur) => mergeFeed(cur, held));
    setHeld([]);
    setPaused(false);
  };

  const shown = useMemo(() => entries.filter((e) => matchesFilter(e, { source, level, q })), [entries, source, level, q]);
  const selection: Selection = picked ?? (linked ? { kind: "alert", id: linked } : null);

  const select = (s: Selection) => {
    setPicked(s);
    if (linked) router.replace("/operations");
  };

  const selectedAlertId = selection?.kind === "alert" ? selection.id : null;
  const selectedDiagnosticId = selection?.kind === "diagnostic" ? selection.d.id : null;

  return (
    <>
      <div className="page-head">
        <h1>Operations Feed</h1>
        <StreamBadge status={status} />
      </div>
      <ErrorBanner error={queue.error ?? backfillError} />

      <div className="grid ops-grid">
        <Panel
          title="Open alerts"
          note={queue.data ? `${queue.data.alerts.length}${queue.data.alerts.length === 100 ? "+" : ""}` : undefined}
          actions={
            <select aria-label="Filter open alerts by level" value={queueLevel} onChange={(e) => setQueueLevel(e.target.value)}>
              <option value="">All levels</option>
              <option value="critical">Critical</option>
              <option value="warning">Warning</option>
              <option value="info">Info</option>
            </select>
          }
        >
          <div className="feed">
            {(queue.data?.alerts ?? []).map((a) => (
              <button key={a.id} type="button" className="feed-item" aria-pressed={selectedAlertId === a.id} onClick={() => select({ kind: "alert", id: a.id })}>
                <span className="feed-line">
                  <LevelBadge level={a.level} />
                  <span className="feed-subject">{a.subject}</span>
                  {a.status === "acknowledged" ? <span className="badge">Acknowledged</span> : null}
                </span>
                <span className="feed-detail" style={{ display: "block" }}>
                  {a.title}
                </span>
              </button>
            ))}
            {queue.data && queue.data.alerts.length === 0 ? <p className="empty">No open alerts.</p> : null}
          </div>
        </Panel>

        <Panel
          title="Live events"
          note={`${shown.length} shown`}
          actions={
            <div className="filters">
              <input type="search" aria-label="Filter by vehicle or category" placeholder="Vehicle or category" value={q} onChange={(e) => setQ(e.target.value)} />
              <Segmented
                label="Event source"
                value={source}
                onChange={setSource}
                options={[
                  { value: "", label: "All" },
                  { value: "alert", label: "Alerts" },
                  { value: "diagnostic", label: "Diagnostics" },
                ]}
              />
              <select aria-label="Filter events by level" value={level} onChange={(e) => setLevel(e.target.value)}>
                <option value="">All levels</option>
                <option value="critical">Critical</option>
                <option value="warning">Warning</option>
                <option value="info">Info</option>
              </select>
              <button type="button" className="btn" onClick={() => (paused ? resume() : setPaused(true))}>
                {paused ? `Resume (${held.length} new)` : "Pause"}
              </button>
            </div>
          }
        >
          <div className="feed" aria-live="off">
            {shown.map((e) => {
              const active = e.alertId !== undefined ? selectedAlertId === e.alertId : selectedDiagnosticId === e.diagnostic?.id;
              return (
                <button
                  key={e.key}
                  type="button"
                  className="feed-item"
                  aria-pressed={active}
                  onClick={() => {
                    if (e.alertId !== undefined) select({ kind: "alert", id: e.alertId });
                    else if (e.diagnostic) select({ kind: "diagnostic", d: e.diagnostic });
                  }}
                >
                  <span className="feed-line">
                    <span className="feed-time">{clock(e.at)}</span>
                    <LevelBadge level={e.level} />
                    <span className="feed-subject">{e.subject}</span>
                    <span className="badge">{eventLabel(e.type)}</span>
                    {e.actor ? <span className="note">by {e.actor}</span> : null}
                  </span>
                  <span className="feed-detail" style={{ display: "block" }}>
                    {humanize(e.category)}: {e.detail}
                  </span>
                </button>
              );
            })}
            {shown.length === 0 ? <p className="empty">{entries.length ? "No events match these filters." : "Waiting for events…"}</p> : null}
          </div>
        </Panel>

        <Panel title="Details">
          {selection?.kind === "alert" ? <AlertDetail key={selection.id} id={selection.id} onChanged={refreshQueue} version={version} /> : null}
          {selection?.kind === "diagnostic" ? <DiagnosticDetail d={selection.d} /> : null}
          {!selection ? <p className="empty">Select an alert or event to see why it happened.</p> : null}
        </Panel>
      </div>
    </>
  );
}

export default function OperationsPage() {
  return (
    <Suspense>
      <Operations />
    </Suspense>
  );
}
