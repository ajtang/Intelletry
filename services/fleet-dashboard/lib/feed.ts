import type { Alert, Diagnostic, Level, OpsEvent } from "./types";

export interface FeedEntry {
  key: string;
  at: number;
  type: string;
  source: "alert" | "diagnostic";
  subject: string;
  category: string;
  level: Level;
  title: string;
  detail: string;
  alertId?: number;
  actor?: string;
  diagnostic?: Diagnostic;
}

function level(severity: string): Level {
  return severity === "critical" || severity === "warning" ? severity : "info";
}

export function diagnosticEntry(d: Diagnostic): FeedEntry {
  return {
    key: `diagnostic:${d.id}`,
    at: new Date(d.observed_at).getTime(),
    type: `diagnostic.${d.transition}`,
    source: "diagnostic",
    subject: d.subject,
    category: d.category,
    level: level(d.severity),
    title: d.category,
    detail: d.reason,
    diagnostic: d,
  };
}

function alertEntry(type: string, a: Alert, at: number, actor?: string): FeedEntry {
  // Lifecycle steps that happen once have stable keys, so a replay from history after a
  // reconnect cannot duplicate what the live stream already delivered.
  const once = type === "alert.created" || type === "alert.acknowledged" || type === "alert.resolved";
  return {
    key: once ? `${type}:${a.id}` : `${type}:${a.id}:${a.updated_at}`,
    at,
    type,
    source: "alert",
    subject: a.subject,
    category: a.category,
    level: a.level,
    title: a.title,
    detail: a.reason,
    alertId: a.id,
    actor,
  };
}

export function entryFromEvent(ev: OpsEvent): FeedEntry | null {
  if (ev.diagnostic) return diagnosticEntry(ev.diagnostic);
  if (ev.alert) return alertEntry(ev.type, ev.alert, new Date(ev.at).getTime(), ev.actor);
  return null;
}

/** Rebuilds lifecycle entries from stored rows, because the event stream has no replay. */
export function entriesFromAlert(a: Alert): FeedEntry[] {
  const out = [alertEntry("alert.created", a, new Date(a.created_at).getTime())];
  if (a.acknowledged_at) {
    out.push(alertEntry("alert.acknowledged", a, new Date(a.acknowledged_at).getTime(), a.acknowledged_by ?? undefined));
  }
  if (a.resolved_at) {
    out.push(alertEntry("alert.resolved", a, new Date(a.resolved_at).getTime(), a.resolved_by ?? undefined));
  }
  return out;
}

/** Merges entries newest first, removing duplicates and keeping at most max. */
export function mergeFeed(current: FeedEntry[], incoming: FeedEntry[], max = 400): FeedEntry[] {
  const seen = new Map<string, FeedEntry>();
  for (const e of incoming) seen.set(e.key, e);
  for (const e of current) if (!seen.has(e.key)) seen.set(e.key, e);
  return [...seen.values()].sort((a, b) => b.at - a.at || b.key.localeCompare(a.key)).slice(0, max);
}

export function matchesFilter(e: FeedEntry, f: { source: string; level: string; q: string }): boolean {
  if (f.source && e.source !== f.source) return false;
  if (f.level && e.level !== f.level) return false;
  const q = f.q.trim().toLowerCase();
  return !q || e.subject.toLowerCase().includes(q) || e.category.toLowerCase().includes(q);
}
