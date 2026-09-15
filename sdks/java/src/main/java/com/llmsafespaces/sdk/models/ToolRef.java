package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

/** Identifies the tool call that triggered an InputRequest. */
public class ToolRef {
    @SerializedName("messageId")
    public String messageId;
    @SerializedName("callId")
    public String callId;
}
