// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later
package com.llmsafespaces.sdk.services;

import com.google.gson.reflect.TypeToken;
import com.llmsafespaces.sdk.LLMSafeSpacesClient;

import java.util.List;
import java.util.Map;

/**
 * Task Board service (#1699, design 0067 §4.3a): the owner's manager
 * surface at /me/projects, /me/epics, /me/tasks. Row encoding is the
 * engine's canonical JSON (snake_case short_id — identical to the
 * internal automation seam's rendering).
 */
public class BoardService {
    private final LLMSafeSpacesClient c;

    public BoardService(LLMSafeSpacesClient c) { this.c = c; }

    private static final TypeToken<Map<String, Object>> MAP = new TypeToken<Map<String, Object>>() {};
    private static final TypeToken<List<Map<String, Object>>> LIST = new TypeToken<List<Map<String, Object>>>() {};

    public List<Map<String, Object>> listProjects() {
        Map<String, Object> resp = c.request("GET", "/me/projects", null, MAP.getType());
        Object projects = resp.get("projects");
        if (projects instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) projects;
            return result;
        }
        return List.of();
    }

    public Map<String, Object> getProject(String id) {
        return c.request("GET", "/me/projects/" + id, null, MAP.getType());
    }

    public Map<String, Object> createProject(Map<String, Object> req) {
        return c.request("POST", "/me/projects", req, MAP.getType());
    }

    public Map<String, Object> updateProject(String id, Map<String, Object> req) {
        return c.request("PATCH", "/me/projects/" + id, req, MAP.getType());
    }

    public Map<String, Object> archiveProject(String id) {
        return c.request("POST", "/me/projects/" + id + "/archive", Map.of(), MAP.getType());
    }

    public Map<String, Object> unarchiveProject(String id) {
        return c.request("POST", "/me/projects/" + id + "/unarchive", Map.of(), MAP.getType());
    }

    public List<Map<String, Object>> listProjectTasks(String projectId, String stage) {
        String path = "/me/projects/" + projectId + "/tasks";
        if (stage != null && !stage.isEmpty()) {
            path += "?stage=" + stage;
        }
        Map<String, Object> resp = c.request("GET", path, null, MAP.getType());
        Object tasks = resp.get("tasks");
        if (tasks instanceof List) {
            @SuppressWarnings("unchecked")
            List<Map<String, Object>> result = (List<Map<String, Object>>) tasks;
            return result;
        }
        return List.of();
    }

    public Map<String, Object> createEpic(String projectId, Map<String, Object> req) {
        return c.request("POST", "/me/projects/" + projectId + "/epics", req, MAP.getType());
    }

    public Map<String, Object> getEpic(String id) {
        return c.request("GET", "/me/epics/" + id, null, MAP.getType());
    }

    public Map<String, Object> updateEpic(String id, Map<String, Object> req) {
        return c.request("PATCH", "/me/epics/" + id, req, MAP.getType());
    }

    private Map<String, Object> epicVerb(String id, String verb) {
        return c.request("POST", "/me/epics/" + id + "/" + verb, Map.of(), MAP.getType());
    }

    public Map<String, Object> epicPause(String id) { return epicVerb(id, "pause"); }

    public Map<String, Object> epicArchive(String id) { return epicVerb(id, "archive"); }

    public Map<String, Object> epicUnarchive(String id) { return epicVerb(id, "unarchive"); }

    /** #1747: destructive delete (cancel is eliminated); force required
     * while claims are in-flight. */
    public Map<String, Object> deleteEpic(String id, boolean force) {
        return c.request("DELETE", "/me/epics/" + id + (force ? "?force=true" : ""), null, MAP.getType());
    }

    public Map<String, Object> epicActivate(String id, boolean ackInactiveDeps) {
        return c.request("POST", "/me/epics/" + id + "/activate",
                Map.of("acknowledgeInactiveDeps", ackInactiveDeps), MAP.getType());
    }

    public Map<String, Object> pauseProject(String id) { return projectVerb(id, "pause"); }

    public Map<String, Object> resumeProject(String id) { return projectVerb(id, "resume"); }

    private Map<String, Object> projectVerb(String id, String verb) {
        return c.request("POST", "/me/projects/" + id + "/" + verb, Map.of(), MAP.getType());
    }

    public Map<String, Object> createTask(String epicId, Map<String, Object> req) {
        return c.request("POST", "/me/epics/" + epicId + "/tasks", req, MAP.getType());
    }

    public Map<String, Object> getTask(String id) {
        return c.request("GET", "/me/tasks/" + id, null, MAP.getType());
    }

    public Map<String, Object> updateTask(String id, Map<String, Object> req) {
        return c.request("PATCH", "/me/tasks/" + id, req, MAP.getType());
    }

    public Map<String, Object> moveStage(String id, Map<String, Object> req) {
        return c.request("POST", "/me/tasks/" + id + "/move-stage", req, MAP.getType());
    }

    public Map<String, Object> startTask(String id, Map<String, Object> req) {
        return c.request("POST", "/me/tasks/" + id + "/start",
                req != null ? req : Map.of(), MAP.getType());
    }

    /** #1747: destructive delete; reconnect lets orphans inherit the
     * deleted node's upstream edges (default off). */
    public Map<String, Object> deleteTask(String id, boolean reconnect) {
        return c.request("DELETE", "/me/tasks/" + id + (reconnect ? "?reconnect=true" : ""), null, MAP.getType());
    }

    public Map<String, Object> pauseTask(String id) { return taskVerb(id, "pause"); }

    public Map<String, Object> resumeTask(String id) { return taskVerb(id, "resume"); }

    private Map<String, Object> taskVerb(String id, String verb) {
        return c.request("POST", "/me/tasks/" + id + "/" + verb, Map.of(), MAP.getType());
    }

    public Map<String, Object> importPlan(String projectId, Map<String, Object> req) {
        return c.request("POST", "/me/projects/" + projectId + "/import-plan", req, MAP.getType());
    }

    public Map<String, Object> addDependency(String id, Map<String, Object> req) {
        return c.request("POST", "/me/tasks/" + id + "/dependencies", req, MAP.getType());
    }

    public Map<String, Object> removeDependency(String id, String dependsOn) {
        return c.request("DELETE", "/me/tasks/" + id + "/dependencies/" + dependsOn, null, MAP.getType());
    }
}
