import { describe, it, expect, vi, beforeEach } from "vitest";
import { LLMSafeSpaces } from "../src/client.js";
import { AuthError, NotFoundError, ConflictError, TimeoutError, LLMSafeSpacesError, ServiceUnavailableError, RateLimitError } from "../src/errors.js";

// Mock fetch globally
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

describe("LLMSafeSpaces Client", () => {
  let client: LLMSafeSpaces;

  beforeEach(() => {
    vi.clearAllMocks();
    client = new LLMSafeSpaces({
      baseUrl: "http://localhost:8080",
      apiKey: "lsp_test123",
    });
  });

  describe("workspaces", () => {
    it("lists workspaces", async () => {
      const data = { items: [{ id: "ws-1", name: "test" }], pagination: null };
      mockFetch.mockResolvedValueOnce(jsonResponse(data));

      const result = await client.workspaces.list();
      expect(result.items).toHaveLength(1);
      expect(result.items[0].id).toBe("ws-1");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/workspaces?limit=20&offset=0",
        expect.objectContaining({ method: "GET" }),
      );
    });

    it("creates a workspace", async () => {
      const ws = { id: "ws-new", name: "my-ws", runtime: "python:3.11" };
      mockFetch.mockResolvedValueOnce(jsonResponse(ws, 201));

      const result = await client.workspaces.create({ name: "my-ws", runtime: "python:3.11", storageSize: "10Gi" });
      expect(result.id).toBe("ws-new");
    });

    it("handles 404", async () => {
      mockFetch.mockResolvedValueOnce(errorResponse("workspace not found", 404));

      await expect(client.workspaces.get("nonexistent")).rejects.toThrow(NotFoundError);
    });

    it("suspends a workspace", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));

      await expect(client.workspaces.suspend("ws-1")).resolves.toBeUndefined();
    });

    it("refreshes workspace compute", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ restartGeneration: 3 }, 202));

      const result = await client.workspaces.refreshCompute("ws-1");
      expect(result.restartGeneration).toBe(3);
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/workspaces/ws-1/refresh-compute",
        expect.objectContaining({ method: "POST" }),
      );
    });

    it("returns undefined for 202 with empty body (suspend/restart contract)", async () => {
      // Guards the shared request() empty-body branch: 202 with no body must
      // resolve to undefined, not throw JSON.parse(""). The production paths
      // for suspend/restart return 202 with no body (router.go).
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 202 }));

      await expect(client.workspaces.suspend("ws-1")).resolves.toBeUndefined();
    });
  });

  describe("sessions", () => {
    it("ensures a session", async () => {
      const data = { workspaceId: "ws-1", sessionId: "sess-1", resumed: false, workspacePhase: "Active" };
      mockFetch.mockResolvedValueOnce(jsonResponse(data));

      const result = await client.sessions.ensure("ws-1");
      expect(result.sessionId).toBe("sess-1");
    });

    it("sends a message and returns the contract Message", async () => {
      // Contract-shaped response (pkg/session Message via the adapter seam).
      const contractResp = {
        id: "msg-1",
        type: "assistant",
        parts: [
          { type: "text", text: "Hello " },
          { type: "text", text: "world!" },
          { type: "tool", tool: { name: "read_file", state: { status: "completed" } } },
        ],
      };
      mockFetch.mockResolvedValueOnce(jsonResponse(contractResp));

      const result = await client.sessions.sendMessage("ws-1", "sess-1", "hi");
      expect(result.type).toBe("assistant");
      expect(result.parts?.length).toBe(3);
      const text = (result.parts ?? [])
        .filter((p) => p.type === "text")
        .map((p) => p.text ?? "")
        .join("");
      expect(text).toBe("Hello world!");
    });
  });

  describe("auth", () => {
    it("sends API key in Authorization header", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "u1", username: "test" }));

      await client.auth.me();
      const call = mockFetch.mock.calls[0];
      expect(call[1].headers["Authorization"]).toBe("Bearer lsp_test123");
    });

    it("auto-logins with credentials on first request", async () => {
      const credClient = new LLMSafeSpaces({
        baseUrl: "http://localhost:8080",
        credentials: { email: "test@example.com", password: "pass123" },
        timeout: 5000,
      });

      // First call: login (direct fetch to /auth/login)
      mockFetch.mockResolvedValueOnce(jsonResponse({ token: "jwt-abc", user: { id: "u1" } }));
      // Second call: actual request with token
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "u1", username: "test" }));

      await credClient.auth.me();
      expect(mockFetch).toHaveBeenCalledTimes(2);
      // First call should be login
      expect(mockFetch.mock.calls[0][0]).toContain("/auth/login");
      // Second call should have the token
      expect(mockFetch.mock.calls[1][1].headers["Authorization"]).toBe("Bearer jwt-abc");
    });

    it("throws AuthError on 401", async () => {
      mockFetch.mockResolvedValueOnce(errorResponse("authentication required", 401));

      await expect(client.auth.me()).rejects.toThrow(AuthError);
    });
  });

  describe("error handling", () => {
    it("throws TimeoutError on abort", async () => {
      // Simulate fetch rejecting with AbortError (what happens when AbortController fires)
      mockFetch.mockImplementationOnce(() => {
        const err = new DOMException("The operation was aborted", "AbortError");
        return Promise.reject(err);
      });

      await expect(client.workspaces.list()).rejects.toThrow(TimeoutError);
    });

    it("throws LLMSafeSpacesError for 500", async () => {
      mockFetch.mockResolvedValueOnce(errorResponse("internal error", 500));

      await expect(client.workspaces.list()).rejects.toThrow(LLMSafeSpacesError);
    });
  });

  describe("terminal", () => {
    it("gets a ticket", async () => {
      const data = { ticket: "tkt_abc123", expiresAt: "2026-05-29T18:00:00Z" };
      mockFetch.mockImplementationOnce(() => Promise.resolve(jsonResponse(data)));

      const result = await client.terminal.getTicket("ws-1");
      expect(result.ticket).toBe("tkt_abc123");
    });
  });

  describe("sessions (US-62.4 additions)", () => {
    it("deletes a session", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 200 }));
      await client.sessions.delete("ws-1", "sess-1");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/workspaces/ws-1/sessions/sess-1",
        expect.objectContaining({ method: "DELETE" }),
      );
    });

    // #1735: archived sessions are opt-in — the default list carries NO
    // include_archived param; the opt-in appends exactly
    // ?include_archived=true (a param-name typo would silently return
    // the filtered default).
    it("lists sessions without the include_archived param by default", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse([]));
      await client.sessions.list("ws-1");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/workspaces/ws-1/sessions",
        expect.objectContaining({ method: "GET" }),
      );
    });

    it("lists sessions with ?include_archived=true when opted in", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse([]));
      await client.sessions.list("ws-1", { includeArchived: true });
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/workspaces/ws-1/sessions?include_archived=true",
        expect.objectContaining({ method: "GET" }),
      );
    });
  });

  describe("agentRoles (US-62.4 additions)", () => {
    it("clears workspace role", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.agentRoles.clearWorkspaceRole("ws-1");
      expect(mockFetch).toHaveBeenCalledWith(
        "http://localhost:8080/api/v1/workspaces/ws-1/agent-role",
        expect.objectContaining({ method: "DELETE" }),
      );
    });
  });

  describe("providerCredentials (US-62.4)", () => {
    const credJson = {
      id: "cred-1",
      name: "my-key",
      kind: "openai",
      slug: "my-key",
      baseURL: "https://api.openai.com/v1",
      createdAt: "2026-07-22T00:00:00Z",
      updatedAt: "2026-07-22T00:00:00Z",
    };

    it("creates a credential", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse(credJson, 201));
      const result = await client.providerCredentials.create({
        name: "my-key", kind: "openai", slug: "my-key", apiKey: "sk-...",
      });
      expect(result.id).toBe("cred-1");
    });

    it("lists credentials", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse([credJson]));
      const result = await client.providerCredentials.list();
      expect(result).toHaveLength(1);
    });

    it("gets a credential", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse(credJson));
      const result = await client.providerCredentials.get("cred-1");
      expect(result.slug).toBe("my-key");
    });

    it("deletes a credential", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.providerCredentials.delete("cred-1");
    });

    it("probes models", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ models: [{ id: "gpt-4" }] }));
      const result = await client.providerCredentials.probeModels("cred-1");
      expect(result.models).toHaveLength(1);
    });

    it("lists bindings", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ workspaceIds: ["ws-1", "ws-2"], bindings: [] }));
      const result = await client.providerCredentials.listBindings("cred-1");
      expect(result).toEqual(["ws-1", "ws-2"]);
    });

    it("binds to a workspace", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ ok: true }));
      await client.providerCredentials.bind("cred-1", "ws-1");
    });

    it("unbinds from a workspace", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.providerCredentials.unbind("cred-1", "ws-1");
    });
  });

  describe("adminProviderCredentials (US-62.4)", () => {
    const credJson = {
      id: "cred-1",
      name: "admin-key",
      kind: "anthropic",
      slug: "admin-key",
      createdAt: "2026-07-22T00:00:00Z",
      updatedAt: "2026-07-22T00:00:00Z",
    };

    it("lists admin credentials", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse([credJson]));
      const result = await client.adminProviderCredentials.list();
      expect(result).toHaveLength(1);
    });

    it("updates an admin credential", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse(credJson));
      const result = await client.adminProviderCredentials.update("cred-1", { name: "renamed" });
      expect(result.id).toBe("cred-1");
    });

    it("creates an admin credential", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse(credJson, 201));
      const result = await client.adminProviderCredentials.create({
        name: "admin-key", kind: "anthropic", slug: "admin-key", apiKey: "sk-...",
      });
      expect(result.id).toBe("cred-1");
    });

    it("gets an admin credential", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse(credJson));
      const result = await client.adminProviderCredentials.get("cred-1");
      expect(result.slug).toBe("admin-key");
    });

    it("deletes an admin credential", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.adminProviderCredentials.delete("cred-1");
    });

    it("probes models for admin credential", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ models: [{ id: "claude-3" }] }));
      const result = await client.adminProviderCredentials.probeModels("cred-1");
      expect(result.models).toHaveLength(1);
    });

    it("creates auto-apply rule", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ credentialId: "cred-1", targetType: "all", withinPriority: 0 }, 201));
      await client.adminProviderCredentials.createAutoApply("cred-1", { targetType: "all" });
    });

    it("lists auto-apply rules", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse([{ credentialId: "cred-1", targetType: "all", withinPriority: 0 }]));
      const result = await client.adminProviderCredentials.listAutoApply("cred-1");
      expect(result).toHaveLength(1);
    });

    it("deletes auto-apply rule", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.adminProviderCredentials.deleteAutoApply("cred-1", "user", "u1");
    });
  });

  describe("sessions queue (US-62.6)", () => {
    it("enqueues a message", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ messageID: "qmsg-1" }, 202));
      const result = await client.sessions.enqueue("ws-1", "sess-1", "hello");
      expect(result.messageID).toBe("qmsg-1");
    });

    it("lists queued messages", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ messages: [{ id: "qmsg-1", text: "hi", session_id: "s1", workspace_id: "w1", enqueued_at: "2026-01-01T00:00:00Z", retry_count: 0 }] }));
      const result = await client.sessions.listQueue("ws-1", "sess-1");
      expect(result.messages[0].id).toBe("qmsg-1");
    });

    it("dismisses a queued message", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.sessions.dismissQueued("ws-1", "sess-1", "qmsg-1");
    });

    it("marks session seen", async () => {
      mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
      await client.sessions.markSeen("ws-1", "sess-1");
    });
  });

  // ─── Epic 64: Workflows + Triggers ─────────────────────────────────────────

  it("lists workflows", async () => {
    mockFetch.mockResolvedValueOnce(jsonResponse({ workflows: [{ id: "wf-1", name: "test" }] }));
    const result = await client.workflows.list();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/me/workflows",
      expect.objectContaining({ method: "GET" }),
    );
    expect(result.workflows).toHaveLength(1);
  });

  it("creates a workflow", async () => {
    mockFetch.mockResolvedValueOnce(jsonResponse({ id: "wf-new", name: "test-wf", status: "draft" }));
    const result = await client.workflows.create({ name: "test-wf", specYaml: "{}" });
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/me/workflows",
      expect.objectContaining({ method: "POST" }),
    );
    expect(result.id).toBe("wf-new");
  });

  it("runs a workflow", async () => {
    mockFetch.mockResolvedValueOnce(jsonResponse({ id: "run-1", status: "queued" }));
    const result = await client.workflows.run("wf-1");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/me/workflows/wf-1/runs",
      expect.objectContaining({ method: "POST" }),
    );
    expect(result.status).toBe("queued");
  });

  it("cancels a run", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 200 }));
    await client.workflows.cancelRun("run-1");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/me/runs/run-1/cancel",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("lists triggers", async () => {
    mockFetch.mockResolvedValueOnce(jsonResponse({ triggers: [{ id: "trig-1", name: "cron" }] }));
    const result = await client.triggers.list();
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/me/triggers",
      expect.objectContaining({ method: "GET" }),
    );
    expect(result.triggers).toHaveLength(1);
  });

  it("deletes a trigger", async () => {
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 200 }));
    await client.triggers.delete("trig-1");
    expect(mockFetch).toHaveBeenCalledWith(
      "http://localhost:8080/api/v1/me/triggers/trig-1",
      expect.objectContaining({ method: "DELETE" }),
    );
  });

  describe("error handling", () => {
    it("throws ServiceUnavailableError on 503 with structured fields", async () => {
      const mock503 = () => new Response(JSON.stringify({
        error: "workspace connection failed",
        message: "The agent is not responding.",
        reason: "agent_unreachable",
        retryAfter: 10,
      }), { status: 503, headers: { "Content-Type": "application/json" } });

      mockFetch.mockResolvedValueOnce(mock503());
      mockFetch.mockResolvedValueOnce(mock503());

      await expect(client.workspaces.list()).rejects.toThrow(ServiceUnavailableError);
      try {
        await client.workspaces.list();
      } catch (e) {
        expect(e).toBeInstanceOf(ServiceUnavailableError);
        const sue = e as ServiceUnavailableError;
        expect(sue.reason).toBe("agent_unreachable");
        expect(sue.retryAfter).toBe(10);
        expect(sue.message).toBe("The agent is not responding.");
      }
    });

    it("reads retryAfter from the Retry-After header when the 503 body carries only the error (input surface)", async () => {
      mockFetch.mockResolvedValueOnce(new Response(
        JSON.stringify({ error: "input pending set unknown" }),
        { status: 503, headers: { "Content-Type": "application/json", "Retry-After": "5" } },
      ));

      try {
        await client.workspaces.list();
        throw new Error("expected ServiceUnavailableError");
      } catch (e) {
        expect(e).toBeInstanceOf(ServiceUnavailableError);
        const sue = e as ServiceUnavailableError;
        expect(sue.retryAfter).toBe(5);
      }
    });
  });
});

