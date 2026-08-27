package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

/**
 * Result of a workspace file upload (Epic 67): the absolute path of the
 * stored file on the workspace PVC (/workspace/uploads/&lt;uuid&gt;-&lt;name&gt;),
 * its sanitized name, and the stored byte count.
 */
public class FileUpload {
    @SerializedName("path") public String path;
    @SerializedName("name") public String name;
    @SerializedName("size") public long size;
}
