/**
 * Tests for ChatPage's pending-question/permission UI lifecycle after the
 * US-69.10 hard cutover. Prompts are no longer pushed by SSE agent.*
 * events: the contract fold is projection-authoritative (I12 prompt sync).
 * A snapshot's pendingInputs seed the provider store (spied here) with
 * mapped shapes; a later snapshot without an input removes the stored
 * prompt; the QuestionPrompt/PermissionPrompt components render from what
 * the (mocked) provider read-hooks return and answer/reject through
 * inputApi. No extra fetch happens beyond the stream itself.
 */
import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { create } from "@bufbuild/protobuf";
import { ChatPage } from "./ChatPage";
import { TooltipProvider } from "../components/ui";
import type { Event } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { EventSchema, EventType, InputKind, SessionStatus } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { SessionSnapshot } from "../abi/llmsafespaces/abi/v1/abi_pb";
import { SessionSnapshotSchema } from "../abi/llmsafespaces/abi/v1/abi_pb";

// --- Mocks ---

vi.mock("../api/workspaces", () => ({
  workspacesApi: {
    getStatus: vi.fn(),
    activate: vi.fn(),
    list: vi.fn().mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } }),
    renameSession: vi.fn(),
    renameWorkspace: vi.fn().mockResolvedValue({}),
    deleteSession: vi.fn().mockResolvedValue(undefined),
    abortSession: vi.fn(),
    markSessionSeen: vi.fn().mockResolvedValue(undefined),
    getSessions: vi.fn().mockResolvedValue([]),
  },
}));

// The provider is mocked: the add/remove/clear hooks are SPIES, and the
// read hooks return what the test controls (filtered by session at read
// time, mirroring the real provider) so the real QuestionPrompt /
// PermissionPrompt components render.
const promptStore = vi.hoisted(() => ({
  questions: [] as Array<Record<string, any>>,
  permissions: [] as Array<Record<string, any>>,
  addQuestion: vi.fn(),
  addPermission: vi.fn(),
  removeAction: vi.fn(),
  clearSessionPrompts: vi.fn(),
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
  useRemovePendingAction: () => promptStore.removeAction,
  useAddPendingQuestion: () => promptStore.addQuestion,
  useAddPendingPermission: () => promptStore.addPermission,
  usePendingQuestionsForSession: (sessionId: string) =>
    promptStore.questions.filter(
      (q) => (q.root_session_id ?? q.session_id) === sessionId || q.session_id === sessionId,
    ),
  usePendingPermissionsForSession: (sessionId: string) =>
    promptStore.permissions.filter(
      (p) => (p.root_session_id ?? p.session_id) === sessionId || p.session_id === sessionId,
    ),
  useClearSessionPendingPrompts: () => promptStore.clearSessionPrompts,
  useWorkspaceInputSnapshot: () => undefined,
  SessionActivityProvider: ({ children }: { children: any }) => <>{children}</>,
}));

