/**
 * Regression guard for React error #310:
 * "Rendered more hooks than during the previous render"
 *
 * Root cause (fixed): useEffect(() => { doSendNowRef.current = doSendNow; })
 * was defined AFTER the early return `if (!workspaceId) { return ... }`.
 *
 * When workspaceId is undefined the component exits early — that useEffect is
 * never registered.  On a subsequent re-render where workspaceId is defined
 * the component runs past the guard and calls the extra useEffect.  React
 * detects the hook count increase and throws error #310.
 *
 * NOTE on test strategy: React 19 concurrent mode defers low-priority
 * re-renders, which means the hook-count check does not always fire
 * synchronously inside act() in a jsdom environment.  Attempting to assert on
 * console.error or thrown errors is therefore fragile.
 *
 * Instead, this test exercises the affected render path directly:
 *   1. Static assertion: no hooks are defined after the early return in
 *      ChatPage (verified by searching the source).
 *   2. Runtime assertion: ChatPage renders and re-renders without crashing
 *      when params change from empty → workspace+session.
 *
 * If the useEffect is accidentally moved back after the early return, the
 * static check will fail immediately, and the runtime check will also fail
 * once React's scheduler executes the deferred re-render.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import * as fs from "fs";
import * as path from "path";

// ── useParams mock ─────────────────────────────────────────────────────────
let paramsRef: Record<string, string | undefined> = {};
const mockNavigate = vi.fn();

vi.mock("react-router-dom", async (orig) => {
  const actual = await orig<typeof import("react-router-dom")>();
  return {
    ...actual,
    useParams: () => paramsRef,
    useNavigate: () => mockNavigate,
  };
});

// ── minimal API mocks ──────────────────────────────────────────────────────
vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn().mockResolvedValue({ phase: "Active", sessions: [] }),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    listModels: vi.fn().mockResolvedValue({ models: [], currentModel: "" }),
    setModel: vi.fn().mockResolvedValue({ model: "", applied: false }),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    renameSession: vi.fn().mockResolvedValue({}),
    abortSession: vi.fn(),
    getSession: vi.fn().mockResolvedValue({ title: "" }),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));
vi.mock("../providers/SessionActivityProvider", () => ({
  useClearPendingUnread: () => () => {},
  useIsSessionBusy: () => false,
  useIsSessionUnread: () => false,
  useWorkspaceBusyCount: () => 0,
    useWorkspaceHung: () => false,
  useIsSessionPendingAction: () => false,
  useSessionPendingActions: () => new Set<string>(),
  useAddPendingAction: () => () => {},
  useRemovePendingAction: () => () => {},
  useAddPendingQuestion: () => () => {},
  useAddPendingPermission: () => () => {},
  usePendingQuestionsForSession: () => [],
  usePendingPermissionsForSession: () => [],
  useClearSessionPendingPrompts: () => () => {},
    useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
}));
vi.mock("../api/messages", () => ({
  messagesApi: {
    getHistory: vi.fn().mockResolvedValue([]),
    getHistoryPage: vi.fn().mockResolvedValue({ messages: [], nextCursor: undefined }),
    sendAsync: vi.fn(), queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }), getQueue: vi.fn().mockResolvedValue({ messages: [] }), deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("../api/sessions", () => ({
  sessionsApi: { create: vi.fn().mockResolvedValue({ sessionId: "ses-1" }) },
}));
vi.mock("../hooks/useEventStream", () => ({ useEventStream: vi.fn() }));
vi.mock("../hooks/useUserEventStream", () => ({ useUserEventStream: vi.fn() }));
vi.mock("../hooks/useChatStream", () => ({
  useChatStream: vi.fn(() => ({
    send: vi.fn(),
    abort: vi.fn(),
    streaming: false,
    localStreaming: false,
    notifySessionIdle: vi.fn(),
    error: null,
    clearError: vi.fn(),
    atCapRetryAfter: null,
    clearAtCap: vi.fn(),
  })),
}));

// ── stateful wrapper ───────────────────────────────────────────────────────
const bumpTickRef: { current: (() => void) | null } = { current: null };
function Wrapper({ qc }: { qc: QueryClient }) {
  const [tick, setTick] = useState(0);
  const bump = () => setTick((t) => t + 1);
  // store in ref so tests can trigger re-renders without render-phase assignment
  bumpTickRef.current = bump;
  void tick;
  return (
    <QueryClientProvider client={qc}>
      <TooltipProvider delayDuration={0}>
        <ChatPage />
      </TooltipProvider>
    </QueryClientProvider>
  );
}
function bumpTick() { bumpTickRef.current?.(); }

function makeQC() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
}

/**
 * Extracts ChatPage's component body: from the function declaration to the
 * render-path early return (`if (!workspaceId) {` — note the brace: inline
 * guards like `if (!workspaceId) return;` inside callbacks must NOT match),
 * with comments stripped so commented-out hook names don't count.
 */
