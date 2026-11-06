export const dynamic = "force-dynamic";

// Liveness only: it must not depend on the alert service, or an API outage would restart
// every dashboard pod.
export function GET() {
  return Response.json({ status: "ok" });
}
