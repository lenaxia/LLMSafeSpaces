// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Slash-command parsing (#1496, part 1) — pure functions.
 *
 * A slash command is live when the ENTIRE input is `/name` or `/name
 * <args...>` with the slash at position 0 (input-start only; a slash
 * mid-text is literal). `matchSlash` returns the command word and arg
 * remainder for the current input; the palette matches on the word
 * prefix. Unknown words stay literal text — no interception.
 */

export interface SlashMatch {
  /** Command word without the slash, lowercased for matching. */
  word: string;
  /** Text after the first whitespace run (may be ""). */
  args: string;
  /** Caret-eligible for palette display: no whitespace typed yet. */
  palette: boolean;
}

export function matchSlash(text: string): SlashMatch | null {
  if (!text.startsWith("/")) return null;
  const body = text.slice(1);
  const ws = body.search(/\s/);
  if (ws === -1) {
    return { word: body.toLowerCase(), args: "", palette: true };
  }
  const word = body.slice(0, ws).toLowerCase();
  const args = body.slice(ws).replace(/^\s+/, "");
  // palette stays armed with args typed: the command word is still the
  // first token, so Enter executes with args and Tab is the only
  // action that requires the word-untouched state.
  return { word, args, palette: true };
}

/** Filter predicate used by the palette: prefix match on word. */
export function slashMatches(word: string, query: string): boolean {
  if (!query) return true;
  return word.startsWith(query.toLowerCase());
}