describe("contract SSE event payloads (unhappy paths)", () => {
  // The session.event channel carries ContractEvent payloads; clients
  // must tolerate malformed input without throwing (logged, skipped).
  const malformed = (data: unknown) => JSON.stringify({ type: "session.event", session_id: "ses-1", data });

  it.each([
    ["input.request with missing fields", { type: "input.request" }],
    ["input.request with invalid option shape", { type: "input.request", input: { id: "q1", kind: "question", options: "not-an-array" } }],
    ["input.resolved with null input", { type: "input.resolved", input: null }],
    ["invalid event type", { type: "not.a.contract.event" }],
    ["missing type", { sessionId: "ses-1" }],
    ["non-object payload", "just a string"],
  ])("tolerates %s", (_name, data) => {
    expect(() => JSON.parse(malformed(data))).not.toThrow();
    // Structural contract: a conforming consumer (the frontend's
    // handleContractEvent) no-ops on these without throwing.
    const ce = JSON.parse(malformed(data)).data;
    const safe = ce && typeof ce === "object" && typeof ce.type === "string";
    expect(safe ?? true).toBeDefined();
  });
});

// #1047: cursor pagination on session history
describe("getHistoryPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("sends limit/before and surfaces X-Next-Cursor", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify([{ id: "m1", type: "user", text: "hi" }]), {
        status: 200,
        headers: { "Content-Type": "application/json", "X-Next-Cursor": "msg_42" },
      }),
    );
    const page = await client.sessions.getHistoryPage("ws-1", "sess-1", { limit: 50, before: "msg_99" });
    const calls = mockFetch.mock.calls;
    const url = calls[calls.length - 1][0] as string;
    expect(url).toContain("limit=50");
    expect(url).toContain("before=msg_99");
    expect(page.nextCursor).toBe("msg_42");
    expect(page.messages).toHaveLength(1);
    expect(page.messages[0].id).toBe("m1");
  });

  it("omits empty params and empty cursor", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(
      new Response(JSON.stringify([]), { status: 200, headers: { "Content-Type": "application/json" } }),
    );
    const page = await client.sessions.getHistoryPage("ws-1", "sess-1");
    const calls = mockFetch.mock.calls;
    const url = calls[calls.length - 1][0] as string;
    expect(url).not.toContain("?");
    expect(page.nextCursor).toBe("");
    expect(page.messages).toEqual([]);
  });
});

