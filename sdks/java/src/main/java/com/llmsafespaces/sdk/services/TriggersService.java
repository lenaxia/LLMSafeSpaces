package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

public class TriggersService {
    private final LLMSafeSpacesClient c;

    public TriggersService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list() {
        Map<String, Object> resp = c.request("GET", "/me/triggers", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object triggers = resp.get("triggers");
        if (triggers instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) triggers;
            return result;
        }
        return List.of();
    }

    public Map<String, Object> create(String name, String sourceType, Object sourceConfig,
                                       String workspaceId, String workflowId, String prompt,
                                       String agent, String scriptPath, List<String> scriptArgs,
                                       Object scriptEnv, String memoryMode, String captureMode,
                                       String preserveSession) {
        Map<String, Object> body = new java.util.LinkedHashMap<>();
        body.put("name", name);
        body.put("sourceType", sourceType);
        body.put("sourceConfig", sourceConfig);
        if (workspaceId != null && !workspaceId.isEmpty()) body.put("workspaceId", workspaceId);
        if (workflowId != null && !workflowId.isEmpty()) body.put("workflowId", workflowId);
        if (prompt != null && !prompt.isEmpty()) body.put("prompt", prompt);
        if (agent != null && !agent.isEmpty()) body.put("agent", agent);
        if (scriptPath != null && !scriptPath.isEmpty()) body.put("scriptPath", scriptPath);
        if (scriptArgs != null) body.put("scriptArgs", scriptArgs);
        if (scriptEnv != null) body.put("scriptEnv", scriptEnv);
        if (memoryMode != null && !memoryMode.isEmpty()) body.put("memoryMode", memoryMode);
        if (captureMode != null && !captureMode.isEmpty()) body.put("captureMode", captureMode);
        if (preserveSession != null && !preserveSession.isEmpty()) body.put("preserveSession", preserveSession);
        return c.request("POST", "/me/triggers", body,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public Map<String, Object> update(String id, Map<String, Object> updates) {
        return c.request("PUT", "/me/triggers/" + id, updates,
                new TypeToken<Map<String, Object>>(){}.getType());
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/me/triggers/" + id, null);
    }
}
