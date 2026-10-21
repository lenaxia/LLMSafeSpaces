// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { boardApi } from "./board";

// The board client speaks the engine's canonical snake_case wire format
// verbatim (#1699's adapter contract) — these pins hold the encoding and
// the REST surface's 23-operation shape against a mocked fetch.

const mockGet = vi.fn();
const mockPost = vi.fn();
const mockPatch = vi.fn();
const mockDelete = vi.fn();

vi.mock("./client", () => ({
  api: {
    get: (path: string) => mockGet(path),
    post: (path: string, body?: unknown) => mockPost(path, body),
    patch: (path: string, body?: unknown) => mockPatch(path, body),
    delete: (path: string) => mockDelete(path),
  },
  ApiClientError: class extends Error {},
}));

describe("boardApi wire shapes", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("lists projects through the {projects} envelope", async () => {
    mockGet.mockResolvedValue({ projects: [{ id: "p1", short_id: "prj-aaaa1111" }] });
    const out = await boardApi.listProjects();
    expect(mockGet).toHaveBeenCalledWith("/me/projects");
    expect(out[0]!.short_id).toBe("prj-aaaa1111");
  });

  it("lists project tasks through the {tasks} envelope", async () => {
    mockGet.mockResolvedValue({ tasks: [] });
    await boardApi.listProjectTasks("prj-aaaa1111");
    expect(mockGet).toHaveBeenCalledWith("/me/projects/prj-aaaa1111/tasks");
  });

  it("threads the enumerated stage filter", async () => {
    mockGet.mockResolvedValue({ tasks: [] });
    await boardApi.listProjectTasks("prj-aaaa1111", "backlog");
    expect(mockGet).toHaveBeenCalledWith("/me/projects/prj-aaaa1111/tasks?stage=backlog");
  });

  it("threads the #1813 failed-state filter (true/false/composed)", async () => {
    mockGet.mockResolvedValue({ tasks: [] });
    await boardApi.listProjectTasks("prj-aaaa1111", undefined, { failed: true });
    expect(mockGet).toHaveBeenCalledWith("/me/projects/prj-aaaa1111/tasks?failed=true");
    await boardApi.listProjectTasks("prj-aaaa1111", undefined, { failed: false });
    expect(mockGet).toHaveBeenCalledWith("/me/projects/prj-aaaa1111/tasks?failed=false");
    await boardApi.listProjectTasks("prj-aaaa1111", "backlog", { failed: true });
    expect(mockGet).toHaveBeenCalledWith("/me/projects/prj-aaaa1111/tasks?stage=backlog&failed=true");
  });

  it("creates a project with a requestId (idempotency discipline)", async () => {
    mockPost.mockResolvedValue({ id: "p1" });
    await boardApi.createProject({ name: "Board", targetWorkspaceId: "ws-1", workerSlots: 2 });
    const [path, body] = mockPost.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe("/me/projects");
    expect(body.name).toBe("Board");
    expect(body.targetWorkspaceId).toBe("ws-1");
    expect(typeof body.requestId).toBe("string");
    expect(body.requestId).not.toBe("");
  });

  it("creates an epic under the project route", async () => {
    mockPost.mockResolvedValue({ id: "e1" });
    await boardApi.createEpic("prj-aaaa1111", { title: "Epic", outcome: "Ship" });
    expect(mockPost.mock.calls[0]![0]).toBe("/me/projects/prj-aaaa1111/epics");
  });

  it("addresses epics on the flat /me/epics group", async () => {
    mockPost.mockResolvedValue({});
    await boardApi.activateEpic("epc-bbbb2222");
    expect(mockPost.mock.calls[0]![0]).toBe("/me/epics/epc-bbbb2222/activate");
  });

  it("creates tasks under the epic route with OR4-carrying tags", async () => {
    mockPost.mockResolvedValue({ id: "t1" });
    await boardApi.createTask("epc-bbbb2222", { title: "T", tags: ["pr:1702"], acceptanceCriteria: [{ text: "c", met: false }] });
    const [path, body] = mockPost.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe("/me/epics/epc-bbbb2222/tasks");
    expect(body.tags).toEqual(["pr:1702"]);
    expect(body.acceptanceCriteria).toEqual([{ text: "c", met: false }]);
  });

  it("move-stage posts {toStage, evidenceUrl, requestId}", async () => {
    mockPost.mockResolvedValue({ stage: "done" });
    await boardApi.moveStage("tsk-cccc3333", "done", "https://pr/1");
    const [path, body] = mockPost.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe("/me/tasks/tsk-cccc3333/move-stage");
    expect(body.toStage).toBe("done");
    expect(body.evidenceUrl).toBe("https://pr/1");
    expect(typeof body.requestId).toBe("string");
  });

  it("task start, pause/resume, and delete ride their action routes (#1747)", async () => {
    mockPost.mockResolvedValue({ escalated: false });
    await boardApi.startTask("tsk-cccc3333");
    expect(mockPost.mock.calls[0]![0]).toBe("/me/tasks/tsk-cccc3333/start");

    mockPost.mockClear();
    mockPost.mockResolvedValue({ paused: true });
    await boardApi.pauseTask("tsk-cccc3333");
    expect(mockPost.mock.calls[0]![0]).toBe("/me/tasks/tsk-cccc3333/pause");

    mockDelete.mockResolvedValue({ item_kind: "task" });
    await boardApi.deleteTask("tsk-cccc3333", true);
    expect(mockDelete.mock.calls[0]![0]).toBe("/me/tasks/tsk-cccc3333?reconnect=true");
  });

  it("dependencies POST/DELETE with their bodies/params", async () => {
    mockPost.mockResolvedValue({ task: {} });
    await boardApi.addDependency("tsk-cccc3333", "tsk-dddd4444");
    const [path, body] = mockPost.mock.calls[0] as [string, Record<string, unknown>];
    expect(path).toBe("/me/tasks/tsk-cccc3333/dependencies");
    expect(body.dependsOn).toBe("tsk-dddd4444");

    mockDelete.mockResolvedValue({ task: {} });
    await boardApi.removeDependency("tsk-cccc3333", "tsk-dddd4444");
    expect(mockDelete).toHaveBeenCalledWith("/me/tasks/tsk-cccc3333/dependencies/tsk-dddd4444");
  });

  it("encodes ids defensively in path segments", async () => {
    mockGet.mockResolvedValue({});
    await boardApi.getTask("tsk weird/id");
    expect(mockGet).toHaveBeenCalledWith("/me/tasks/tsk%20weird%2Fid");
  });
});
