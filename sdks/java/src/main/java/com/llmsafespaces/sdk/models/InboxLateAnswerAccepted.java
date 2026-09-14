package com.llmsafespaces.sdk.models;

import com.google.gson.annotations.SerializedName;

/**
 * The 202 body for a reply that landed as a late answer through the
 * delivery outbox (#1313): the ask was no longer live, so the answer
 * rides a Q&A user message instead of the live ask.
 */
public class InboxLateAnswerAccepted {
    public String status;
    /** Ask-scoped dedupe key ({@code inbox-{requestID}-answer}); duplicate clicks return the original. */
    @SerializedName("clientMessageID")
    public String clientMessageID;
    /** The outbox entry ID (202) or the original accepted entry (duplicate). */
    @SerializedName("messageID")
    public String messageID;
    /** Present and true when the ask-scoped key was already accepted. */
    public boolean duplicate;
}
