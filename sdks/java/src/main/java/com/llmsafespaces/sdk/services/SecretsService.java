package com.llmsafespaces.sdk.services;

import com.google.gson.JsonObject;
import com.google.gson.JsonParser;
import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

public class SecretsService {
    private final LLMSafeSpacesClient c;

    public SecretsService(LLMSafeSpacesClient c) { this.c = c; }

    public Map<String, Object> create(String name, String type, String value) {
        return c.request("POST", "/secrets",
                Map.of("name", name, "type", type, "value", value), Map.class);
    }

    @SuppressWarnings("unchecked")
    public List<Map<String, Object>> list() {
        var resp = c.requestJson("GET", "/secrets", null);
        if (resp != null && resp.has("secrets")) {
            return c.gson.fromJson(resp.get("secrets"), List.class);
        }
        return c.gson.fromJson(resp, List.class);
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/secrets/" + id, null);
    }

    public String reveal(String id, String password) {
        var resp = c.requestJson("POST", "/secrets/" + id + "/reveal", Map.of("password", password));
        return resp != null && resp.has("value") ? resp.get("value").getAsString() : null;
    }
}
