import { useEffect, useRef, useState } from "react";
import type { Dispatch, SetStateAction } from "react";
import { loadSessionDraft, saveSessionDraft } from "../lib/sessionDrafts";

/**
 * useSessionDraft — the Composer's text state, persisted per session.
 *
 * The Composer remounts on every session switch (the history-loading
 * spinner branch unmounts ChatView), so plain useState loses the
 * draft. This hook restores the session's stored draft on mount and
 * on session-change-without-remount (defensive — covers a future
 * keepPreviousData composer), write-through persists every edit, and
 * a "" text consumes the stored draft (a sent message clears it).
 *
 * Effect ORDER is load-bearing: the write-through effect is declared
 * FIRST so that, on a session switch, it runs BEFORE the hydrate
 * effect in the same commit and its owner guard (the text still
 * belongs to the outgoing session) skips the write — otherwise the
 * old session's draft would land under the incoming session's key.
 * The hydrate effect then re-keys the owner and loads the new draft.
 * Both effects' deps change together in one commit on a switch, so
 * the pair fires once, in declaration order.
 *
 * Without a session (no sessionId/workspaceId) the hook is a plain
 * useState: nothing persists.
 */
export function useSessionDraft(
  workspaceId: string | undefined,
  sessionId: string | undefined,
): [string, Dispatch<SetStateAction<string>>] {
  const [text, setText] = useState<string>(() =>
    workspaceId && sessionId ? loadSessionDraft(workspaceId, sessionId) : "",
  );

  // The session the current `text` belongs to. Guards the write-through
  // across the switch window (see the order note above).
  const ownerRef = useRef<{ ws: string; sid: string } | null>(
    workspaceId && sessionId ? { ws: workspaceId, sid: sessionId } : null,
  );

  // Write-through: persist the current text under its OWNING session
  // only. "" consumes the draft (never stored, never accumulated).
  useEffect(() => {
    if (!workspaceId || !sessionId) return;
    const owner = ownerRef.current;
    if (!owner || owner.ws !== workspaceId || owner.sid !== sessionId) return;
    saveSessionDraft(workspaceId, sessionId, text);
  }, [text, workspaceId, sessionId]);

  // Hydrate + follow session switches — declared SECOND (order note).
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