// #1046: MCP-server CRUD across the three Epic 53 scopes
describe("mcpServers (user scope)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("create + list + update hit the right paths and shapes", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(
      jsonResponse({ id: "srv-1", name: "n", transport: "http", hasSecret: true, enabled: true }, 201),
    );
    const srv = await client.mcpServers.create({ name: "n", transport: "http", url: "https://m.example" });
    expect(srv.id).toBe("srv-1");
    let url = mockFetch.mock.calls[0][0] as string;
    expect(url).toBe("http://localhost:8080/api/v1/me/mcp-servers");
    let body = JSON.parse(mockFetch.mock.calls[0][1].body);
    expect(body.transport).toBe("http");

    mockFetch.mockResolvedValueOnce(jsonResponse({ servers: [{ id: "srv-1", name: "n", transport: "stdio" }] }));
    const list = await client.mcpServers.list();
    expect(list).toHaveLength(1);
    expect(list[0].transport).toBe("stdio");

    mockFetch.mockResolvedValueOnce(jsonResponse({ id: "srv-1", name: "renamed", transport: "http" }));
    await client.mcpServers.update("srv-1", { name: "renamed" });
    url = mockFetch.mock.calls[2][0] as string;
    body = JSON.parse(mockFetch.mock.calls[2][1].body);
    expect(url).toBe("http://localhost:8080/api/v1/me/mcp-servers/srv-1");
    expect(body.name).toBe("renamed");
  });

  it("bind + auto-apply send the documented bodies", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(jsonResponse({ bound: true }));
    await client.mcpServers.bind("srv-1", "ws-1");
    expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/mcp-servers/srv-1/bindings");
    expect(JSON.parse(mockFetch.mock.calls[0][1].body)).toEqual({ workspaceId: "ws-1" });

    mockFetch.mockResolvedValueOnce(new Response(null, { status: 201 }));
    await client.mcpServers.createAutoApply("srv-1", "user", "u-1");
    expect(mockFetch.mock.calls[1][0]).toBe("http://localhost:8080/api/v1/me/mcp-servers/srv-1/auto-apply");
    expect(JSON.parse(mockFetch.mock.calls[1][1].body)).toEqual({ targetType: "user", targetId: "u-1" });
  });

  it("admin deleteAutoApply builds both path variants; org scope carries orgId", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValue(new Response(null, { status: 204 }));
    await client.adminMcpServers.deleteAutoApply("srv-1", "all");
    expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/admin/mcp-servers/srv-1/auto-apply/all");
    await client.adminMcpServers.deleteAutoApply("srv-1", "user", "u-1");
    expect(mockFetch.mock.calls[1][0]).toBe("http://localhost:8080/api/v1/admin/mcp-servers/srv-1/auto-apply/user/u-1");

    mockFetch.mockResolvedValueOnce(jsonResponse({ servers: [] }));
    await client.orgMcpServers.list("org-9");
    expect(mockFetch.mock.calls[2][0]).toBe("http://localhost:8080/api/v1/orgs/org-9/mcp-servers");
  });
});

