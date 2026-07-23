package com.llmsafespaces.sdk.services;

import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.Map;

public class SecretsService {
    private final LLMSafeSpacesClient c;

    public SecretsService(LLMSafeSpacesClient c) { this.c = c; }

    public Map<String, Object> create(String name, String type, String value) {
        return c.request("POST", "/secrets",
                Map.of("name", name, "type", type, "value", value), Map.class);
    }

    public Object list() {
        return c.request("GET", "/secrets", null, Object.class);
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/secrets/" + id, null);
    }

    public String reveal(String id) {
        var resp = c.requestJson("POST", "/secrets/" + id + "/reveal", Map.of("password", ""));
        return resp != null && resp.has("value") ? resp.get("value").getAsString() : null;
    }
}
