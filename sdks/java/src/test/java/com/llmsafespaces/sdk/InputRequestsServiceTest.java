package com.llmsafespaces.sdk;

import com.llmsafespaces.sdk.exceptions.ConflictException;
import com.llmsafespaces.sdk.models.InboxLateAnswerAccepted;
import com.llmsafespaces.sdk.models.InputRequest;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.util.List;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.*;

/** Contract rows for the input-requests surface (#1302 / 4b). */
class InputRequestsServiceTest {

    static final String QUESTION_ROW = """
        [{"id":"que_1","sessionId":"ses_1","rootSessionId":"ses_root","kind":"question",
          "question":"What language?","header":"Choose language",
          "options":[{"label":"Go","description":"Fast"},{"label":"Python"}],
          "multiple":false,"custom":true,
          "tool":{"messageId":"msg_abc","callId":"call_xyz"}}]""";

    static final String PERMISSION_ROW = """
        [{"id":"per_1","kind":"permission","permission":"bash",
          "patterns":["/workspace/src/main.go"],"always":["/workspace/*"],
          "metadata":{"command":"go build"}}]""";

    static final String LATE_ANSWER_BODY =
        "{\"status\":\"queued\",\"clientMessageID\":\"inbox-que_1-answer\","
            + "\"messageID\":\"msg_out_1\",\"duplicate\":true}";

    /** Serves one canned response and records the last method+path+body. */
    static final class Recorder {
        final AtomicReference<String> method = new AtomicReference<>();
        final AtomicReference<String> path = new AtomicReference<>();
        final AtomicReference<String> body = new AtomicReference<>("");
    }

    private record Mock(HttpServer server, Recorder rec) {}

    private Mock startMock(int statusCode, String responseBody) throws IOException {
        Recorder rec = new Recorder();
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/", exchange -> {
            rec.method.set(exchange.getRequestMethod());
            rec.path.set(exchange.getRequestURI().getPath());
            byte[] bodyBytes = exchange.getRequestBody().readAllBytes();
            rec.body.set(new String(bodyBytes));
            byte[] out = responseBody.getBytes();
            exchange.sendResponseHeaders(statusCode, out.length == 0 ? -1 : out.length);
            if (out.length > 0) {
                exchange.getResponseBody().write(out);
            }
            exchange.close();
        });
        server.start();
        return new Mock(server, rec);
    }

    private LLMSafeSpacesClient client(HttpServer server) {
        return LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                .apiKey("lsp_test").build();
    }

    @Test
    void listQuestionsReturnsTypedInputRequests() throws Exception {
        var mock = startMock(200, QUESTION_ROW);
        try {
            var c = client(mock.server());
            List<InputRequest> rows = c.inputRequests.listQuestions("ws-1");
            assertEquals(1, rows.size());
            InputRequest q = rows.get(0);
            assertEquals("que_1", q.id);
            assertEquals("ses_root", q.rootSessionId);
            assertEquals("question", q.kind);
            assertEquals("What language?", q.question);
            assertTrue(q.custom);
            assertFalse(q.multiple);
            assertEquals(2, q.options.size());
            assertEquals("Go", q.options.get(0).label);
            assertEquals("Fast", q.options.get(0).description);
            assertEquals("call_xyz", q.tool.callId);
            assertEquals("GET", mock.rec().method.get());
            assertEquals("/api/v1/workspaces/ws-1/question", mock.rec().path.get());
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void listPermissionsReturnsTypedInputRequests() throws Exception {
        var mock = startMock(200, PERMISSION_ROW);
        try {
            var c = client(mock.server());
            List<InputRequest> rows = c.inputRequests.listPermissions("ws-1");
            assertEquals(1, rows.size());
            InputRequest p = rows.get(0);
            assertEquals("permission", p.kind);
            assertEquals("bash", p.permission);
            assertEquals(List.of("/workspace/src/main.go"), p.patterns);
            assertEquals(List.of("/workspace/*"), p.always);
            assertEquals("go build", p.metadata.get("command").getAsString());
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void replyQuestionLiveAnswerReturnsNull() throws Exception {
        var mock = startMock(200, "");
        try {
            var c = client(mock.server());
            InboxLateAnswerAccepted late =
                    c.inputRequests.replyQuestion("ws-1", "que_1", List.of(List.of("Go")));
            assertNull(late);
            assertEquals("POST", mock.rec().method.get());
            assertEquals("/api/v1/workspaces/ws-1/question/que_1/reply", mock.rec().path.get());
            assertEquals("{\"answers\":[[\"Go\"]]}", mock.rec().body.get());
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void replyQuestionLateAnswerReturns202Body() throws Exception {
        var mock = startMock(202, LATE_ANSWER_BODY);
        try {
            var c = client(mock.server());
            InboxLateAnswerAccepted late =
                    c.inputRequests.replyQuestion("ws-1", "que_1", List.of(List.of("Go")));
            assertNotNull(late);
            assertEquals("queued", late.status);
            assertEquals("inbox-que_1-answer", late.clientMessageID);
            assertEquals("msg_out_1", late.messageID);
            assertTrue(late.duplicate);
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void replyPermissionSendsVocabularyAndOptionalMessage() throws Exception {
        var mock = startMock(200, "");
        try {
            var c = client(mock.server());
            InboxLateAnswerAccepted late =
                    c.inputRequests.replyPermission("ws-1", "per_1", "always", "context note");
            assertNull(late);
            assertEquals("/api/v1/workspaces/ws-1/permission/per_1/reply", mock.rec().path.get());
            String body = mock.rec().body.get();
            assertTrue(body.contains("\"reply\":\"always\""));
            assertTrue(body.contains("\"message\":\"context note\""));
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void replyConflictMapsToConflictException() throws Exception {
        var mock = startMock(409, "{\"error\":\"record dismissed\"}");
        try {
            var c = client(mock.server());
            assertThrows(ConflictException.class,
                    () -> c.inputRequests.replyQuestion("ws-1", "que_1", List.of(List.of("Go"))));
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void rejectQuestionPostsTheRejectRoute() throws Exception {
        var mock = startMock(200, "");
        try {
            var c = client(mock.server());
            c.inputRequests.rejectQuestion("ws-1", "que_1");
            assertEquals("POST", mock.rec().method.get());
            assertEquals("/api/v1/workspaces/ws-1/question/que_1/reject", mock.rec().path.get());
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void dismissInboxRecordDeletes() throws Exception {
        var mock = startMock(204, "");
        try {
            var c = client(mock.server());
            c.inputRequests.dismissInboxRecord("ws-1", "ses_1", "que_1");
            assertEquals("DELETE", mock.rec().method.get());
            assertEquals("/api/v1/workspaces/ws-1/sessions/ses_1/inbox/que_1", mock.rec().path.get());
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void requestInputSnapshotPosts() throws Exception {
        var mock = startMock(202, "");
        try {
            var c = client(mock.server());
            c.inputRequests.requestInputSnapshot("ws-1");
            assertEquals("POST", mock.rec().method.get());
            assertEquals("/api/v1/workspaces/ws-1/input-snapshot", mock.rec().path.get());
        } finally {
            mock.server().stop(0);
        }
    }
}
