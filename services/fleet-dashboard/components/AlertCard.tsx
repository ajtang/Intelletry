"use client";

import { useState } from "react";
import { alertAction } from "@/lib/api";
import { ago, dateTime, humanize, num } from "@/lib/format";
import { useNow, useOperator } from "@/lib/hooks";
import type { Alert } from "@/lib/types";
import { LevelBadge, SubjectLink } from "./ui";

/** Acknowledge and resolve controls. A conflict means another operator got there first. */
export function AlertActions({ alert, onChanged }: { alert: Alert; onChanged: () => void }) {
  const [operator] = useOperator();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (alert.status === "resolved") return null;

  const run = async (action: "acknowledge" | "resolve") => {
    setBusy(true);
    setError(null);
    try {
      await alertAction(alert.id, action, operator.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : "action failed");
    } finally {
      setBusy(false);
      onChanged();
    }
  };

  const noName = operator.trim() === "";
  return (
    <div className="alert-actions">
      {alert.status === "active" ? (
        <button type="button" className="btn" disabled={busy || noName} onClick={() => run("acknowledge")}>
          Acknowledge
        </button>
      ) : null}
      <button type="button" className="btn" disabled={busy || noName} onClick={() => run("resolve")}>
        Resolve
      </button>
      {noName ? <span className="note">Enter your operator name at the top of the page to act on alerts.</span> : null}
      {error ? (
        <span className="action-error" role="alert">
          {error}
        </span>
      ) : null}
    </div>
  );
}

export function AlertWhy({ alert }: { alert: Alert }) {
  return (
    <>
      <p className="alert-body">
        <span className="field-label">Why: </span>
        {alert.reason}
      </p>
      <p className="alert-body">
        <span className="field-label">Likely cause: </span>
        {alert.likely_cause}
      </p>
    </>
  );
}

export function AlertMeta({ alert }: { alert: Alert }) {
  const now = useNow();
  const created = new Date(alert.created_at).getTime();
  return (
    <p className="alert-meta">
      Confidence {num(alert.confidence * 100)}% · Seen {alert.occurrences}× ·{" "}
      <span title={dateTime(alert.created_at)}>Opened {now ? ago(created, Math.max(now, created)) : ""}</span>
      {alert.acknowledged_by ? ` · Acknowledged by ${alert.acknowledged_by}` : ""}
      {alert.resolved_by ? ` · Resolved by ${alert.resolved_by}` : ""}
    </p>
  );
}

export function AlertCard({ alert, onChanged, showSubject }: { alert: Alert; onChanged: () => void; showSubject?: boolean }) {
  return (
    <article className="alert">
      <div className="alert-head">
        <LevelBadge level={alert.level} />
        <span className="alert-title">{alert.title}</span>
        {showSubject ? <SubjectLink subject={alert.subject} /> : null}
        <span className="badge">{humanize(alert.status)}</span>
      </div>
      <AlertWhy alert={alert} />
      <AlertMeta alert={alert} />
      <AlertActions alert={alert} onChanged={onChanged} />
    </article>
  );
}
