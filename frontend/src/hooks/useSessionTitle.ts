import { useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { workspacesApi } from "../api/workspaces";
import type { SessionListItem } from "../api/types";

/**
 * Fetches the active session's title via the session contract endpoint
 * (GET /workspaces/:id/sessions/:sessionId — AgentSession).
 *
 * Behaviour:
 * - Fetches on mount when workspaceId + sessionId are available and active is true.
 * - Re-fetches whenever `streaming` transitions from true → false (i.e. after
 *   the agent finishes a response, which is when the agent generates a title).
 * - When a non-empty title is received, invalidates ["sessions", workspaceId]
 *   so the sidebar reflects the new title immediately.
 */
export function useSessionTitle(
  workspaceId: string | undefined,
  sessionId: string | undefined,
  active: boolean,
  streaming: boolean,
) {
  const queryClient = useQueryClient();
  const prevStreaming = useRef(streaming);

  const { data, refetch } = useQuery({
    queryKey: ["session-title", workspaceId, sessionId],
    queryFn: () => workspacesApi.getSession(workspaceId!, sessionId!),
    enabled: !!workspaceId && !!sessionId && active,
    // Don't auto-retry on 404 — a brand new session may not exist on the
    // agent yet.
    retry: false,
    // Don't refetch on window focus; title changes only happen after messages.
    refetchOnWindowFocus: false,
  });

  // Re-fetch when streaming ends — the agent generates the title after
  // the first exchange.
  // Retry after a delay since title generation is async.
  useEffect(() => {
    if (prevStreaming.current && !streaming && workspaceId && sessionId && active) {
      console.log("[SessionTitle] streaming ended, refetching title for", sessionId);
      // First attempt immediately
      refetch();
      // Retry after 2s in case title wasn't ready
      const timer = setTimeout(() => refetch(), 2000);
      return () => clearTimeout(timer);
    }
    prevStreaming.current = streaming;
  }, [streaming, workspaceId, sessionId, active, refetch]);

  // When a title arrives, update the sidebar cache and persist to DB.
  const persistedRef = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (!data?.title || !workspaceId || !sessionId) return;
    queryClient.setQueryData<SessionListItem[]>(["sessions", workspaceId], (old) => {
      if (!old) return old;
      return old.map((s) =>
        s.id === sessionId ? { ...s, title: data.title } : s,
      );
    });
    // Persist to PostgreSQL so title survives page reload
    if (persistedRef.current !== data.title) {
      persistedRef.current = data.title;
      workspacesApi.renameSession(workspaceId, sessionId, data.title).catch(() => {});
    }
  }, [data?.title, workspaceId, sessionId, queryClient]);

  return data?.title ?? undefined;
}
