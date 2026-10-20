package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

public class Workspace {
    @SerializedName("id") public String id;
    @SerializedName("name") public String name;
    @SerializedName("userId") public String userId;
    @SerializedName("runtime") public String runtime;
    @SerializedName("storageSize") public String storageSize;
    @SerializedName("phase") public String phase;
    @SerializedName("createdAt") public String createdAt;
    @SerializedName("updatedAt") public String updatedAt;
}
