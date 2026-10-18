// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import type { BoardEventPayload } from "../api/board";

/**
 * boardFeed — the §4.8 activity-feed seam: a tiny in-page pub/sub that
 * carries board.event payloads from the root user stream
 * (useUserEventStream's board branch) to whichever surface wants the
 * events themselves (the epic drawer's activity feed, filtered to the
 * epic). One SSE connection remains the only transport — this is a fan-
 * out pointer, not a second stream.
 */

type Listener = (evt: BoardEventPayload) => void;

const listeners = new Set<Listener>();

/** Emitted by useUserEventStream's board.event branch (alongside the
 * ["board"] query invalidation — the snapshot refetch stays the
 * authoritative update path; this feed is display-only). */
export function emitBoardEvent(evt: BoardEventPayload): void {
  for (const l of listeners) {
    try {
      l(evt);
    } catch {
      // a broken consumer never breaks the stream
    }
  }
}

/** Subscribe to board events; returns the unsubscribe fn. */
export function subscribeBoardEvents(listener: Listener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}
