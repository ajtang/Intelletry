import type { NextRequest } from "next/server";
import { isStream, sameOrigin, upstreamPath } from "@/lib/proxy";

// Browsers call this route instead of the alert service, so the API token stays on the
// server. API_URL and API_TOKEN are read per request and are never exposed to client code.
export const dynamic = "force-dynamic";

const REQUEST_TIMEOUT_MS = 10_000;

type Context = { params: Promise<{ path: string[] }> };

function json(status: number, error: string): Response {
  return Response.json({ error }, { status });
}

// Next.js holds response headers until the first body chunk. A quiet stream would stay
// "connecting" until its next event or keepalive, so a comment is sent straight away.
function withOpeningComment(body: ReadableStream<Uint8Array>): ReadableStream<Uint8Array> {
  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(": connected\n\n"));
    },
  });
  // A browser disconnect cancels the readable side, which rejects this pipe and cancels upstream.
  body.pipeTo(writable).catch(() => {});
  return readable;
}

async function forward(req: NextRequest, ctx: Context): Promise<Response> {
  const base = process.env.API_URL;
  const token = process.env.API_TOKEN;
  if (!base || !token) return json(503, "dashboard is missing API_URL or API_TOKEN");

  const path = upstreamPath((await ctx.params).path);
  if (!path) return json(404, "not found");

  const init: RequestInit = {
    method: req.method,
    headers: { Authorization: `Bearer ${token}` },
    cache: "no-store",
    signal: isStream(path) ? req.signal : AbortSignal.any([req.signal, AbortSignal.timeout(REQUEST_TIMEOUT_MS)]),
  };
  if (req.method === "POST") {
    // The proxy adds the token itself, so a cross-site page must not be able to trigger
    // acknowledge or resolve through a visitor's browser.
    const host = req.headers.get("x-forwarded-host") ?? req.headers.get("host");
    if (!sameOrigin(req.headers.get("origin"), host)) return json(403, "cross-origin request rejected");
    init.body = await req.text();
    init.headers = { ...init.headers, "Content-Type": "application/json" };
  }

  let upstream: globalThis.Response;
  try {
    upstream = await fetch(base + path + req.nextUrl.search, init);
  } catch (err) {
    if (req.signal.aborted) return new Response(null, { status: 499 });
    console.error(JSON.stringify({ level: "error", msg: "upstream request failed", path, error: String(err) }));
    return json(502, "alert service unavailable");
  }

  const headers = new Headers({ "Content-Type": upstream.headers.get("content-type") ?? "application/json" });
  if (isStream(path) && upstream.ok && upstream.body) {
    headers.set("Cache-Control", "no-cache, no-transform");
    headers.set("X-Accel-Buffering", "no");
    return new Response(withOpeningComment(upstream.body), { status: upstream.status, headers });
  }
  headers.set("Cache-Control", "no-store");
  return new Response(upstream.body, { status: upstream.status, headers });
}

export const GET = forward;
export const POST = forward;
