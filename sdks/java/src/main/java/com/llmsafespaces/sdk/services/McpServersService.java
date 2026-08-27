package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

/**
 * MCP servers owned by the caller (/me/mcp-servers, Epic 53). Secrets
 * (env/headers) are write-only — responses carry hasSecret only.
 */
public class McpServersService {
    private final LLMSafeSpacesClient c;

    public McpServersService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list() {
        Map<String, Object> resp = c.request("GET", "/me/mcp-servers", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        return extractServers(resp);
    }

    public Map<String, Object> get(String id) {
        return c.request("GET", "/me/mcp-servers/" + id, null,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> create(Map<String, Object> req) {
        return c.request("POST", "/me/mcp-servers", req,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> update(String id, Map<String, Object> req) {
        return c.request("PUT", "/me/mcp-servers/" + id, req,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/me/mcp-servers/" + id, null);
    }

    public void bind(String id, String workspaceId) {
        c.requestVoid("POST", "/me/mcp-servers/" + id + "/bindings",
                Map.of("workspaceId", workspaceId));
    }

    public void unbind(String id, String workspaceId) {
        c.requestVoid("DELETE", "/me/mcp-servers/" + id + "/bindings/" + workspaceId, null);
    }

    public void createAutoApply(String id, String targetType, String targetId) {
        Map<String, Object> body = new java.util.LinkedHashMap<>();
        body.put("targetType", targetType);
        if (targetId != null) body.put("targetId", targetId);
        c.requestVoid("POST", "/me/mcp-servers/" + id + "/auto-apply", body);
    }

    public List<Map<String, Object>> listAutoApply(String id) {
        Map<String, Object> resp = c.request("GET", "/me/mcp-servers/" + id + "/auto-apply", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object rules = resp.get("rules");
        if (rules instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) rules;
            return result;
        }
        return List.of();
    }

    static List<Map<String, Object>> extractServers(Map<String, Object> resp) {
        Object servers = resp.get("servers");
        if (servers instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) servers;
            return result;
        }
        return List.of();
    }
}
