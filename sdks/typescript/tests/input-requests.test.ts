import { describe, it, expect, vi, beforeEach } from "vitest";
import { LLMSafeSpaces } from "../src/client.js";
import { ConflictError, ServiceUnavailableError } from "../src/errors.js";
import type { InputRequest } from "../src/types.js";

const mockFetch = vi.fn();
vi.stubGlobal("fetch", mockFetch);

function jsonResponse(data: unknown, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function errorResponse(error: string, status: number) {
  return new Response(JSON.stringify({ error }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("inputRequests (contract InputRequest surface)", () => {
  let client: LLMSafeSpaces;

  beforeEach(() => {
    vi.clearAllMocks();
    client = new LLMSafeSpaces({
      baseUrl: "http://localhost:8080",
      apiKey: "lsp_test123",
    });
  });

  it("lists questions typed as InputRequest", async () => {
    const rows = [
      {
        id: "que_1",
        sessionId: "ses_1",
        rootSessionId: "ses_root",
        kind: "question",
        question: "What language?",
        header: "Choose language",
        options: [{ label: "Go", description: "Fast" }, { label: "Python" }],
        multiple: false,
        custom: true,
        tool: { messageId: "msg_abc", callId: "call_xyz" },
      },
    ];
    mockFetch.mockResolvedValueOnce(jsonResponse(rows));

    const result: InputRequest[] = await client.inputRequests.listQuestions("ws-1");
    expect(result).toHaveLength(1);
    expect(result[0].kind).toBe("question");
    expect(result[0].question).toBe("What language?");
    expect(result[0].options?.[0].label).toBe("Go");
    expect(result[0].custom).toBe(true);
    expect(result[0].tool?.callId).toBe("call_xyz");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/question",
      expect.objectContaining({ method: "GET" }),
    );
  });

  it("lists permissions typed as InputRequest", async () => {
    const rows = [
      {
        id: "per_1",
        kind: "permission",
        permission: "bash",
        patterns: ["/workspace/src/main.go"],
        always: ["/workspace/*"],
        metadata: { command: "go build" },
      },
    ];
    mockFetch.mockResolvedValueOnce(jsonResponse(rows));

    const result: InputRequest[] = await client.inputRequests.listPermissions("ws-1");
    expect(result[0].kind).toBe("permission");
    expect(result[0].permission).toBe("bash");
    expect(result[0].patterns).toEqual(["/workspace/src/main.go"]);
    expect(result[0].metadata?.command).toBe("go build");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/permission",
      expect.objectContaining({ method: "GET" }),
    );
  });

  it("replies to a question — live answer returns undefined", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 200 }));

    const late = await client.inputRequests.replyQuestion("ws-1", "que_1", [["Go"]]);
    expect(late).toBeUndefined();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/question/que_1/reply",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ answers: [["Go"]] }),
      }),
    );
  });

  it("replies to a question — late answer returns the 202 body", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse(
        { status: "queued", clientMessageID: "inbox-que_1-answer", messageID: "msg_out_1", duplicate: true },
        202,
      ),
    );

    const late = await client.inputRequests.replyQuestion("ws-1", "que_1", [["Go"]]);
    expect(late?.status).toBe("queued");
    expect(late?.clientMessageID).toBe("inbox-que_1-answer");
    expect(late?.messageID).toBe("msg_out_1");
    expect(late?.duplicate).toBe(true);
  });

  it("replies to a permission with the reply vocabulary + optional message", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 200 }));

    const late = await client.inputRequests.replyPermission("ws-1", "per_1", "always", "context note");
    expect(late).toBeUndefined();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/permission/per_1/reply",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ reply: "always", message: "context note" }),
      }),
    );
  });

  it("surfaces 409 (dismissed record) as ConflictError", async () => {
    mockFetch.mockResolvedValueOnce(errorResponse("record dismissed", 409));
    await expect(client.inputRequests.replyQuestion("ws-1", "que_1", [["Go"]])).rejects.toThrow(ConflictError);
  });

  it("surfaces 503 (delivery unavailable) as ServiceUnavailableError", async () => {
    mockFetch.mockResolvedValueOnce(errorResponse("pending set unknown", 503));
    await expect(client.inputRequests.replyPermission("ws-1", "per_1", "reject")).rejects.toThrow(
      ServiceUnavailableError,
    );
  });

  it("rejects a question", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 200 }));
    await expect(client.inputRequests.rejectQuestion("ws-1", "que_1")).resolves.toBeUndefined();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/question/que_1/reject",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("dismisses an inbox record (DELETE, 204)", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await expect(client.inputRequests.dismissInboxRecord("ws-1", "ses_1", "que_1")).resolves.toBeUndefined();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/sessions/ses_1/inbox/que_1",
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  it("requests an input snapshot (202)", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 202 }));
    await expect(client.inputRequests.requestInputSnapshot("ws-1")).resolves.toBeUndefined();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/workspaces/ws-1/input-snapshot",
      expect.objectContaining({ method: "POST" }),
    );
  });
});
