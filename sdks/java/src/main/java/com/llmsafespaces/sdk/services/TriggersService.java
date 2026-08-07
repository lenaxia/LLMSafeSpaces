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

    public Map<String, Object> create(String name, String sourceType, String targetType,
                                       Object sourceConfig, Object targetConfig) {
        return c.request("POST", "/me/triggers",
                Map.of("name", name, "sourceType", sourceType, "targetType", targetType,
                       "sourceConfig", sourceConfig, "targetConfig", targetConfig),
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
