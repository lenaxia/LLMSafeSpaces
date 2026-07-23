package com.llmsafespaces.sdk.services;

import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.Map;

public class TerminalService {
    private final LLMSafeSpacesClient c;

    public TerminalService(LLMSafeSpacesClient c) { this.c = c; }

    public Map<String, Object> getTicket(String workspaceId) {
        return c.request("POST", "/workspaces/" + workspaceId + "/terminal/ticket", null, Map.class);
    }
}
