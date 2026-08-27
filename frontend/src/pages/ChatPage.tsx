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
import { SessionRetryBanner, type RetryStatus } from "../components/chat/SessionRetryBanner";
import { AgentReloadBanner } from "../components/workspace/AgentReloadBanner";
import { DiskUsageBar } from "../components/workspace/DiskUsageBar";
import { Spinner } from "../components/ui/Spinner";
import { KebabMenu } from "../components/ui/KebabMenu";
import type { KebabMenuItem } from "../components/ui/KebabMenu";
import { sessionsApi } from "../api/sessions";
import type { Message, SessionListItem, WorkspaceStreamEvent, SessionContractEvent, SessionStatusEvent, ContractEvent, QuestionRequest, PermissionRequest, WorkspaceAlertEvent } from "../api/types";
import { QuestionPrompt } from "../components/chat/QuestionPrompt";
import { PermissionPrompt } from "../components/chat/PermissionPrompt";
import { useClearPendingUnread, useAddPendingQuestion, useAddPendingPermission, useRemovePendingAction, usePendingQuestionsForSession, usePendingPermissionsForSession, useClearSessionPendingPrompts, useIsSessionBusy, useWorkspaceInputSnapshot } from "../providers/SessionActivityProvider";

type StreamPart = { type: "text" | "thinking" | "tool"; text: string; toolState?: string; toolStartedAt?: string; toolCallID?: string; toolInput?: unknown; toolOutput?: string; messageID?: string };

// Reconnect-mode activation window. Reconnect mode ("mounted into an
// in-progress run") may only ARM within this long of the page mounting into
// the session (or an SSE reconnect) — never from a mid-session busy
// transition (e.g. the 60s send timeout dropping localStreaming while the
// session legitimately stays busy waiting for an answer). Without the
// window, that mid-session transition armed the stuck-session auto-abort
// against live prompts (the "Session was interrupted" false positive).
const RECONNECT_ACTIVATION_WINDOW_MS = 15_000;

// Dwell before the stuck-session auto-abort fires once all evidence
// conditions hold. Guards the sub-second race where a question registers in
// opencode's queue between the snapshot fetch and its marker.
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

