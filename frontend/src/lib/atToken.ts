// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * @-prompt recall token logic (#1496, part 2) — pure functions so the
 * cursor/token rules are pinnable without a DOM.
 *
 * A live @-token is the span from an `@` to the caret where the `@` is
 * at the start of the text or preceded by whitespace (mid-sentence use
 * is the point), and the span contains no whitespace after the `@`
 * (typing past a space closes the token). The query is the text after
 * the `@` (may be empty — popup opens on bare `@`).
 */

export interface AtToken {
  /** Index of the `@` character. */
  at: number;
  /** Index one past the last character of the token (== caret). */
  end: number;
  /** Filter text after the `@` ("" when the caret sits right after it). */
  query: string;
}

export function findAtToken(text: string, caret: number): AtToken | null {
  if (caret < 0 || caret > text.length) return null;
  let i = caret - 1;
  while (i >= 0 && !/\s/.test(text[i] ?? "")) {
    if (text[i] === "@") {
      const at = i;
      if (at === 0 || /\s/.test(text[at - 1] ?? "")) {
        return { at, end: caret, query: text.slice(at + 1, caret) };
      }
      return null; // @ embedded in a word ("a@b") is not a recall token
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
export function expandAtToken(
  text: string,
  token: AtToken,
  content: string,
): { text: string; caret: number } {
  const next = text.slice(0, token.at) + content + text.slice(token.end);
  return { text: next, caret: token.at + content.length };
}
