export function num(v: number | null | undefined, digits = 0): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "–";
  return v.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

export function ago(ms: number, now: number): string {
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

export function clock(ms: number | string): string {
  return new Date(ms).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

export function clockShort(ms: number): string {
  return new Date(ms).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
}

export function dateTime(v: string): string {
  return new Date(v).toLocaleString("en-GB", { dateStyle: "medium", timeStyle: "medium" });
}

const MODES: Record<string, string> = {
  idle: "Parked",
  city: "City driving",
  hwy: "Highway driving",
  ac: "AC charging",
  dc: "DC charging",
};

export function modeLabel(mode: string): string {
  return MODES[mode] ?? mode;
}

export function humanize(id: string): string {
  const text = id.replace(/[_.]/g, " ");
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export function isSoftwareSubject(subject: string): boolean {
  return subject.startsWith("software:");
}
