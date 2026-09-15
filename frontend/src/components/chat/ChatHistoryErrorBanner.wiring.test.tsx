import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { ChatHistoryErrorBanner } from "./ChatHistoryErrorBanner";
import { messagesApi } from "../../api/messages";
import { ApiClientError } from "../../api/client";

/**
 * Wiring tests for the history-error path (#1303): the REAL api client
 * (getRaw → fetch → ApiClientError) against a stubbed fetch returning
 * the EXACT bodies the API authors, feeding the REAL banner component —
 * covering what the isolated unit tests mock away (path construction,
 * response parsing, error-class construction, banner rendering) without
 * a browser or live cluster. The endpoint's server side (GetHistory →
 * adapter → 502 {"error":"failed to fetch history"}) is covered by the
 * Go suite; the kind-cluster leg (agent pod killed mid-session) is the
 * e2e continuation documented in worklogs.
 */
describe("history-error wiring: API-authored bodies → banner", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("GET history 502: API-authored {error} body flows client → ApiClientError → banner message", async () => {
    // Exact body from proxy_handlers.go GetHistory's failure branch.
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ error: "failed to fetch history" }), {
        status: 502,
        headers: { "Content-Type": "application/json" },
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const rejection = await messagesApi
      .getHistoryPage("ws-1", "ses_1")
      .then(() => undefined, (e: unknown) => e);
    expect(rejection).toBeInstanceOf(ApiClientError);
    const err = rejection as ApiClientError;
    expect(err.status).toBe(502);
    expect(err.body.error).toBe("failed to fetch history");

    // The request the real client built: correct path + pagination params.
    const [url] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toContain("/api/v1/workspaces/ws-1/sessions/ses_1/message");
    expect(url).toContain("limit=");

    // The real banner renders the API's message; no ref exists to invent.
    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);
    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("HTTP 502")).toBeInTheDocument();
    expect(screen.getByText("failed to fetch history")).toBeInTheDocument();
    expect(screen.queryByText(/^Ref:/)).not.toBeInTheDocument();
    expect(screen.queryByText(/^undefined$/)).not.toBeInTheDocument();
  });

  it("GET history 503: API-authored recovery body renders the reconnecting state from API fields only", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          JSON.stringify({
            error: "workspace connection failed",
            message: "The agent is not responding. Please try again in a moment.",
            reason: "agent_unreachable",
            retryAfter: 10,
          }),
          { status: 503, headers: { "Content-Type": "application/json" } },
        ),
      ),
    );

    const rejection = await messagesApi
      .getHistoryPage("ws-1", "ses_1")
      .then(() => undefined, (e: unknown) => e);
    const err = rejection as ApiClientError;
    expect(err.status).toBe(503);

    render(<ChatHistoryErrorBanner error={err} onRetry={vi.fn()} />);
    expect(screen.getByText("Reconnecting…")).toBeInTheDocument();
    fireEvent.click(screen.getByText("Details"));
    expect(screen.getByText("Reason: agent_unreachable")).toBeInTheDocument();
    expect(
      screen.getByText(/The agent is not responding/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/^Ref:/)).not.toBeInTheDocument();
  });
});
