import { describe, it, expect, vi, beforeEach } from "vitest";
import { inputApi } from "./input";
import { ApiClientError } from "./client";

// Mock fetch globally
const mockFetch = vi.fn();
global.fetch = mockFetch;

// Mock env
vi.mock("../env", () => ({ getEnv: () => ({ apiBaseUrl: "http://test" }) }));

function jsonResponse(body: unknown, status = 200) {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    json: () => Promise.resolve(body), text: () => Promise.resolve(JSON.stringify(body)),
    statusText: "OK",
  });
}

describe("inputApi", () => {
  beforeEach(() => { mockFetch.mockReset(); });

  it("questionReply sends correct body", async () => {
    mockFetch.mockReturnValue(jsonResponse(true));
    await inputApi.questionReply("ws-1", "que_abc", [["Go"]]);
    expect(mockFetch).toHaveBeenCalledWith(
      "http://test/workspaces/ws-1/question/que_abc/reply",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ answers: [["Go"]] }),
      }),
    );
  });

  it("questionReject sends POST with empty body", async () => {
    mockFetch.mockReturnValue(jsonResponse(true));
    await inputApi.questionReject("ws-1", "que_abc");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://test/workspaces/ws-1/question/que_abc/reject",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({}),
      }),
    );
  });

  it("permissionReply('once') sends correct body", async () => {
    mockFetch.mockReturnValue(jsonResponse(true));
    await inputApi.permissionReply("ws-1", "per_abc", "once");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://test/workspaces/ws-1/permission/per_abc/reply",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ reply: "once" }),
      }),
    );
  });

  it("permissionReply('reject', message) includes message", async () => {
    mockFetch.mockReturnValue(jsonResponse(true));
    await inputApi.permissionReply("ws-1", "per_abc", "reject", "too risky");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://test/workspaces/ws-1/permission/per_abc/reply",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ reply: "reject", message: "too risky" }),
      }),
    );
  });

  it("listQuestions returns typed array", async () => {
    const data = [{ id: "que_1", session_id: "ses_1", questions: [] }];
    mockFetch.mockReturnValue(jsonResponse(data));
    const result = await inputApi.listQuestions("ws-1");
    expect(result).toEqual(data);
  });

  it("API error returns ApiClientError", async () => {
    mockFetch.mockReturnValue(
      Promise.resolve({
        ok: false,
        status: 500,
        json: () => Promise.resolve({ error: "internal" }), text: () => Promise.resolve(JSON.stringify({ error: "internal" })),
        statusText: "Internal Server Error",
      }),
    );
    await expect(inputApi.listQuestions("ws-1")).rejects.toBeInstanceOf(ApiClientError);
  });
});
