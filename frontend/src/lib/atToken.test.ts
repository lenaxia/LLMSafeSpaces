// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from "vitest";
import { expandAtToken, findAtToken } from "./atToken";

describe("findAtToken", () => {
  it("opens on a bare @ at the start (caret directly after)", () => {
    expect(findAtToken("@", 1)).toEqual({ at: 0, end: 1, query: "" });
  });

  it("opens mid-sentence after whitespace", () => {
    expect(findAtToken("check @dep", 10)).toEqual({ at: 6, end: 10, query: "dep" });
  });

  it("does NOT open mid-word (a@b is not a recall token)", () => {
    expect(findAtToken("a@b", 3)).toBeNull();
  });

  it("does NOT open at text start preceded by nothing but with leading text before caret scanning back past whitespace", () => {
    // caret after "hello " — no @ before the caret within the word run
    expect(findAtToken("hello world", 11)).toBeNull();
  });

  it("closes the token once whitespace follows the @word (caret past the space)", () => {
    expect(findAtToken("check @dep and", 11)).toBeNull();
  });

  it("tracks only the token nearest the caret when multiple @tokens exist", () => {
    expect(findAtToken("@first then @sec", 16)).toEqual({ at: 12, end: 16, query: "sec" });
  });

  it("an earlier @token is inert once the caret moved into later text without an @", () => {
    expect(findAtToken("@first then plain", 17)).toBeNull();
  });

  it("query may be empty mid-sentence (caret right after the @)", () => {
    expect(findAtToken("hey ", 4)).toBeNull(); // no @ at all
    expect(findAtToken("hey @", 5)).toEqual({ at: 4, end: 5, query: "" });
  });
});

describe("expandAtToken", () => {
  it("replaces the @token span with the prompt content", () => {
    const text = "check @dep please";
    const token = findAtToken(text, 10)!;
    const out = expandAtToken(text, token, "deploy steps:\n1. push");
    expect(out.text).toBe("check deploy steps:\n1. push please");
    expect(out.caret).toBe("check deploy steps:\n1. push".length);
  });

  it("replaces a start-of-text token", () => {
    const out = expandAtToken("@greet", { at: 0, end: 6, query: "greet" }, "hello there");
    expect(out.text).toBe("hello there");
    expect(out.caret).toBe(11);
  });

  it("leaves other @tokens in the surrounding text untouched", () => {
    const text = "@one and @two";
    const token = findAtToken(text, 13)!;
    const out = expandAtToken(text, token, "X");
    expect(out.text).toBe("@one and X");
  });
});
