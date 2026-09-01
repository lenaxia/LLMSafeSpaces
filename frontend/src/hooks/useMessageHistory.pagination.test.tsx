// Integration-level test for useMessageHistory: drives the full
// useInfiniteQuery pagination through messagesApi, exercising the
// X-Next-Cursor contract from page-1 to page-2 to end-of-history.
//
// Pages arrive NEWEST-FIRST (page 1 = newest messages); selectByIdentity
// reverses the page list so the chronological output is stable as older
// pages load — older pages prepend at the front, the newest page stays last.
// Timestamps are never consulted for stitching; createdAt values in these
// fixtures are deliberately scrambled to prove it.
//
// Also replicates the production bug observed in session
// ses_0f01dd6f1ffe8awjS68zzWTjI5 where 84 upstream messages collapsed to a
// single page with hasNextPage=false because the server never set
// X-Next-Cursor.

import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";
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

describe("useMessageHistory — full pagination flow", () => {
  it("exposes hasNextPage=true when server returns X-Next-Cursor", async () => {
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      messages: [
        { id: "msg_0034", role: "user", parts: [{ type: "text", text: "page1-a" }], createdAt: "1970-01-01T00:00:34.000Z" },
        { id: "msg_0083", role: "assistant", parts: [{ type: "text", text: "page1-b" }], createdAt: "1970-01-01T00:01:23.000Z" },
      ],
      nextCursor: "msg_0034",
    });

    const { result } = renderHook(() => useMessageHistory("ws-1", "ses_1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.hasNextPage).toBe(true);
  });

  it("walks backwards through two pages: page reversal keeps output chronological as pages load", async () => {
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce({
        // First page = newest 2 of 4 (newest-first delivery; createdAt
        // scrambled so any timestamp sort would visibly reorder).
        messages: [
          { id: "msg_2", role: "user", parts: [{ type: "text", text: "third" }], createdAt: "1970-01-01T00:00:04.000Z" },
          { id: "msg_3", role: "assistant", parts: [{ type: "text", text: "fourth" }], createdAt: "1970-01-01T00:00:03.000Z" },
        ],
        nextCursor: "msg_2",
      })
      .mockResolvedValueOnce({
        // Second page = oldest 2.
        messages: [
          { id: "msg_0", role: "user", parts: [{ type: "text", text: "first" }], createdAt: "1970-01-01T00:00:02.000Z" },
          { id: "msg_1", role: "assistant", parts: [{ type: "text", text: "second" }], createdAt: "1970-01-01T00:00:01.000Z" },
        ],
        nextCursor: undefined,
      });

    const { result } = renderHook(() => useMessageHistory("ws-1", "ses_1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.hasNextPage).toBe(true);
    // Only the newest page is loaded; its within-page order is preserved.
    expect(result.current.data!.map((m) => m.id)).toEqual(["msg_2", "msg_3"]);

    // Fetch the next (older) page.
    await act(async () => {
      await result.current.fetchNextPage();
    });
    await waitFor(() => expect(result.current.isFetching).toBe(false));

    // Server was called with the cursor as ?before= for the second page.
    const calls = (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mock.calls;
    const beforeArgs = calls.map((c) => c[2]?.before);
    expect(beforeArgs).toContain("msg_2");

    // Older page prepended (page-reversed): all four messages present in
    // chronological order with no timestamp consultation.
    expect(result.current.data!.map((m) => m.id)).toEqual(["msg_0", "msg_1", "msg_2", "msg_3"]);
    // And we know there are no more.
    expect(result.current.hasNextPage).toBe(false);
  });

  it("dedupes ids repeated across pages, keeping the oldest-page occurrence", async () => {
    // msg_dup appears in both pages (the newest-page copy and the
    // older-page copy). selectByIdentity walks pages oldest-first, so the
    // older page's copy is "first seen" and the newest page's copy drops.
    //
    // Both pages are seeded directly into the query cache: select is a pure
    // function of the InfiniteData, and awaiting fetchNextPage inside act
    // races React 19's commit of the appended page (the fetch resolves
    // before the observer state lands).
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(["messages", "ws-1", "ses_1"], {
      pages: [
        { messages: [
          { id: "msg_dup", role: "user", parts: [{ type: "text", text: "newest-page copy" }] },
          { id: "msg_1", role: "assistant", parts: [{ type: "text", text: "latest" }] },
        ], nextCursor: "msg_dup" },
        { messages: [
          { id: "msg_0", role: "user", parts: [{ type: "text", text: "oldest" }] },
          { id: "msg_dup", role: "user", parts: [{ type: "text", text: "oldest-page copy" }] },
        ], nextCursor: undefined },
      ],
      pageParams: [undefined, "msg_dup"],
    });

    const { result } = renderHook(() => useMessageHistory("ws-1", "ses_1"), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={qc}>{children}</QueryClientProvider>
      ),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    const out = result.current.data!;
    expect(out).toHaveLength(3);
    expect(out.map((m) => m.id)).toEqual(["msg_0", "msg_dup", "msg_1"]);
    // The surviving copy is the oldest-page (first-seen) occurrence.
    expect(out[1]!.parts[0]!.text).toBe("oldest-page copy");
  });

  it("reproduces the production bug: when server never sets nextCursor, hasNextPage stays false even with many messages", async () => {
    // This is what the server does TODAY — returns the full history in one
    // shot with no cursor header. The hook believes there's nothing older,
    // so the 'Load earlier messages' button never renders.
    const eightyFour = Array.from({ length: 84 }, (_, i) => ({
      id: `msg_${String(i).padStart(4, "0")}`,
      role: i % 2 === 0 ? ("user" as const) : ("assistant" as const),
      parts: [{ type: "text" as const, text: `body ${i}` }],
      createdAt: new Date(1000 + i).toISOString(),
    }));
    (messagesApi.getHistoryPage as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      messages: eightyFour,
      nextCursor: undefined,
    });

    const { result } = renderHook(() => useMessageHistory("ws-1", "ses_1"), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    // The hook has all 84 messages, but no way to know there could have been more.
    // hasNextPage is correctly false IFF the server has truly returned everything.
    // The bug shows up in the SERVER test — server fails to filter+cap+set-cursor.
    expect(result.current.data).toHaveLength(84);
    expect(result.current.hasNextPage).toBe(false);
  });
});
