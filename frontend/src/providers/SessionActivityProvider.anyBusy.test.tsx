/**
 * useAnySessionBusy (the Workers-tree header's aggregate activity
 * signal): an aggregation over the SAME busy map useIsSessionBusy
 * reads — never a second source of truth. Pinned here through the
 * REAL provider (SSE-shaped events, the SessionActivityProvider test
 * harness): the flip both directions, the any-vs-all semantics (one
 * busy among many idles lights it), worker-scoping-by-id-set (a busy
 * session OUTSIDE the set never leaks in), and the empty-set floor.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { SessionActivityProvider, useAnySessionBusy } from "./SessionActivityProvider";

let capturedOnEvent: ((data: unknown) => void) | undefined;

vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void }) => {
    capturedOnEvent = options?.onEvent;
  },
}));

function AnyBusyProbe({ sessionIds }: { sessionIds: string[] }) {
  const anyBusy = useAnySessionBusy(sessionIds);
  return <span data-testid="any-busy">{anyBusy ? "yes" : "no"}</span>;
}

function renderProbe(sessionIds: string[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          <AnyBusyProbe sessionIds={sessionIds} />
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function statusEvent(sessionId: string, status: string) {
  capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: sessionId, status });
}

describe("useAnySessionBusy — the Workers-tree aggregate", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
  });

  it("is never busy for an empty id set (a hidden group has no signal)", () => {
    renderProbe([]);
    expect(screen.getByTestId("any-busy").textContent).toBe("no");
    act(() => statusEvent("ses-any", "busy"));
    expect(screen.getByTestId("any-busy").textContent).toBe("no");
  });

  it("lights when ONE of the set goes busy (any, not all) and only for sessions IN the set", () => {
    renderProbe(["ses-worker-1", "ses-worker-2"]);
    expect(screen.getByTestId("any-busy").textContent).toBe("no");

    // A busy session OUTSIDE the set never leaks into the aggregate.
    act(() => statusEvent("ses-user-chat", "busy"));
    expect(screen.getByTestId("any-busy").textContent).toBe("no");

    act(() => statusEvent("ses-worker-1", "busy"));
    expect(screen.getByTestId("any-busy").textContent).toBe("yes");
  });

  it("flips both directions as activity transitions (busy → idle → busy)", () => {
    renderProbe(["ses-worker-1", "ses-worker-2"]);
    expect(screen.getByTestId("any-busy").textContent).toBe("no");

    act(() => statusEvent("ses-worker-2", "busy"));
    expect(screen.getByTestId("any-busy").textContent).toBe("yes");

    act(() => statusEvent("ses-worker-2", "idle"));
    expect(screen.getByTestId("any-busy").textContent).toBe("no");

    act(() => statusEvent("ses-worker-1", "busy"));
    expect(screen.getByTestId("any-busy").textContent).toBe("yes");
  });

  it("stays lit while ANY member remains busy (the aggregate is OR, not last-write)", () => {
    renderProbe(["ses-worker-1", "ses-worker-2"]);
    act(() => statusEvent("ses-worker-1", "busy"));
    act(() => statusEvent("ses-worker-2", "busy"));
    expect(screen.getByTestId("any-busy").textContent).toBe("yes");

    // One going idle must not extinguish the other's activity.
    act(() => statusEvent("ses-worker-1", "idle"));
    expect(screen.getByTestId("any-busy").textContent).toBe("yes");

    act(() => statusEvent("ses-worker-2", "idle"));
    expect(screen.getByTestId("any-busy").textContent).toBe("no");
  });
});
