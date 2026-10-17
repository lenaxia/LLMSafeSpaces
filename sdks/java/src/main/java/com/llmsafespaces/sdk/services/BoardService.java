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
 * engine's canonical JSON (snake_case short_id etc.). Red-first lane
 * stub — every method throws until the projection impl lands.
 */
public class BoardService {
    private final LLMSafeSpacesClient c;

    public BoardService(LLMSafeSpacesClient c) { this.c = c; }

    private static UnsupportedOperationException todo() {
        return new UnsupportedOperationException("board: not implemented");
    }

    public List<Map<String, Object>> listProjects() { throw todo(); }

    public Map<String, Object> getProject(String id) { throw todo(); }

    public Map<String, Object> createProject(Map<String, Object> req) { throw todo(); }

    public Map<String, Object> updateProject(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> archiveProject(String id) { throw todo(); }

    public Map<String, Object> unarchiveProject(String id) { throw todo(); }

    public List<Map<String, Object>> listProjectTasks(String projectId, String stage) { throw todo(); }

    public Map<String, Object> createEpic(String projectId, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> getEpic(String id) { throw todo(); }

    public Map<String, Object> updateEpic(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> epicActivate(String id) { throw todo(); }

    public Map<String, Object> epicPause(String id) { throw todo(); }

    public Map<String, Object> epicArchive(String id) { throw todo(); }

    public Map<String, Object> epicUnarchive(String id) { throw todo(); }

    public Map<String, Object> cancelEpic(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> createTask(String epicId, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> getTask(String id) { throw todo(); }

    public Map<String, Object> updateTask(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> moveStage(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> startTask(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> cancelTask(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> addDependency(String id, Map<String, Object> req) { throw todo(); }

    public Map<String, Object> removeDependency(String id, String dependsOn) { throw todo(); }
}
