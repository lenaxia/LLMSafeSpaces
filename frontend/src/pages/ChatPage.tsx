import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { workspacesApi } from "../api/workspaces";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { ApiClientError } from "../api/client";
import { workspaceWorkflowApi } from "../api/workflows";
import { useWorkspaceStatus } from "../hooks/useWorkspaces";
import { useMessageHistory } from "../hooks/useMessageHistory";
import { ChatHistoryErrorBanner } from "../components/chat/ChatHistoryErrorBanner";
import { useActivateWorkspace } from "../hooks/useActivateWorkspace";
import { useChatStream } from "../hooks/useChatStream";
import { useEventStream } from "../hooks/useEventStream";
import { useContractStream } from "../hooks/useContractStream";
import type { Event, InputRequest } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { EventType, InputKind, MessageType, SessionStatus } from "../abi/llmsafespaces/abi/v1/contract_pb";
import type { Part } from "../abi/llmsafespaces/abi/v1/contract_pb";
import { useSessionTitle } from "../hooks/useSessionTitle";
import { useMessageQueue } from "../hooks/useMessageQueue";
import { useComposerAttachments } from "../hooks/useComposerAttachments";
import { parseAttachments } from "../lib/attachments";
import { wsLog } from "../lib/wsLog";
import { extractUserMessageTexts } from "../lib/composerHistory";
import { ChatView } from "../components/chat/ChatView";
import { SuspendedBanner } from "../components/chat/SuspendedBanner";
import { AtCapBanner } from "../components/chat/AtCapBanner";
import { HealthBanner } from "../components/chat/HealthBanner";
import { AgentReloadBanner } from "../components/workspace/AgentReloadBanner";
import { DiskUsageBar } from "../components/workspace/DiskUsageBar";
import { Spinner } from "../components/ui/Spinner";
import { KebabMenu } from "../components/ui/KebabMenu";
import type { KebabMenuItem } from "../components/ui/KebabMenu";
import { sessionsApi } from "../api/sessions";
import type { Message, SessionListItem, WorkspaceStreamEvent, WorkspaceAlertEvent } from "../api/types";
import { QuestionPrompt } from "../components/chat/QuestionPrompt";
import { PermissionPrompt } from "../components/chat/PermissionPrompt";
import { useClearPendingUnread, useAddPendingQuestion, useAddPendingPermission, useRemovePendingAction, usePendingQuestionsForSession, usePendingPermissionsForSession, useClearSessionPendingPrompts, useIsSessionBusy } from "../providers/SessionActivityProvider";

type StreamPart = { type: "text" | "thinking" | "tool"; text: string; partID?: string; toolState?: string; toolStartedAt?: string; toolCallID?: string; toolInput?: unknown; toolOutput?: string; messageID?: string };

// Dwell before the stuck-session auto-abort fires once all evidence
// conditions hold. Guards the sub-second race where a question registers
// in the projection between the stamped snapshot and its observation.
const AUTO_ABORT_DWELL_MS = 1_500;

// messageIdentityKey returns a stable identity for a chat message, used to tell
// when an optimistic local message (id `local-N`) has round-tripped into server
// history. It deliberately excludes `id` (optimistic ids never match server
// ids) and `createdAt` (server messages may omit it — see transformHistory), so
// neither is a reliable match key. (role, text) is the simplest key that
// recognises the same user message on both sides of the wire. User text is
// manifest-stripped before keying (Epic 68 D11): the optimistic bubble carries
// the raw prose while server history carries the composed text — both must key
// identically or the optimistic bubble lingers beside the history bubble.
// Known limitation: two consecutive identical messages collide on this key
// (see issue #447).
function messageIdentityKey(m: Message): string {
  const text = m.parts
    .map((p) => {
      if ("text" in p && typeof p.text === "string") {
        return m.role === "user" ? parseAttachments(p.text).text : p.text;
      }
      return "";
    })
    .join("");
  return `${m.role}|${text}`;
}

// partToStreamPart maps an ABI Part (contract 5-type union) onto the chat
// renderer's streaming bubble shape. file-change and custom parts render
// from history only (Epic 65 has no live renderer branch for them yet).
function partToStreamPart(part: Part, messageID: string | undefined): StreamPart | null {
  switch (part.payload.case) {
    case "text":
      return { type: "text", text: part.payload.value, partID: part.id, messageID };
    case "reasoning":
      return { type: "thinking", text: part.payload.value, partID: part.id, messageID };
    case "tool": {
      const tool = part.payload.value;
      return {
        type: "tool",
        text: tool.name || "",
        partID: part.id,
        toolState: toolStateLabel(tool.state?.status),
        toolCallID: tool.callId || undefined,
        toolInput: toolJsonBytes(tool.input),
        toolOutput: toolOutputText(tool.output),
        toolStartedAt: timestampToISO(tool.state?.startedAt),
        messageID,
      };
    }
    default:
      return null;
  }
}

// protobuf Timestamp → ISO string (the elapsed badge's anchor).
function timestampToISO(ts: { seconds: bigint; nanos: number } | undefined): string | undefined {
  if (!ts) return undefined;
  const ms = Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1_000_000);
  if (!Number.isFinite(ms) || ms <= 0) return undefined;
  return new Date(ms).toISOString();
}

// ABI tool input/output are raw-JSON bytes on the wire; the renderers
// expect a parsed object (input) and display text (output). A JSON
// string literal unwraps to its value — history's adapter path delivers
// the same text unquoted.
function toolJsonBytes(b: Uint8Array | undefined): unknown {
  if (!b || b.length === 0) return undefined;
  try {
    return JSON.parse(new TextDecoder().decode(b));
  } catch {
    return undefined;
  }
}

function toolOutputText(b: Uint8Array | undefined): string | undefined {
  if (!b || b.length === 0) return undefined;
  const text = new TextDecoder().decode(b);
  try {
    const parsed = JSON.parse(text);
    if (typeof parsed === "string") return parsed;
  } catch {
    // not JSON — display the raw text
  }
  return text;
}

function toolStateLabel(status: number | undefined): string {
  switch (status) {
    case 1: return "pending";
    case 2: return "running";
    case 3: return "completed";
    case 4: return "error";
    default: return "";
  }
}

// Maps known error codes to actionable user-facing messages.
function sessionErrorText(code: string, rawMessage: string): string {
  if (code === "ContextOverflowError") {
    return "Context limit reached — type /compact to summarize the conversation and continue";
  }
  if (code === "MessageOutputLengthError") {
    return "Response was too long for this model's output limit";
  }
  if (code === "ProviderAuthError") {
    return rawMessage
      ? `Authentication failed: ${rawMessage} — check the API key in Settings`
      : "The configured provider rejected the request — check the API key in Settings";
  }
  if (code === "ProviderRateLimitError" || /rate limit/i.test(rawMessage)) {
    return "The provider rate-limited this workspace — retrying shortly";
  }
  return rawMessage || code || "The agent hit an unexpected error";
}

