package com.llmsafespaces.sdk.services;

import com.llmsafespaces.sdk.LLMSafeSpacesClient;
import com.llmsafespaces.sdk.models.InboxLateAnswerAccepted;
import com.llmsafespaces.sdk.models.InputRequest;

import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Agent question/permission requests — the whole surface speaks the
 * platform contract shape (InputRequest, #1302): typed list returns, the
 * late-answer 202 body on replies, and the inbox dismiss exit (#1313).
 */
public class InputRequestsService {
    private final LLMSafeSpacesClient c;

    public InputRequestsService(LLMSafeSpacesClient c) { this.c = c; }

    /** Pending questions as contract InputRequest values (kind=question). */
    public List<InputRequest> listQuestions(String workspaceId) {
        InputRequest[] rows = c.request("GET", "/workspaces/" + workspaceId + "/question",
                null, InputRequest[].class);
        return rows == null ? List.of() : List.of(rows);
    }

    /**
     * Answers a live question. When the ask is no longer live but its
     * unanswered-question inbox record is pending (#1313), the answer is
     * accepted as a late answer through the delivery outbox (202) and
     * the outbox entry is returned; a live answer (200) returns null.
     */
    public InboxLateAnswerAccepted replyQuestion(String workspaceId, String requestId, List<List<String>> answers) {
        return lateAnswerOnly(c.request("POST",
                "/workspaces/" + workspaceId + "/question/" + requestId + "/reply",
                questionReplyBody(answers), InboxLateAnswerAccepted.class));
    }

    /** Rejects (dismisses) a pending question. */
    public void rejectQuestion(String workspaceId, String requestId) {
        c.requestVoid("POST",
                "/workspaces/" + workspaceId + "/question/" + requestId + "/reject", null);
    }

    /** Pending permissions as contract InputRequest values (kind=permission). */
    public List<InputRequest> listPermissions(String workspaceId) {
        InputRequest[] rows = c.request("GET", "/workspaces/" + workspaceId + "/permission",
                null, InputRequest[].class);
        return rows == null ? List.of() : List.of(rows);
    }

    /**
     * Answers a live permission with the reply vocabulary
     * ("once" | "always" | "reject") and an optional message. A late
     * decision (ask no longer live, inbox record pending) returns the
     * 202 outbox entry; a live answer (200) returns null.
     */
    public InboxLateAnswerAccepted replyPermission(String workspaceId, String requestId, String reply, String message) {
        return lateAnswerOnly(c.request("POST",
                "/workspaces/" + workspaceId + "/permission/" + requestId + "/reply",
                permissionReplyBody(reply, message), InboxLateAnswerAccepted.class));
    }

    /**
     * Dismisses an unanswered-question inbox record (#1313): the record
     * becomes dismissed; a still-live ask is rejected first server-side.
     */
    public void dismissInboxRecord(String workspaceId, String sessionId, String requestId) {
        c.requestVoid("DELETE",
                "/workspaces/" + workspaceId + "/sessions/" + sessionId + "/inbox/" + requestId, null);
    }

    /** Triggers an input-snapshot flight (202; events arrive on the event streams). */
    public void requestInputSnapshot(String workspaceId) {
        c.requestVoid("POST", "/workspaces/" + workspaceId + "/input-snapshot", null);
    }

    static Map<String, Object> questionReplyBody(List<List<String>> answers) {
        Map<String, Object> body = new HashMap<>();
        body.put("answers", answers);
        return body;
    }

    static Map<String, Object> permissionReplyBody(String reply, String message) {
        Map<String, Object> body = new HashMap<>();
        body.put("reply", reply);
        if (message != null && !message.isEmpty()) {
            body.put("message", message);
        }
        return body;
    }

    /**
     * Live-vs-late classification for input replies: only the outbox's
     * accepted-entry body (status "queued") is a late answer. A live
     * answer is bodyless per the published contract — and if a server
     * ever answers 200 with a body anyway, it must still classify as
     * live (r1 review).
     */
    private static InboxLateAnswerAccepted lateAnswerOnly(InboxLateAnswerAccepted late) {
        return late != null && "queued".equals(late.status) ? late : null;
    }
}
