package com.llmsafespaces.sdk.services;

import com.google.gson.JsonElement;
import com.google.gson.JsonParser;
import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

public class WorkflowsService {
    private final LLMSafeSpacesClient c;

    public WorkflowsService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list() {
        Map<String, Object> resp = c.request("GET", "/me/workflows", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object wfs = resp.get("workflows");
        if (wfs instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) wfs;
            return result;
        }
        return List.of();
    }

    public Map<String, Object> get(String id) {
        return c.request("GET", "/me/workflows/" + id, null,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> create(String name, String specYaml, String status) {
        return c.request("POST", "/me/workflows",
                Map.of("name", name, "specYaml", specYaml, "status", status),
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> update(String id, Map<String, Object> updates) {
        return c.request("PUT", "/me/workflows/" + id, updates,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/me/workflows/" + id, null);
    }

    public Map<String, Object> run(String id, Object input, String workspaceId) {
        return c.request("POST", "/me/workflows/" + id + "/runs",
                Map.of("input", input != null ? input : Map.of(), "workspaceId", workspaceId != null ? workspaceId : ""),
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> getRun(String runId) {
        return c.request("GET", "/me/runs/" + runId, null,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void cancelRun(String runId) {
        c.requestVoid("POST", "/me/runs/" + runId + "/cancel", null);
    }
}
