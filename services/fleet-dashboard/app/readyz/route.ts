export const dynamic = "force-dynamic";

export function GET() {
  const ready = Boolean(process.env.API_URL && process.env.API_TOKEN);
  return Response.json({ status: ready ? "ok" : "missing configuration" }, { status: ready ? 200 : 503 });
}
