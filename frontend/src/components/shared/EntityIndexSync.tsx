// EntityIndexSync — tier (a) of conservative entity resolution
// (tsk-vibrant-cadet-pearls): mirrors already-loaded react-query
// caches into the entityLinks registry so candidates that name a
// KNOWN entity resolve at zero cost, before any lazy probing.
//
//   ["sessions", wsId]            → session entities (phrase ids)
//   ["board", "projects"]         → board projects (short ids)
//   ["board", "tasks"]            → board stories (short ids)
//   ["board", "epics", projectId] → board epics (short ids)
//
// Renders nothing. Mount once inside QueryClientProvider (App). The
// cache subscription is coalesced (trailing 150ms) so streaming-time
// invalidations do not walk the cache per event, and publishing is
// idempotent (entityLinks bumps its version only when data actually
// changed — the rerender-stability pin).

import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import {
  publishEntities,
  setSessionWorkspaceScope,
  type EntityRef,
} from "../../lib/entityLinks";
import type { SessionListItem } from "../../api/types";
import type { BoardEpic, BoardProject, BoardTask } from "../../api/board";

const SYNC_COALESCE_MS = 150;

function isRecordArray(data: unknown): data is Record<string, unknown>[] {
  return Array.isArray(data);
}

export function EntityIndexSync() {
  const queryClient = useQueryClient();

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined;

    const sync = () => {
      const refs: EntityRef[] = [];
      const sessionWorkspaces = new Set<string>();
      for (const query of queryClient.getQueryCache().getAll()) {
        const key = query.queryKey;
        if (!Array.isArray(key) || key.length < 2) continue;
        const data = query.state.data;
        if (data === undefined) continue;

        if (key[0] === "sessions" && typeof key[1] === "string") {
          if (!isRecordArray(data)) continue;
          const wsId = key[1];
          sessionWorkspaces.add(wsId);
          for (const s of data as unknown as SessionListItem[]) {
            if (!s || typeof s.id !== "string" || s.id === "") continue;
            refs.push({
              kind: "session",
              key: `session:${s.id.toLowerCase()}`,
              id: s.id,
              title: typeof s.title === "string" && s.title !== "" ? s.title : undefined,
              workspaceId: wsId,
            });
          }
          continue;
        }

        if (key[0] !== "board" || typeof key[1] !== "string") continue;
        if (key[1] === "projects" && isRecordArray(data)) {
          for (const p of data as unknown as BoardProject[]) {
            if (!p || typeof p.short_id !== "string" || p.short_id === "") continue;
            refs.push({
              kind: "project",
              key: `project:${p.short_id.toLowerCase()}`,
              id: p.short_id,
              title: typeof p.name === "string" ? p.name : undefined,
            });
          }
        } else if (key[1] === "tasks" && isRecordArray(data)) {
          for (const t of data as unknown as BoardTask[]) {
            if (!t || typeof t.short_id !== "string" || t.short_id === "") continue;
            refs.push({
              kind: "task",
              key: `task:${t.short_id.toLowerCase()}`,
              id: t.short_id,
              title: typeof t.title === "string" ? t.title : undefined,
            });
          }
        } else if (key[1] === "epics" && isRecordArray(data)) {
          for (const e of data as unknown as BoardEpic[]) {
            if (!e || typeof e.short_id !== "string" || e.short_id === "") continue;
            refs.push({
              kind: "epic",
              key: `epic:${e.short_id.toLowerCase()}`,
              id: e.short_id,
              title: typeof e.title === "string" ? e.title : undefined,
            });
          }
        }
      }
      // Scope-only changes (a workspace gained a session index, no new
      // entities) stay silent: consumers that render ses_* candidates
      // schedule their probes through effects that re-run on registry
      // bumps, and entity publishes accompany scope growth in
      // practice — avoiding re-render storms on workspace churn.
      setSessionWorkspaceScope(sessionWorkspaces);
      publishEntities(refs);
    };

    const unsubscribe = queryClient.getQueryCache().subscribe(() => {
      clearTimeout(timer);
      timer = setTimeout(sync, SYNC_COALESCE_MS);
    });
    sync();

    return () => {
      unsubscribe();
      clearTimeout(timer);
    };
  }, [queryClient]);

  return null;
}
