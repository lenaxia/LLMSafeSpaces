package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;
import com.google.gson.JsonObject;

import java.util.List;

/**
 * The unified pending-input shape ("the agent needs a human", design
 * 0049 §4.5) — the contract the question/permission list routes return
 * (#1302). Question-specific fields apply when kind is question;
 * permission-specific fields when kind is permission.
 */
public class InputRequest {
    public String id;
    @SerializedName("sessionId")
    public String sessionId;
    /** The user-visible ancestor session (subtask prompts bubble up). */
    @SerializedName("rootSessionId")
    public String rootSessionId;
    public String kind;
    public String question;
    public String header;
    public List<InputOption> options;
    public boolean multiple;
    /** Whether free-text answers are accepted. */
    public boolean custom;
    /** Permission action (e.g. bash, edit). */
    public String permission;
    /** Glob patterns a permission applies to. */
    public List<String> patterns;
    public List<String> always;
    /** Open-ended extension data. */
    public JsonObject metadata;
    public ToolRef tool;
}