export function ChatPage() {
  const { workspaceId, sessionId } = useParams();
  const navigate = useNavigate();
  const { confirm: confirmDelete, dialog: confirmDialog } = useConfirmDialog();
  const [localMessages, setLocalMessages] = useState<Message[]>([]);
  // sessionErrors holds error messages surfaced by contract ERROR events.
  // Kept separate from localMessages so they survive between send and idle.
  // Cleared in reconcileOnIdle (session goes idle → history is authoritative)
  // and on session change.
  const [sessionErrors, setSessionErrors] = useState<Message[]>([]);
  const queryClient = useQueryClient();

  useEffect(() => {
    setLocalMessages([]);
    setSessionErrors([]);
    setSseStreamParts([]);
    // Pending prompt content is NOT cleared here — it lives in the global
    // SessionActivityProvider (keyed by requestId) so it survives within-tab
    // navigation between a parent session and its subtasks (issue #346).
    // Reset compaction state on session switch to prevent false positives:
    // prevContextUsedRef from the old session would otherwise be compared against
    // the new session's first contextUsed value, triggering spurious compaction banners.
    prevContextUsedRef.current = undefined;
    setCompactionDetected(false);
  }, [sessionId]);

  const { data: status } = useWorkspaceStatus(workspaceId);

  const { data: activeRuns } = useQuery({
    queryKey: ["workspace-active-runs", workspaceId],
    queryFn: () => workspaceWorkflowApi.activeRuns(workspaceId!),
    enabled: !!workspaceId,
    refetchInterval: 10000,
  });

  const { data: workspaceName } = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => workspacesApi.list(),
    select: (data) => {
      const ws = data.items?.find((w) => w.id === workspaceId);
      return ws?.name ?? (workspaceId ? `workspace-${workspaceId.slice(0, 8)}` : "");
    },
  });

  const { data: activeWorkspaceData } = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => workspacesApi.list(),
    select: (data) => data.items?.find((w) => w.id === workspaceId),
  });

  const activateMutation = useActivateWorkspace();

  const isReady = status?.phase === "Active";
  const clearPendingUnread = useClearPendingUnread();
  const addPendingQuestion = useAddPendingQuestion();
  const addPendingPermission = useAddPendingPermission();
  const removePendingAction = useRemovePendingAction();
  const clearSessionPendingPrompts = useClearSessionPendingPrompts();

  useEffect(() => {
    if (!workspaceId || !sessionId || !isReady) return;

    clearPendingUnread(sessionId);

    workspacesApi.markSessionSeen(workspaceId, sessionId).catch(() => {});

    queryClient.invalidateQueries({ queryKey: ["sessions", workspaceId] });
  }, [sessionId, workspaceId, isReady]); // eslint-disable-line react-hooks/exhaustive-deps

  const prevSessionRef = useRef<{ wsId: string; sId: string } | null>(null);
  const markSeenDebounceRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    if (prevSessionRef.current) {
      const { wsId, sId } = prevSessionRef.current;
      if (markSeenDebounceRef.current) clearTimeout(markSeenDebounceRef.current);
      markSeenDebounceRef.current = setTimeout(() => {
        workspacesApi.markSessionSeen(wsId, sId).catch(() => {});
      }, 1000);
    }

    prevSessionRef.current = workspaceId && sessionId ? { wsId: workspaceId, sId: sessionId } : null;

    return () => {
      if (markSeenDebounceRef.current) clearTimeout(markSeenDebounceRef.current);
    };
  }, [sessionId, workspaceId]);

  // Subscribe to sessions query so lastSeenAt is reactive: re-renders when
  // the sessions list refetches (e.g. after mark-seen invalidates the query).
  const { data: lastSeenAt } = useQuery({
    queryKey: ["sessions", workspaceId],
    queryFn: () => workspacesApi.getSessions(workspaceId!),
    enabled: !!workspaceId && !!sessionId,
    select: (sessions) => sessions.find((s) => s.id === sessionId)?.lastSeenAt,
    staleTime: 30_000,
    notifyOnChangeProps: ["data"],
  });

  // Reactive subscription to sessions list for context_used.
  // Uses the same query key as the Sidebar's sessions query so no extra fetch is made.
  // staleTime:Infinity prevents re-fetching (Sidebar owns the fetch lifecycle).
  // notifyOnChangeProps:["data"] limits re-renders to data changes only.
  // We find the active session from the full list in the render body (not via `select`)
  // to avoid TanStack Query's structural-sharing optimisation dropping updates.
  const { data: sessionsListData } = useQuery({
    queryKey: ["sessions", workspaceId],
    queryFn: () => workspacesApi.getSessions(workspaceId!),
    enabled: !!workspaceId,
    staleTime: Infinity,
    notifyOnChangeProps: ["data"],
  });
  const activeSessionData = sessionsListData?.find((s) => s.id === sessionId);

  // Subtask (subagent) sessions have a parentId — they are spawned by the
  // opencode `task` tool and driven by their parent. They must be read-only:
  // chatting in them would spawn an extra active session and circumvent the
  // workspace's max-active-sessions limit. See ChatView.viewOnly.
  const isSubtask = !!activeSessionData?.parentId;

  // Current model for prompt injection — subscribes to the same cache key that
  // ModelSelector populates. enabled:!!workspaceId (not gated on isReady) so
  // it fires at the same time as ModelSelector's query and shares the cache.
  // staleTime matches ModelSelector so no duplicate re-fetches are triggered.
  // notifyOnChangeProps keeps re-renders minimal.
  const { data: modelsData } = useQuery({
    queryKey: ["models", workspaceId],
    queryFn: () => workspacesApi.listModels(workspaceId!),
    enabled: !!workspaceId,
    staleTime: 10_000,
    placeholderData: keepPreviousData,
    notifyOnChangeProps: ["data"],
  });

  // [ws-timing] Log every phase change and the moment isReady flips true.
  // prevPhaseRef tracks the last seen phase so we only log on actual changes.
  const prevPhaseRef = useRef<string | undefined>(undefined);
  useEffect(() => {
    const phase = status?.phase;
    if (phase !== prevPhaseRef.current) {
      wsLog("ui.phase_changed", workspaceId,
        `prev=${prevPhaseRef.current ?? "none"} → next=${phase ?? "none"}`);
      if (phase === "Active" && prevPhaseRef.current !== "Active") {
        wsLog("ui.workspace_ready", workspaceId,
          "spinner dismissed — chat UI now visible");
      }
      prevPhaseRef.current = phase;
    }
  }, [status?.phase, workspaceId]);

  const createSessionMutation = useMutation({
    mutationFn: (wsId: string) => sessionsApi.create(wsId, "New chat"),
    onSuccess: (data) => {
      queryClient.invalidateQueries({ queryKey: ["sessions", workspaceId] });
      if (workspaceId && data.sessionId) {
        navigate(`/chat/${workspaceId}/${data.sessionId}`, { replace: true });
      }
    },
  });

  useEffect(() => {
    if (isReady && workspaceId && !sessionId && !createSessionMutation.isPending) {
      createSessionMutation.mutate(workspaceId);
    }
  }, [isReady, workspaceId, sessionId]); // eslint-disable-line react-hooks/exhaustive-deps

  // activeWorkspaceId gates history fetching, chat, and session hooks on the
  // workspace being Active — these all require a reachable pod.
  //
  // sseWorkspaceId is NOT gated on isReady. SSE connects as soon as the
  // workspace page loads so that workspace.phase events (including the
  // Creating→Active transition) are received and drive the status invalidation
  // that dismisses the spinner. The backend SSE endpoint accepts connections
  // for non-Active workspaces (verified: returns 200 for Suspended).
  //
  // Without this separation, the SSE connection only opens after the workspace
  // is already Active, making the transition detection entirely dependent on
  // polling. See worklog 0132 and the frontend timing analysis for the full
  // root-cause trace.
  const activeWorkspaceId = isReady ? workspaceId : undefined;
  const sseWorkspaceId = workspaceId;
  const {
    data: history,
    isLoading: historyLoading,
    isError: historyIsError,
    error: historyError,
    refetch: historyRefetch,
    fetchNextPage,
    hasNextPage,
    isFetchingNextPage,
  } = useMessageHistory(activeWorkspaceId, sessionId);

  // Newest-first user-message texts for Composer history navigation.
  // Built from the loaded `history` only — localMessages/sessionErrors
  // are excluded so navigation reflects what's persisted, not optimistic
  // bubbles. Depth is bounded by what's loaded; older pages load via
  // the "Load earlier messages" button. Reversed because extractUserMessageTexts
  // returns chronological (oldest-first) and Composer expects newest-first.
  const userMessageHistory = useMemo(
    () => (history ? extractUserMessageTexts(history).reverse() : []),
    [history],
  );

  const isSessionBusy = useIsSessionBusy(sessionId ?? "");

  // Real-time context_used from session.next.step.ended SSE events.
  // The ref map is updated synchronously on each event; setContextVersion triggers
  // a re-render so contextUsedForDisplay reads the new ref value.
  const contextBySessionRef = useRef<Map<string, number>>(new Map());
  const [contextVersion, setContextVersion] = useState(0);

  // Derive the current session's context_used: SSE real-time value takes precedence
  // over the durable DB value from the sessions list query (cold-start fallback).
  // contextVersion is intentionally read to make this block reactive when SSE fires.
  const contextUsedForDisplay: number | undefined = (() => {
    void contextVersion; // consumed to trigger re-evaluation when SSE updates the ref
    const realtimeValue = contextBySessionRef.current.get(sessionId ?? "");
    if (realtimeValue !== undefined) return realtimeValue;
    return activeSessionData?.contextUsed ?? undefined;
  })();

  // Compaction indicator — detect when contextUsed drops >50% (opencode auto-compact).
  // Uses useLayoutEffect (runs synchronously after DOM update, before paint) so that
  // prevContextUsedRef is always up-to-date before the next render's comparison.
  const prevContextUsedRef = useRef<number | undefined>(undefined);
  const [compactionDetected, setCompactionDetected] = useState(false);
  // D6 (#998): active hung-session alert (workspace.alert/session_hung).
  const [hungAlert, setHungAlert] = useState<WorkspaceAlertEvent | null>(null);
  useLayoutEffect(() => {
    const cur = contextUsedForDisplay;
    const prev = prevContextUsedRef.current;
    if (prev != null && cur != null && prev > 0 && cur < prev * 0.5) {
      setCompactionDetected(true);
    }
    if (cur != null) {
      prevContextUsedRef.current = cur;
    }
  }, [contextUsedForDisplay]);

  // US-16.11: Pending input requests from the agent
  // Pending prompt content comes from the global SessionActivityProvider
  // (keyed by requestId, filtered to this session) so it survives within-tab
  // navigation (#346). No session-local state — nothing to clear on switch.
  const pendingQuestions = usePendingQuestionsForSession(sessionId ?? "");
  const pendingPermissions = usePendingPermissionsForSession(sessionId ?? "");

  const queue = useMessageQueue(activeWorkspaceId, sessionId);

  // Epic 68 D12/D17: workspace-scoped attachment chips. Persist across
  // session switches inside the workspace; cleared on workspace switch
  // (inside the hook). Uploads target the workspace uploads route.
  const composerAttachments = useComposerAttachments(workspaceId);

  const idCounterRef = useRef(0);

  const { send, streaming, notifySessionIdle, error: chatError, clearError, atCapRetryAfter, clearAtCap, streamTimedOut, clearStreamTimedOut } = useChatStream(activeWorkspaceId, sessionId, isSessionBusy);

  const sessionTitle = useSessionTitle(activeWorkspaceId, sessionId, isReady, streaming);

  // US-15.4/US-69.10: the fold tracks the streaming render buffer keys.
  // knownUserMessageIds gates user-echo parts (I12: the user message's id
  // arrives via MESSAGE_START(type USER); its part echoes are dropped by
  // entity ID, not by text matching).
  const knownUserMessageIds = useRef<Set<string>>(new Set());
  // Timestamp of the last contract-stream snapshot application — the
  // auto-abort's evidence gate (replaces the D9/D10 input-snapshot flight:
  // a stamped snapshot is authoritative for pending inputs, I12).
  const lastContractSnapshotAtRef = useRef(0);
  // Dwell anchor: set when all abort evidence first holds; cleared when any
  // evidence breaks (or on session change — evidence is per-session).
  // A persistent anchor (not a fresh timer) means frequent effect re-runs
  // (history refetches on reconnect churn) cannot defer the abort
  // indefinitely — each timer schedules only the REMAINING dwell.
  const abortDwellStartRef = useRef<number | null>(null);
  // Last-known history for the stuck-tool check. A workspace-status refetch
  // gap can momentarily gate the messages query off (history === undefined
  // while isReady flickers) — the stuck tool did not vanish; evaluating the
  // last-known transcript keeps the anchor from being reset by refetch churn.
  // Reset on session change (N4): stale transcripts must never satisfy the
  // stuck-tool check for the newly-viewed session.
  const lastHistoryRef = useRef<Message[] | null>(null);
  const sessionMountedAtRef = useRef(0); // set in the session-change effect (runs on mount) — keeps render pure

  const [sessionWasInterrupted, setSessionWasInterrupted] = useState(false);
  const [agentDied, setAgentDied] = useState(false);
  const [agentDiedMessage, setAgentDiedMessage] = useState<string | null>(null);
  const hasAutoAbortedRef = useRef(false);

  // Reset per-session state on session change.
  useEffect(() => {
    hasAutoAbortedRef.current = false;
    knownUserMessageIds.current.clear();
    setSessionWasInterrupted(false);
    setAgentDied(false);
    setAgentDiedMessage(null);
    // S36.4: Reset compaction state when navigating to a different session
    prevContextUsedRef.current = undefined;
    setCompactionDetected(false);
    // Stamp this view's mount time (auto-abort snapshot-evidence gate):
    // only a snapshot received AFTER this page started viewing the session
    // counts as evidence.
    sessionMountedAtRef.current = Date.now();
    // Per-session abort state must not leak across the switch — see the
    // lastHistoryRef declaration below.
    lastHistoryRef.current = null;
    abortDwellStartRef.current = null;
  }, [sessionId]);

  // US-15.5: Reconcile on idle — fetch authoritative history and clear streaming state
  const reconcileOnIdle = useCallback(async () => {
    if (!workspaceId || !sessionId) return;
    try {
      queryClient.setQueryData(["messages", workspaceId, sessionId], (old: unknown) => {
        if (!old) return old;
        const inf = old as { pages: unknown[]; pageParams: unknown[] };
        return { pages: inf.pages.slice(0, 1), pageParams: inf.pageParams.slice(0, 1) };
      });
      await queryClient.refetchQueries({ queryKey: ["messages", workspaceId, sessionId] });
      const freshHistory = queryClient.getQueryData<{ pages: Array<{ messages: Message[] }> }>(
        ["messages", workspaceId, sessionId],
      );
      const msgs = freshHistory?.pages.flatMap((p) => p.messages) ?? [];
      if (msgs.length > 0) {
        setSseStreamParts([]);
        // #447: only drop optimistic messages the server has demonstrably caught
        // up with. Clearing localMessages unconditionally wiped the just-sent user
        // bubble when an idle/reconnect refetch landed during the eventual-
        // consistency window before opencode persisted the new message.
        const historyKeys = new Set(msgs.map(messageIdentityKey));
        setLocalMessages((prev) => prev.filter((m) => !historyKeys.has(messageIdentityKey(m))));
      }
      setSessionErrors([]);
      if (freshHistory) {
        void queue.refreshQueue();
      }
    } catch {
      // Reconnect history fetch is best-effort; stale state self-corrects on next poll.
    }
  }, [workspaceId, sessionId, queryClient, queue]);

  // The contract stream: snapshot-first sync + discard rule + live events
  // (the only agent-derived consumption path after the hard cutover).
  const contractStream = useContractStream(sseWorkspaceId, {
    onEvent: (evt) => handleContractABIEvent(evt),
    onSnapshot: () => { lastContractSnapshotAtRef.current = Date.now(); },
    onReconnect: () => {
      // A stream gap may have missed MESSAGE_END events whose messages
      // are already persisted — refetch page 1 so the transcript catches
      // up; the fresh snapshot replaces in-flight state exactly.
      if (workspaceId) {
        queryClient.invalidateQueries({ queryKey: ["workspace-status", workspaceId] });
      }
      void queue.refreshQueue();
      void reconcileOnIdle();
    },
  });

  // I12 prompt sync (pod-wide fold): the fold's pendingInputs are the
  // projection-authoritative standing prompts — seeded from the snapshot
  // alone (no extra fetch) and kept current by INPUT_REQUEST/RESOLVED
  // events. The fold is pod-wide, so subtask sessions' prompts are
  // included; root routing (a subtask's prompt bubbling to the parent
  // view) resolves through the loaded sessions list's parentId chain —
  // the same resolution the API-side emitter performed for the retired
  // user-stream copies (US-69.11 deletes those emitters).
  const resolvedInputIdsRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (!workspaceId || !sessionId) return;

    const parentOf = (sid: string): string | undefined =>
      sessionsListData?.find((s) => s.id === sid)?.parentId;
    // Inputs visible from this view: the viewed session's own, and any
    // session whose parent is the viewed session (subtask bubbling).
    const visibleInputs: Array<{ input: InputRequest; rootSessionId: string }> = [];
    for (const [sid, snap] of contractStream.sessions) {
      const root = sid === sessionId ? sid : parentOf(sid) === sessionId ? sessionId : undefined;
      if (root === undefined) continue;
      for (const input of snap.pendingInputs) visibleInputs.push({ input, rootSessionId: root });
    }

    for (const { input, rootSessionId } of visibleInputs) {
      // An input the user just answered in this view: do not re-add from
      // the fold until the projection drops it (INPUT_RESOLVED / fresh
      // snapshot) — that is the answer→event latency window.
      if (resolvedInputIdsRef.current.has(input.id)) continue;
      if (input.kind === InputKind.PERMISSION) {
        addPendingPermission(workspaceId, {
          id: input.id,
          session_id: input.sessionId,
          root_session_id: rootSessionId,
          permission: input.permission,
          patterns: [...input.patterns],
          always: [...input.always],
          ...(input.tool ? { tool: { message_id: input.tool.messageId, call_id: input.tool.callId } } : {}),
        });
      } else {
        addPendingQuestion(workspaceId, {
          id: input.id,
          session_id: input.sessionId,
          root_session_id: rootSessionId,
          questions: [{
            question: input.question,
            header: input.header,
            options: input.options.map((o) => ({ label: o.label, description: o.description })),
            multiple: input.multiple,
          }],
          ...(input.tool ? { tool: { message_id: input.tool.messageId, call_id: input.tool.callId } } : {}),
        });
      }
    }

    // Removals: prompts visible from this view that the fold no longer
    // carries were resolved outside this view's event flow. A resolved-id
    // latch clears once the fold drops the input.
    const pendingIds = new Set(visibleInputs.map((v) => v.input.id));
    for (const id of [...resolvedInputIdsRef.current]) {
      if (!pendingIds.has(id)) resolvedInputIdsRef.current.delete(id);
    }
    const isOwn = (sid: string) => sid === sessionId || parentOf(sid) === sessionId;
    for (const q of pendingQuestions) {
      if (isOwn(q.session_id) && !pendingIds.has(q.id) && !resolvedInputIdsRef.current.has(q.id)) removePendingAction(q.id);
    }
    for (const perm of pendingPermissions) {
      if (isOwn(perm.session_id) && !pendingIds.has(perm.id) && !resolvedInputIdsRef.current.has(perm.id)) removePendingAction(perm.id);
    }
    // The sync is idempotent (adds are first-wins by request id; removals
    // key off the fold), so re-running on prompt-store changes converges.
  }, [workspaceId, sessionId, contractStream, sessionsListData, pendingQuestions, pendingPermissions, addPendingQuestion, addPendingPermission, removePendingAction]);

  // Auto-abort sessions stuck on a question/permission tool that the agent
  // lost from its queue (e.g. the harness restarting while a question was
  // pending; agentd reseeds its projection from the store on boot, so a
  // lost question is absent from the fold by construction — I3/I12).
  //
  // Trigger: the fold says the viewed session is BUSY + history has loaded
  // + the last assistant message ends with a question/permission tool in
  // "running" state + no pending inputs in the fold (projection-authoritative)
  // + no prompts in the provider store + a stamped snapshot received AFTER
  // this page started viewing the session. A short dwell then guards the
  // cut→observe race where a just-asked question is not yet in the fold.
  //
  // After abort we reconcile history and surface an "interrupted" banner.
  const pendingPromptCount = pendingQuestions.length + pendingPermissions.length;
  const foldViewedSession = sessionId ? contractStream.sessions.get(sessionId) : undefined;
  useEffect(() => {
    const stuckToolPresent = (() => {
      const h = history ?? lastHistoryRef.current;
      if (!h || h.length === 0) return false;
      const lastAssistant = [...h].reverse().find((m) => m.role === "assistant");
      if (!lastAssistant) return false;
      return lastAssistant.parts.some(
        (p) =>
          p.type === "tool_use" &&
          p.toolState === "running" &&
          (p.text?.startsWith("question") || p.text?.startsWith("permission")),
      );
    })();
    if (history) lastHistoryRef.current = history;
    const strongEvidenceHold =
      !!workspaceId &&
      !!sessionId &&
      stuckToolPresent &&
      !hasAutoAbortedRef.current &&
      pendingPromptCount === 0 &&
      foldViewedSession?.status === SessionStatus.BUSY &&
      (foldViewedSession?.pendingInputs.length ?? 0) === 0 &&
      lastContractSnapshotAtRef.current > sessionMountedAtRef.current;
    if (!strongEvidenceHold) {
      abortDwellStartRef.current = null;
      return;
    }
    if (abortDwellStartRef.current === null) {
      abortDwellStartRef.current = Date.now();
    }

    // All evidence holds — dwell briefly so a question registering in the
    // projection between the snapshot cut and its fold publication (which
    // would arrive as an INPUT_REQUEST and break the evidence, clearing
    // the anchor) cannot be killed.
    const remaining = Math.max(0, AUTO_ABORT_DWELL_MS - (Date.now() - abortDwellStartRef.current));
    const timer = setTimeout(() => {
      if (hasAutoAbortedRef.current) return;
      if (abortDwellStartRef.current === null) return;
      // A user send during the dwell resets the abort anchor (doSendNow)
      // — this session is actively in use, not stuck.
      hasAutoAbortedRef.current = true;
      workspacesApi.abortSession(workspaceId!, sessionId!)
        .then(() => { setSessionWasInterrupted(true); reconcileOnIdle(); })
        .catch(() => { setSessionWasInterrupted(true); reconcileOnIdle(); });
    }, remaining);
    return () => clearTimeout(timer);
  }, [workspaceId, sessionId, history, pendingPromptCount, foldViewedSession, reconcileOnIdle]);
  const hasAutoRenamedRef = useRef(false);
  useEffect(() => {
    if (!sessionTitle || !workspaceName || !workspaceId || hasAutoRenamedRef.current) return;
    // Skip temporary opencode titles (e.g. "New session - 2026-05-27T23:03:56.256Z")
    if (/^New session\s*-\s*\d{4}-/.test(sessionTitle)) return;
    // Detect auto-generated name: adjective-noun-number OR "New session - <timestamp>"
    const isAutoName = /^[a-z]+-[a-z]+-\d+$/.test(workspaceName) ||
      /^New session\s*-\s*\d{4}-/.test(workspaceName);
    if (isAutoName) {
      hasAutoRenamedRef.current = true;
      workspacesApi.renameWorkspace(workspaceId, sessionTitle).then(() => {
        queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      });
    }
  }, [sessionTitle, workspaceName, workspaceId, queryClient]);
  const [sseStreamParts, setSseStreamParts] = useState<StreamPart[]>([]);

  // upsertStreamPart applies one rendered part bubble keyed by entity ID
  // (part id, or the tool call id for tool parts): the I12 stitch rule for
  // the live buffer — START/END replace, nothing matches on text.
  const upsertStreamPart = useCallback((next: StreamPart) => {
    setSseStreamParts((prev) => {
      const keyOf = (p: StreamPart) => p.type === "tool" ? (p.toolCallID ?? `tool:${p.partID ?? ""}`) : (p.partID ?? `${p.type}:${prev.indexOf(p)}`);
      const target = next.type === "tool" ? (next.toolCallID ?? `tool:${next.partID ?? ""}`) : next.partID;
      if (target !== undefined) {
        const idx = prev.findIndex((p) => keyOf(p) === target);
        if (idx >= 0) {
          const updated = [...prev];
          // The agent rewrites the tool start time on later part
          // snapshots; anchor to the FIRST-seen value so the elapsed
          // badge doesn't reset.
          updated[idx] = { ...next, toolStartedAt: prev[idx]!.toolStartedAt ?? next.toolStartedAt };
          return updated;
        }
      }
      return [...prev, next];
    });
  }, []);

  const appendStreamDelta = useCallback((partId: string, delta: string, messageID: string | undefined) => {
    if (!delta) return;
    setSseStreamParts((prev) => {
      const idx = prev.findIndex((p) => p.partID === partId);
      if (idx < 0) {
        // Mirror the server projection: a delta for an unseen part id
        // materializes the streaming text part.
        return [...prev, { type: "text", text: delta, partID: partId, messageID }];
      }
      const updated = [...prev];
      updated[idx] = { ...updated[idx]!, text: updated[idx]!.text + delta };
      return updated;
    });
  }, []);

  // handleContractABIEvent renders one ABI contract event (US-69.10: the
  // /contract-events stream is the only agent-derived stream the chat
  // consumes; events arrive post-discard-rule, in seq order).
  const handleContractABIEvent = useCallback((evt: Event) => {
    if (evt.sessionId && sessionId && evt.sessionId !== sessionId) {
      // Session-scoped events for other sessions only feed the sidebar
      // title (updated below); the viewed session's renderer ignores them.
      if (evt.type === EventType.SESSION_UPDATED && evt.session?.title && workspaceId) {
        queryClient.setQueryData<SessionListItem[]>(["sessions", workspaceId], (old) => {
          if (!old) return old;
          return old.map((s) => s.id === evt.sessionId ? { ...s, title: evt.session!.title } : s);
        });
      }
      return;
    }

    switch (evt.type) {
      case EventType.SESSION_STATUS: {
        if (!workspaceId) return;
        queryClient.invalidateQueries({ queryKey: ["sessions", workspaceId] });
        if (evt.status === SessionStatus.IDLE) {
          notifySessionIdle(evt.sessionId);
          clearStreamTimedOut();
          setHungAlert((prev) => (prev && prev.session_id === evt.sessionId ? null : prev));
          reconcileOnIdle();
          queue.refreshQueue();
          // US-16.12: Clear stale prompts on session idle (global, scoped to
          // this session — survives across views, cleared when idle).
          clearSessionPendingPrompts(evt.sessionId);
        }
        return;
      }
      case EventType.SESSION_UPDATED: {
        const title = evt.session?.title;
        if (title && workspaceId) {
          queryClient.setQueryData<SessionListItem[]>(["sessions", workspaceId], (old) => {
            if (!old) return old;
            return old.map((s) => s.id === evt.sessionId ? { ...s, title } : s);
          });
        }
        return;
      }
      case EventType.MESSAGE_START: {
        if (evt.message?.type === MessageType.USER && evt.message.id) {
          knownUserMessageIds.current.add(evt.message.id);
        }
        return;
      }
      case EventType.MESSAGE_END: {
        // Real-time context usage (per-step occupancy semantics): the
        // ABI translator never maps session-updated tokens to
        // contextUsage (they are cumulative); the step's tokens are the
        // numerator — same rule the old US-65.8 bridge guaranteed.
        const cost = evt.message?.cost;
        if (evt.sessionId && cost) {
          const used = Number(cost.inputTokens) + Number(cost.cacheReadTokens) + Number(cost.cacheWriteTokens);
          if (used > 0) {
            contextBySessionRef.current.set(evt.sessionId, used);
            setContextVersion((v) => v + 1);
          }
        }
        return;
      }
      case EventType.PART_START:
      case EventType.PART_END: {
        // Echo gate (I12): parts belonging to a known USER message are the
        // harness echoing the user's own text — dropped by message ID.
        if (evt.messageId && knownUserMessageIds.current.has(evt.messageId)) return;
        const part = evt.part;
        if (!part) return;
        const bubble = partToStreamPart(part, evt.messageId || undefined);
        if (bubble) upsertStreamPart(bubble);
        return;
      }
      case EventType.PART_DELTA: {
        if (evt.messageId && knownUserMessageIds.current.has(evt.messageId)) return;
        if (evt.partId) appendStreamDelta(evt.partId, evt.delta, evt.messageId || undefined);
        return;
      }
      case EventType.ERROR: {
        if (!sessionId) return;
        const code = evt.error?.code ?? "";
        const rawMessage = evt.error?.message ?? "";
        setSessionErrors((prev) => [...prev, {
          id: `error-${++idCounterRef.current}`,
          role: "assistant",
          parts: [{ type: "error" as const, text: `⚠️ ${sessionErrorText(code, rawMessage)}` }],
        }]);
        // US-16.12: Clear stale prompts on session error (global, scoped).
        clearSessionPendingPrompts(sessionId);
        return;
      }
      default:
        return;
    }
  }, [workspaceId, sessionId, queryClient, notifySessionIdle, reconcileOnIdle, queue, clearSessionPendingPrompts, clearStreamTimedOut, upsertStreamPart, appendStreamDelta]);

  // Platform events only: the workspace stream's session-state dialect is
  // deleted (hard cutover); workspace.phase/alert, queue.update, and
  // agent_died are API-owned platform events with no contract-stream
  // equivalent yet (tracker retirement is US-69.11).
  const handleSSEEvent = useCallback((data: unknown) => {
    const event = data as WorkspaceStreamEvent;
    if (!event?.type) return;

    if (event.type === "workspace.phase") {
      queue.onPhaseChange(event.phase);
      return;
    }

    // D6 (#998): notify-only hung-session escalation. Banner until
    // dismissed; a contract SESSION_STATUS idle for the same session
    // auto-clears it (the hang resolved).
    if (event.type === "workspace.alert" && event.data?.alert === "session_hung") {
      setHungAlert(event);
      return;
    }

    if (event.type === "queue.update" && workspaceId) {
      const qe = (event.data ?? {}) as { event?: string; messageID?: string; error?: string };
      if (qe.event === "sent") {
        // Targeted removal first (instant), then the refresh as
        // authoritative catch-up — the GET can transiently race the
        // server-side LRem.
        if (qe.messageID) queue.removeById(qe.messageID);
        void queue.refreshQueue();
      } else if (qe.event === "enqueued" || qe.event === "delivering") {
        // delivering = the worker picked the entry up (POST in flight).
        // The refresh drops it from display: the turn may run for
        // minutes and the message is already owned by the agent — it
        // must not render as "queued" for that whole window.
        void queue.refreshQueue();
      } else if (qe.event === "error" && qe.messageID) {
        queue.markError(qe.messageID, qe.error ?? "Send failed");
      } else if (qe.event === "dismissed" && qe.messageID) {
        queue.removeById(qe.messageID);
      }
      return;
    }

    if (event.type === "agent_died") {
      setAgentDied(true);
      if (event.data?.message) setAgentDiedMessage(event.data.message);
      return;
    }

    console.debug("[ChatPage] unhandled platform event type:", event.type);
  }, [workspaceId, queue]);

  // Connect the platform stream unconditionally (even before workspace is
  // Active) so we can detect the Pending→Active phase transition and
  // auto-create a session.
  useEventStream(sseWorkspaceId, handleSSEEvent, {
    onReconnect: () => {
      if (workspaceId) {
        queryClient.invalidateQueries({ queryKey: ["workspace-status", workspaceId] });
      }
      void queue.refreshQueue();
    },
  });

  // doSendNow MUST be defined before the early return below.
  // Placing any hook after an early return violates the Rules of Hooks — React
  // throws error #310 ("Rendered more hooks than during the previous render").
  const doSendNow = (text: string, files: string[]) => {
    // Resolve current model selection into opencode's PromptInput.model format.
    // currentModel is the flat model ID stored in the DB (e.g. "glm-5.1", never
    // "provider/model"). The backend resolves the providerID and returns it as
    // currentModelProviderID. Fall back to a find() on the models array for
    // older API responses that don't include currentModelProviderID, or when
    // the backend detected a collision (currentModelProviderID === "").
    const currentModelRef = (() => {
      const id = modelsData?.currentModel;
      if (!id) return undefined;
      const providerID =
        modelsData?.currentModelProviderID ||
        modelsData?.models?.find((m) => m.id === id)?.providerID;
      if (!providerID) return undefined;
      return { providerID, modelID: id };
    })();

    setSseStreamParts([]);
    knownUserMessageIds.current.clear();
    // A send is active use — drop the abort anchor outright (review round 5
    // recommendation). If stuck evidence genuinely re-establishes later, it
    // re-anchors and re-dwells from scratch; no zero-dwell carryover.
    abortDwellStartRef.current = null;
    const userMsg: Message = {
      id: `local-${++idCounterRef.current}`,
      role: "user",
      parts: [{ type: "text", text }],
      createdAt: new Date().toISOString(),
    };
    setLocalMessages((prev) => [...prev, userMsg]);
    // Note: we deliberately do NOT add the assistant response to
    // localMessages here. The streaming bubble shows it during streaming,
    // and reconcileOnIdle's history refetch is authoritative once idle.
    // Adding it here causes a race with reconcileOnIdle: if reconcile's
    // refetch resolves first (clears localMessages, populates history),
    // then this onComplete re-adds the assistant message → it renders
    // twice (once from history, once from localMessages).
    // The user message stays in localMessages until reconcileOnIdle clears
    // it (after history catches up), preserving optimistic UX.
    send(text, (_msg: Message) => {
      reconcileOnIdle();
    }, currentModelRef, files);
  };

  const allMessages = [...(history ?? []), ...localMessages, ...sessionErrors];

  if (!workspaceId) {
    return (
      <div className="flex h-full items-center justify-center text-muted-foreground">
        <p>Select a workspace to start chatting</p>
      </div>
    );
  }

  const isSuspended = status?.phase === "Suspended";
  const isTransitioning = !status?.phase || status?.phase === "Pending" || status?.phase === "Creating" || status?.phase === "Resuming" || status?.phase === "Suspending";
  const phaseLabel = status?.phase ? status.phase.toLowerCase() : "loading";

  const handleSend = (text: string, files: string[]) => {
    // If busy OR there are still messages waiting in the queue, hold the
    // new message in the queue too. Without the queue-length check, a
    // direct send races ahead of the drain goroutine when the session
    // transitions busy→idle: opencode assigns the direct send an earlier
    // info.time.created than the still-draining queued message, so on
    // next reload selectChronological (sort by createdAt) places the
    // queued message AFTER the direct send — out of FIFO order.
    //
    // Only PENDING entries hold the gate: a parked error pill is not
    // in flight — holding every future send hostage to a manual
    // Retry/Dismiss would permanently reroute sends through the queue
    // path (which skips the direct-send echo strip).
    //
    // Residual: there is still a small window between the queue emptying
    // client-side and opencode finishing persistence of the drained
    // message; a direct send in that window can still race. Closing it
    // requires a server-side check in SendPromptAsync — out of scope for
    // this fix.
    if (isSessionBusy || streaming || queue.queuedMessages.some((m) => m.status === "pending")) {
      queue.enqueue(text, files);
      composerAttachments.clearAttached();
      return;
    }
    doSendNow(text, files);
    composerAttachments.clearAttached();
  };

  const sessionDisplayName = sessionTitle || "New chat";
  const kebabItems: KebabMenuItem[] = [
    {
      label: "Copy link",
      onClick: () => navigator.clipboard.writeText(`${window.location.origin}/chat/${workspaceId}/${sessionId}`),
    },
    {
      label: "Rename session",
      onClick: () => {
        let name: string | null = null;
        try { name = window.prompt("Session name:", sessionDisplayName); } catch { /* blocked */ }
        if (name && name.trim() && workspaceId && sessionId) {
          workspacesApi.renameSession(workspaceId, sessionId, name.trim()).then(() => {
            queryClient.invalidateQueries({ queryKey: ["sessions", workspaceId] });
            queryClient.invalidateQueries({ queryKey: ["session-title", workspaceId, sessionId] });
          });
        }
      },
    },
    {
      label: "Force Stop",
      onClick: () => {
        if (!workspaceId || !sessionId) return;
        workspacesApi.abortSession(workspaceId, sessionId)
          .catch(() => {
            try { window.alert("Failed to force stop session."); } catch { /* blocked */ }
          });
      },
    },
    {
      label: "Delete session",
      onClick: () => {
        if (!workspaceId || !sessionId) return;
        confirmDelete({
          title: "Delete this session?",
          description: "This action cannot be undone.",
          confirmLabel: "Delete",
          destructive: true,
          onConfirm: () => {
            workspacesApi.deleteSession(workspaceId, sessionId)
              .catch((err: unknown) => {
                if (err instanceof ApiClientError && err.status === 404) return;
                throw err;
              })
              .then(() => {
                queryClient.invalidateQueries({ queryKey: ["sessions", workspaceId] });
                navigate(`/chat/${workspaceId}`);
              })
              .catch(() => {
                try { window.alert("Failed to delete session."); } catch { /* blocked */ }
              });
          },
        });
      },
      destructive: true,
    },
  ];

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center justify-between border-b border-border px-4 py-2">
        <div className="flex items-center gap-2 min-w-0">
          <h2 className="text-sm font-semibold truncate">
            <span className="text-muted-foreground">{workspaceName}</span>
            <span className="text-muted-foreground/50 mx-1">/</span>
            <span>{sessionDisplayName}</span>
          </h2>
          {activeRuns && activeRuns.length > 0 && (
            <span className="flex items-center gap-1 rounded-md border border-blue-500/30 bg-blue-500/10 px-2 py-0.5 text-xs text-blue-500 shrink-0">
              <span className="inline-block h-1.5 w-1.5 animate-pulse rounded-full bg-blue-500" />
              {activeRuns.length} active run{activeRuns.length > 1 ? "s" : ""}
            </span>
          )}
        </div>
        <div className="flex items-center gap-2">
          <KebabMenu items={kebabItems} footer={[
            ...(status?.agentHealth?.agentVersion ? [`opencode v${status.agentHealth.agentVersion}`] : []),
            ...(status?.imageTag ? [`image: ${status.imageTag}`] : []),
          ]} />
        </div>
      </div>

      {isSuspended && (
        <SuspendedBanner
          workspaceName={workspaceId}
          onActivate={() => {
            wsLog("ui.user_clicked_activate", workspaceId);
            activateMutation.mutate(workspaceId);
          }}
          activating={activateMutation.isPending}
        />
      )}

      {isReady && activeWorkspaceData?.agentNeedsRefresh && (
        <AgentReloadBanner
          workspaceId={workspaceId!}
          workspaceName={workspaceName || "this workspace"}
          credentialsPendingSince={activeWorkspaceData.credentialsPendingSince}
          onReloaded={() => {
            queryClient.invalidateQueries({ queryKey: ["workspaces"] });
          }}
        />
      )}

      {isTransitioning && (
        <div className="flex flex-1 flex-col items-center justify-center gap-4 text-muted-foreground">
          <Spinner size="lg" />
          <div className="text-center">
            <p className="text-base font-medium">Workspace is {phaseLabel}...</p>
            <p className="mt-1 text-sm">This usually takes a few seconds</p>
          </div>
        </div>
      )}

      {isReady && (
        <HealthBanner
          credentialState={status?.credentialState}
          agentHealth={status?.agentHealth}
        />
      )}

      {isReady && sessionWasInterrupted && (
        <div className="flex items-center gap-2 border-b border-yellow-200 bg-yellow-50 px-4 py-2 text-xs text-yellow-800 dark:border-yellow-800 dark:bg-yellow-950 dark:text-yellow-200">
          <span>⚠ Session was interrupted while waiting for your input. You can continue in this session or start a new one.</span>
          <button
            className="ml-auto shrink-0 underline hover:no-underline"
            onClick={() => setSessionWasInterrupted(false)}
          >
            Dismiss
          </button>
        </div>
      )}

      {isReady && agentDied && (
        <div role="alert" className="flex items-center gap-2 border-b border-yellow-200 bg-yellow-50 px-4 py-2 text-xs text-yellow-800 dark:border-yellow-800 dark:bg-yellow-950 dark:text-yellow-200">
          <span>⚠ {agentDiedMessage ?? "The agent stopped responding and is being restarted automatically. Reconnecting…"}</span>
          <button
            className="ml-auto shrink-0 underline hover:no-underline"
            onClick={() => setAgentDied(false)}
          >
            Dismiss
          </button>
        </div>
      )}

      {isReady && (
        <DiskUsageBar
          diskUsedBytes={status?.diskUsedBytes}
          diskTotalBytes={status?.diskTotalBytes}
          memoryUsedBytes={status?.memoryUsedBytes}
          memoryTotalBytes={status?.memoryTotalBytes}
          contextUsed={contextUsedForDisplay ?? 0}
          contextTotal={status?.contextTotal ?? 0}
        />
      )}

      {compactionDetected && (
        <div className="flex items-center justify-between gap-2 border-b border-blue-500/30 bg-blue-500/10 px-4 py-2 text-xs text-blue-700 dark:text-blue-300">
          <span>Context compacted — conversation history was summarised to free context space.</span>
          <button onClick={() => setCompactionDetected(false)} className="underline hover:no-underline shrink-0">Dismiss</button>
        </div>
      )}

      {atCapRetryAfter !== null && (
        <AtCapBanner retryAfter={atCapRetryAfter} onRetry={clearAtCap} />
      )}

      {hungAlert && (
        <div className="flex items-center justify-between gap-2 border-b border-amber-500/50 bg-amber-500/10 px-4 py-3 text-sm text-amber-600 dark:text-amber-400">
          <span>
            This session has been busy for {Math.round(hungAlert.data.oldest_busy_seconds / 60)} min without
            completing — it may be hung. Nothing was stopped automatically; abort or resume manually.
          </span>
          <button onClick={() => setHungAlert(null)} className="underline hover:no-underline shrink-0">Dismiss</button>
        </div>
      )}

      {streamTimedOut && (
        <div className="flex items-center justify-between gap-2 border-b border-destructive/50 bg-destructive/10 px-4 py-3 text-sm text-destructive">
          <span>Response interrupted — the agent may be processing a large operation or recovering. Try resending your message.</span>
          <button onClick={clearStreamTimedOut} className="underline hover:no-underline">Dismiss</button>
        </div>
      )}

      {chatError && (
        <div className="flex flex-col gap-1 border-b border-destructive/50 bg-destructive/10 px-4 py-3 text-sm text-destructive">
          <div className="flex items-center justify-between gap-2">
            <span>{chatError}</span>
            <button onClick={clearError} className="shrink-0 underline hover:no-underline">Dismiss</button>
          </div>
        </div>
      )}

      {/* LLMSafeSpaces#490: surface message-history query failures as an
          inline diagnostic banner rather than silently rendering an
          empty state. Companion to the server-side observability in
          #488 — the banner exposes opencode's err_XXXXXXXX ref so
          operators can cross-reference workspace-pod logs. */}
      {historyIsError && (
        <ChatHistoryErrorBanner
          error={historyError}
          onRetry={() => void historyRefetch()}
        />
      )}

      {historyLoading || createSessionMutation.isPending ? (
        <div className="flex flex-1 items-center justify-center">
          <Spinner />
        </div>
      ) : (
        <div className="flex-1 min-h-0">
          <ChatView
            messages={allMessages}
            streaming={streaming}
            streamParts={sseStreamParts}
            disabled={!workspaceId || !sessionId || isSuspended}
            onSend={handleSend}
            onAbort={() => {
              if (workspaceId && sessionId) {
                workspacesApi.abortSession(workspaceId, sessionId);
              }
              void queue.clearAll();
            }}
            onLoadEarlier={() => fetchNextPage()}
            hasOlderMessages={hasNextPage}
            loadingOlder={isFetchingNextPage}
            queuedMessages={queue.queuedMessages}
            onQueueRetry={queue.retry}
            onQueueDismiss={queue.dismiss}
            models={modelsData?.models}
            lastSeenAt={lastSeenAt}
            userMessageHistory={userMessageHistory}
            viewOnly={isSubtask}
            workspaceId={workspaceId}
            orgId={activeWorkspaceData?.orgId}
            attachments={composerAttachments.chips}
            capViolation={composerAttachments.capViolation}
            onAddFiles={composerAttachments.addFiles}
            onRemoveAttachment={composerAttachments.remove}
            onRetryAttachment={composerAttachments.retry}
            onDismissCapViolation={composerAttachments.dismissCapViolation}
            prompts={
              (pendingQuestions.length > 0 || pendingPermissions.length > 0) ? (
                <>
                  {pendingQuestions.map((q) => (
                    <QuestionPrompt key={q.id} workspaceId={workspaceId!} request={q}
                      onResolved={() => { resolvedInputIdsRef.current.add(q.id); removePendingAction(q.id); }} />
                  ))}
                  {pendingPermissions.map((p) => (
                    <PermissionPrompt key={p.id} workspaceId={workspaceId!} request={p}
                      onResolved={() => { resolvedInputIdsRef.current.add(p.id); removePendingAction(p.id); }} />
                  ))}
                </>
              ) : undefined
            }
          />
        </div>
      )}
      {confirmDialog}
    </div>
  );
}
