package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

/**
 * The caller's saved prompts (/me/prompts, #1499). Map-based facade
 * per the TriggersService convention; responses carry the named
 * {"prompt": ...} / {"prompts": [...]} envelopes unwrapped.
 */
public class UserPromptsService {
    private final LLMSafeSpacesClient c;

    public UserPromptsService(LLMSafeSpacesClient c) { this.c = c; }

    public List<Map<String, Object>> list() {
        Map<String, Object> resp = c.request("GET", "/me/prompts", null,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object prompts = resp.get("prompts");
        if (prompts instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) prompts;
            return result;
        }
        return List.of();
    }

    public Map<String, Object> create(String name, String content) {
        Map<String, Object> body = new java.util.LinkedHashMap<>();
        body.put("name", name);
        body.put("content", content);
        Map<String, Object> resp = c.request("POST", "/me/prompts", body,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object prompt = resp.get("prompt");
        return prompt instanceof Map ? (Map<String, Object>) prompt : resp;
    }

    public Map<String, Object> update(String id, Map<String, Object> updates) {
        Map<String, Object> resp = c.request("PUT", "/me/prompts/" + id, updates,
                new TypeToken<Map<String, Object>>(){}.getType());
        Object prompt = resp.get("prompt");
        return prompt instanceof Map ? (Map<String, Object>) prompt : resp;
    }

    public void delete(String id) {
        c.requestVoid("DELETE", "/me/prompts/" + id, null);
    }
}
