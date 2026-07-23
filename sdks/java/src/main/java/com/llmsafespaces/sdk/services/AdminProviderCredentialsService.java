package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.ProviderCredential;

import java.util.List;
import java.util.Map;

public class AdminProviderCredentialsService {
    private final LLMSafeSpacesClient c;

    public AdminProviderCredentialsService(LLMSafeSpacesClient c) { this.c = c; }

    public List<ProviderCredential> list() {
        return c.request("GET", "/admin/provider-credentials", null,
                new TypeToken<List<ProviderCredential>>(){}.getType());
    }

    public ProviderCredential create(String name, String kind, String slug, String apiKey, String baseURL) {
        var body = new java.util.HashMap<String, String>();
        body.put("name", name);
        body.put("kind", kind);
        body.put("slug", slug);
        body.put("apiKey", apiKey);
        if (baseURL != null && !baseURL.isEmpty()) body.put("baseURL", baseURL);
        var resp = c.requestJson("POST", "/admin/provider-credentials", body);
        if (resp != null && resp.has("credential")) {
            return c.gson.fromJson(resp.get("credential"), ProviderCredential.class);
        }
        return c.gson.fromJson(resp, ProviderCredential.class);
    }

    public ProviderCredential get(String id) {
        return c.request("GET", "/admin/provider-credentials/" + id, null, ProviderCredential.class);
    }

    public ProviderCredential update(String id, Map<String, Object> fields) {
        return c.request("PUT", "/admin/provider-credentials/" + id, fields, ProviderCredential.class);
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/admin/provider-credentials/" + id, null);
    }

    public Map<String, Object> probeModels(String id) {
        return c.request("GET", "/admin/provider-credentials/" + id + "/models", null, Map.class);
    }
}
