import { describe, it, expect, vi, beforeEach } from "vitest";
import { LLMSafeSpaces } from "../src/client.js";
import type { PromptAccepted, Session } from "../src/types.js";

const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const sessionRow: Session = {
  id: "ses_7f9d",
  workspaceId: "ws-1",
  parentId: "ses_root",
  title: "Refactor the adapter",
  agentId: "plan",
  model: { id: "claude-sonnet-4.5", provider: "anthropic" },
  status: "busy",
  cost: { inputTokens: 120, outputTokens: 80, totalTokens: 200 },
  contextUsage: { used: 45000, window: 200000 },
  time: { startedAt: "2026-09-15T10:00:00Z" },
  summary: "Wire the seam",
};

describe("sessions (contract Session surface)", () => {
  let client: LLMSafeSpaces;

  beforeEach(() => {
    vi.clearAllMocks();
    client = new LLMSafeSpaces({
      baseUrl: "http://localhost:8080",
      apiKey: "lsp_test123",
    });
  });

  it("gets a session typed as the contract Session", async () => {
    mockFetch.mockResolvedValueOnce(jsonResponse(sessionRow));

    const result: Session = await client.sessions.get("ws-1", "ses_7f9d");
    expect(result.id).toBe("ses_7f9d");
    expect(result.workspaceId).toBe("ws-1");
    expect(result.parentId).toBe("ses_root");
    expect(result.status).toBe("busy");
    expect(result.model?.id).toBe("claude-sonnet-4.5");
    expect(result.cost?.totalTokens).toBe(200);
    expect(result.contextUsage?.used).toBe(45000);
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/sessions/ses_7f9d",
      expect.objectContaining({ method: "GET" }),
    );
  });

  it("sendPromptAsync returns the accepted receipt (202 queued)", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse({ messageID: "msg_9", clientMessageID: "cmid-1", status: "queued" }, 202),
    );

    const rcpt: PromptAccepted = await client.sessions.sendPromptAsync("ws-1", "ses_1", "hello");
    expect(rcpt.messageID).toBe("msg_9");
    expect(rcpt.clientMessageID).toBe("cmid-1");
    expect(rcpt.status).toBe("queued");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/sessions/ses_1/prompt",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ parts: [{ type: "text", text: "hello" }] }),
      }),
    );
  });

  it("sendPromptAsync duplicate retry (200) returns the original receipt", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse({ messageID: "msg_orig", clientMessageID: "cmid-1", status: "duplicate" }, 200),
    );

    const rcpt: PromptAccepted = await client.sessions.sendPromptAsync("ws-1", "ses_1", "hello");
    expect(rcpt.messageID).toBe("msg_orig");
    expect(rcpt.status).toBe("duplicate");
  });

  it("abort and delete resolve on the bodyless 204", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await expect(client.sessions.abort("ws-1", "ses_1")).resolves.toBeUndefined();

    mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await expect(client.sessions.delete("ws-1", "ses_1")).resolves.toBeUndefined();
  });
});
