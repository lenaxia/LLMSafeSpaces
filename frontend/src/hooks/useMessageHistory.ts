import { useInfiniteQuery } from "@tanstack/react-query";
import { messagesApi, type HistoryPage } from "../api/messages";
import type { Message } from "../api/types";

interface InfiniteData {
  pages: HistoryPage[];
  pageParams: (string | undefined)[];
}

function selectChronological(data: InfiniteData): Message[] {
  // Pair each message with its original index in the flattened array so
  // the sort can fall back to backend-provided order when timestamps tie.
  // The backend returns pages oldest-first within each page and the
  // infinite-query concatenates pages newest-page-first; the flattened
  // order is therefore a meaningful "as-delivered" sequence that
  // reflects the backend's view of chronology even when two messages
  // share a createdAt millisecond.
  //
  // Pre-fix the tiebreaker was `a.id.localeCompare(b.id)`, which broke
  // for opencode-format IDs that don't lex-sort by creation time
  // (worklog 0555). Using the flattened index makes the sort stable
  // and immune to ID format changes.
  const indexed = data.pages.flatMap((p, pageIdx) =>
    p.messages.map((m, msgIdx) => ({ m, origIdx: pageIdx * 100000 + msgIdx })),
  );
  indexed.sort((a, b) => {
    const aTime = a.m.createdAt ? new Date(a.m.createdAt).getTime() : 0;
    const bTime = b.m.createdAt ? new Date(b.m.createdAt).getTime() : 0;
    if (aTime !== bTime) return aTime - bTime;
    return a.origIdx - b.origIdx;
  });
  return indexed.map((x) => x.m);
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
    select: selectChronological,
  });
}
