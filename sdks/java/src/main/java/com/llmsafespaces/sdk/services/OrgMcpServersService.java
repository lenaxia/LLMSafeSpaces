package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

/** Organization MCP servers (/orgs/{orgId}/mcp-servers; org-admin scope). */
public class OrgMcpServersService {
    private final LLMSafeSpacesClient c;

    public OrgMcpServersService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list(String orgId) {
        Map<String, Object> resp = c.request("GET", "/orgs/" + orgId + "/mcp-servers", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        return McpServersService.extractServers(resp);
    }

    public Map<String, Object> get(String orgId, String id) {
        return c.request("GET", "/orgs/" + orgId + "/mcp-servers/" + id, null,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> create(String orgId, Map<String, Object> req) {
        return c.request("POST", "/orgs/" + orgId + "/mcp-servers", req,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> update(String orgId, String id, Map<String, Object> req) {
        return c.request("PUT", "/orgs/" + orgId + "/mcp-servers/" + id, req,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void delete(String orgId, String id) {
        c.requestVoid("DELETE", "/orgs/" + orgId + "/mcp-servers/" + id, null);
    }

    public void bind(String orgId, String id, String workspaceId) {
        c.requestVoid("POST", "/orgs/" + orgId + "/mcp-servers/" + id + "/bindings",
                Map.of("workspaceId", workspaceId));
    }

    public void unbind(String orgId, String id, String workspaceId) {
        c.requestVoid("DELETE", "/orgs/" + orgId + "/mcp-servers/" + id + "/bindings/" + workspaceId, null);
    }

    public void createAutoApply(String orgId, String id, String targetType, String targetId) {
        Map<String, Object> body = new java.util.LinkedHashMap<>();
        body.put("targetType", targetType);
        if (targetId != null) body.put("targetId", targetId);
        c.requestVoid("POST", "/orgs/" + orgId + "/mcp-servers/" + id + "/auto-apply", body);
    }

    public List<Map<String, Object>> listAutoApply(String orgId, String id) {
        Map<String, Object> resp = c.request("GET", "/orgs/" + orgId + "/mcp-servers/" + id + "/auto-apply", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object rules = resp.get("rules");
        if (rules instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) rules;
            return result;
        }
        return List.of();
    }
}
