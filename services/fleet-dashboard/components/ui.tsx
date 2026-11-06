"use client";

import Link from "next/link";
import type { ReactNode } from "react";
import { ago, humanize, isSoftwareSubject } from "@/lib/format";
import { useNow, type StreamStatus } from "@/lib/hooks";
import type { Level } from "@/lib/types";

const GLYPHS: Record<string, string> = {
  healthy: "●",
  ok: "●",
  warning: "▲",
  critical: "◆",
  info: "●",
  offline: "○",
};

/** Every state pairs its colour with a shape and a word, so colour never carries it alone. */
export function State({ tone, label }: { tone: string; label?: string }) {
  return (
    <span className={`state tone-${tone}`}>
      <span className="glyph" aria-hidden="true">
        {GLYPHS[tone] ?? "●"}
      </span>
      <span className="state-label">{label ?? humanize(tone)}</span>
    </span>
  );
}

export function LevelBadge({ level }: { level: Level }) {
  return <State tone={level} />;
}

export function Tile({
  label,
  value,
  sub,
  tone,
  href,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  tone?: string;
  href?: string;
}) {
  const body = (
    <>
      <div className="tile-label">
        {tone ? <State tone={tone} label={label} /> : label}
      </div>
      <div className="tile-value">{value}</div>
      {sub ? <div className="tile-sub">{sub}</div> : null}
    </>
  );
  return href ? (
    <Link href={href} className="tile">
      {body}
    </Link>
  ) : (
    <div className="tile">{body}</div>
  );
}

export function Panel({ title, note, actions, children }: { title: string; note?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="panel">
      <div className="panel-head">
        <h2>{title}</h2>
        {note ? <span className="note">{note}</span> : null}
        {actions}
      </div>
      {children}
    </section>
  );
}

export function ErrorBanner({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <div className="banner" role="alert">
      Could not refresh data: {error}. Values shown may be out of date.
    </div>
  );
}

export function Freshness({ updatedAt }: { updatedAt: number | null }) {
  const now = useNow();
  if (!updatedAt || !now) return <span className="fresh">Loading…</span>;
  return <span className="fresh">Updated {ago(updatedAt, Math.max(now, updatedAt))}</span>;
}

export function StreamBadge({ status }: { status: StreamStatus }) {
  const map = {
    live: { tone: "ok", label: "Live" },
    connecting: { tone: "offline", label: "Connecting" },
    reconnecting: { tone: "warning", label: "Reconnecting" },
  } as const;
  const s = map[status];
  return <State tone={s.tone} label={s.label} />;
}

export function SubjectLink({ subject }: { subject: string }) {
  if (isSoftwareSubject(subject)) {
    return <span title="Fleet-level finding for a software version">{subject}</span>;
  }
  return <Link href={`/vehicles/${encodeURIComponent(subject)}`}>{subject}</Link>;
}

export function Segmented<T extends string>({
  options,
  value,
  onChange,
  label,
}: {
  options: { value: T; label: string }[];
  value: T;
  onChange: (v: T) => void;
  label: string;
}) {
  return (
    <div className="segmented" role="group" aria-label={label}>
      {options.map((o) => (
        <button key={o.value} type="button" aria-pressed={o.value === value} onClick={() => onChange(o.value)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}
