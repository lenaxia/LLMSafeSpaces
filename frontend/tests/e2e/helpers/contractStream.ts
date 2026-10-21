import type { Page, Route } from "@playwright/test";

/**
 * Passive contract-stream mock for specs that don't assert streaming
 * content: delivers one idle snapshot on the first connections (React
 * StrictMode's dev double-mount destroys connection #1), then HOLDS —
 * later connections never fulfill. A completed body means "server closed"
 * to the client, which reconnects forever (each reconnect fires the
 * reconcile path); holding after the first delivery keeps the page quiet.
 *
 * `internalSessionId` models a post-fix agentd serving a phrase-keyed row
 * with the harness id additive (#1817) — the pair the frontend heal-window
 * seam learns from. Omit it for the legacy shape (other specs unchanged).
 */
export async function mockIdleContractStream(
  page: Page,
  pattern: string,
  sessionId: string,
  extraSessions: Array<Record<string, unknown>> = [],
  internalSessionId = "",
  onDelivered?: () => void,
): Promise<void> {
  let hits = 0;
  await page.route(pattern, async (route: Route) => {
    hits++;
    if (hits <= 2) {
      const body = `data: ${JSON.stringify({
        snapshot: {
          atSeq: "1",
          snapshot: {
            sessions: [
              {
                sessionId,
                ...(internalSessionId ? { internalSessionId } : {}),
                status: "SESSION_STATUS_IDLE",
                inFlightParts: [],
                queueDepth: 0,
                pendingInputs: [],
              },
              ...extraSessions,
            ],
          },
        },
      })}\n\n`;
      await route.fulfill({
        status: 200,
        headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" },
        body,
      });
      onDelivered?.();
      return;
    }
    await new Promise<never>(() => {}); // hold — no further delivery
  });
}
