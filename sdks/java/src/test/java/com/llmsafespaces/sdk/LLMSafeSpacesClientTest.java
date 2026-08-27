package com.llmsafespaces.sdk;

import com.llmsafespaces.sdk.exceptions.AuthException;
import com.llmsafespaces.sdk.exceptions.ConflictException;
import com.llmsafespaces.sdk.exceptions.LLMSafeSpacesException;
import com.llmsafespaces.sdk.exceptions.NotFoundException;
import com.llmsafespaces.sdk.exceptions.RateLimitException;
import com.llmsafespaces.sdk.models.Message;
import com.llmsafespaces.sdk.models.Workspace;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.util.concurrent.atomic.AtomicInteger;

import static org.junit.jupiter.api.Assertions.*;

class LLMSafeSpacesClientTest {

    private HttpServer startMockServer(int statusCode, String responseBody) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/", exchange -> {
            exchange.sendResponseHeaders(statusCode, responseBody.length());
            exchange.getResponseBody().write(responseBody.getBytes());
            exchange.close();
        });
        server.start();
        return server;
    }

    @Test
    void workspacesCreate_returnsTypedWorkspace() throws Exception {
        String json = """
            {"id":"ws-1","name":"test","userId":"u1","runtime":"base",
             "storageSize":"10Gi","phase":"Pending",
             "createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}""";
        var server = startMockServer(201, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var ws = client.workspaces.create("test", "base", "10Gi");
            assertEquals("ws-1", ws.id);
            assertEquals("test", ws.name);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void notFound_throwsNotFoundException() throws Exception {
        var server = startMockServer(404, "{\"error\":\"workspace not found\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            assertThrows(NotFoundException.class, () -> client.workspaces.get("nonexistent"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void sendMessage_extractsContent() throws Exception {
        // Contract-shaped response (pkg/session Message via the adapter seam).
        String json = """
            {"id":"msg-1","type":"assistant","parts":[
                {"type":"text","text":"Hello "},
                {"type":"text","text":"world!"}
            ]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.sessions.sendMessage("ws-1", "sess-1", "hi");
            assertEquals(Message.Type.ASSISTANT, result.type);
            assertEquals("Hello world!", result.textContent());
        } finally {
            server.stop(0);
        }
    }

    @Test
    void delete_returns204_noException() throws Exception {
        var server = startMockServer(204, "");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            assertDoesNotThrow(() -> client.workspaces.delete("ws-1"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void notFound_throwsNotFoundException_unchecked() throws Exception {
        var server = startMockServer(404, "{\"error\":\"not found\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            assertThrows(NotFoundException.class, () -> client.workspaces.get("x"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void conflict_throwsConflictException() throws Exception {
        var server = startMockServer(409, "{\"error\":\"duplicate slug\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            assertThrows(ConflictException.class, () -> client.workspaces.create("x", "base", "1Gi"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void exceptionsAreUnchecked_noThrowsNeeded() {
        assertDoesNotThrow(() -> {
            var client = LLMSafeSpacesClient.builder("http://localhost:1")
                    .apiKey("lsp_test").build();
            try {
                client.workspaces.get("x");
            } catch (LLMSafeSpacesException e) {
                // Expected — connection refused
            }
        });
    }

    @Test
    void sendMessage_extractsContentFromMultipleParts() throws Exception {
        String json = """
            {"id":"msg-1","type":"assistant","parts":[
                {"type":"text","text":"Hello "},
                {"type":"text","text":"world!"},
                {"type":"tool","tool":{"name":"read","state":{"status":"completed"}}}
            ]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.sessions.sendMessage("ws-1", "sess-1", "hi");
            assertEquals(Message.Type.ASSISTANT, result.type);
            assertEquals("Hello world!", result.textContent());
            assertNotNull(result.parts);
            assertEquals(3, result.parts.size());
        } finally {
            server.stop(0);
        }
    }

    @Test
    void secretsReveal_acceptsPasswordParameter() throws Exception {
        String json = "{\"value\":\"secret-val\"}";
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/secrets/sec-1/reveal", exchange -> {
            var body = new String(exchange.getRequestBody().readAllBytes());
            if (!body.contains("\"password\":\"mypw\"")) {
                exchange.sendResponseHeaders(403, 0);
                exchange.close();
                return;
            }
            exchange.sendResponseHeaders(200, json.length());
            exchange.getResponseBody().write(json.getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var val = client.secrets.reveal("sec-1", "mypw");
            assertEquals("secret-val", val);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void authError_throwsAuthException() throws Exception {
        var server = startMockServer(401, "{\"error\":\"authentication required\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_bad").build();
            assertThrows(AuthException.class, () -> client.workspaces.list());
        } finally {
            server.stop(0);
        }
    }

    @Test
    void rateLimit_throwsRateLimitException() throws Exception {
        var server = startMockServer(429, "{\"error\":\"rate limit exceeded\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            assertThrows(RateLimitException.class, () -> client.workspaces.get("ws-1"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void providerCredentialsCreate_207UnwrapsCredential() throws Exception {
        String json = """
            {"credential":{"id":"cred-1","name":"my-key","kind":"openai","slug":"my-key",
             "createdAt":"2026-07-22T00:00:00Z","updatedAt":"2026-07-22T00:00:00Z"},
             "bindWarning":"failed to auto-bind"}""";
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/provider-credentials", exchange -> {
            exchange.sendResponseHeaders(207, json.length());
            exchange.getResponseBody().write(json.getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var cred = client.providerCredentials.create("my-key", "openai", "my-key", "sk-test", "");
            assertEquals("cred-1", cred.id);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void listBindings_extractsWorkspaceIds() throws Exception {
        String json = "{\"workspaceIds\":[\"ws-1\",\"ws-2\"],\"bindings\":[]}";
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/provider-credentials/cred-1/bindings", exchange -> {
            exchange.sendResponseHeaders(200, json.length());
            exchange.getResponseBody().write(json.getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var bindings = client.providerCredentials.listBindings("cred-1");
            assertEquals(2, bindings.size());
            assertTrue(bindings.contains("ws-1"));
            assertTrue(bindings.contains("ws-2"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void secretsList_unwrapsEnvelope() throws Exception {
        String json = "{\"secrets\":[{\"id\":\"sec-1\",\"name\":\"my-secret\",\"type\":\"env-secret\"}]}";
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/secrets", exchange -> {
            exchange.sendResponseHeaders(200, json.length());
            exchange.getResponseBody().write(json.getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var secrets = client.secrets.list();
            assertEquals(1, secrets.size());
        } finally {
            server.stop(0);
        }
    }

    @Test
    void auth401_retriesOnceWithRelogin() throws Exception {
        AtomicInteger loginCount = new AtomicInteger(0);
        AtomicInteger requestCount = new AtomicInteger(0);
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/auth/login", exchange -> {
            loginCount.incrementAndGet();
            String resp = "{\"token\":\"jwt" + loginCount.get() + "\"}";
            exchange.sendResponseHeaders(200, resp.length());
            exchange.getResponseBody().write(resp.getBytes());
            exchange.close();
        });
        server.createContext("/", exchange -> {
            requestCount.incrementAndGet();
            exchange.sendResponseHeaders(401, 35);
            exchange.getResponseBody().write("{\"error\":\"authentication required\"}".getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .credentials("u@x.com", "pw").build();
            assertThrows(AuthException.class, () -> client.workspaces.get("ws-1"));
            // loginCount: 2 (initial + 1 retry), requestCount: 2 (initial + 1 retry)
            assertEquals(2, loginCount.get(), "login should be called exactly twice");
            assertEquals(2, requestCount.get(), "request should be called exactly twice");
        } finally {
            server.stop(0);
        }
    }

    // ─── Epic 64: Workflows + Triggers ───────────────────────────────────────

    @Test
    void workflowsList_returnsList() throws Exception {
        String json = """
            {"workflows":[{"id":"wf-1","name":"test","status":"active"}]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.workflows.list();
            assertEquals(1, result.size());
            assertEquals("wf-1", result.get(0).get("id"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void workflowsCreate_returnsWorkflow() throws Exception {
        String json = """
            {"id":"wf-new","name":"test-wf","status":"draft"}""";
        var server = startMockServer(201, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.workflows.create("test-wf", "{}", "draft");
            assertEquals("wf-new", result.get("id"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void workflowsRun_returnsRun() throws Exception {
        String json = """
            {"id":"run-1","workflowId":"wf-1","status":"queued"}""";
        var server = startMockServer(202, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.workflows.run("wf-1", null, null);
            assertEquals("run-1", result.get("id"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void triggersList_returnsList() throws Exception {
        String json = """
            {"triggers":[{"id":"trig-1","name":"cron","enabled":true}]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.triggers.list();
            assertEquals(1, result.size());
            assertEquals("trig-1", result.get(0).get("id"));
        } finally {
            server.stop(0);
        }
    }

    @org.junit.jupiter.api.Test
    void getHistoryPage_paramsAndCursor() throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        final String[] gotQuery = {null};
        server.createContext("/api/v1/workspaces/ws-1/sessions/sess-1/message", exchange -> {
            gotQuery[0] = exchange.getRequestURI().getRawQuery();
            byte[] body = "[{\"id\":\"m1\",\"type\":\"user\",\"text\":\"hi\"}]".getBytes();
            exchange.getResponseHeaders().set("X-Next-Cursor", "msg_42");
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, body.length);
            exchange.getResponseBody().write(body);
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var page = client.sessions.getHistoryPage("ws-1", "sess-1", 50, "msg_99");
            org.junit.jupiter.api.Assertions.assertEquals("limit=50&before=msg_99", gotQuery[0]);
            org.junit.jupiter.api.Assertions.assertEquals("msg_42", page.nextCursor);
            org.junit.jupiter.api.Assertions.assertEquals(1, page.messages.size());
            org.junit.jupiter.api.Assertions.assertEquals("m1", page.messages.get(0).id);
        } finally {
            server.stop(0);
        }
    }

    @org.junit.jupiter.api.Test
    void getHistoryPage_emptyCursorWhenHeaderAbsent() throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/workspaces/ws-1/sessions/sess-1/message", exchange -> {
            byte[] body = "[]".getBytes();
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, body.length);
            exchange.getResponseBody().write(body);
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var page = client.sessions.getHistoryPage("ws-1", "sess-1", 0, null);
            org.junit.jupiter.api.Assertions.assertEquals("", page.nextCursor);
            org.junit.jupiter.api.Assertions.assertTrue(page.messages.isEmpty());
        } finally {
            server.stop(0);
        }
    }

    @org.junit.jupiter.api.Test
    void mcpServers_userScopeCrud() throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        final String[] gotPath = {null};
        final String[] gotMethod = {null};
        server.createContext("/api/v1/", exchange -> {
            gotPath[0] = exchange.getRequestURI().getPath();
            gotMethod[0] = exchange.getRequestMethod();
            byte[] body;
            int status = 200;
            if ("POST".equals(gotMethod[0]) && gotPath[0].equals("/api/v1/me/mcp-servers")) {
                body = "{\"id\":\"srv-1\",\"name\":\"n\",\"transport\":\"http\"}".getBytes();
                status = 201;
            } else if ("GET".equals(gotMethod[0]) && gotPath[0].equals("/api/v1/me/mcp-servers")) {
                body = "{\"servers\":[{\"id\":\"srv-1\",\"name\":\"n\",\"transport\":\"stdio\"}]}".getBytes();
            } else if ("PUT".equals(gotMethod[0]) && gotPath[0].equals("/api/v1/me/mcp-servers/srv-1")) {
                body = "{\"id\":\"srv-1\",\"name\":\"renamed\",\"transport\":\"http\"}".getBytes();
            } else if ("DELETE".equals(gotMethod[0])) {
                body = "{\"deleted\":true}".getBytes();
            } else {
                body = "{\"id\":\"srv-1\",\"name\":\"n\"}".getBytes();
            }
            if (body.length == 0) {
                exchange.sendResponseHeaders(status, -1);
            } else {
                exchange.sendResponseHeaders(status, body.length);
                exchange.getResponseBody().write(body);
            }
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var created = client.mcpServers.create(java.util.Map.of("name", "n", "transport", "http"));
            org.junit.jupiter.api.Assertions.assertEquals("srv-1", created.get("id"));
            var listed = client.mcpServers.list();
            org.junit.jupiter.api.Assertions.assertEquals(1, listed.size());
            var updated = client.mcpServers.update("srv-1", java.util.Map.of("name", "renamed"));
            org.junit.jupiter.api.Assertions.assertEquals("renamed", updated.get("name"));
            client.mcpServers.delete("srv-1");
            org.junit.jupiter.api.Assertions.assertEquals("/api/v1/me/mcp-servers/srv-1", gotPath[0]);
            org.junit.jupiter.api.Assertions.assertEquals("DELETE", gotMethod[0]);
        } finally {
            server.stop(0);
        }
    }

    @org.junit.jupiter.api.Test
    void mcpServers_bindAutoApplyAndScopes() throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        final java.util.List<String> paths = new java.util.ArrayList<>();
        server.createContext("/api/v1/", exchange -> {
            paths.add(exchange.getRequestMethod() + " " + exchange.getRequestURI().getPath());
            byte[] body = exchange.getRequestURI().getPath().endsWith("auto-apply")
                    && "GET".equals(exchange.getRequestMethod())
                    ? "{\"rules\":[{\"targetType\":\"all\"}]}".getBytes()
                    : "{}".getBytes();
            exchange.sendResponseHeaders(200, body.length);
            exchange.getResponseBody().write(body);
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            client.mcpServers.bind("srv-1", "ws-1");
            client.mcpServers.createAutoApply("srv-1", "user", "u-1");
            var rules = client.mcpServers.listAutoApply("srv-1");
            org.junit.jupiter.api.Assertions.assertEquals(1, rules.size());
            client.adminMcpServers.deleteAutoApply("srv-1", "user", null);
            client.orgMcpServers.list("org-9");
            org.junit.jupiter.api.Assertions.assertTrue(
                    paths.contains("POST /api/v1/me/mcp-servers/srv-1/bindings"));
            org.junit.jupiter.api.Assertions.assertTrue(
                    paths.contains("POST /api/v1/me/mcp-servers/srv-1/auto-apply"));
            org.junit.jupiter.api.Assertions.assertTrue(
                    paths.contains("DELETE /api/v1/admin/mcp-servers/srv-1/auto-apply/user"));
            org.junit.jupiter.api.Assertions.assertTrue(
                    paths.contains("GET /api/v1/orgs/org-9/mcp-servers"));
        } finally {
            server.stop(0);
        }
    }

    @org.junit.jupiter.api.Test
    void mcpServers_unhappyPaths() throws Exception {
        var server = startMockServer(404, "{\"error\":\"mcp server not found\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            org.junit.jupiter.api.Assertions.assertThrows(
                    NotFoundException.class, () -> client.mcpServers.get("missing"));
        } finally {
            server.stop(0);
        }
    }

    @org.junit.jupiter.api.Test
    void mcpServers_adminDeleteAutoApplyVariants() throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        final java.util.List<String> paths = new java.util.ArrayList<>();
        server.createContext("/api/v1/", exchange -> {
            paths.add(exchange.getRequestURI().getPath());
            exchange.sendResponseHeaders(204, -1);
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            client.adminMcpServers.deleteAutoApply("srv-1", "all", null);
            client.adminMcpServers.deleteAutoApply("srv-1", "user", "u-1");
            org.junit.jupiter.api.Assertions.assertEquals(
                    "/api/v1/admin/mcp-servers/srv-1/auto-apply/all", paths.get(0));
            org.junit.jupiter.api.Assertions.assertEquals(
                    "/api/v1/admin/mcp-servers/srv-1/auto-apply/user/u-1", paths.get(1));
        } finally {
            server.stop(0);
        }
    }

    // ── Epic 68: workspace upload + files-on-send (wire-level) ──────────────

    private static final String UPLOAD_PATH =
            "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt";

    @Test
    void workspacesUpload_postsMultipartAndReturnsTypedFileUpload() throws Exception {
        var captured = new java.util.concurrent.ConcurrentHashMap<String, String>();
        var capturedBytes = new java.util.concurrent.atomic.AtomicReference<byte[]>();
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/workspaces/ws-1/uploads", exchange -> {
            captured.put("method", exchange.getRequestMethod());
            captured.put("contentType", exchange.getRequestHeaders().getFirst("Content-Type"));
            capturedBytes.set(exchange.getRequestBody().readAllBytes());
            String json = "{\"path\":\"" + UPLOAD_PATH + "\",\"name\":\"notes.txt\",\"size\":5}";
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(201, json.length());
            exchange.getResponseBody().write(json.getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var up = client.workspaces.upload("ws-1", "notes.txt", "hello".getBytes());
            assertEquals("POST", captured.get("method"));
            assertTrue(captured.get("contentType").startsWith("multipart/form-data; boundary="));
            var body = new String(capturedBytes.get(), java.nio.charset.StandardCharsets.UTF_8);
            assertTrue(body.contains("name=\"file\"; filename=\"notes.txt\""));
            assertTrue(body.contains("hello"));
            assertEquals(UPLOAD_PATH, up.path);
            assertEquals("notes.txt", up.name);
            assertEquals(5, up.size);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void workspacesUpload_surfacesPhaseOn409() throws Exception {
        HttpServer server = startMockServer(409,
                "{\"error\":\"workspace not active\",\"phase\":\"Suspended\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var ex = assertThrows(com.llmsafespaces.sdk.exceptions.ConflictException.class,
                    () -> client.workspaces.upload("ws-1", "f.txt", "x".getBytes()));
            assertEquals("Suspended", ex.phase);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void sendPromptAsync_sendsPartsAndFiles_noDeadFields() throws Exception {
        var captured = new java.util.concurrent.atomic.AtomicReference<String>();
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/workspaces/ws-1/sessions/ses_1/prompt", exchange -> {
            captured.set(new String(exchange.getRequestBody().readAllBytes(),
                    java.nio.charset.StandardCharsets.UTF_8));
            exchange.sendResponseHeaders(202, -1);
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            client.sessions.sendPromptAsync("ws-1", "ses_1", "review please", java.util.List.of(UPLOAD_PATH));
            var json = com.google.gson.JsonParser.parseString(captured.get()).getAsJsonObject();
            var part = json.getAsJsonArray("parts").get(0).getAsJsonObject();
            assertEquals("text", part.get("type").getAsString());
            assertEquals("review please", part.get("text").getAsString());
            assertEquals(UPLOAD_PATH, json.getAsJsonArray("files").get(0).getAsString());
            assertFalse(json.has("message"));
            assertFalse(json.has("content"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void enqueue_carriesFiles() throws Exception {
        var captured = new java.util.concurrent.atomic.AtomicReference<String>();
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/workspaces/ws-1/sessions/ses_1/queue", exchange -> {
            captured.set(new String(exchange.getRequestBody().readAllBytes(),
                    java.nio.charset.StandardCharsets.UTF_8));
            String json = "{\"messageID\":\"qm-1\"}";
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(202, json.length());
            exchange.getResponseBody().write(json.getBytes());
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            String mid = client.sessions.enqueue("ws-1", "ses_1", "later", java.util.List.of(UPLOAD_PATH));
            assertEquals("qm-1", mid);
            var json = com.google.gson.JsonParser.parseString(captured.get()).getAsJsonObject();
            assertEquals("later", json.get("text").getAsString());
            assertEquals(UPLOAD_PATH, json.getAsJsonArray("files").get(0).getAsString());
        } finally {
            server.stop(0);
        }
    }

    @Test
    void workspacesUpload_neutralizesHostileFilenameInMultipartFraming() throws Exception {
        var capturedBytes = new java.util.concurrent.atomic.AtomicReference<byte[]>();
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/workspaces/ws-1/uploads", exchange -> {
            capturedBytes.set(exchange.getRequestBody().readAllBytes());
            exchange.sendResponseHeaders(201, -1);
            exchange.close();
        });
        server.start();
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            client.workspaces.upload("ws-1", "we\"ird\r\ninjected.txt", "x".getBytes());
            var body = new String(capturedBytes.get(), java.nio.charset.StandardCharsets.UTF_8);
            assertFalse(body.contains("\"ird"));
            assertFalse(body.contains("\r\ninjected"));
            assertTrue(body.contains("filename=\"we_ird__injected.txt\""));
        } finally {
            server.stop(0);
        }
    }
}
