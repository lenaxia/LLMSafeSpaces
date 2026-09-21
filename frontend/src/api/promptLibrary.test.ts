// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach } from "vitest";
import { promptLibraryApi } from "./promptLibrary";

// The #1496↔#1499 contract pin, at the adapter itself: the backend
// ships the NAMED envelope ({"prompts":[...]}, handler-verified in the
// merge), and the adapter is the single seam that unwraps it. Mocked
// at the api-client boundary — below this line everything is the real
// network path shape.

const mockGet = vi.fn();

vi.mock("./client", () => ({
  api: { get: (path: string) => mockGet(path) },
}));

beforeEach(() => {
  vi.clearAllMocks();
});

describe("promptLibraryApi (the #1499 contract seam)", () => {
  it("requests /me/prompts and unwraps the named envelope", async () => {
    mockGet.mockResolvedValue({
      prompts: [{ id: "p1", name: "deploy", content: "c", createdAt: "t1", updatedAt: "t2" }],
    });

    const prompts = await promptLibraryApi.list();

    expect(mockGet).toHaveBeenCalledWith("/me/prompts");
    expect(prompts).toHaveLength(1);
    expect(prompts[0]?.name).toBe("deploy");
    expect(prompts[0]?.content).toBe("c");
  });

  it("degrades a missing prompts key to an empty list", async () => {
    mockGet.mockResolvedValue({});

    const prompts = await promptLibraryApi.list();

    expect(prompts).toEqual([]);
  });

  it("propagates transport errors to the hook's failure path (empty-list degradation lives above)", async () => {
    mockGet.mockRejectedValue(new Error("network down"));

    await expect(promptLibraryApi.list()).rejects.toThrow("network down");
  });
});
