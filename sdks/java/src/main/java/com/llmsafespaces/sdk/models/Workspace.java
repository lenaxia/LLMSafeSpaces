package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

import java.util.List;

public class Workspace {
    @SerializedName("id") public String id;
    @SerializedName("name") public String name;
    @SerializedName("userId") public String userId;
    @SerializedName("runtime") public String runtime;
    @SerializedName("storageSize") public String storageSize;
    @SerializedName("phase") public String phase;
    @SerializedName("createdAt") public String createdAt;
    @SerializedName("updatedAt") public String updatedAt;
    /** #1757 workspace phrase (2 words + 1 digit, fleet-unique) — the display address. */
    @SerializedName("phraseId") public String phraseId;
    /**
     * The bound coding-agent harness (owner ruling 2026-11-16:
     * bind-once, immutable forever). null = unbound — no pod until
     * the bind (first session or bindHarness).
     */
    @SerializedName("harness") public String harness;
    /** The instance's enabled harness set (picker context; opencode always present). */
    @SerializedName("harnessOptions") public List<String> harnessOptions;
}
