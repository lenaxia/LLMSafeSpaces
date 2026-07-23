package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.ProviderCredential;

import java.util.List;
import java.util.Map;

public class ProviderCredentialsService {
    private final LLMSafeSpacesClient c;

    public ProviderCredentialsService(LLMSafeSpacesClient c) { this.c = c; }

    public ProviderCredential create(String name, String kind, String slug, String apiKey, String baseURL) {
        var body = new java.util.HashMap<String, String>();
        body.put("name", name);
        body.put("kind", kind);
        body.put("slug", slug);
        body.put("apiKey", apiKey);
        if (baseURL != null && !baseURL.isEmpty()) body.put("baseURL", baseURL);
        var resp = c.requestJson("POST", "/provider-credentials", body);
        if (resp != null && resp.has("credential")) {
            return c.gson.fromJson(resp.get("credential"), ProviderCredential.class);
        }
        return c.gson.fromJson(resp, ProviderCredential.class);
    }

    public List<ProviderCredential> list() {
        return c.request("GET", "/provider-credentials", null,
                new TypeToken<List<ProviderCredential>>(){}.getType());
    }

    public ProviderCredential get(String id) {
        return c.request("GET", "/provider-credentials/" + id, null, ProviderCredential.class);
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/provider-credentials/" + id, null);
    }

    public Map<String, Object> probeModels(String id) {
        return c.request("GET", "/provider-credentials/" + id + "/models", null, Map.class);
    }

    public List<String> listBindings(String id) {
        var resp = c.requestJson("GET", "/provider-credentials/" + id + "/bindings", null);
        if (resp != null && resp.has("workspaceIds")) {
            return c.gson.fromJson(resp.get("workspaceIds"), List.class);
        }
        return List.of();
    }

    public void bind(String credId, String workspaceId) {
        c.requestJson("POST", "/provider-credentials/" + credId + "/bind/" + workspaceId, null);
    }

    public void unbind(String credId, String workspaceId) {
        c.requestVoid("DELETE", "/provider-credentials/" + credId + "/bind/" + workspaceId, null);
    }
}
