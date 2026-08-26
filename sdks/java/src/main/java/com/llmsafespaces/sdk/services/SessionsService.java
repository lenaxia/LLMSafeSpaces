package com.llmsafespaces.sdk.services;

import com.google.gson.JsonObject;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.EnsureSessionResponse;
import com.llmsafespaces.sdk.models.Message;

import java.util.Map;

public class SessionsService {
    private final LLMSafeSpacesClient c;

    public SessionsService(LLMSafeSpacesClient c) { this.c = c; }

    public EnsureSessionResponse ensure(String workspaceId) {
        return c.request("POST", "/workspaces/" + workspaceId + "/sessions/new",
                null, EnsureSessionResponse.class);
    }

    /** Sends synchronously; returns the completed assistant Message (contract shape). */
    public Message sendMessage(String workspaceId, String sessionId, String content) {
        return c.request("POST",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/message",
                Map.of("content", content, "parts", new Object[]{Map.of("type", "text", "text", content)}),
                Message.class);
    }

    /**
     * One page of session history with cursor pagination. limit <= 0 and a
     * null/empty before are omitted (server defaults: limit 50, newest
     * page). nextCursor is "" when the beginning was reached.
     */
    public HistoryPage getHistoryPage(String workspaceId, String sessionId, int limit, String before) {
        StringBuilder path = new StringBuilder(
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/message");
        String sep = "?";
        if (limit > 0) {
            path.append(sep).append("limit=").append(limit);
            sep = "&";
        }
        if (before != null && !before.isEmpty()) {
            path.append(sep).append("before=").append(before);
        }
        var resp = c.requestJsonWithCursor("GET", path.toString(), null);
        Message[] messages = resp.body() == null
                ? new Message[0]
                : c.gson.fromJson(resp.body(), Message[].class);
        return new HistoryPage(java.util.Arrays.asList(messages), resp.nextCursor());
    }

    public void abort(String workspaceId, String sessionId) {
        c.requestVoid("POST",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/abort", null);
    }

    public void delete(String workspaceId, String sessionId) {
        c.requestVoid("DELETE",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId, null);
    }

    public String enqueue(String workspaceId, String sessionId, String text) {
        var resp = c.requestJson("POST",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/queue",
                Map.of("text", text));
        return resp != null && resp.has("messageID") ? resp.get("messageID").getAsString() : null;
    }

    /**
     * @deprecated Under the V2 session-queue model (Epic 63), abort is
     * non-destructive and queued messages survive. This removes from the
     * best-effort shadow only; it does not revoke the durable input.
     * Removed in next major version.
     */
    @Deprecated
    public void dismissQueued(String workspaceId, String sessionId, String messageId) {
        c.requestVoid("DELETE",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/queue/" + messageId, null);
    }

    public void markSeen(String workspaceId, String sessionId) {
        c.requestVoid("PUT",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/seen", null);
    }

    public void sendPromptAsync(String workspaceId, String sessionId, String message) {
        c.requestVoid("POST",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/prompt",
                Map.of("message", message));
    }
}
