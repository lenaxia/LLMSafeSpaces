import type { Message } from "../api/types";

/**
 * Extract the plain text of every user message in `messages`, in
 * chronological order (oldest first — the same order as the input).
 *
 * Only `text` parts are concatenated; thinking/tool/error parts are
 * skipped. User messages from opencode history only ever contain text
 * parts (see transformHistory in api/messages.ts), but we filter
 * defensively so a malformed or future-shaped message cannot corrupt
 * the history list.
 *
 * Whitespace-only and part-less user messages are dropped — they are
 * not useful as prompt-history candidates.
 *
 * The caller is responsible for reversing the result if it wants
 * newest-first ordering for Up/Down navigation.
 */
export function extractUserMessageTexts(messages: Message[]): string[] {
  const out: string[] = [];
  for (const msg of messages) {
    if (msg.role !== "user") continue;
    let text = "";
    for (const part of msg.parts) {
      if (part.type === "text" && typeof part.text === "string") {
        text += part.text;
      }
    }
    if (text.trim().length > 0) {
      out.push(text);
    }
  }
  return out;
}

export interface CursorLineInfo {
  /** True when the cursor is on the first visual line of the buffer. */
  onFirstLine: boolean;
  /** True when the cursor is on the last visual line of the buffer. */
  onLastLine: boolean;
  /** Zero-indexed line number the cursor is on. */
  currentLine: number;
  /** Total number of lines in the buffer. */
  totalLines: number;
}

/**
 * Compute which line of a textarea the cursor is on, without touching
 * the DOM. Pure function so it is trivially unit-testable.
 *
 * Line semantics:
 *   - Lines are separated by `\n`.
 *   - A trailing `\n` produces one final empty line — so the cursor
 *     sitting after it is on the last (empty) line. This is what users
 *     expect: if you typed "hello\n" you are visually on a new line,
 *     and pressing Down should not navigate history.
 *
 * Input contract: `value` and `selectionStart` come from a textarea's
 * `.value` and `.selectionStart`. Per the HTML spec, a textarea's API
 * value normalizes `\r` and `\r\n` to `\n`, so this function never
 * receives CR characters from a real textarea. No CRLF handling is
 * performed — adding it would be dead code (Rule 4) and would require
 * renormalizing `selectionStart`, which is itself a source of bugs.
 *
 * Mirrors the cursor-gating logic from the opencode TUI
 * (packages/tui/src/component/prompt/index.tsx:867-925) where history
 * navigation only fires when the cursor is at the buffer boundary.
 */
export function getCursorLineInfo(value: string, selectionStart: number): CursorLineInfo {
  const before = value.slice(0, selectionStart);
  // Count line breaks before the cursor. Each \n means the cursor is
  // on the line after it. Counting chars is O(n) with no allocation;
  // split("\n").length - 1 would allocate an extra array for the same
  // result.
  let breaksBeforeCursor = 0;
  for (let i = 0; i < before.length; i++) {
    if (before.charCodeAt(i) === 10) breaksBeforeCursor++;
  }
  const currentLine = breaksBeforeCursor;

  // Total lines = number of \n in the whole buffer + 1.
  // For "a\nb" that's 1 break + 1 = 2 lines. For "a\n" that's
  // 1 break + 1 = 2 lines (the second being empty). Correct.
  let totalBreaks = 0;
  for (let i = 0; i < value.length; i++) {
    if (value.charCodeAt(i) === 10) totalBreaks++;
  }
  const totalLines = totalBreaks + 1;

  return {
    onFirstLine: currentLine === 0,
    onLastLine: currentLine === totalLines - 1,
    currentLine,
    totalLines,
  };
}
