package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.Workspace;
import com.llmsafespaces.sdk.models.EnsureSessionResponse;

import java.util.List;
import java.util.Map;

public class WorkspacesService {
    private final LLMSafeSpacesClient c;

    public WorkspacesService(LLMSafeSpacesClient c) { this.c = c; }

    public Workspace create(String name, String runtime, String storageSize) {
        return c.request("POST", "/workspaces",
                Map.of("name", name, "runtime", runtime, "storageSize", storageSize),
                Workspace.class);
    }

    public Workspace get(String id) {
        return c.request("GET", "/workspaces/" + id, null, Workspace.class);
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/workspaces/" + id, null);
    }

    public void suspend(String id) {
        c.requestVoid("POST", "/workspaces/" + id + "/suspend", null);
    }

    public void activate(String id) {
        c.requestJson("POST", "/workspaces/" + id + "/activate", null);
    }

    public void restart(String id) {
        c.requestVoid("POST", "/workspaces/" + id + "/restart", null);
    }

    public void reloadAgent(String id) {
        c.requestVoid("POST", "/workspaces/" + id + "/agent/reload", null);
    }

    public Map<String, Object> getStatus(String id) {
        return c.request("GET", "/workspaces/" + id + "/status", null, Map.class);
    }
}
