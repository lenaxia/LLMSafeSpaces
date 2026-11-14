import { useEffect, useRef, useState } from "react";
import type { Dispatch, SetStateAction } from "react";
import { loadSessionDraft, saveSessionDraft } from "../lib/sessionDrafts";

/**
 * useSessionDraft — the Composer's text state, persisted per session.
 *
 * The Composer remounts on most session switches (the history-loading
 * spinner branch unmounts ChatView), so plain useState loses the
 * draft; on a fresh-cache switch the Composer stays mounted and this
 * hook's hydrate path carries the switch instead. Either way the
 * session's stored draft restores into the box, write-through persists
 * every edit, and a "" text consumes the stored draft (a sent message
 * clears it).
 *
 * THE OWNER GUARD IS THE LOAD-BEARING SEAM. On a session switch, the
 * write-through effect runs with the outgoing session's text while the
 * incoming key is already in props; without the guard it would save
 * that text under the INCOMING session's key, and the hydrate (running
 * after it) would then load the bled-in text back as the incoming
 * session's "draft" — box bleed AND store bleed (pinned: removing the
 * guard fails the switch tests). The guard skips the write until the
 * hydrate has re-keyed the owner and loaded the incoming draft.
 *
 * Effect DECLARATION ORDER (write-through first) is secondary hygiene,
 * not correctness: it keeps that transient cross-key write from
 * happening at all — the post-hydrate write-through would repair the
 * store anyway, so swapping the effects passes the suite. Both notes
 * exist so a future simplification removes the right thing: the guard
 * stays, the order may go.
 *
 * `persistOverride` (when provided) is what the write-through persists
 * instead of `text`. The Composer's history navigation uses it: while
 * browsing history the textarea shows a RECALLED entry, not the user's
 * draft — persisting the pre-browse snapshot keeps the feature's
 * promise (switch away and back restores the DRAFT, never the recalled
 * view). Mid-browse edits to a recalled entry are transient by the
 * Composer's own recall semantics (Up always reloads the original) and
 * are deliberately not persisted as the draft.
 *
 * Without a session (no sessionId/workspaceId) the hook is a plain
 * useState: nothing persists.
 */
export function useSessionDraft(
  workspaceId: string | undefined,
  sessionId: string | undefined,
  persistOverride?: string,
): [string, Dispatch<SetStateAction<string>>] {
  const [text, setText] = useState<string>(() =>
    workspaceId && sessionId ? loadSessionDraft(workspaceId, sessionId) : "",
  );

  // The session the current `text` belongs to. Guards the write-through
  // across the switch window (see the owner-guard note above).
  const ownerRef = useRef<{ ws: string; sid: string } | null>(
    workspaceId && sessionId ? { ws: workspaceId, sid: sessionId } : null,
  );

  // Write-through: persist the current text (or the Composer's
  // browse-time override) under its OWNING session only. "" consumes
  // the draft (never stored, never accumulated).
  useEffect(() => {
    if (!workspaceId || !sessionId) return;
    const owner = ownerRef.current;
    if (!owner || owner.ws !== workspaceId || owner.sid !== sessionId) return;
    saveSessionDraft(workspaceId, sessionId, persistOverride ?? text);
  }, [text, persistOverride, workspaceId, sessionId]);

  // Hydrate + follow session switches (order note above).
  useEffect(() => {
    if (!workspaceId || !sessionId) {
      ownerRef.current = null;
      setText("");
      return;
    }
    ownerRef.current = { ws: workspaceId, sid: sessionId };
    setText(loadSessionDraft(workspaceId, sessionId));
  }, [workspaceId, sessionId]);

  return [text, setText];
}
