package com.llmsafespaces.sdk;

import com.llmsafespaces.sdk.exceptions.AuthException;
import com.llmsafespaces.sdk.exceptions.ConflictException;
import com.llmsafespaces.sdk.exceptions.LLMSafeSpacesException;
import com.llmsafespaces.sdk.exceptions.NotFoundException;
import com.llmsafespaces.sdk.exceptions.RateLimitException;
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
        String json = """
            {"id":"msg-1","role":"assistant","parts":[
                {"type":"text","text":"Hello "},
                {"type":"text","text":"world!"}
            ]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.sessions.sendMessage("ws-1", "sess-1", "hi");
            assertEquals("Hello world!", result.getContent());
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
            {"id":"msg-1","role":"assistant","parts":[
                {"type":"text","text":"Hello "},
                {"type":"text","text":"world!"},
                {"type":"tool-invocation","toolName":"read"}
            ]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var result = client.sessions.sendMessage("ws-1", "sess-1", "hi");
            assertEquals("Hello world!", result.getContent());
            assertNotNull(result.getRaw());
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
}
