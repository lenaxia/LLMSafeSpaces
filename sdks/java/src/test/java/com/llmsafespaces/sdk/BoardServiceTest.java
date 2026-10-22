// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
package com.llmsafespaces.sdk;

import com.llmsafespaces.sdk.exceptions.NotFoundException;
import com.sun.net.httpserver.HttpServer;
import org.junit.jupiter.api.Test;

import java.io.IOException;
import java.net.InetSocketAddress;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

/**
 * Wire-level pins for the board service (#1699, design 0067 §4.3a):
 * paths + row encoding (snake_case short_id — the engine's canonical
 * JSON), the OR4 pr:N anchor tag form, and the dependency envelope.
 */
class BoardServiceTest {

    private final java.util.concurrent.atomic.AtomicReference<String> lastPath =
            new java.util.concurrent.atomic.AtomicReference<>();

    private HttpServer startMockServer(int statusCode, String responseBody) throws IOException {
        HttpServer server = HttpServer.create(new InetSocketAddress(0), 0);
        server.createContext("/api/v1/", exchange -> {
            lastPath.set(exchange.getRequestURI().toString());
            exchange.sendResponseHeaders(statusCode, responseBody.length());
            exchange.getResponseBody().write(responseBody.getBytes());
            exchange.close();
        });
        server.start();
        return server;
    }

    @Test
    void boardProjectRoundTrip() throws Exception {
        String json = """
            {"id":"p1","short_id":"prj-aaaa1111","user_id":"u1","name":"B",
             "status":"active","worker_slots":2,"tags":["t"],
             "created_at":"2026-10-17T00:00:00Z","updated_at":"2026-10-17T00:00:00Z"}""";
        var server = startMockServer(201, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var proj = client.board.createProject(Map.of("name", "B", "targetWorkspaceId", "ws-1"));
            assertEquals("prj-aaaa1111", proj.get("short_id"));
            assertEquals("active", proj.get("status"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardListProjectTasksFailedFilter() throws Exception {
        String json = """
            {"tasks":[{"id":"t1","last_attempt_failed":true,"failure_count":2}]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            List<Map<String, Object>> tasks = client.board.listProjectTasks("prj-aaaa1111", null, Boolean.TRUE);
            assertEquals(1, tasks.size());
            assertEquals(Boolean.TRUE, tasks.get(0).get("last_attempt_failed"));
            assertEquals(2.0, tasks.get(0).get("failure_count"));
            String path = lastPath.get();
            assertTrue(path != null && path.contains("failed=true"), "the failed filter rides the query: " + path);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardListProjects() throws Exception {
        String json = """
            {"projects":[{"id":"p1","short_id":"prj-aaaa1111","status":"active"}]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            List<Map<String, Object>> list = client.board.listProjects();
            assertEquals(1, list.size());
            assertEquals("prj-aaaa1111", list.get(0).get("short_id"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardEpicLifecycle() throws Exception {
        String json = """
            {"id":"e1","short_id":"epc-bbbb2222","status":"active",
             "progress_derived":true,"progress_done":1,"progress_total":2}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var epic = client.board.epicActivate("epc-bbbb2222", false);
            assertEquals("active", epic.get("status"));
            assertEquals(Boolean.TRUE, epic.get("progress_derived"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardSeamsReads() throws Exception {
        String json = """
            {"epics":[{"id":"e1","short_id":"epc-bbbb2222","status":"active",
             "progress_derived":true,"progress_done":1,"progress_total":2}]}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var epics = client.board.listBoardEpics("prj-aaaa1111");
            assertEquals(1, epics.size());
            assertEquals(2, ((Number) epics.get(0).get("progress_total")).intValue());
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardSearchAndHealth() throws Exception {
        String search = """
            {"results":[{"id":"t1","short_id":"tsk-cccc3333","title":"Fix login bug"}],"suggestions":[]}""";
        var server = startMockServer(200, search);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var res = client.board.searchBoard("login", java.util.Map.of("tags", java.util.List.of("auth")));
            assertEquals("tsk-cccc3333", ((Map<?, ?>) ((List<?>) res.get("results")).get(0)).get("short_id"));
            var health = client.board.getBoardHealth();
            assertNotNull(health);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardLifecycleReplayKeyOverload() throws Exception {
        String json = """
            {"id":"e1","short_id":"epc-bbbb2222","status":"active"}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var epic = client.board.epicActivate("epc-bbbb2222", "key-1");
            assertEquals("active", epic.get("status"));
            var proj = client.board.archiveProject("prj-aaaa1111", "key-2");
            assertNotNull(proj);
            var dep = client.board.removeDependency("tsk-cccc3333", "tsk-aaaa0000", "key-3");
            assertNotNull(dep);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardTaskCarriesOR4AnchorTag() throws Exception {
        String json = """
            {"id":"t1","short_id":"tsk-cccc3333","stage":"backlog","tags":["pr:1234"],
             "is_blocked":false,"attempts":0,
             "acceptance_criteria":[{"text":"c","met":false}]}""";
        var server = startMockServer(201, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var task = client.board.createTask("epc-bbbb2222",
                    Map.of("title", "T", "tags", List.of("pr:1234")));
            assertEquals("tsk-cccc3333", task.get("short_id"));
            @SuppressWarnings("unchecked")
            List<String> tags = (List<String>) task.get("tags");
            assertTrue(tags.get(0).matches("^pr:[1-9][0-9]*$"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardDependencyEnvelope() throws Exception {
        String json = """
            {"task":{"id":"t1","short_id":"tsk-cccc3333","is_blocked":true,
                     "blocked_by":["tsk-aaaa0000"]}}""";
        var server = startMockServer(200, json);
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            var dep = client.board.addDependency("tsk-cccc3333", Map.of("dependsOn", "tsk-aaaa0000"));
            @SuppressWarnings("unchecked")
            Map<String, Object> task = (Map<String, Object>) dep.get("task");
            assertEquals(Boolean.TRUE, task.get("is_blocked"));
        } finally {
            server.stop(0);
        }
    }

    @Test
    void boardGetProjectNotFound() throws Exception {
        var server = startMockServer(404, "{\"error\":\"board: project \\\"prj-zzzzzzzz\\\" not found\"}");
        try {
            var client = LLMSafeSpacesClient.builder("http://localhost:" + server.getAddress().getPort())
                    .apiKey("lsp_test").build();
            assertThrows(NotFoundException.class, () -> client.board.getProject("prj-zzzzzzzz"));
        } finally {
            server.stop(0);
        }
    }
}
