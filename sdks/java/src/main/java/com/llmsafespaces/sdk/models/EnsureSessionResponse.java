package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

public class EnsureSessionResponse {
    @SerializedName("workspaceId") public String workspaceId;
    @SerializedName("workspacePhase") public String workspacePhase;
    @SerializedName("sessionId") public String sessionId;
    @SerializedName("resumed") public boolean resumed;
    /**
     * The bound harness (owner ruling 2026-11-16: bind-once). On an
     * unbound workspace, the request's harness (or the opencode default
     * when omitted) bound atomically with this session.
     */
    @SerializedName("harness") public String harness;
}
