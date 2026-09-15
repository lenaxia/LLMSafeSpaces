package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

/**
 * The delivery-outbox receipt for an async prompt: the 202 body of a
 * fresh accept, and the 200 body of a retried clientMessageID (which
 * echoes the ORIGINAL accepted entry with status "duplicate").
 */
public class PromptAccepted {
    /** The outbox entry ID (202) or the originally accepted entry (duplicate retry). */
    @SerializedName("messageID")
    public String messageID;
    /** The caller-chosen idempotency key, echoed back. */
    @SerializedName("clientMessageID")
    public String clientMessageID;
    /** queued | duplicate */
    public String status;
}