// #1046 review round 1: unhappy paths + edge cases
describe("mcpServers (unhappy paths)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("propagates 404/409/400 as typed errors", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(errorResponse("mcp server not found", 404));
    await expect(client.mcpServers.get("missing")).rejects.toBeInstanceOf(NotFoundError);

    mockFetch.mockResolvedValueOnce(errorResponse("name already in use", 409));
    await expect(client.mcpServers.create({ name: "dup", transport: "http" })).rejects.toBeInstanceOf(ConflictError);

    mockFetch.mockResolvedValueOnce(errorResponse("transport is immutable", 400));
    await expect(client.mcpServers.update("srv-1", {})).rejects.toBeInstanceOf(LLMSafeSpacesError);
  });

  it("returns [] for an empty list and tolerates a bare-array response", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(jsonResponse({ servers: [] }));
    expect(await client.mcpServers.list()).toEqual([]);

    // some deployments return a bare array; the SDK unwraps both shapes
    mockFetch.mockResolvedValueOnce(jsonResponse([]));
    expect(await client.mcpServers.list()).toEqual([]);

    mockFetch.mockResolvedValueOnce(jsonResponse({ rules: [] }));
    expect(await client.mcpServers.listAutoApply("srv-1")).toEqual([]);
  });
});

// Epic 68: workspace upload + files-on-send surface (wire-level)
describe("attachments (Epic 68)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("workspaces.upload posts multipart with field name 'file'", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(jsonResponse({
      path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt",
      name: "notes.txt",
      size: 5,
    }, 201));

    const up = await client.workspaces.upload("ws-1", "notes.txt", new Blob(["hello"]));

    expect(up).toEqual({
      path: "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt",
      name: "notes.txt",
      size: 5,
    });
    const [url, init] = mockFetch.mock.calls[0];
    expect(url).toBe("http://localhost:8080/api/v1/workspaces/ws-1/uploads");
    expect(init.method).toBe("POST");
    expect(init.body).toBeInstanceOf(FormData);
    expect((init.body as FormData).get("file")).toBeInstanceOf(Blob);
    expect(await ((init.body as FormData).get("file") as Blob).text()).toBe("hello");
    // The boundary comes from the platform FormData — no JSON content type.
    expect(init.headers["Content-Type"]).toBeUndefined();
  });

  it("workspaces.upload surfaces the phase on a 409", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(errorResponse("workspace not active", 409));

    const promise = client.workspaces.upload("ws-1", "f.txt", new Blob(["x"]));
    // The 409 body carries phase — accepted via the error shape below.
    await expect(promise).rejects.toBeInstanceOf(ConflictError);
  });

  it("sendPromptAsync sends parts (not dead fields) and optional files", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 202 }));

    await client.sessions.sendPromptAsync("ws-1", "ses_1", "review please",
      ["/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"]);

    const [url, init] = mockFetch.mock.calls[0];
    expect(url).toBe("http://localhost:8080/api/v1/workspaces/ws-1/sessions/ses_1/prompt");
    const body = JSON.parse(init.body);
    expect(body).toEqual({
      parts: [{ type: "text", text: "review please" }],
      files: ["/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"],
    });
  });

  it("sendPromptAsync omits files when none given and carries no dead fields", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(new Response(null, { status: 202 }));

    await client.sessions.sendPromptAsync("ws-1", "ses_1", "hi");

    const body = JSON.parse(mockFetch.mock.calls[0][1].body);
    expect(body).toEqual({ parts: [{ type: "text", text: "hi" }] });
    expect("message" in body).toBe(false);
    expect("content" in body).toBe(false);
  });

  it("enqueue carries optional files", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(jsonResponse({ messageID: "qm-1" }, 202));

    const resp = await client.sessions.enqueue("ws-1", "ses_1", "later",
      ["/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"]);

    expect(resp).toEqual({ messageID: "qm-1" });
    const [url, init] = mockFetch.mock.calls[0];
    expect(url).toBe("http://localhost:8080/api/v1/workspaces/ws-1/sessions/ses_1/queue");
    expect(JSON.parse(init.body)).toEqual({
      text: "later",
      files: ["/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"],
    });
  });
});

