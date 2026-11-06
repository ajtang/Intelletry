import type { Alert } from "./types";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

async function parse<T>(res: Response): Promise<T> {
  if (!res.ok) {
    const body = (await res.json().catch(() => null)) as { error?: string } | null;
    throw new ApiError(body?.error ?? `request failed (${res.status})`, res.status);
  }
  return (await res.json()) as T;
}

export async function apiGet<T>(path: string, signal?: AbortSignal): Promise<T> {
  return parse<T>(await fetch(`/api/v1${path}`, { signal, cache: "no-store" }));
}

export async function alertAction(id: number, action: "acknowledge" | "resolve", actor: string, note = ""): Promise<Alert> {
  const res = await fetch(`/api/v1/alerts/${id}/${action}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ actor, note }),
  });
  return parse<Alert>(res);
}
