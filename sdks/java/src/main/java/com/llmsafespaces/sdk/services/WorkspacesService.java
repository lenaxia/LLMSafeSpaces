package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.FileUpload;
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

    public Map<String, Object> activate(String id) {
        return c.request("POST", "/workspaces/" + id + "/activate", null, Map.class);
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

    /**
     * Uploads a file into the workspace (Epic 67): multipart POST with a
     * single part named {@code file}; the file lands on the workspace PVC
     * under /workspace/uploads/. The returned path feeds the {@code files}
     * parameter of {@code sessions.sendPromptAsync}/{@code sessions.enqueue}.
     * The workspace must be Active; a 409 rejects with ConflictException
     * carrying {@code phase}.
     */
    public FileUpload upload(String id, String filename, byte[] content) {
        return c.requestMultipart("/workspaces/" + id + "/uploads", filename, content, FileUpload.class);
    }

    @SuppressWarnings("unchecked")
    public List<Workspace> list() {
        return c.request("GET", "/workspaces", null, new TypeToken<List<Workspace>>(){}.getType());
    }

    public void setDevPreview(String id, boolean enabled) {
        c.requestVoid("PUT", "/workspaces/" + id + "/dev-preview", Map.of("enabled", enabled));
    }

    public String devPreviewUrl(String id, int port, String path) {
        if (path == null || path.isEmpty()) {
            path = "/";
        }
        if (!path.startsWith("/")) {
            path = "/" + path;
        }
        return c.getBaseUrl() + "/api/v1/workspaces/" + id + "/dev-preview/" + port + path;
    }
}
