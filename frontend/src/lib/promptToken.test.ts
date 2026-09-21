// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from "vitest";
import { expandPromptToken, findPromptToken } from "./promptToken";

describe("findPromptToken", () => {
  it("opens mid-sentence after whitespace", () => {
    expect(findPromptToken("check #dep", 10)).toEqual({ at: 6, end: 10, query: "dep" });
  });

  it("does NOT open mid-word (C# is not a recall token)", () => {
    expect(findPromptToken("written in C#", 12)).toBeNull();
    expect(findPromptToken("a#b", 3)).toBeNull();
  });

  it("does NOT open with no # in the word run before the caret", () => {
    expect(findPromptToken("hello world", 11)).toBeNull();
  });

  it("closes the token once whitespace follows the #word (caret past the space)", () => {
    expect(findPromptToken("check #dep and", 11)).toBeNull();
  });

  it("tracks only the token nearest the caret when multiple #tokens exist", () => {
    expect(findPromptToken("#first then #sec", 16)).toEqual({ at: 12, end: 16, query: "sec" });
  });

  it("an earlier #token is inert once the caret moved into later text without a #", () => {
    expect(findPromptToken("#first then plain", 17)).toBeNull();
  });

  it("query may be empty mid-sentence (caret right after the #)", () => {
    expect(findPromptToken("hey #", 5)).toEqual({ at: 4, end: 5, query: "" });
  });

  // Markdown-heading suppression (the deliberate #-collision pin):
  // a bare # at line start is a heading's first keystroke — suppressed.
  it("does NOT open on a bare # at text start (markdown heading shape)", () => {
    expect(findPromptToken("#", 1)).toBeNull();
  });

  it("opens at line start once a non-space char follows (#deploy is explicit intent)", () => {
    expect(findPromptToken("#dep", 4)).toEqual({ at: 0, end: 4, query: "dep" });
    expect(findPromptToken("note:\n#dep", 10)).toEqual({ at: 6, end: 10, query: "dep" });
  });

  it("does NOT open on a bare # after a newline (heading on a later line)", () => {
    // REAL newlines (a literal \\n pair is a vacuous pin — r1's
    // mutation proof); caret right after the # — the newline branch of
    // the line-start rule. Deleting the rule flips this to a token.
    expect(findPromptToken("para\n\n#", 7)).toBeNull();
  });

  it("a bare # mid-sentence still opens (not a heading shape)", () => {
    expect(findPromptToken("see #", 5)).toEqual({ at: 4, end: 5, query: "" });
  });
});

describe("expandPromptToken", () => {
  it("replaces the #token span with the prompt content", () => {
    const text = "check #dep please";
    const token = findPromptToken(text, 10)!;
    const out = expandPromptToken(text, token, "deploy steps:\n1. push");
    expect(out.text).toBe("check deploy steps:\n1. push please");
    expect(out.caret).toBe("check deploy steps:\n1. push".length);
  });

  it("replaces a start-of-text token", () => {
    const out = expandPromptToken("#greet", { at: 0, end: 6, query: "greet" }, "hello there");
    expect(out.text).toBe("hello there");
    expect(out.caret).toBe(11);
  });

  it("leaves other #tokens in the surrounding text untouched", () => {
    const text = "#one and #two";
    const token = findPromptToken(text, 13)!;
    const out = expandPromptToken(text, token, "X");
    expect(out.text).toBe("#one and X");
  });

  // The recursion guard's token-level half: prompt content ENDING in a
  // #token leaves the composer holding a live-looking token at the new
  // caret — suppression of the programmatic change is the composer's
  // half (pinned in Composer.atRecall.test.tsx).
  it("content ending in a #token yields text whose trailing token is findable (suppression is the composer's job)", () => {
    const text = "#chain";
    const token = findPromptToken(text, 6)!;
    const out = expandPromptToken(text, token, "first #second");
    expect(out.text).toBe("first #second");
    expect(findPromptToken(out.text, out.caret)).toEqual({ at: 6, end: 13, query: "second" });
  });
});
