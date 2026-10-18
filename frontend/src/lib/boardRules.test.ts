// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect } from "vitest";
import {
  PR_ANCHOR_PATTERN,
  validateBoardTags,
  isManagerDragAllowed,
  doneReadiness,
  suggestBoardItems,
  describeBoardError,
  BOARD_STAGES,
  MAIN_STAGES,
  SIDE_STAGES,
} from "./boardRules";

// ─── OR4: the pr:N anchor form ───────────────────────────────────────────────

describe("PR_ANCHOR_PATTERN (ruling OR4: ^pr:[1-9][0-9]*$)", () => {
  it.each(["pr:1", "pr:9", "pr:10", "pr:1234", "pr:999999"])("accepts %s", (tag) => {
    expect(PR_ANCHOR_PATTERN.test(tag)).toBe(true);
  });
  it.each([
    "pr:0", // zero
    "pr:01", // leading zero
    "pr:", // empty number
    "pr:abc", // non-numeric
    "pr:12a", // trailing junk
    "pr:-1", // negative
    "pr:1.5", // decimal
    "PR:12", // case
    "Pr:3", // case
    "pr :12", // space
  ])("rejects %s", (tag) => {
    expect(PR_ANCHOR_PATTERN.test(tag)).toBe(false);
  });
});

describe("validateBoardTags", () => {
  it("passes clean anchor + free-form tags", () => {
    expect(validateBoardTags(["pr:1702", "backend", "needs-review"])).toEqual([]);
  });

  it("flags a zero anchor", () => {
    const errs = validateBoardTags(["pr:0"]);
    expect(errs).toHaveLength(1);
    expect(errs[0]!.tag).toBe("pr:0");
  });

  it("flags leading zeros", () => {
    expect(validateBoardTags(["pr:01"])).toHaveLength(1);
  });

  it("flags case near-misses (the engine anchors are lowercase only)", () => {
    const errs = validateBoardTags(["PR:12"]);
    expect(errs).toHaveLength(1);
    expect(errs[0]!.reason).toContain("lowercase");
  });

  it("flags non-numeric anchors", () => {
    expect(validateBoardTags(["pr:seven"])).toHaveLength(1);
  });

  it("flags each offending tag independently", () => {
    expect(validateBoardTags(["ok", "pr:0", "PR:2"])).toHaveLength(2);
  });

  it("leaves non-anchor tags alone (free-form vocabulary)", () => {
    expect(validateBoardTags(["urgent", "infra", "pr"])).toEqual([]);
  });
});

// ─── Stage machine mirror ─────────────────────────────────────────────────────

describe("stage vocabulary", () => {
  it("carries the six enumerated stages (ruling R2)", () => {
    expect(BOARD_STAGES).toEqual(["backlog", "in_progress", "in_review", "done", "pending_input", "cancelled"]);
  });
  it("splits four through-flow columns from the two side columns", () => {
    expect(MAIN_STAGES).toEqual(["backlog", "in_progress", "in_review", "done"]);
    expect(SIDE_STAGES).toEqual(["pending_input", "cancelled"]);
  });
});

describe("isManagerDragAllowed (stage.go's legal map, manager view)", () => {
  it("allows the in_review exits and the pending_input human exits", () => {
    expect(isManagerDragAllowed("in_review", "in_progress")).toBe(true);
    expect(isManagerDragAllowed("in_review", "done")).toBe(true);
    expect(isManagerDragAllowed("in_review", "pending_input")).toBe(true);
    expect(isManagerDragAllowed("pending_input", "backlog")).toBe(true);
    expect(isManagerDragAllowed("pending_input", "in_progress")).toBe(true);
  });

  it("does not offer in_progress sources (claim-token guarded — the worker owns them)", () => {
    expect(isManagerDragAllowed("in_progress", "in_review")).toBe(false);
    expect(isManagerDragAllowed("in_progress", "done")).toBe(false);
  });

  it("does not offer backlog sources (backlog exits via the engine's claim / task_start)", () => {
    expect(isManagerDragAllowed("backlog", "in_progress")).toBe(false);
    expect(isManagerDragAllowed("backlog", "done")).toBe(false);
  });

  it("refuses no-ops, backward jumps, and terminal entries", () => {
    expect(isManagerDragAllowed("done", "backlog")).toBe(false);
    expect(isManagerDragAllowed("cancelled", "backlog")).toBe(false);
    expect(isManagerDragAllowed("backlog", "backlog")).toBe(false);
  });
});

