package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.BindHarnessResult;
import com.llmsafespaces.sdk.models.FileUpload;
import com.llmsafespaces.sdk.models.Workspace;
import com.llmsafespaces.sdk.models.EnsureSessionResponse;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

public class WorkspacesService {
    private final LLMSafeSpacesClient c;

    public WorkspacesService(LLMSafeSpacesClient c) { this.c = c; }

    /** Creates a workspace UNBOUND (owner ruling 2026-11-16: bind-once) —
     *  bind it (first session with harness or {@link #bindHarness}) before
     *  first use. DEFAULT CHANGE (2026-11-16): omitted harness no longer
     *  means opencode implicitly. */
    public Workspace create(String name, String runtime, String storageSize) {
        return create(name, runtime, storageSize, null);
    }

    /** Creates a workspace, optionally binding the harness at create
     *  (owner ruling 2026-11-16: bind-once; must be in the instance's
     *  enabled set). A null/empty harness creates the workspace UNBOUND. */
    public Workspace create(String name, String runtime, String storageSize, String harness) {
        Map<String, Object> body = new HashMap<>();
        body.put("name", name);
        body.put("runtime", runtime);
        body.put("storageSize", storageSize);
        if (harness != null && !harness.isEmpty()) {
            body.put("harness", harness);
        }
        return c.request("POST", "/workspaces", body, Workspace.class);
    }

    /**
     * Binds an UNBOUND workspace's harness exactly once (PUT
     * /workspaces/{id}/harness; owner ruling 2026-11-16: workspaces do
     * NOT switch harnesses). Same value on a bound workspace is the
     * idempotent already-bound path; any other value is a 409
     * harness_immutable. There is no switch or recreate path.
     */
    public BindHarnessResult bindHarness(String id, String harness) {
        return c.request("PUT", "/workspaces/" + id + "/harness",
                Map.of("harness", harness), BindHarnessResult.class);
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
     * Uploads a file into the workspace (Epic 68): multipart POST with a
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
