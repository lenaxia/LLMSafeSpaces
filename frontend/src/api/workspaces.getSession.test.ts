import { afterEach, describe, expect, it, vi } from "vitest";
import { workspacesApi } from "./workspaces";

/**
 * Wiring tests for the session-status recheck (PR #1051 / issue #1050):
 * the REAL api client building the REAL request path against a stubbed
 * fetch — covering what the hook tests mock away (path construction,
 * signal plumbing) without a browser. The endpoint's server side
 * (GetSession → adapter → statusz) is covered by the Go suite
 * (adapter_path_test.go: TestGetSession_AdapterPath_ReturnsContractJSON).
 */

describe("workspacesApi.getSession wiring (recheck path)", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("GETs /workspaces/{id}/sessions/{sid} and parses status", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ id: "ses_1", title: "t", status: "busy" }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const session = await workspacesApi.getSession("ws-1", "ses_1");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url.endsWith("/api/v1/workspaces/ws-1/sessions/ses_1")).toBe(true);
    expect(init.method ?? "GET").toBe("GET");
    expect(session.status).toBe("busy");
  });

  it("propagates the AbortSignal to fetch (bounded recheck)", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ id: "ses_1", status: "idle" }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const ctl = new AbortController();

    await workspacesApi.getSession("ws-1", "ses_1", { signal: ctl.signal });

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(init.signal).toBe(ctl.signal);
  });

  it("surfaces non-2xx as a rejection the hook's fail-open path catches", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("{}", { status: 502 })));

    await expect(workspacesApi.getSession("ws-1", "ses_1")).rejects.toThrow();
  });
});
