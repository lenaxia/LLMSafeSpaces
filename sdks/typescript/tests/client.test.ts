import { describe, it, expect, vi, beforeEach } from "vitest";
import { LLMSafeSpaces } from "../src/client.js";
import { AuthError, NotFoundError, TimeoutError, LLMSafeSpacesError } from "../src/errors.js";

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

    it("sends a message and extracts content", async () => {
      const openCodeResp = {
        id: "msg-1",
        role: "assistant",
        parts: [
          { type: "text", text: "Hello " },
          { type: "text", text: "world!" },
          { type: "tool-invocation", toolName: "read_file" },
        ],
      };
      mockFetch.mockResolvedValueOnce(jsonResponse(openCodeResp));

      const result = await client.sessions.sendMessage("ws-1", "sess-1", "hi");
      expect(result.content).toBe("Hello world!");
      expect(result.raw).toEqual(openCodeResp);
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
});
