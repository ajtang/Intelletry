/** Builds the upstream path, or returns null for anything outside the versioned API. */
export function upstreamPath(segments: string[]): string | null {
  if (segments[0] !== "v1" || segments.length < 2) return null;
  if (segments.some((s) => s === "" || s === "." || s === "..")) return null;
  return "/api/" + segments.map(encodeURIComponent).join("/");
}

export function isStream(path: string): boolean {
  return path === "/api/v1/events" || path.endsWith("/stream");
}

/** True when a browser request's Origin matches the host it was sent to. */
export function sameOrigin(origin: string | null, host: string | null): boolean {
  if (!origin || !host) return false;
  try {
    return new URL(origin).host === host;
  } catch {
    return false;
  }
}
