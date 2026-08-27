package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

/** Platform MCP servers (/admin/mcp-servers; admin scope). */
public class AdminMcpServersService {
    private final LLMSafeSpacesClient c;

    public AdminMcpServersService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list() {
        Map<String, Object> resp = c.request("GET", "/admin/mcp-servers", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        return McpServersService.extractServers(resp);
    }

    public Map<String, Object> get(String id) {
        return c.request("GET", "/admin/mcp-servers/" + id, null,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> create(Map<String, Object> req) {
        return c.request("POST", "/admin/mcp-servers", req,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> update(String id, Map<String, Object> req) {
        return c.request("PUT", "/admin/mcp-servers/" + id, req,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/admin/mcp-servers/" + id, null);
    }

    public void bind(String id, String workspaceId) {
        c.requestVoid("POST", "/admin/mcp-servers/" + id + "/bindings",
                Map.of("workspaceId", workspaceId));
    }

    public void unbind(String id, String workspaceId) {
        c.requestVoid("DELETE", "/admin/mcp-servers/" + id + "/bindings/" + workspaceId, null);
    }

    public void createAutoApply(String id, String targetType, String targetId) {
        Map<String, Object> body = new java.util.LinkedHashMap<>();
        body.put("targetType", targetType);
        if (targetId != null) body.put("targetId", targetId);
        c.requestVoid("POST", "/admin/mcp-servers/" + id + "/auto-apply", body);
    }

    public List<Map<String, Object>> listAutoApply(String id) {
        Map<String, Object> resp = c.request("GET", "/admin/mcp-servers/" + id + "/auto-apply", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object rules = resp.get("rules");
        if (rules instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) rules;
            return result;
        }
        return List.of();
    }

    /** targetId null → removes every rule of the targetType (admin-only variant). */
    public void deleteAutoApply(String id, String targetType, String targetId) {
        String path = "/admin/mcp-servers/" + id + "/auto-apply/" + targetType;
        if (targetId != null) path += "/" + targetId;
        c.requestVoid("DELETE", path, null);
    }
}