// ─── Done readiness (R2 mirror — server still arbitrates) ─────────────────────

describe("doneReadiness", () => {
  it("reports all-met on zero criteria (the engine's vacuous truth)", () => {
    expect(doneReadiness({ acceptance_criteria: [] })).toEqual({ allMet: true, unmet: [] });
  });
  it("lists the unmet criteria", () => {
    const r = doneReadiness({ acceptance_criteria: [{ text: "tests green", met: true }, { text: "docs", met: false }] });
    expect(r.allMet).toBe(false);
    expect(r.unmet).toEqual(["docs"]);
  });
  it("all flags set = ready (evidence still required by the server)", () => {
    expect(doneReadiness({ acceptance_criteria: [{ text: "a", met: true }] }).allMet).toBe(true);
  });
});

// ─── Did-you-mean (0066 shape) ────────────────────────────────────────────────

const ITEMS = [
  { shortId: "tsk-aaaa1111", title: "Fix login redirect" },
  { shortId: "tsk-bbbb2222", title: "Board SSE feed" },
  { shortId: "tsk-abaa3333", title: "Kanban drag" },
];

describe("suggestBoardItems", () => {
  it("returns nothing for an exact hit context (no query) ", () => {
    expect(suggestBoardItems("", ITEMS)).toEqual([]);
  });

  it("suggests near-miss short ids (distance ≤ 2)", () => {
    const out = suggestBoardItems("tsk-aaaa1112", ITEMS);
    expect(out.map((i) => i.shortId)).toContain("tsk-aaaa1111");
  });

  it("suggests on title near-miss", () => {
    const out = suggestBoardItems("kanban dra", ITEMS);
    expect(out.map((i) => i.title)).toContain("Kanban drag");
  });

  it("suggests truncated-id prefixes (the truncated-tail case)", () => {
    const out = suggestBoardItems("tsk-bbbb22", ITEMS);
    expect(out.map((i) => i.shortId)).toContain("tsk-bbbb2222");
  });

  it("caps at 3 suggestions", () => {
    const many = Array.from({ length: 10 }, (_, i) => ({ shortId: `tsk-aaa${i}0000`, title: `same-stem-${i}` }));
    expect(suggestBoardItems("tsk-aaa0000", many).length).toBeLessThanOrEqual(3);
  });

  it("stays empty on far misses", () => {
    expect(suggestBoardItems("zzzzzzzzzzz", ITEMS)).toEqual([]);
  });
});

// ─── Error shape mirror (renderBoardError's client side) ─────────────────────

describe("describeBoardError", () => {
  it("mirrors 404s with suggestions", () => {
    const out = describeBoardError({
      status: 404,
      body: { error: 'board: task "tsk-aaaa1112" not found (did you mean: [tsk-aaaa1111]?)', suggestions: ["tsk-aaaa1111"] },
    });
    expect(out.suggestions).toEqual(["tsk-aaaa1111"]);
    expect(out.code).toBeUndefined();
  });

  it("mirrors 409 guard codes with their hint", () => {
    const out = describeBoardError({
      status: 409,
      body: { error: "board: guard done_guard: all acceptance criteria must be met", code: "done_guard" },
    });
    expect(out.code).toBe("done_guard");
    expect(out.hint).toContain("ALL acceptance criteria");
  });

  it("mirrors dependency 409s (code + suggestions together, R4a)", () => {
    const out = describeBoardError({
      status: 409,
      body: { error: "board: dependency cycle: …", code: "cycle", suggestions: ["tsk-aaaa1111", "tsk-bbbb2222"] },
    });
    expect(out.code).toBe("cycle");
    expect(out.suggestions).toHaveLength(2);
  });

  it("falls back to the exception message", () => {
    expect(describeBoardError({ message: "network down" }).message).toBe("network down");
  });
});