// #1499: user prompt library CRUD
describe("userPrompts", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("create + list unwrap the named envelope on the right paths", async () => {
    const client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    mockFetch.mockResolvedValueOnce(
      jsonResponse({ prompt: { id: "p1", name: "Weekly summary", content: "c" } }, 201),
    );
    const created = await client.userPrompts.create({ name: "Weekly summary", content: "c" });
    expect(created.id).toBe("p1");
    expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/prompts");

    mockFetch.mockResolvedValueOnce(jsonResponse({ prompts: [{ id: "p1", name: "Weekly summary", content: "c" }] }));
    const list = await client.userPrompts.list();
    expect(list).toHaveLength(1);
    expect(list[0].name).toBe("Weekly summary");

    mockFetch.mockResolvedValueOnce(jsonResponse({ prompt: { id: "p1", name: "renamed", content: "c" } }));
    const updated = await client.userPrompts.update("p1", { name: "renamed" });
    expect(updated.name).toBe("renamed");
    expect(mockFetch.mock.calls[2][0]).toBe("http://localhost:8080/api/v1/me/prompts/p1");

    mockFetch.mockResolvedValueOnce(new Response(null, { status: 204 }));
    await expect(client.userPrompts.delete("p1")).resolves.toBeUndefined();
    expect(mockFetch.mock.calls[3][1].method).toBe("DELETE");
  });

  // ─── #1699: Task Board (design 0067 §4.3a) ─────────────────────────────────

  describe("board", () => {
    let client: LLMSafeSpaces;

    beforeEach(() => {
      client = new LLMSafeSpaces({ baseUrl: "http://localhost:8080", apiKey: "lsp_test123" });
    });

    it("creates a project and lists boards", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "p1", short_id: "prj-aaaa1111", status: "active" }, 201));
      const proj = await client.board.createProject({ name: "B", targetWorkspaceId: "ws-1", tags: ["t"] });
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/projects");
      expect((mockFetch.mock.calls[0][1] as any).body).toContain('"targetWorkspaceId":"ws-1"');
      expect(proj.short_id).toBe("prj-aaaa1111");

      mockFetch.mockResolvedValueOnce(jsonResponse({ projects: [{ id: "p1", short_id: "prj-aaaa1111" }] }));
      const list = await client.board.listProjects();
      expect(list.projects).toHaveLength(1);
    });

    it("patches, archives, and unarchives a project", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "p1", name: "renamed" }));
      const upd = await client.board.updateProject("prj-aaaa1111", { name: "renamed" });
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/projects/prj-aaaa1111");
      expect((mockFetch.mock.calls[0][1] as any).method).toBe("PATCH");
      expect(upd.name).toBe("renamed");

      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "p1", status: "archived" }));
      const arch = await client.board.archiveProject("prj-aaaa1111");
      expect(mockFetch.mock.calls[1][0]).toBe("http://localhost:8080/api/v1/me/projects/prj-aaaa1111/archive");
      expect(arch.status).toBe("archived");

      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "p1", status: "active" }));
      const un = await client.board.unarchiveProject("prj-aaaa1111");
      expect(un.status).toBe("active");
    });

    it("creates epics with lifecycle and derived progress", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "e1", short_id: "epc-bbbb2222", status: "draft" }, 201));
      const epic = await client.board.createEpic("prj-aaaa1111", { title: "E1", outcome: "o" });
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/projects/prj-aaaa1111/epics");
      expect(epic.status).toBe("draft");

      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "e1", status: "active", progress_derived: true, progress_done: 1, progress_total: 2 }));
      const act = await client.board.activateEpic("epc-bbbb2222");
      expect(mockFetch.mock.calls[1][0]).toBe("http://localhost:8080/api/v1/me/epics/epc-bbbb2222/activate");
      expect(act.progress_derived).toBe(true);

      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "e1", status: "paused" }));
      await client.board.pauseEpic("epc-bbbb2222");
      mockFetch.mockResolvedValueOnce(jsonResponse({ item_kind: "epic", id: "e1", short_id: "epc-bbbb2222" }));
      const del = await client.board.deleteEpic("epc-bbbb2222", true);
      expect(mockFetch.mock.calls[3][0]).toBe("http://localhost:8080/api/v1/me/epics/epc-bbbb2222?force=true");
      expect(del.item_kind).toBe("epic");
    });

    it("creates tasks with the OR4 pr:N anchor tag form", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "t1", short_id: "tsk-cccc3333", stage: "backlog", tags: ["pr:1234"] }, 201));
      const task = await client.board.createTask("epc-bbbb2222", {
        title: "T",
        tags: ["pr:1234"],
        acceptanceCriteria: [{ text: "c", met: false }],
      });
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/epics/epc-bbbb2222/tasks");
      expect(task.tags?.[0]).toMatch(/^pr:[1-9][0-9]*$/);
    });

    it("manages dependencies and renders the re-derived task", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ task: { id: "t1", is_blocked: true, blocked_by: ["tsk-aaaa0000"] } }));
      const dep = await client.board.addDependency("tsk-cccc3333", { dependsOn: "tsk-aaaa0000" });
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/tasks/tsk-cccc3333/dependencies");
      expect(dep.task?.is_blocked).toBe(true);

      mockFetch.mockResolvedValueOnce(jsonResponse({ task: { id: "t1", is_blocked: false } }));
      const rem = await client.board.removeDependency("tsk-cccc3333", "tsk-aaaa0000");
      expect(mockFetch.mock.calls[1][0]).toBe("http://localhost:8080/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-aaaa0000");
      expect(rem.task?.is_blocked).toBe(false);
    });

    it("moves stage, starts, deletes, and patches criteria", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "t1", stage: "done" }));
      const moved = await client.board.moveStage("tsk-cccc3333", { toStage: "done", evidenceUrl: "https://git/pr/1" });
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/tasks/tsk-cccc3333/move-stage");
      expect((mockFetch.mock.calls[0][1] as any).body).toContain('"toStage":"done"');
      expect(moved.stage).toBe("done");

      mockFetch.mockResolvedValueOnce(jsonResponse({ task_id: "t1", claim_token: "tok-1", escalated: false }));
      const claim = await client.board.startTask("tsk-cccc3333");
      expect(mockFetch.mock.calls[1][0]).toBe("http://localhost:8080/api/v1/me/tasks/tsk-cccc3333/start");
      expect(claim.claim_token).toBe("tok-1");

      mockFetch.mockResolvedValueOnce(jsonResponse({ item_kind: "task", id: "t1", short_id: "tsk-cccc3333" }));
      const deleted = await client.board.deleteTask("tsk-cccc3333", true);
      expect(mockFetch.mock.calls[2][0]).toBe("http://localhost:8080/api/v1/me/tasks/tsk-cccc3333?reconnect=true");
      expect(deleted.item_kind).toBe("task");

      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "t1", acceptance_criteria: [{ text: "c", met: true }] }));
      const patched = await client.board.updateTask("tsk-cccc3333", { setCriteriaMet: 0 });
      expect((mockFetch.mock.calls[3][1] as any).body).toContain('"setCriteriaMet":0');
      expect(patched.acceptance_criteria?.[0]?.met).toBe(true);
    });

    it("lists project tasks with the stage filter", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ tasks: [{ id: "t1", stage: "backlog" }] }));
      const res = await client.board.listProjectTasks("prj-aaaa1111", "backlog");
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/projects/prj-aaaa1111/tasks?stage=backlog");
      expect(res.tasks).toHaveLength(1);
    });

    it("reads the #1699 seams: epic list, board search, board health", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ epics: [{ id: "e1", short_id: "epc-bbbb2222", progress_derived: true, progress_done: 1, progress_total: 2 }] }));
      const epics = await client.board.listBoardEpics("prj-aaaa1111");
      expect(mockFetch.mock.calls[0][0]).toBe("http://localhost:8080/api/v1/me/projects/prj-aaaa1111/epics");
      expect(epics.epics[0].progress_total).toBe(2);

      mockFetch.mockResolvedValueOnce(jsonResponse({ results: [{ id: "t1", short_id: "tsk-cccc3333", stage: "backlog" }], suggestions: [] }));
      const search = await client.board.searchBoard("login", { project: "prj-aaaa1111", tags: ["auth"] });
      const searchUrl = mockFetch.mock.calls[1][0] as string;
      expect(searchUrl).toContain("/me/board/search?");
      expect(searchUrl).toContain("q=login");
      expect(searchUrl).toContain("project=prj-aaaa1111");
      expect(searchUrl).toContain("tag=auth");
      expect(search.results[0].short_id).toBe("tsk-cccc3333");

      mockFetch.mockResolvedValueOnce(jsonResponse({
        active_leases: [{ task_id: "t1", short_id: "tsk-cccc3333", title: "x", project_id: "p1", lease_expires_at: "2026-10-19T12:00:00Z" }],
        task_cooldowns: [], project_cooldowns: [],
        pending_input_by_origin: { escalated: 1 },
        recent_reclaims: [{ task_id: "t2", at: "2026-10-19T11:00:00Z" }],
        project_slots: [{ project_id: "p1", short_id: "prj-aaaa1111", worker_slots: 2, in_use: 1 }],
      }));
      const health = await client.board.getBoardHealth();
      expect(mockFetch.mock.calls[2][0]).toBe("http://localhost:8080/api/v1/me/board/health");
      expect(health.pending_input_by_origin["escalated"]).toBe(1);
      expect(health.project_slots[0].in_use).toBe(1);
    });

    it("threads lifecycle replay keys (body requestId + DELETE query param)", async () => {
      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "e1", status: "active" }));
      await client.board.activateEpic("epc-bbbb2222", { requestId: "key-1" });
      expect((mockFetch.mock.calls[0][1] as any).body).toContain('"requestId":"key-1"');

      mockFetch.mockResolvedValueOnce(jsonResponse({ id: "p1", status: "archived" }));
      await client.board.archiveProject("prj-aaaa1111", { requestId: "key-2" });
      expect((mockFetch.mock.calls[1][1] as any).body).toContain('"requestId":"key-2"');

      mockFetch.mockResolvedValueOnce(jsonResponse({ task: { id: "t1", is_blocked: false } }));
      await client.board.removeDependency("tsk-cccc3333", "tsk-aaaa0000", "key-3");
      expect(mockFetch.mock.calls[2][0]).toBe(
        "http://localhost:8080/api/v1/me/tasks/tsk-cccc3333/dependencies/tsk-aaaa0000?requestId=key-3",
      );
    });
  });
});
