import { useInfiniteQuery } from "@tanstack/react-query";
import { messagesApi, type HistoryPage } from "../api/messages";
import type { Message } from "../api/types";

interface InfiniteData {
  pages: HistoryPage[];
  pageParams: (string | undefined)[];
}

// I12 stitch (US-69.10): transcript order is the backend's own order —
// pages arrive newest-page-first and each page is oldest-first within
// itself, so chronological order is the page-reversed concatenation.
// Messages reconcile by entity ID (store IDs are preserved through
// translation); timestamps are never used for stitching.
function selectByIdentity(data: InfiniteData): Message[] {
  const seen = new Set<string>();
  const out: Message[] = [];
  for (const page of [...data.pages].reverse()) {
    for (const m of page.messages) {
      if (m.id && seen.has(m.id)) continue;
      if (m.id) seen.add(m.id);
      out.push(m);
    }
  }
  return out;
}

export function useMessageHistory(workspaceId: string | undefined, sessionId: string | undefined) {
  return useInfiniteQuery({
    queryKey: ["messages", workspaceId, sessionId],
    queryFn: ({ pageParam }) =>
      messagesApi.getHistoryPage(workspaceId!, sessionId!, { before: pageParam }),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (lastPage) => lastPage?.nextCursor,
    enabled: !!workspaceId && !!sessionId,
    staleTime: 10_000,
    refetchOnWindowFocus: false,
    select: selectByIdentity,
  });
}