export function ChatPage() {
  const { workspaceId, sessionId } = useParams();
  const navigate = useNavigate();
  const { confirm: confirmDelete, dialog: confirmDialog } = useConfirmDialog();
  const [localMessages, setLocalMessages] = useState<Message[]>([]);
  // sessionErrors holds error messages surfaced by session.error SSE events.
  // Kept separate from localMessages so they survive between send and idle.
  // Cleared in reconcileOnIdle (session goes idle → history is authoritative)
  // and on session change.
  const [sessionErrors, setSessionErrors] = useState<Message[]>([]);
  const queryClient = useQueryClient();

  useEffect(() => {
    setLocalMessages([]);
    setSessionErrors([]);
    setSseStreamParts([]);
    setRetryStatus(null);
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

  const { send, streaming, localStreaming, notifySessionIdle, error: chatError, clearError, atCapRetryAfter, clearAtCap, streamTimedOut, clearStreamTimedOut } = useChatStream(activeWorkspaceId, sessionId, isSessionBusy);

  // Ref mirrors so async continuations (reconcileOnIdle's post-refetch
  // code) read the CURRENT busy/streaming state rather than the stale
  // closure captured when the callback was created.
  const isSessionBusyRef = useRef(false);
  isSessionBusyRef.current = isSessionBusy;
  const localStreamingRef = useRef(false);
  localStreamingRef.current = localStreaming;
  const [retryStatus, setRetryStatus] = useState<RetryStatus | null>(null);
  const sessionTitle = useSessionTitle(activeWorkspaceId, sessionId, isReady, streaming);

  // US-15.3: Compute historyPartIds from fetched history for boundary detection.
  // Holds BOTH part ids and tool call ids — events are matched on either —
  // because a history part and its live event share the call id even when
  // the part id is unavailable on one side.
  const historyPartIds = useRef<Set<string>>(new Set());
  useEffect(() => {
    const ids = new Set<string>();
    if (history) {
      for (const msg of history) {
        for (const part of msg.parts) {
          if (part.id) ids.add(part.id);
          if (part.toolCallId) ids.add(part.toolCallId);
        }
      }
    }
    historyPartIds.current = ids;
  }, [history]);

  // US-15.4: Reconnect mode — active when page loads into a busy session
  const isReconnectMode = useRef(false);
  const knownLivePartIds = useRef<Set<string>>(new Set());
  // Dwell anchor: set when all abort evidence first holds; cleared when any
  // evidence breaks (or on session change — evidence is per-session).
  // A persistent anchor (not a fresh timer) means frequent effect re-runs
  // (history refetches on SSE reconnect churn) cannot defer the abort
  // indefinitely — each timer schedules only the REMAINING dwell.
  const abortDwellStartRef = useRef<number | null>(null);
  // Last-known history for the stuck-tool check. A workspace-status refetch
  // gap can momentarily gate the messages query off (history === undefined
  // while isReady flickers) — the stuck tool did not vanish; evaluating the
  // last-known transcript keeps the anchor from being reset by refetch churn.
  // Reset on session change (N4): stale transcripts must never satisfy the
  // stuck-tool check for the newly-viewed session.
  const lastHistoryRef = useRef<Message[] | null>(null);
  // F1 fix: activation is only permitted within RECONNECT_ACTIVATION_WINDOW_MS
  // of mount/session-change or an SSE reconnect. sessionMountedAt feeds the
  // auto-abort snapshot gate (only snapshots newer than this page's view of
  // the session count as evidence) — it is intentionally STABLE across SSE
  // reconnect churn: a per-reconnect reset livelocks against the user
  // stream's own reconnect cadence (each re-arm would demand a newer
  // snapshot, perpetually clearing the abort dwell anchor).
  const reconnectWindowOpenRef = useRef(true);
  const sessionMountedAtRef = useRef(0); // set in the session-change effect (runs on mount) — keeps render pure
  const reconnectWindowTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

  const armReconnectWindow = useCallback(() => {
    reconnectWindowOpenRef.current = true;
    if (reconnectWindowTimerRef.current) clearTimeout(reconnectWindowTimerRef.current);
    reconnectWindowTimerRef.current = setTimeout(() => {
      reconnectWindowOpenRef.current = false;
    }, RECONNECT_ACTIVATION_WINDOW_MS);
  }, []);

  const [sessionWasInterrupted, setSessionWasInterrupted] = useState(false);
  const [agentDied, setAgentDied] = useState(false);
  const [agentDiedMessage, setAgentDiedMessage] = useState<string | null>(null);
  const hasAutoAbortedRef = useRef(false);

  // Reset reconnect state on session change — MUST be defined before the
  // reconnect-mode activation effect below so it runs first on mount.
  useEffect(() => {
    isReconnectMode.current = false;
    hasAutoAbortedRef.current = false;
    knownLivePartIds.current.clear();
    // Queued-text echo tracking is per-session: entries in another
    // session's outbox deliver there, and their echoes must never strip
    // this session's stream. Deliberately NOT cleared on reconcile — a
    // mid-turn SSE reconnect between enqueue and late delivery must not
    // lose the strip (the reconnect boundary gate only covers parts
    // already in history at refetch time).
    pendingQueuedTextsRef.current.clear();
    setSessionWasInterrupted(false);
    setAgentDied(false);
    setAgentDiedMessage(null);
    // S36.4: Reset compaction state when navigating to a different session
    prevContextUsedRef.current = undefined;
    setCompactionDetected(false);
    // F1 fix: open the activation window for the newly-mounted session and
    // stamp this view's mount time (auto-abort snapshot-evidence gate).
    sessionMountedAtRef.current = Date.now();
    // N4: per-session abort state must not leak across the switch — see the
    // lastHistoryRef declaration comment above.
    lastHistoryRef.current = null;
    abortDwellStartRef.current = null;
    armReconnectWindow();
    return () => {
      if (reconnectWindowTimerRef.current) clearTimeout(reconnectWindowTimerRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [sessionId]);

  // Enter reconnect mode when session is busy — MUST be after the session-change
  // reset effect so it runs second on mount and isn't cleared.
  // F1 fix: only while the activation window is open (mount / SSE reconnect).
  // R1 (review round 3): sessionId is a dep — a busy→busy same-workspace
  // switch (isSessionBusy never transitions) must re-run the effect to
  // re-arm after the session-change reset cleared isReconnectMode, or the
  // stuck-session recovery never fires for the newly-viewed session.
  useEffect(() => {
    if (reconnectWindowOpenRef.current && isSessionBusy && !localStreaming) {
      if (!isReconnectMode.current) {
        isReconnectMode.current = true;
        // F1/C3: guarantee a fresh input-snapshot flight exists after arming.
        // Page loads and SSE reconnects fire one server-side, but an
        // in-workspace session switch arms reconnect mode with no stream
        // (re)connect — without this trigger the auto-abort gate would wait
        // on a stale snapshot forever.
        if (workspaceId) {
          workspacesApi.requestInputSnapshot(workspaceId).catch(() => {});
        }
      }
    }
  }, [isSessionBusy, localStreaming, workspaceId, sessionId]);

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
      // Rebuild the boundary-gate ID set SYNCHRONOUSLY from the fresh
      // transcript. The [history] effect runs on the next commit — without
      // this, resumed SSE events arriving in that window are matched
      // against the pre-refetch (stale) set and re-append parts history
      // already renders (the duplicate-bubble bug: one bash run rendering
      // as both a history bubble and a stream bubble after reconnect).
      const freshIds = new Set<string>();
      for (const msg of msgs) {
        for (const part of msg.parts) {
          if (part.id) freshIds.add(part.id);
          if (part.toolCallId) freshIds.add(part.toolCallId);
        }
      }
      historyPartIds.current = freshIds;
      setSessionErrors([]);
      knownLivePartIds.current.clear();
      sentTextRef.current = "";
      activePartTypeRef.current = null;
      currentThinkingIdxRef.current = -1;
      currentTextIdxRef.current = -1;
      if (isSessionBusyRef.current || localStreamingRef.current) {
        // The session is STILL in flight (this reconcile came from an SSE
        // reconnect mid-turn, not a genuine idle). History now renders the
        // in-flight messages, so keep the boundary gate ARMED: resumed
        // part events for parts already in history must be dropped, or
        // they re-enter sseStreamParts as duplicates. Disarmed normally
        // by doSendNow (new user send) and the session-change reset.
        isReconnectMode.current = true;
      } else {
        isReconnectMode.current = false;
      }
      if (freshHistory) {
        void queue.refreshQueue();
      }
    } catch {
      // Reconnect history fetch is best-effort; stale state self-corrects on next poll.
    }
  }, [workspaceId, sessionId, queryClient, queue]);

  // Auto-abort sessions that are stuck on a question/permission tool that opencode
  // lost from its queue (e.g. due to opencode restarting while a question was pending).
  //
  // Trigger: reconnect mode (busy at page load / SSE reconnect) + history has
  // loaded + last assistant message ends with a question or permission tool in
  // "running" state + no pending questions/permissions arrived via SSE.
  //
  // F2 fix: additionally requires a SUCCESSFUL input snapshot
  // (agent.input.snapshot_complete ok:true) received AFTER reconnect mode
  // armed — opencode's queue was actually consulted and reports nothing
  // pending. A failed fetch (ok:false, e.g. opencode mid-restart) or a
  // pre-arming snapshot is not evidence. A short dwell then guards the
  // fetch→marker race where a just-asked question isn't in the fetched set.
  //
  // After abort we reconcile history and surface an "interrupted" banner.
  const pendingPromptCount = pendingQuestions.length + pendingPermissions.length;
  const inputSnapshot = useWorkspaceInputSnapshot(workspaceId ?? "");
  useEffect(() => {
    // Strong evidence: the conditions that, if they break, genuinely
    // invalidate the stuck-session diagnosis. The dwell ANCHOR survives
    // transient flips of isReconnectMode (reconcileOnIdle clears it on every
    // SSE reconnect; the window re-arms immediately) — otherwise reconnect
    // churn faster than the dwell would clear the anchor forever.
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
      !!inputSnapshot?.ok &&
      inputSnapshot.at > sessionMountedAtRef.current;
    if (!strongEvidenceHold) {
      abortDwellStartRef.current = null;
      return;
    }
    if (abortDwellStartRef.current === null) {
      abortDwellStartRef.current = Date.now();
    }

    // All evidence holds — dwell briefly so a question registering in
    // opencode's queue between the snapshot fetch and its marker (which
    // would arrive as an agent.question event and break the evidence,
    // clearing the anchor) cannot be killed.
    //
    // The timer is scheduled even while reconnect mode is momentarily
    // disarmed (reconcile churn clears it; a user send disarms it
    // permanently) — the CALLBACK re-checks isReconnectMode at fire time so
    // a disarmed state fails safe (no abort). Scheduling here rather than
    // only-when-armed matters: effect cleanups clear the timer on every dep
    // churn, so an armed-only schedule would silently cancel the dwell the
    // first time a render happens while disarmed.
    const remaining = Math.max(0, AUTO_ABORT_DWELL_MS - (Date.now() - abortDwellStartRef.current));
    const timer = setTimeout(() => {
      if (hasAutoAbortedRef.current) return;
      if (abortDwellStartRef.current === null) return;
      // B (review round 4): a user send during the dwell disarms reconnect
      // mode (doSendNow) — this session is actively in use, not stuck. The
      // anchor persists across reconnect churn by design, so the disarm must
      // be re-checked here, not only via effect deps (a send changes no dep).
      if (!isReconnectMode.current) return;
      hasAutoAbortedRef.current = true;
      workspacesApi.abortSession(workspaceId!, sessionId!)
        .then(() => { setSessionWasInterrupted(true); reconcileOnIdle(); })
        .catch(() => { setSessionWasInterrupted(true); reconcileOnIdle(); });
    }, remaining);
    return () => clearTimeout(timer);
  }, [workspaceId, sessionId, history, pendingPromptCount, inputSnapshot, reconcileOnIdle]);
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
  // Store the text the user just sent so we can strip the user echo from
  // the SSE stream. Opencode echoes the user's message as the first
  // message.part.updated event(s) before the assistant response begins.
  const sentTextRef = useRef<string>("");
  // Texts enqueued through the D3 queue that have not been observed in
  // the stream yet, with multiplicity (duplicate texts queue as
  // separate entries and each gets its own echo). The queue path skips
  // doSendNow (which primes sentTextRef), so without this map the
  // user-echo of a late-delivered queued message falls through to the
  // assistant stream buffer and renders the user's own text as an agent
  // bubble (suspend/resume casualty: entries park while the agent is
  // unreachable, then drain after resume). Matching an echo also clears
  // the pending pill — the agent owns the message now, it is in the
  // conversation, not the queue (TUI parity).
  const pendingQueuedTextsRef = useRef<Map<string, number>>(new Map());
  // Tracks which buffer to route message.part.delta events to.
  const activePartTypeRef = useRef<"user-echo" | "reasoning" | "text" | null>(null);
  const currentThinkingIdxRef = useRef<number>(-1);
  const currentTextIdxRef = useRef<number>(-1);

  // Returns the queued text this echo matches — exact, or composed with
  // a pure attachment manifest (Epic 67: echoes of sends with files come
  // back as prose + manifest) — decrementing its multiplicity; undefined
  // when the echo belongs to no pending queued message.
  const matchQueuedEcho = useCallback((echoText: string): string | undefined => {
    const pending = pendingQueuedTextsRef.current;
    if (pending.size === 0 || !echoText) return undefined;
    const take = (text: string) => {
      const n = pending.get(text) ?? 0;
      if (n <= 1) pending.delete(text);
      else pending.set(text, n - 1);
      return text;
    };
    if (pending.has(echoText)) return take(echoText);
    for (const t of pending.keys()) {
      if (echoText.startsWith(t)) {
        const remainder = echoText.slice(t.length);
        const parsedRemainder = parseAttachments(remainder);
        if (parsedRemainder.attachments !== null && parsedRemainder.text === "") {
          return take(t);
        }
      }
    }
    return undefined;
  }, []);

  // handleContractEvent renders one CONTRACT event (US-65.8: clients
  // consume pkg/session shapes only — the agent's wire names, envelopes,
  // and part shapes are translated server-side behind the adapter seam).
  const handleContractEvent = useCallback((ce: ContractEvent, currentSessionId: string) => {
    if (!ce?.type) return;
    if (ce.sessionId && ce.sessionId !== currentSessionId) return;

    // US-15.4 boundary gate: in reconnect mode, ignore events for parts
    // already rendered from history. Tool events match on the call id too
    // (part id may be absent on either side; call id is shared).
    if (isReconnectMode.current) {
      const livePartId = ce.partId || ce.part?.id;
      const liveCallId = ce.part?.tool?.callId;
      const inHistory = (id: string | undefined) => !!id && historyPartIds.current.has(id);
      if (ce.type === "part.delta") {
        if (inHistory(livePartId)) return;
        if (!knownLivePartIds.current.has(livePartId ?? "")) return;
      } else if (inHistory(livePartId) || inHistory(liveCallId)) {
        return;
      }
      if (livePartId) knownLivePartIds.current.add(livePartId);
    }

    if (ce.type === "part.delta") {
      const delta = ce.delta;
      if (!delta) return;
      const target = activePartTypeRef.current;
      if (target === "reasoning" || target === "text") {
        const expectedType = target === "reasoning" ? "thinking" : "text";
        setSseStreamParts((prev) => {
          if (prev.length === 0) return prev;
          const last: StreamPart | undefined = prev[prev.length - 1];
          if (!last || last.type !== expectedType) return prev;
          return [...prev.slice(0, -1), { ...last, text: last.text + delta }];
        });
      }
      // "user-echo" and null: discard
    } else if (ce.type === "part.end") {
      const part = ce.part;
      if (!part) return;
      const partMessageID = ce.messageId || undefined;

      if (part.type === "reasoning") {
        activePartTypeRef.current = "reasoning";
        const text = part.reasoning ?? "";
        if (text) {
          const idx = currentThinkingIdxRef.current;
          setSseStreamParts((prev) => {
            if (idx >= 0 && idx < prev.length && prev[idx]!.type === "thinking") {
              const updated = [...prev];
              updated[idx] = { ...updated[idx]!, type: "thinking", text, messageID: partMessageID ?? prev[idx]!.messageID };
              return updated;
            }
            return [...prev, { type: "thinking", text, messageID: partMessageID }];
          });
        } else {
          setSseStreamParts((prev) => {
            currentThinkingIdxRef.current = prev.length;
            return [...prev, { type: "thinking", text: "", messageID: partMessageID }];
          });
        }
      } else if (part.type === "text") {
        const text = part.text ?? "";
        // Queued-path echo: the user's own message landing in the
        // stream after outbox delivery. Strip it from the assistant
        // buffer AND clear its pending pill — the agent owns it now.
        const queuedEchoText = matchQueuedEcho(text);
        if (queuedEchoText !== undefined) {
          activePartTypeRef.current = "user-echo";
          queue.removeFirstByText(queuedEchoText);
        } else if (sentTextRef.current && text === sentTextRef.current) {
          activePartTypeRef.current = "user-echo";
        } else if (sentTextRef.current && text.startsWith(sentTextRef.current)) {
          // Epic 68 D11: with attached files the user echo comes back as the
          // composed text (prose + manifest). A remainder that is ONLY a
          // manifest block is still the user echo — never streamed as
          // assistant content.
          const remainder = text.slice(sentTextRef.current.length);
          const parsedRemainder = parseAttachments(remainder);
          if (parsedRemainder.attachments !== null && parsedRemainder.text === "") {
            activePartTypeRef.current = "user-echo";
          } else {
            activePartTypeRef.current = "text";
            const stripped = remainder;
            const idx = currentTextIdxRef.current;
            setSseStreamParts((prev) => {
              if (idx >= 0 && idx < prev.length && prev[idx]!.type === "text") {
                const updated = [...prev];
                updated[idx] = { ...updated[idx]!, type: "text", text: stripped, messageID: partMessageID ?? prev[idx]!.messageID };
                return updated;
              }
              return [...prev, { type: "text", text: stripped, messageID: partMessageID }];
            });
          }
        } else {
          activePartTypeRef.current = "text";
          if (text) {
            const idx = currentTextIdxRef.current;
            setSseStreamParts((prev) => {
              if (idx >= 0 && idx < prev.length && prev[idx]!.type === "text") {
                const updated = [...prev];
                updated[idx] = { ...updated[idx]!, type: "text", text, messageID: partMessageID ?? prev[idx]!.messageID };
                return updated;
              }
              return [...prev, { type: "text", text, messageID: partMessageID }];
            });
          } else {
            setSseStreamParts((prev) => {
              currentTextIdxRef.current = prev.length;
              return [...prev, { type: "text", text: "", messageID: partMessageID }];
            });
          }
        }
      } else if (part.type === "tool" && part.tool) {
        // Flat contract ToolPart (mirrors api/messages.ts history
        // rendering): name/input/output on the tool; state carries
        // status + ISO startedAt — no agent-shape parsing.
        const tool = part.tool;
        const toolName = tool.name || "";
        const toolState = tool.state?.status || "";
        const callID = tool.callId;
        const toolInput = tool.input;
        const toolOutput = typeof tool.output === "string" ? tool.output : undefined;
        const toolStartedAt = tool.state?.startedAt;
        setSseStreamParts((prev) => {
          if (callID) {
            const existingIdx = prev.findIndex((p: StreamPart) => p.type === "tool" && p.toolCallID === callID);
            if (existingIdx >= 0) {
              const updated = [...prev];
              updated[existingIdx] = {
                ...prev[existingIdx]!,
                type: "tool",
                text: toolName || prev[existingIdx]!.text,
                toolState,
                toolCallID: callID,
                toolInput,
                toolOutput,
                messageID: partMessageID ?? prev[existingIdx]!.messageID,
                // The agent rewrites the tool start time on every part
                // snapshot (verified live against opencode 1.18.10: same
                // part, start moved 75.7s between updates) — it is the
                // snapshot time, not the call start. Anchor to the
                // FIRST-seen value so the elapsed badge doesn't reset on
                // every output line.
                toolStartedAt: prev[existingIdx]!.toolStartedAt ?? toolStartedAt,
              };
              return updated;
            }
          }
          return [...prev, { type: "tool", text: toolName, toolState, toolStartedAt, toolCallID: callID, toolInput, toolOutput, messageID: partMessageID }];
        });
        activePartTypeRef.current = null;
      }
      // file-change / custom parts: rendered from history on reconcile;
      // no live streaming treatment.
    }
  }, [queue, matchQueuedEcho]);

  const handleSSEEvent = useCallback((data: unknown) => {
    const event = data as WorkspaceStreamEvent;
    if (!event?.type) return;

    if (event.type === "workspace.phase") {
      queue.onPhaseChange(event.phase);
    }

    // D6 (#998): notify-only hung-session escalation. Banner until
    // dismissed; a later session.status=idle for the same session
    // auto-clears it (the hang resolved).
    if (event.type === "workspace.alert" && event.data?.alert === "session_hung") {
      setHungAlert(event);
    }

    if (event.type === "session.status" && workspaceId) {
      queryClient.invalidateQueries({ queryKey: ["sessions", workspaceId] });
      if (event.session_id === sessionId) {
        if (event.status === "idle") {
          notifySessionIdle(event.session_id);
          setRetryStatus(null);
          clearStreamTimedOut();
          setHungAlert((prev) => (prev && prev.session_id === event.session_id ? null : prev));
          reconcileOnIdle();
          queue.refreshQueue();
          // US-16.12: Clear stale prompts on session idle (global, scoped to
          // this session — survives across views, cleared when idle).
          clearSessionPendingPrompts(event.session_id);
        } else if (event.status === "busy") {
          setRetryStatus(null);
        } else if (event.status === "retry") {
          // Platform retry payload (translated server-side, US-65.8).
          const r = (event as SessionStatusEvent).data;
          if (r) {
            setRetryStatus({
              attempt: typeof r.attempt === "number" ? r.attempt : 1,
              message: typeof r.message === "string" ? r.message : "",
              next: typeof r.next === "number" ? r.next : Date.now(),
              action: r.action as unknown as RetryStatus["action"],
            });
          }
        }
      }
    } else if (event.type === "queue.update" && workspaceId) {
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
    } else if (event.type === "session.event" && workspaceId) {
      // US-65.8: contract events — the only agent-derived stream the
      // client consumes. Agent wire shapes are translated server-side.
      const ce = (event as SessionContractEvent).data;
      if (!ce) return;

      if (ce.type === "session.updated" && ce.session) {
        // Sidebar title in real time
        if (ce.session.title) {
          const sid = ce.session.id;
          const title = ce.session.title;
          queryClient.setQueryData<SessionListItem[]>(["sessions", workspaceId], (old) => {
            if (!old) return old;
            return old.map((s) => s.id === sid ? { ...s, title } : s);
          });
        }
        // Real-time context usage (per-step occupancy semantics — the
        // contract guarantees session.updated tokens are NEVER mapped to
        // contextUsage, so this value is always the correct numerator).
        if (ce.session.contextUsage && ce.session.id) {
          contextBySessionRef.current.set(ce.session.id, ce.session.contextUsage.used);
          setContextVersion((v) => v + 1);
        }
      } else if (ce.type === "error" && sessionId && ce.sessionId === sessionId) {
        // Map known error codes to actionable user-facing messages.
        const code = ce.error?.code ?? "";
        const rawMessage = ce.error?.message ?? "";
        let text: string;
        if (code === "ContextOverflowError") {
          text = "Context limit reached — type /compact to summarize the conversation and continue";
        } else if (code === "MessageOutputLengthError") {
          text = "Response was too long for this model's output limit";
        } else if (code === "ProviderAuthError") {
          text = rawMessage
            ? `Authentication failed: ${rawMessage} — check the API key in Settings`
            : "The configured provider rejected the request — check the API key in Settings";
        } else if (code === "ProviderRateLimitError" || /rate limit/i.test(rawMessage)) {
          text = "The provider rate-limited this workspace — retrying shortly";
        } else {
          text = rawMessage || code || "The agent hit an unexpected error";
        }
        setSessionErrors((prev) => [...prev, {
          id: `error-${++idCounterRef.current}`,
          role: "assistant",
          parts: [{ type: "error" as const, text: `⚠️ ${text}` }],
        }]);
        // US-16.12: Clear stale prompts on session error (global, scoped).
        clearSessionPendingPrompts(sessionId ?? "");
      }

      // Route streaming events to the active session renderer
      if (sessionId) {
        handleContractEvent(ce, sessionId);
      }
    } else if (event.type === "agent.question") {
      const req = event.data as QuestionRequest;
      // Store content globally (keyed by requestId); the selector filters by
      // session at render. Storing unconditionally (not gated by the viewed
      // session) means the prompt survives navigation to/from this session.
      addPendingQuestion(workspaceId ?? "", req);
    } else if (event.type === "agent.question.resolved") {
      const { request_id } = event.data as { request_id: string };
      removePendingAction(request_id);
    } else if (event.type === "agent.permission") {
      const req = event.data as PermissionRequest;
      addPendingPermission(workspaceId ?? "", req);
    } else if (event.type === "agent.permission.resolved") {
      const { request_id } = event.data as { request_id: string };
      removePendingAction(request_id);
    } else if (event.type === "agent_died") {
      setAgentDied(true);
      if (event.data?.message) setAgentDiedMessage(event.data.message);
    } else {
      console.debug("[ChatPage] unhandled SSE event type:", event.type);
    }
  }, [queryClient, workspaceId, sessionId, handleContractEvent, notifySessionIdle, reconcileOnIdle, queue, addPendingQuestion, addPendingPermission, removePendingAction, clearSessionPendingPrompts, clearStreamTimedOut]);

  // US-15.2: On SSE reconnect, re-poll status to catch missed transitions.
  // Also resync the transcript: opencode history is authoritative after an
  // SSE gap (e.g. in-place opencode restart for credential reload, OOM, or
  // crash). reconcileOnIdle refetches history and clears stale local state;
  // idempotent if the transcript is already current. Closes issue 440's
  // "silent hang" symptom — the reconnect now actively recovers rather than
  // leaving the user on a stale, possibly-interrupted transcript.
  const handleSSEReconnect = useCallback(() => {
    if (workspaceId) {
      queryClient.invalidateQueries({ queryKey: ["workspace-status", workspaceId] });
    }
    void queue.refreshQueue();
    void reconcileOnIdle();
    // F1 fix: an SSE gap means we may have missed part events — re-arm the
    // reconnect activation window so boundary detection gates the resumed
    // stream. The auto-abort remains snapshot-gated, so re-arming cannot
    // reintroduce false aborts.
    armReconnectWindow();
  }, [queryClient, workspaceId, queue, reconcileOnIdle, armReconnectWindow]);

  // Connect SSE unconditionally (even before workspace is Active) so we can
  // detect the Pending→Active phase transition and auto-create a session.
  useEventStream(sseWorkspaceId, handleSSEEvent, { onReconnect: handleSSEReconnect });

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
    sentTextRef.current = text;
    activePartTypeRef.current = null;
    currentThinkingIdxRef.current = -1;
    currentTextIdxRef.current = -1;
    isReconnectMode.current = false;
    reconnectWindowOpenRef.current = false;
    if (reconnectWindowTimerRef.current) clearTimeout(reconnectWindowTimerRef.current);
    // A send is active use — drop the abort anchor outright (review round 5
    // recommendation). If stuck evidence genuinely re-establishes later, it
    // re-anchors and re-dwells from scratch; no zero-dwell carryover.
    abortDwellStartRef.current = null;
    knownLivePartIds.current.clear();
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
      // Track the text so the user echo of the LATE delivery is
      // stripped (and its pill cleared) instead of rendering the
      // user's own words as an agent bubble.
      pendingQueuedTextsRef.current.set(
        text,
        (pendingQueuedTextsRef.current.get(text) ?? 0) + 1,
      );
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

      {retryStatus && (
        <SessionRetryBanner status={retryStatus} />
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
                      onResolved={() => removePendingAction(q.id)} />
                  ))}
                  {pendingPermissions.map((p) => (
                    <PermissionPrompt key={p.id} workspaceId={workspaceId!} request={p}
                      onResolved={() => removePendingAction(p.id)} />
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
