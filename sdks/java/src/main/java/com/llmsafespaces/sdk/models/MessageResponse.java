package com.llmsafespaces.sdk.models;

import com.google.gson.JsonObject;

public class MessageResponse {
    private final JsonObject raw;
    private final String content;

    public MessageResponse(JsonObject raw, String content) {
        this.raw = raw;
        this.content = content;
    }

    public JsonObject getRaw() { return raw; }
    public String getContent() { return content; }

    public static String extractContent(JsonObject raw) {
        if (raw == null || !raw.has("parts")) return "";
        var sb = new StringBuilder();
        for (var el : raw.getAsJsonArray("parts")) {
            var part = el.getAsJsonObject();
            if (part.has("type") && "text".equals(part.get("type").getAsString()) && part.has("text")) {
                sb.append(part.get("text").getAsString());
            }
        }
        return sb.toString();
    }
}
