package com.llmsafespaces.sdk.services;

import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.Map;

public class UserSettingsService {
    private final LLMSafeSpacesClient c;

    public UserSettingsService(LLMSafeSpacesClient c) { this.c = c; }

    public Map<String, Object> get() {
        return c.request("GET", "/users/me/settings", null, Map.class);
    }

    public Map<String, Object> getSchema() {
        return c.request("GET", "/users/me/settings/schema", null, Map.class);
    }

    public Map<String, Object> set(String key, Object value) {
        return c.request("PUT", "/users/me/settings/" + key, Map.of("value", value), Map.class);
    }
}
