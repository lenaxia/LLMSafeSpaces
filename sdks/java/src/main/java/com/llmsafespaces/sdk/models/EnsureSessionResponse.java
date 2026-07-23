package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

public class EnsureSessionResponse {
    @SerializedName("workspaceId") public String workspaceId;
    @SerializedName("workspacePhase") public String workspacePhase;
    @SerializedName("sessionId") public String sessionId;
    @SerializedName("resumed") public boolean resumed;
}
