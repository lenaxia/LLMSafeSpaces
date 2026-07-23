package com.llmsafespaces.sdk.services;

import com.google.gson.JsonObject;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.EnsureSessionResponse;
import com.llmsafespaces.sdk.models.MessageResponse;

import java.util.Map;

public class SessionsService {
    private final LLMSafeSpacesClient c;

    public SessionsService(LLMSafeSpacesClient c) { this.c = c; }

    public EnsureSessionResponse ensure(String workspaceId) {
        return c.request("POST", "/workspaces/" + workspaceId + "/sessions/new",
                null, EnsureSessionResponse.class);
    }

    public MessageResponse sendMessage(String workspaceId, String sessionId, String content) {
        var raw = c.requestJson("POST",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/message",
                Map.of("content", content, "parts", new Object[]{Map.of("type", "text", "text", content)}));
        return new MessageResponse(raw, MessageResponse.extractContent(raw));
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
