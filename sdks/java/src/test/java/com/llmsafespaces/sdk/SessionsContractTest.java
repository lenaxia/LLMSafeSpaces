package com.llmsafespaces.sdk;

import com.llmsafespaces.sdk.models.PromptAccepted;
import com.llmsafespaces.sdk.models.Session;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.jupiter.api.Assertions.*;

/** Contract rows for the session surface (#1304 / 4b-c2). */
class SessionsContractTest {

    static final String SESSION_ROW = """
        {"id":"ses_7f9d","workspaceId":"ws-1","parentId":"ses_root",
         "title":"Refactor the adapter","agentId":"plan",
         "model":{"id":"claude-sonnet-4.5","provider":"anthropic"},
         "status":"busy",
         "cost":{"inputTokens":120,"outputTokens":80,"totalTokens":200},
         "contextUsage":{"used":45000,"window":200000},
         "time":{"startedAt":"2026-09-15T10:00:00Z"},
         "summary":"Wire the seam"}""";

    static final String PROMPT_QUEUED_BODY =
        "{\"messageID\":\"msg_9\",\"clientMessageID\":\"cmid-1\",\"status\":\"queued\"}";

    static final String PROMPT_DUPLICATE_BODY =
        "{\"messageID\":\"msg_orig\",\"clientMessageID\":\"cmid-1\",\"status\":\"duplicate\"}";

    private record Mock(HttpServer server, AtomicReference<String> method, AtomicReference<String> path) {}

    private Mock startMock(int statusCode, String responseBody) throws IOException {
        var method = new AtomicReference<String>();
        var path = new AtomicReference<String>();
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/", exchange -> {
            method.set(exchange.getRequestMethod());
            path.set(exchange.getRequestURI().getPath());
            exchange.getRequestBody().readAllBytes();
            byte[] out = responseBody.getBytes();
            exchange.sendResponseHeaders(statusCode, out.length == 0 ? -1 : out.length);
            if (out.length > 0) {
                exchange.getResponseBody().write(out);
            }
            exchange.close();
        });
        server.start();
        return new Mock(server, method, path);
    }

    private LLMSafeSpacesClient client(HttpServer server) {
        return LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                .apiKey("lsp_test").build();
    }

    @Test
    void getSessionReturnsTypedContractSession() throws Exception {
        var mock = startMock(200, SESSION_ROW);
        try {
            var c = client(mock.server());
            Session s = c.sessions.get("ws-1", "ses_7f9d");
            assertEquals("ses_7f9d", s.id);
            assertEquals("ws-1", s.workspaceId);
            assertEquals("ses_root", s.parentId);
            assertEquals("Refactor the adapter", s.title);
            assertEquals("busy", s.status);
            assertEquals("claude-sonnet-4.5", s.model.id);
            assertEquals("anthropic", s.model.provider);
            assertEquals(200L, s.cost.totalTokens);
            assertEquals(45000L, s.contextUsage.used);
            assertEquals("GET", mock.method().get());
            assertEquals("/api/v1/workspaces/ws-1/sessions/ses_7f9d", mock.path().get());
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void sendPromptAsyncReturnsAcceptedReceipt() throws Exception {
        var mock = startMock(202, PROMPT_QUEUED_BODY);
        try {
            var c = client(mock.server());
            PromptAccepted rcpt = c.sessions.sendPromptAsync("ws-1", "ses_1", "hello");
            assertEquals("msg_9", rcpt.messageID);
            assertEquals("cmid-1", rcpt.clientMessageID);
            assertEquals("queued", rcpt.status);
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void sendPromptAsyncDuplicateReturnsOriginalReceipt() throws Exception {
        var mock = startMock(200, PROMPT_DUPLICATE_BODY);
        try {
            var c = client(mock.server());
            PromptAccepted rcpt = c.sessions.sendPromptAsync("ws-1", "ses_1", "hello");
            assertEquals("msg_orig", rcpt.messageID);
            assertEquals("duplicate", rcpt.status);
        } finally {
            mock.server().stop(0);
        }
    }

    @Test
    void abortAndDeleteAreNoContent() throws Exception {
        var mock = startMock(204, "");
        try {
            var c = client(mock.server());
            c.sessions.abort("ws-1", "ses_1");
            assertEquals("POST", mock.method().get());
            c.sessions.delete("ws-1", "ses_1");
            assertEquals("DELETE", mock.method().get());
        } finally {
            mock.server().stop(0);
        }
    }
}