function chatPageBody(src: string): { body: string; afterEarlyReturn: string } {
  const fnStart = src.indexOf("export function ChatPage()");
  expect(fnStart).toBeGreaterThan(0);
  const earlyReturnIdx = src.indexOf("if (!workspaceId) {", fnStart);
  expect(earlyReturnIdx).toBeGreaterThan(fnStart);

  const noComments = src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/\/\/[^\n]*/g, "");

  const bodyStart = noComments.indexOf("export function ChatPage()");
  const guardStart = noComments.indexOf("if (!workspaceId) {", bodyStart);
  const closingBrace = noComments.indexOf("\n  }", guardStart);
  return {
    body: noComments.slice(bodyStart, guardStart),
    afterEarlyReturn: noComments.slice(closingBrace + 4),
  };
}

/** Any hook-like call: `useXxx(` or `useXxx<...>(` (generic hooks such as
 * useState<Message[]> / useRef<Map<...>> must count too). */
const HOOK_CALL = /\buse[A-Z][A-Za-z0-9]*\s*[<(]/g;

// ChatPage's hook-call count before the early return (US-69.10 state:
// useContractStream added; input-snapshot refs/state removed). If you
// intentionally add or remove a hook in ChatPage, update this number —
// an unexpected change means hooks moved across the early-return guard
// or the hook order was accidentally restructured.
const EXPECTED_HOOK_CALLS = 67;

describe("ChatPage hook count stability (React error #310 regression guard)", () => {
  beforeEach(() => {
    paramsRef = {};
    vi.clearAllMocks();
  });

  it("no hook calls after the early-return guard in ChatPage source", () => {
    // Parse the ChatPage source and verify no useEffect/useState/useRef/useCallback
    // calls appear after the `if (!workspaceId)` early return.
    const src = fs.readFileSync(
      path.resolve(__dirname, "ChatPage.tsx"),
      "utf-8",
    );

    const { afterEarlyReturn } = chatPageBody(src);
    const hookPattern = /\buse(Effect|State|Ref|Callback|Memo|LayoutEffect)\s*\(/;
    expect(hookPattern.test(afterEarlyReturn)).toBe(false);
  });

  it("calls exactly the expected number of hooks before the early return", () => {
    // Asserts hook-call stability: every hook must be registered before the
    // early return on EVERY render path. A count drift means a hook was
    // added/removed/moved — update EXPECTED_HOOK_CALLS deliberately.
    const src = fs.readFileSync(
      path.resolve(__dirname, "ChatPage.tsx"),
      "utf-8",
    );

    const { body } = chatPageBody(src);
    const calls = body.match(HOOK_CALL) ?? [];
    expect(calls).toHaveLength(EXPECTED_HOOK_CALLS);
  });

  it("renders without crashing when params change from empty to workspace+session", async () => {
    const qc = makeQC();
    qc.setQueryData(["workspace-status", "ws-1"], { phase: "Suspended", sessions: [] });
    qc.setQueryData(["workspaces"], { items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    qc.setQueryData(["messages", "ws-1", "ses-1"], {
      pages: [{ messages: [], nextCursor: undefined }],
      pageParams: [undefined],
    });

    // Step 1: render with no params → early return
    render(<Wrapper qc={qc} />);
    expect(screen.getByText("Select a workspace to start chatting")).toBeTruthy();

    // Step 2: update params and trigger re-render of the same component instance
    paramsRef = { workspaceId: "ws-1", sessionId: "ses-1" };
    await act(async () => { bumpTick(); });

    // Component should render without throwing
    expect(screen.queryByText("Select a workspace to start chatting")).toBeNull();
  });
});
