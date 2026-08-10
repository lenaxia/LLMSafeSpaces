// Wire-contract tests for messagesApi.getHistoryPage — these mirror the
// server contract documented in api/internal/handlers/proxy_history_pagination_test.go.
// The frontend depends on:
//   1. Request shape: GET /workspaces/{ws}/sessions/{sid}/message?limit=N[&before=ID]
//   2. Response shape: array of contract Message objects (pkg/session.Message), oldest-first
//   3. Header `X-Next-Cursor` is read into nextCursor; absence => no more pages

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { messagesApi } from "./messages";
import { getEnv } from "../env";

vi.mock("../env", () => ({
  getEnv: vi.fn(),
}));

const originalFetch = global.fetch;

describe("messagesApi.getHistoryPage — pagination wire contract", () => {
  beforeEach(() => {
    vi.mocked(getEnv).mockReturnValue({ apiBaseUrl: "http://api.test" });
  });
  afterEach(() => {
    global.fetch = originalFetch;
    vi.restoreAllMocks();
  });

  it("requests first page with limit=50 and no before param", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("[]", {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    global.fetch = fetchMock as typeof fetch;

    await messagesApi.getHistoryPage("ws-1", "ses_1");

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const url = fetchMock.mock.calls[0]![0] as string;
    const parsed = new URL(url);
    expect(parsed.pathname).toBe("/workspaces/ws-1/sessions/ses_1/message");
    expect(parsed.searchParams.get("limit")).toBe("50");
    expect(parsed.searchParams.get("before")).toBeNull();
  });

  it("sends ?before=<cursor> on subsequent pages", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response("[]", {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );
    global.fetch = fetchMock as typeof fetch;

    await messagesApi.getHistoryPage("ws-1", "ses_1", { before: "msg_0034" });

    const url = fetchMock.mock.calls[0]![0] as string;
    const parsed = new URL(url);
    expect(parsed.searchParams.get("before")).toBe("msg_0034");
    expect(parsed.searchParams.get("limit")).toBe("50");
  });

  it("reads X-Next-Cursor header into nextCursor", async () => {
    const upstream = [
      { id: "msg_0034", type: "user", createdAt: "2026-01-01T00:00:00.001Z", parts: [{ type: "text", text: "x" }] },
      { id: "msg_0035", type: "assistant", createdAt: "2026-01-01T00:00:00.002Z", parts: [{ type: "text", text: "y" }] },
    ];
    global.fetch = vi.fn().mockResolvedValue(
      new Response(JSON.stringify(upstream), {
        status: 200,
        headers: {
          "Content-Type": "application/json",
          "X-Next-Cursor": "msg_0034",
        },
      }),
    ) as typeof fetch;

    const page = await messagesApi.getHistoryPage("ws-1", "ses_1");
    expect(page.nextCursor).toBe("msg_0034");
    expect(page.messages).toHaveLength(2);
    expect(page.messages[0]!.id).toBe("msg_0034");
  });

  it("yields nextCursor=undefined when header absent (end of history)", async () => {
    global.fetch = vi.fn().mockResolvedValue(
      new Response("[]", {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    ) as typeof fetch;

    const page = await messagesApi.getHistoryPage("ws-1", "ses_1");
    expect(page.nextCursor).toBeUndefined();
    expect(page.messages).toEqual([]);
  });
});
