// #1824: a harness→phrase pair learned AFTER a harness-keyed activity event
// must re-key the provider's activity maps, not just the contract fold.
// Otherwise the sidebar row stays unbadged and workspaceBusyCount keeps
// counting the stale harness key until reconnect.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import {
  SessionActivityProvider,
  useIsSessionBusy,
  useIsSessionUnread,
  useIsSessionAborted,
  useIsSessionPendingAction,
  useWorkspaceBusyCount,
} from "./SessionActivityProvider";
import { learnSessionAlias, resetSessionAliases } from "../session/sessionAliases";

let capturedOnEvent: ((data: unknown) => void) | undefined;

vi.mock("../hooks/useUserEventStream", () => ({
  useUserEventStream: (options?: { onEvent?: (data: unknown) => void }) => {
    capturedOnEvent = options?.onEvent;
  },
}));

const WS = "ws-alias";
const PHRASE = "quiet-mountain-9";
const HARNESS = "ses_alias_harness";

function renderProvider(children: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <SessionActivityProvider>{children}</SessionActivityProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function ActivityDisplay() {
  return (
    <>
      <span data-testid="phrase-busy">{useIsSessionBusy(PHRASE) ? "yes" : "no"}</span>
      <span data-testid="harness-busy">{useIsSessionBusy(HARNESS) ? "yes" : "no"}</span>
      <span data-testid="count">{useWorkspaceBusyCount(WS)}</span>
    </>
  );
}

describe("SessionActivityProvider — late alias learn re-keys activity maps", () => {
  beforeEach(() => {
    resetSessionAliases();
    capturedOnEvent = undefined;
  });

  it("moves a harness-keyed busy to the phrase when the pair is learned late, then idle clears it", () => {
    renderProvider(<ActivityDisplay />);

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: WS,
        session_id: HARNESS,
        status: "busy",
      });
    });
    expect(screen.getByTestId("phrase-busy").textContent).toBe("no");
    expect(screen.getByTestId("count").textContent).toBe("1");

    // The pair arrives from the other surface (contract stream) after busy
    // already latched under the harness key.
    act(() => {
      expect(learnSessionAlias(WS, PHRASE, HARNESS)).toBe(true);
    });

    expect(screen.getByTestId("phrase-busy").textContent).toBe("yes");
    expect(screen.getByTestId("harness-busy").textContent).toBe("no");
    expect(screen.getByTestId("count").textContent).toBe("1");

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: WS,
        session_id: HARNESS,
        status: "idle",
      });
    });
    expect(screen.getByTestId("phrase-busy").textContent).toBe("no");
    expect(screen.getByTestId("count").textContent).toBe("0");
  });

  it("re-keys unread, aborted, and pending-action keys too", () => {
    function Display() {
      return (
        <>
          <span data-testid="unread">{useIsSessionUnread(PHRASE) ? "yes" : "no"}</span>
          <span data-testid="aborted">{useIsSessionAborted(PHRASE) ? "yes" : "no"}</span>
          <span data-testid="pending">{useIsSessionPendingAction(PHRASE) ? "yes" : "no"}</span>
        </>
      );
    }
    renderProvider(<Display />);

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: WS,
        session_id: HARNESS,
        status: "idle",
      });
      capturedOnEvent!({
        type: "session.status",
        workspace_id: WS,
        session_id: HARNESS,
        status: "aborted",
      });
      capturedOnEvent!({
        type: "agent.question",
        workspace_id: WS,
        session_id: HARNESS,
        request_id: "req-1",
      });
    });
    expect(screen.getByTestId("unread").textContent).toBe("no");

    act(() => {
      expect(learnSessionAlias(WS, PHRASE, HARNESS)).toBe(true);
    });

    expect(screen.getByTestId("unread").textContent).toBe("yes");
    expect(screen.getByTestId("aborted").textContent).toBe("yes");
    expect(screen.getByTestId("pending").textContent).toBe("yes");
  });

  it("does not re-key a harness key through another workspace's alias table", () => {
    const OTHER_WS = "ws-other";
    function Display() {
      return (
        <>
          <span data-testid="busy">{useIsSessionBusy(HARNESS) ? "yes" : "no"}</span>
          <span data-testid="count-a">{useWorkspaceBusyCount(WS)}</span>
          <span data-testid="count-b">{useWorkspaceBusyCount(OTHER_WS)}</span>
        </>
      );
    }
    renderProvider(<Display />);

    act(() => {
      capturedOnEvent!({
        type: "session.status",
        workspace_id: WS,
        session_id: HARNESS,
        status: "busy",
      });
    });

    act(() => {
      expect(learnSessionAlias(OTHER_WS, PHRASE, HARNESS)).toBe(true);
    });

    expect(screen.getByTestId("busy").textContent).toBe("yes");
    expect(screen.getByTestId("count-a").textContent).toBe("1");
    expect(screen.getByTestId("count-b").textContent).toBe("0");
  });
});
