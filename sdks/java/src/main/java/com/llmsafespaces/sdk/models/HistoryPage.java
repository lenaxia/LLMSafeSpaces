package com.llmsafespaces.sdk.models;

import java.util.List;

/** One page of session history plus the cursor for the next (older) page.
 * nextCursor is empty when the beginning of the session was reached. */
public class HistoryPage {
    public List<Message> messages;
    public String nextCursor;

    public HistoryPage(List<Message> messages, String nextCursor) {
        this.messages = messages;
        this.nextCursor = nextCursor == null ? "" : nextCursor;
    }
}
