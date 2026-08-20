package com.llmsafespaces.sdk.models;

import com.google.gson.JsonObject;
import com.google.gson.annotations.SerializedName;

import java.util.List;

/**
 * One entry in a session transcript (pkg/session contract, design 0049).
 * Flat discriminated struct: {@code type} selects which fields are
 * meaningful; the rest omit. Agent-specific shapes never appear — the
 * server's adapter translates at the seam.
 */
public class Message {
    public enum Type {
        @SerializedName("user") USER,
        @SerializedName("assistant") ASSISTANT,
        @SerializedName("shell") SHELL,
        @SerializedName("agent_switch") AGENT_SWITCH,
        @SerializedName("model_switch") MODEL_SWITCH,
        @SerializedName("compaction") COMPACTION,
        @SerializedName("system") SYSTEM
    }

    public enum PartType {
        @SerializedName("text") TEXT,
        @SerializedName("reasoning") REASONING,
        @SerializedName("tool") TOOL,
        @SerializedName("file-change") FILE_CHANGE,
        @SerializedName("custom") CUSTOM
    }

    /** One renderable part of a message — the closed 5-type union. */
    public static class Part {
        public PartType type;
        public String id;
        public String text;
        public String reasoning;
        public ToolPart tool;
        public FileDiff fileChange;
        public CustomPart custom;
    }

    /** Every tool call is a ToolPart discriminated by name. */
    public static class ToolPart {
        @SerializedName("callId")
        public String callId;
        public String name;
        public JsonObject input;
        public JsonObject output;
        public ToolState state;
    }

    public static class ToolState {
        public String status;
        public String error;
        public String startedAt;
        public String completedAt;
    }

    /** Unified-diff payload of a file-change part (patch text is authoritative). */
    public static class FileDiff {
        public String path;
        @SerializedName("oldPath")
        public String oldPath;
        public String status;
        public String patch;
        public int additions;
        public int deletions;
    }

    /** The unified pending-input shape ("the agent needs a human"). */
    public static class InputRequest {
        public String id;
        @SerializedName("sessionId")
        public String sessionId;
        @SerializedName("rootSessionId")
        public String rootSessionId;
        public String kind;
        public String question;
        public String header;
        public java.util.List<InputOption> options;
        public boolean multiple;
        public boolean custom;
        public String permission;
        public java.util.List<String> patterns;
        public java.util.List<String> always;
        public JsonObject metadata;
        public ToolRef tool;
    }

    public static class InputOption {
        public String label;
        public String description;
    }

    public static class ToolRef {
        @SerializedName("messageId")
        public String messageId;
        @SerializedName("callId")
        public String callId;
    }

    /** Extension valve; {@code kind} discriminates. */
    public static class CustomPart {
        public String kind;
        public JsonObject data;
    }

    public static class ModelRef {
        public String id;
        public String provider;
    }

    public String id;
    @SerializedName("sessionId")
    public String sessionId;
    public Type type;
    @SerializedName("createdAt")
    public String createdAt;
    public List<Part> parts;
    public ModelRef model;
    public String text;
    public String command;
    @SerializedName("exitCode")
    public Integer exitCode;

    /** Concatenated text of the message's text parts — the convenience view. */
    public String textContent() {
        if (text != null && !text.isEmpty()) {
            return text;
        }
        if (parts == null) {
            return "";
        }
        var sb = new StringBuilder();
        for (Part p : parts) {
            if (p != null && p.type == PartType.TEXT && p.text != null) {
                sb.append(p.text);
            }
        }
        return sb.toString();
    }
}
