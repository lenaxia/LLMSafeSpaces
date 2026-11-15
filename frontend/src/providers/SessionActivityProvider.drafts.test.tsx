/**
 * Per-session draft lifecycle cleanup (owner ask 2026-11-13): the
 * persisted composer drafts are deleted at their session/workspace
 * lifecycle events — session.status deleted/archived clears the
 * session's draft; workspace.phase Terminating/Terminated clears the
 * workspace's whole subtree; Suspending/Suspended must NOT (a
 * suspended workspace resumes with its drafts intact). Through the
 * REAL provider via the captured user-event-stream callback.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, act } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { SessionActivityProvider } from "./SessionActivityProvider";
import { _resetSessionDraftStoreForTests, loadSessionDraft, saveSessionDraft } from "../lib/sessionDrafts";

let capturedOnEvent: ((data: unknown) => void) | undefined;

vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void }) => {
    capturedOnEvent = options?.onEvent;
  },
}));

function renderProvider() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>
          <div data-testid="child" />
        </SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("SessionActivityProvider: session-draft lifecycle cleanup", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    capturedOnEvent = undefined;
    localStorage.clear();
    _resetSessionDraftStoreForTests();
  });

  it("session.status=deleted removes that session's draft", () => {
    saveSessionDraft("ws-1", "sess-doomed", "draft");
    saveSessionDraft("ws-1", "sess-alive", "stays");
    renderProvider();

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-doomed",
        status: "deleted",
        event_id: 1,
      });
    });

    expect(loadSessionDraft("ws-1", "sess-doomed")).toBe("");
    expect(loadSessionDraft("ws-1", "sess-alive")).toBe("stays");
  });

  it("session.status=archived removes that session's draft (read-only sessions never send)", () => {
    saveSessionDraft("ws-1", "sess-arch", "draft");
    renderProvider();

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-arch",
        status: "archived",
        event_id: 1,
      });
    });

    expect(loadSessionDraft("ws-1", "sess-arch")).toBe("");
  });

  it("session.status=unarchived does NOT resurrect (and does not touch) the draft", () => {
    saveSessionDraft("ws-1", "sess-back", "still here");
    renderProvider();

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: "ws-1",
        session_id: "sess-back",
        status: "unarchived",
        event_id: 1,
      });
    });

    // Unarchive keeps whatever is stored — it neither clears nor
    // restores a pre-archive draft the archive event already deleted.
    expect(loadSessionDraft("ws-1", "sess-back")).toBe("still here");
  });

  it("workspace.phase=Terminated clears the workspace's whole draft subtree", () => {
    saveSessionDraft("ws-dying", "sess-1", "a");
    saveSessionDraft("ws-dying", "sess-2", "b");
    saveSessionDraft("ws-other", "sess-1", "c");
    renderProvider();

    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-dying", phase: "Terminated" });
    });

    expect(loadSessionDraft("ws-dying", "sess-1")).toBe("");
    expect(loadSessionDraft("ws-dying", "sess-2")).toBe("");
    expect(loadSessionDraft("ws-other", "sess-1")).toBe("c");
  });

  it("workspace.phase=Terminating clears the workspace's drafts too", () => {
    saveSessionDraft("ws-1", "sess-1", "a");
    renderProvider();

    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Terminating" });
    });

    expect(loadSessionDraft("ws-1", "sess-1")).toBe("");
  });

  it("workspace.phase=Suspended KEEPS the drafts (suspend resumes; deletion does not)", () => {
    saveSessionDraft("ws-1", "sess-1", "survives the suspend round trip");
    renderProvider();

    act(() => {
      capturedOnEvent!({ type: "workspace.phase", workspace_id: "ws-1", phase: "Suspended" });
    });

    expect(loadSessionDraft("ws-1", "sess-1")).toBe("survives the suspend round trip");
  });

  it("session.status=busy/idle never touches drafts (turn traffic is not lifecycle)", () => {
    saveSessionDraft("ws-1", "sess-1", "draft");
    renderProvider();

    act(() => {
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "busy", event_id: 1 });
      capturedOnEvent!({ type: "session.status", workspace_id: "ws-1", session_id: "sess-1", status: "idle", event_id: 2 });
    });

    expect(loadSessionDraft("ws-1", "sess-1")).toBe("draft");
  });
});
