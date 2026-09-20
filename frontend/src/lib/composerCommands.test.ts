// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from "vitest";
import { matchSlash, slashMatches } from "./composerCommands";

describe("matchSlash", () => {
  it("no slash, no match", () => {
    expect(matchSlash("hello")).toBeNull();
    expect(matchSlash("")).toBeNull();
  });

  it("bare slash: empty word, palette armed", () => {
    expect(matchSlash("/")).toEqual({ word: "", args: "", palette: true });
  });

  it("word only", () => {
    expect(matchSlash("/Rename")).toEqual({ word: "rename", args: "", palette: true });
  });

  it("word + args: args extracted, palette STAYS ARMED (Enter must execute)", () => {
    expect(matchSlash("/rename  Weekly review ")).toEqual({
      word: "rename",
      args: "Weekly review ",
      palette: true,
    });
  });

  it("mid-text slash is not a command", () => {
    expect(matchSlash("see /compact later")).toBeNull();
    expect(matchSlash("multi\nline /compact")).toBeNull();
  });
});

describe("slashMatches", () => {
  it("empty query matches everything", () => {
    expect(slashMatches("compact", "")).toBe(true);
  });

  it("prefix match, case-insensitive", () => {
    expect(slashMatches("compact", "COM")).toBe(true);
    expect(slashMatches("compact", "omp")).toBe(false);
  });
});
