// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * #-prompt recall token logic (#1496 part 2; the symbol moved from `@`
 * to `#` pre-release — `@` is conventionally people-tagging) — pure
 * functions so the cursor/token rules are pinnable without a DOM.
 *
 * A live #-token is the span from a `#` to the caret where the `#` is
 * at the start of the text or preceded by whitespace (mid-sentence use
 * is the point), and the span contains no whitespace after the `#`
 * (typing past a space closes the token). The query is the text after
 * the `#` (may be empty — popup opens on a bare `#`).
 *
 * Markdown-heading suppression: a bare `#` at LINE START never opens
 * the popup (the empty-query + line-start shape is exactly a heading's
 * first keystroke; once a non-space char follows — `#deploy` — the
 * token is explicit recall intent and opens). Mid-sentence bare `#`
 * still opens: it is not a heading shape.
 */

export interface PromptToken {
  /** Index of the `#` character. */
  at: number;
  /** Index one past the last character of the token (== caret). */
  end: number;
  /** Filter text after the `#` ("" when the caret sits right after it). */
  query: string;
}

export function findPromptToken(text: string, caret: number): PromptToken | null {
  if (caret < 0 || caret > text.length) return null;
  let i = caret - 1;
  while (i >= 0 && !/\s/.test(text[i] ?? "")) {
    if (text[i] === "#") {
      const at = i;
      if (at === 0 || /\s/.test(text[at - 1] ?? "")) {
        const query = text.slice(at + 1, caret);
        const lineStart = at === 0 || text[at - 1] === "\n";
        if (query === "" && lineStart) {
          return null; // bare # at line start: a heading's first keystroke
        }
        return { at, end: caret, query };
      }
      return null; // # embedded in a word ("C#") is not a recall token
    }
    i--;
  }
  return null;
}

/**
 * Expands a selected prompt's content over the token span. The
 * replacement itself is NEVER re-scanned for tokens: recall triggers
 * only from typing (the caret lands after the inserted content and the
 * composer suppresses the popup for the programmatic change).
 */
export function expandPromptToken(
  text: string,
  token: PromptToken,
  content: string,
): { text: string; caret: number } {
  const next = text.slice(0, token.at) + content + text.slice(token.end);
  return { text: next, caret: token.at + content.length };
}