vi.mock("../api/messages", () => ({
  messagesApi: {
    getHistory: vi.fn().mockResolvedValue([]),
    getHistoryPage: vi.fn().mockResolvedValue({ messages: [], nextCursor: undefined }),
    sendAsync: vi.fn(),
    queueMessage: vi.fn().mockResolvedValue({ messageID: "msg_q_mock" }),
    getQueue: vi.fn().mockResolvedValue({ messages: [] }),
    deleteQueueMessage: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock("../api/sessions", () => ({ sessionsApi: { create: vi.fn() } }));
vi.mock("../api/input", () => ({
  inputApi: {
    questionReply: vi.fn().mockResolvedValue(true),
    questionReject: vi.fn().mockResolvedValue(true),
    permissionReply: vi.fn().mockResolvedValue(true),
    listQuestions: vi.fn().mockResolvedValue([]),
    listPermissions: vi.fn().mockResolvedValue([]),
  },
}));

// Platform stream (workspace.phase/alert, queue.update, agent_died) —
// stubbed out; not driven in this suite.
vi.mock("../hooks/useEventStream", () => ({
  useEventStream: vi.fn(),
}));

// Contract-stream harness: captures the options ChatPage registers and
// re-renders the component when the fold state is replaced (the real hook
// publishes new state before calling onSnapshot; the mock mirrors that so
// the I12 prompt-sync effect re-runs on snapshot application).
const contractHarness = vi.hoisted(() => ({
  options: null as null | {
    onEvent: (event: any, seq: bigint) => void;
    onSnapshot: (state: any) => void;
    onReconnect: () => void;
  },
  state: { seq: 0n, sessions: new Map<string, any>() },
  listeners: new Set<() => void>(),
}));
vi.mock("../hooks/useContractStream", async () => {
  const { useEffect, useState } = await vi.importActual<typeof import("react")>("react");
  return {
    useContractStream: vi.fn((_workspaceId: string | undefined, options: Record<string, unknown>) => {
      contractHarness.options = options as never;
      const [, bump] = useState(0);
      useEffect(() => {
        const rerender = () => bump((v) => v + 1);
        contractHarness.listeners.add(rerender);
        return () => { contractHarness.listeners.delete(rerender); };
      }, [bump]);
      return contractHarness.state;
    }),
  };
});

// Mock ChatView to render the prompts prop (the prompt components inside
// it are real; only the chat surface is stubbed).
vi.mock("../components/chat/ChatView", () => ({
  ChatView: (props: Record<string, unknown>) => (
    <div data-testid="chat-view">{props.prompts as React.ReactNode}</div>
  ),
}));

import { workspacesApi } from "../api/workspaces";
import { inputApi } from "../api/input";

// --- Helpers ---

function makeQueryClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
}

function renderChat(qc: QueryClient, path: string) {
  // Pre-seed queries so the component renders ChatView immediately
  const wsId = path.split("/")[2];
  const sesId = path.split("/")[3];
  qc.setQueryData(["workspace-status", wsId], { phase: "Active" });
  qc.setQueryData(["workspaces"], { items: [], pagination: { limit: 20, offset: 0, total: 0 } });
  if (sesId) {
    qc.setQueryData(["messages", wsId, sesId], { pages: [{ messages: [], nextCursor: undefined }], pageParams: [undefined] });
  }
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[path]}>
        <TooltipProvider delayDuration={0}>
          <Routes>
            <Route path="/chat/:workspaceId/:sessionId" element={<ChatPage />} />
          </Routes>
        </TooltipProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function makeSessionSnapshot(sessionId: string, init: Record<string, unknown> = {}): SessionSnapshot {
  return create(SessionSnapshotSchema, { sessionId, ...init } as Parameters<typeof create>[1]);
}

/** Replace the fold state and notify the mocked hook's subscribers —
 * the post-cutover equivalent of "the SSE event arrived": the prompt-sync
 * effect re-runs against the new snapshot. */
function applySnapshot(atSeq: bigint, sessions: SessionSnapshot[]) {
  contractHarness.state = { seq: atSeq, sessions: new Map(sessions.map((s) => [s.sessionId, s])) };
  act(() => {
    contractHarness.listeners.forEach((l) => l());
    contractHarness.options?.onSnapshot(contractHarness.state);
  });
}

function sendContractEvent(evt: Event, seq: bigint = 1n) {
  act(() => { contractHarness.options?.onEvent(evt, seq); });
}

function abiEvent(partial: Partial<Event>): Event {
  return create(EventSchema, { sessionId: "ses_1", ...partial } as Parameters<typeof create>[1]);
}

/** ABI InputRequest, kind QUESTION (1). */
function questionInput(overrides: Record<string, unknown> = {}) {
  return {
    id: "que_abc",
    sessionId: "ses_1",
    kind: InputKind.QUESTION,
    question: "Pick one",
    header: "Language",
    options: [{ label: "Go", description: "fast" }],
    multiple: false,
    ...overrides,
  };
}

/** ABI InputRequest, kind PERMISSION (2). */
function permissionInput(overrides: Record<string, unknown> = {}) {
  return {
    id: "per_xyz",
    sessionId: "ses_1",
    kind: InputKind.PERMISSION,
    permission: "shell",
    patterns: ["rm -rf /tmp"],
    always: [],
    ...overrides,
  };
}

/** What the I12 effect hands the provider for questionInput(). */
function mappedQuestionRequest() {
  return {
    id: "que_abc",
    session_id: "ses_1",
    root_session_id: "ses_1",
    questions: [{
      question: "Pick one",
      header: "Language",
      options: [{ label: "Go", description: "fast" }],
      multiple: false,
    }],
  };
}

/** What the I12 effect hands the provider for permissionInput(). */
function mappedPermissionRequest() {
  return {
    id: "per_xyz",
    session_id: "ses_1",
    root_session_id: "ses_1",
    permission: "shell",
    patterns: ["rm -rf /tmp"],
    always: ["bash"],
  };
}

async function renderReady(qc: QueryClient, path = "/chat/ws-1/ses_1") {
  const utils = renderChat(qc, path);
  await waitFor(() => expect(screen.getByTestId("chat-view")).toBeInTheDocument());
  return utils;
}

// --- Tests ---

describe("ChatPage pending prompts (I12 fold-driven, US-69.10)", () => {
  beforeEach(() => {
    contractHarness.options = null;
    contractHarness.state = { seq: 0n, sessions: new Map() };
    contractHarness.listeners.clear();
    promptStore.questions = [];
    promptStore.permissions = [];
    vi.clearAllMocks();
    (workspacesApi.getStatus as ReturnType<typeof vi.fn>).mockResolvedValue({ phase: "Active" });
    (workspacesApi.list as ReturnType<typeof vi.fn>).mockResolvedValue({ items: [], pagination: { limit: 20, offset: 0, total: 0 } });
    (workspacesApi.getSessions as ReturnType<typeof vi.fn>).mockResolvedValue([]);
  });

  describe("I12 prompt sync — snapshot seeding", () => {
    it("snapshot pendingInputs seed question prompts via addPendingQuestion — no extra fetch", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      applySnapshot(5n, [
        makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [questionInput()] }),
      ]);
      await waitFor(() => expect(promptStore.addQuestion).toHaveBeenCalled());
      expect(promptStore.addQuestion).toHaveBeenCalledWith("ws-1", mappedQuestionRequest());
      expect(promptStore.addPermission).not.toHaveBeenCalled();
      // The INPUT flow is snapshot-driven: nothing beyond the stream itself.
      expect(inputApi.listQuestions).not.toHaveBeenCalled();
      expect(inputApi.listPermissions).not.toHaveBeenCalled();
    });

    it("snapshot pendingInputs seed permission prompts with patterns and always", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      applySnapshot(5n, [
        makeSessionSnapshot("ses_1", {
          status: SessionStatus.BUSY,
          pendingInputs: [permissionInput({ always: ["bash"] })],
        }),
      ]);
      await waitFor(() => expect(promptStore.addPermission).toHaveBeenCalled());
      expect(promptStore.addPermission).toHaveBeenCalledWith("ws-1", mappedPermissionRequest());
      expect(promptStore.addQuestion).not.toHaveBeenCalled();
    });

    it("pendingInputs in another session's snapshot are not seeded for the viewed session", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      applySnapshot(5n, [
        makeSessionSnapshot("ses_other", {
          status: SessionStatus.BUSY,
          pendingInputs: [questionInput({ id: "que_x", sessionId: "ses_other" })],
        }),
      ]);
      await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
      expect(promptStore.addQuestion).not.toHaveBeenCalled();
      expect(promptStore.addPermission).not.toHaveBeenCalled();
    });

    it("a later snapshot without the input removes the stored prompt (fold removal)", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      applySnapshot(5n, [
        makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [questionInput()] }),
      ]);
      await waitFor(() => expect(promptStore.addQuestion).toHaveBeenCalled());
      // The provider now stores the prompt; the fold drops it (resolved
      // outside this view's event flow).
      promptStore.questions = [mappedQuestionRequest()];
      applySnapshot(6n, [
        makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [] }),
      ]);
      await waitFor(() => expect(promptStore.removeAction).toHaveBeenCalledWith("que_abc"));
    });

    it("fold removal only touches the viewed session's stored prompts", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      promptStore.questions = [{ ...mappedQuestionRequest(), id: "que_other", session_id: "ses_other" }];
      applySnapshot(6n, [
        makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [] }),
      ]);
      await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
      expect(promptStore.removeAction).not.toHaveBeenCalledWith("que_other");
    });
  });

  describe("prompt rendering", () => {
    it("stored question prompt renders with header, question and options", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      promptStore.questions = [mappedQuestionRequest()];
      applySnapshot(2n, [makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [questionInput()] })]);
      expect(screen.getByText("Pick one")).toBeInTheDocument();
      expect(screen.getByText("Language")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Go" })).toBeInTheDocument();
    });

    it("stored permission prompt renders with patterns", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      promptStore.permissions = [mappedPermissionRequest()];
      applySnapshot(2n, [
        makeSessionSnapshot("ses_1", {
          status: SessionStatus.BUSY,
          pendingInputs: [permissionInput({ always: ["bash"] })],
        }),
      ]);
      expect(screen.getByText("Run shell command")).toBeInTheDocument();
      expect(screen.getByText("rm -rf /tmp")).toBeInTheDocument();
    });

    it("prompts stored for another session do not render on this view", async () => {
      const qc = makeQueryClient();
      const { unmount } = await renderReady(qc);
      promptStore.questions = [mappedQuestionRequest()];
      applySnapshot(2n, [makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [questionInput()] })]);
      expect(screen.getByText("Pick one")).toBeInTheDocument();
      // Navigate to a different session: the stored prompt survives in the
      // global provider but must not render for the new session.
      unmount();
      await renderReady(qc, "/chat/ws-1/ses_2");
      expect(screen.queryByText("Pick one")).not.toBeInTheDocument();
    });
  });

  describe("answer/reject interactions", () => {
    async function renderWithQuestion() {
      const qc = makeQueryClient();
      await renderReady(qc);
      promptStore.questions = [mappedQuestionRequest()];
      applySnapshot(2n, [makeSessionSnapshot("ses_1", { status: SessionStatus.BUSY, pendingInputs: [questionInput()] })]);
      expect(screen.getByText("Pick one")).toBeInTheDocument();
    }

    it("answering a question calls questionReply and resolves via removePendingAction", async () => {
      await renderWithQuestion();
      act(() => { screen.getByRole("button", { name: "Go" }).click(); });
      act(() => { screen.getByText("Submit answers").click(); });
      await waitFor(() => expect(inputApi.questionReply).toHaveBeenCalledWith("ws-1", "que_abc", [["Go"]]));
      await waitFor(() => expect(promptStore.removeAction).toHaveBeenCalledWith("que_abc"));
    });

    it("dismissing a question calls questionReject and resolves", async () => {
      await renderWithQuestion();
      act(() => { screen.getByText("Dismiss").click(); });
      await waitFor(() => expect(inputApi.questionReject).toHaveBeenCalledWith("ws-1", "que_abc"));
      await waitFor(() => expect(promptStore.removeAction).toHaveBeenCalledWith("que_abc"));
    });

    it("allow-once on a permission calls permissionReply and resolves", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      promptStore.permissions = [mappedPermissionRequest()];
      applySnapshot(2n, [
        makeSessionSnapshot("ses_1", {
          status: SessionStatus.BUSY,
          pendingInputs: [permissionInput({ always: ["bash"] })],
        }),
      ]);
      act(() => { screen.getByRole("button", { name: "Allow once" }).click(); });
      await waitFor(() => expect(inputApi.permissionReply).toHaveBeenCalledWith("ws-1", "per_xyz", "once", undefined));
      await waitFor(() => expect(promptStore.removeAction).toHaveBeenCalledWith("per_xyz"));
    });
  });

  describe("stale-prompt clears (US-16.12, contract events)", () => {
    it("ERROR contract event clears the viewed session's prompts", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(abiEvent({
        type: EventType.ERROR,
        error: { code: "timeout", message: "timed out" } as never,
      }));
      await waitFor(() => expect(promptStore.clearSessionPrompts).toHaveBeenCalledWith("ses_1"));
    });

    it("SESSION_STATUS idle clears the viewed session's prompts", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(abiEvent({ type: EventType.SESSION_STATUS, status: SessionStatus.IDLE }));
      await waitFor(() => expect(promptStore.clearSessionPrompts).toHaveBeenCalledWith("ses_1"));
    });

    it("ERROR and idle events for a different session do not clear prompts", async () => {
      const qc = makeQueryClient();
      await renderReady(qc);
      sendContractEvent(abiEvent({
        type: EventType.ERROR,
        sessionId: "ses_other",
        error: { code: "timeout", message: "timed out" } as never,
      }));
      sendContractEvent(abiEvent({ type: EventType.SESSION_STATUS, sessionId: "ses_other", status: SessionStatus.IDLE }));
      await act(async () => { await new Promise((r) => setTimeout(r, 30)); });
      expect(promptStore.clearSessionPrompts).not.toHaveBeenCalled();
    });
  });
});

