package com.llmsafespaces.sdk;

import com.llmsafespaces.sdk.exceptions.NotFoundException;
import com.llmsafespaces.sdk.models.Workspace;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;

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
}
