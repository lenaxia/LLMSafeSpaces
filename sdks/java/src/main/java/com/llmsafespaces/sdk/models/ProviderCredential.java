package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;
import java.util.List;
import java.util.Map;

public class ProviderCredential {
    @SerializedName("id") public String id;
    @SerializedName("name") public String name;
    @SerializedName("kind") public String kind;
    @SerializedName("slug") public String slug;
    @SerializedName("baseURL") public String baseURL;
    @SerializedName("modelAllowlist") public List<String> modelAllowlist;
    @SerializedName("modelContextLimits") public Map<String, Integer> modelContextLimits;
    @SerializedName("modelOutputLimits") public Map<String, Integer> modelOutputLimits;
    @SerializedName("createdAt") public String createdAt;
    @SerializedName("updatedAt") public String updatedAt;
}
