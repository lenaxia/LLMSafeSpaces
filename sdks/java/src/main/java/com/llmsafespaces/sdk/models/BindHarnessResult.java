package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

import java.util.List;

/**
 * PUT /workspaces/{id}/harness response (owner ruling 2026-11-16:
 * bind-once — workspaces do NOT switch harnesses). Re-binding the same
 * value on a bound workspace is the idempotent already-bound path; any
 * other value is a 409 harness_immutable. There is no switch or
 * recreate path.
 */
public class BindHarnessResult {
    @SerializedName("harness") public String harness;
    @SerializedName("alreadyBound") public boolean alreadyBound;
    @SerializedName("harnessOptions") public List<String> harnessOptions;
}
