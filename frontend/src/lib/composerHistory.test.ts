import { describe, expect, it } from "vitest";
import { extractUserMessageTexts, getCursorLineInfo } from "./composerHistory";
import type { Message } from "../api/types";

function userMsg(id: string, text: string): Message {
  return { id, role: "user", parts: [{ type: "text", text }] };
}

function assistantMsg(id: string, text: string): Message {
  return { id, role: "assistant", parts: [{ type: "text", text }] };
}

describe("extractUserMessageTexts", () => {
  it("returns empty array for empty input", () => {
    expect(extractUserMessageTexts([])).toEqual([]);
  });

  it("extracts text from a single user message", () => {
    expect(extractUserMessageTexts([userMsg("u1", "hello world")])).toEqual(["hello world"]);
  });

  it("filters out assistant messages", () => {
    const msgs: Message[] = [
      userMsg("u1", "question"),
      assistantMsg("a1", "answer"),
      userMsg("u2", "follow-up"),
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["question", "follow-up"]);
  });

  it("preserves chronological order (does not reverse — caller reverses)", () => {
    const msgs: Message[] = [
      userMsg("u1", "first"),
      userMsg("u2", "second"),
      userMsg("u3", "third"),
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["first", "second", "third"]);
  });

  it("skips user messages whose only text part is empty/whitespace", () => {
    const msgs: Message[] = [
      userMsg("u1", "real"),
      { id: "u2", role: "user", parts: [{ type: "text", text: "   " }] },
      { id: "u3", role: "user", parts: [{ type: "text", text: "" }] },
      userMsg("u4", "also-real"),
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["real", "also-real"]);
  });

  it("skips user messages with no parts", () => {
    const msgs: Message[] = [
      userMsg("u1", "real"),
      { id: "u2", role: "user", parts: [] },
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["real"]);
  });

  it("concatenates multiple text parts within a single user message", () => {
    const msgs: Message[] = [
      {
        id: "u1",
        role: "user",
        parts: [
          { type: "text", text: "line one" },
          { type: "text", text: "line two" },
        ],
      },
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["line oneline two"]);
  });

  it("skips non-text parts (thinking/tool/error) — user messages shouldn't have these but be defensive", () => {
    const msgs: Message[] = [
      {
        id: "u1",
        role: "user",
        parts: [
          { type: "text", text: "real text" },
          { type: "thinking", text: "should be ignored" },
          { type: "tool_use", text: "also ignored" },
          { type: "error", text: "ignored too" },
        ],
      },
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["real text"]);
  });

  it("skips a user message entirely if it has only non-text parts", () => {
    const msgs: Message[] = [
      {
        id: "u1",
        role: "user",
        parts: [{ type: "thinking", text: "no actual text" }],
      },
      userMsg("u2", "real"),
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["real"]);
  });

  it("treats a text part missing the text field as no contribution", () => {
    const msgs: Message[] = [
      { id: "u1", role: "user", parts: [{ type: "text" }] },
      userMsg("u2", "real"),
    ];
    expect(extractUserMessageTexts(msgs)).toEqual(["real"]);
  });
});

describe("getCursorLineInfo", () => {
  it("single-line input: cursor at start is on first AND last line", () => {
    const info = getCursorLineInfo("hello", 0);
    expect(info.onFirstLine).toBe(true);
    expect(info.onLastLine).toBe(true);
    expect(info.currentLine).toBe(0);
    expect(info.totalLines).toBe(1);
  });

  it("single-line input: cursor at end is on first AND last line", () => {
    const info = getCursorLineInfo("hello", 5);
    expect(info.onFirstLine).toBe(true);
    expect(info.onLastLine).toBe(true);
    expect(info.currentLine).toBe(0);
  });

  it("multi-line: cursor at very start is on first line only", () => {
    const info = getCursorLineInfo("line1\nline2\nline3", 0);
    expect(info.onFirstLine).toBe(true);
    expect(info.onLastLine).toBe(false);
    expect(info.currentLine).toBe(0);
    expect(info.totalLines).toBe(3);
  });

  it("multi-line: cursor at very end is on last line only", () => {
    const value = "line1\nline2\nline3";
    const info = getCursorLineInfo(value, value.length);
    expect(info.onFirstLine).toBe(false);
    expect(info.onLastLine).toBe(true);
    expect(info.currentLine).toBe(2);
  });

  it("multi-line: cursor in middle line is on neither boundary", () => {
    const value = "line1\nline2\nline3";
    // cursor at "line1\n|line2" — position 6
    const info = getCursorLineInfo(value, 6);
    expect(info.onFirstLine).toBe(false);
    expect(info.onLastLine).toBe(false);
    expect(info.currentLine).toBe(1);
  });

  it("cursor at end of first line (before \\n) is still on first line", () => {
    const value = "line1\nline2";
    // cursor at "line1|\nline2" — position 5
    const info = getCursorLineInfo(value, 5);
    expect(info.onFirstLine).toBe(true);
    expect(info.onLastLine).toBe(false);
    expect(info.currentLine).toBe(0);
  });

  it("cursor immediately after \\n is on the next line", () => {
    const value = "line1\nline2";
    // cursor at "line1\n|line2" — position 6
    const info = getCursorLineInfo(value, 6);
    expect(info.onFirstLine).toBe(false);
    expect(info.onLastLine).toBe(true);
    expect(info.currentLine).toBe(1);
  });

  it("empty input: cursor at 0 is on first AND last line", () => {
    const info = getCursorLineInfo("", 0);
    expect(info.onFirstLine).toBe(true);
    expect(info.onLastLine).toBe(true);
    expect(info.currentLine).toBe(0);
    expect(info.totalLines).toBe(1);
  });

  it("trailing newline: cursor at end is on a new empty last line", () => {
    // "hello\n" — pressing Down from the empty line should NOT navigate history
    // because the cursor is already on the last (empty) line.
    const value = "hello\n";
    const info = getCursorLineInfo(value, value.length);
    expect(info.onLastLine).toBe(true);
    expect(info.currentLine).toBe(1);
    expect(info.totalLines).toBe(2);
  });

  it("trailing newline: cursor before \\n is on first line only", () => {
    const value = "hello\n";
    // cursor at "hello|\n" — position 5
    const info = getCursorLineInfo(value, 5);
    expect(info.onFirstLine).toBe(true);
    expect(info.onLastLine).toBe(false);
    expect(info.currentLine).toBe(0);
  });

  it("two consecutive newlines: cursor between them is on a middle line", () => {
    const value = "a\n\nb";
    // cursor at "a\n|\nb" — position 2
    const info = getCursorLineInfo(value, 2);
    expect(info.onFirstLine).toBe(false);
    expect(info.onLastLine).toBe(false);
    expect(info.currentLine).toBe(1);
    expect(info.totalLines).toBe(3);
  });

  // NOTE: getCursorLineInfo does not handle \r\n because real textareas
  // never expose CR in .value — the HTML spec mandates \r and \r\n are
  // normalized to \n in the textarea API value. Handling CRLF would be
  // dead code (Rule 4) and would require renormalizing selectionStart,
  // which is itself a source of bugs. See the function doc for details.
});
