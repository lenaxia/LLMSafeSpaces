package com.llmsafespaces.sdk.services;

import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.Map;

public class AccountService {
    private final LLMSafeSpacesClient c;

    public AccountService(LLMSafeSpacesClient c) { this.c = c; }

    public Map<String, Object> rotateKey(String password) {
        return c.request("POST", "/account/rotate-key", Map.of("password", password), Map.class);
    }

    public void changePassword(String oldPassword, String newPassword) {
        c.requestVoid("POST", "/account/change-password",
                Map.of("oldPassword", oldPassword, "newPassword", newPassword));
    }
}
