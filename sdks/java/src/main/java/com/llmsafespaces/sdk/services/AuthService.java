package com.llmsafespaces.sdk.services;

import com.google.gson.JsonObject;
import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

public class AuthService {
    private final LLMSafeSpacesClient c;

    public AuthService(LLMSafeSpacesClient c) { this.c = c; }

    public Map<String, Object> me() {
        return c.request("GET", "/auth/me", null, Map.class);
    }

    public Map<String, Object> register(String username, String email, String password) {
        return c.request("POST", "/auth/register",
                Map.of("username", username, "email", email, "password", password), Map.class);
    }

    public void logout() {
        c.requestVoid("POST", "/auth/logout", null);
    }

    public void requestPasswordReset(String email) {
        c.requestVoid("POST", "/auth/password-reset/request", Map.of("email", email));
    }

    public void unlockDek(String password) {
        c.requestVoid("POST", "/auth/unlock-dek", Map.of("password", password));
    }
}
