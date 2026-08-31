import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useMessageHistory } from "./useMessageHistory";

vi.mock("../api/messages", () => ({
  messagesApi: {
    getHistory: vi.fn(),
    getHistoryPage: vi.fn(),
  },
}));

import { messagesApi } from "../api/messages";

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

describe("useMessageHistory", () => {
  it("does not fetch when workspaceId is undefined", () => {
    const { result } = renderHook(() => useMessageHistory(undefined, "sess-1"), { wrapper });
    expect(result.current.isFetching).toBe(false);
  });

  it("does not fetch when sessionId is undefined", () => {
    const { result } = renderHook(() => useMessageHistory("sb-1", undefined), { wrapper });
    expect(result.current.isFetching).toBe(false);
  });

  it("preserves within-page order regardless of createdAt values", async () => {
    // US-69.10 I12 stitch: transcript order is the backend's own order —
    // within a page, array order is preserved verbatim and createdAt is
    // never consulted. The timestamps below deliberately contradict the
    // array order to prove no timestamp sorting exists.
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "zz", role: "assistant", parts: [{ type: "text", text: "first in page" }], createdAt: "1970-01-01T00:00:02.000Z" },
        { id: "aa", role: "user", parts: [{ type: "text", text: "second in page" }], createdAt: "1970-01-01T00:00:01.000Z" },
      ],
      nextCursor: undefined,
    });
    const { result } = renderHook(() => useMessageHistory("sb-1", "sess-1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(2);
    expect(result.current.data![0]!.id).toBe("zz");
    expect(result.current.data![1]!.id).toBe("aa");
  });

  it("dedupes repeated ids within a page, keeping the first occurrence", async () => {
    // Messages reconcile by entity ID (store IDs are preserved through
    // translation); a duplicate id in the same page collapses to the
    // first-seen occurrence.
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "a", role: "user", parts: [{ type: "text", text: "first copy" }] },
        { id: "b", role: "assistant", parts: [{ type: "text", text: "unique" }] },
        { id: "a", role: "user", parts: [{ type: "text", text: "duplicate copy" }] },
      ],
      nextCursor: undefined,
    });
    const { result } = renderHook(() => useMessageHistory("sb-1", "sess-1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(2);
    expect(result.current.data![0]!.id).toBe("a");
    expect(result.current.data![0]!.parts[0]!.text).toBe("first copy");
    expect(result.current.data![1]!.id).toBe("b");
  });

  it("keeps all messages that lack an id (nothing to dedupe on)", async () => {
    // Messages without ids must not collide on a shared undefined key.
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { role: "user", parts: [{ type: "text", text: "one" }] },
        { role: "assistant", parts: [{ type: "text", text: "two" }] },
      ],
      nextCursor: undefined,
    });
    const { result } = renderHook(() => useMessageHistory("sb-1", "sess-1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(2);
  });

  it("preserves backend order when createdAt is missing for all messages", async () => {
    // Guards against re-introducing createdAt stitching that chokes on
    // absent timestamps — the select must not depend on them at all.
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValue({
      messages: [
        { id: "c", role: "assistant", parts: [{ type: "text", text: "msg c" }] },
        { id: "a", role: "user", parts: [{ type: "text", text: "msg a" }] },
        { id: "b", role: "assistant", parts: [{ type: "text", text: "msg b" }] },
      ],
      nextCursor: undefined,
    });
    const { result } = renderHook(() => useMessageHistory("sb-1", "sess-1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(3);
    expect(result.current.data![0]!.id).toBe("c");
    expect(result.current.data![1]!.id).toBe("a");
    expect(result.current.data![2]!.id).toBe("b");
  });
});
