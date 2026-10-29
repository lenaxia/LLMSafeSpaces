import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams, useLocation } from "react-router-dom";
import { workspacesApi } from "../../api/workspaces";
import { workspaceWorkflowApi } from "../../api/workflows";
import { useConfirmDialog } from "../../hooks/useConfirmDialog";
import { orgsApi } from "../../api/orgs";
import { ApiClientError } from "../../api/client";
import { useAuth } from "../../providers/AuthProvider";
import { useSessionStatus, useWorkspaceBusyCount, useSessionPendingActions, useWorkspaceHung, useIsSessionAborted } from "../../providers/SessionActivityProvider";
import type { SessionDisplayStatus } from "../../providers/SessionActivityProvider";
import { RenameWorkspaceDialog } from "../workspace/RenameWorkspaceDialog";
import { NewWorkspaceSplitButton } from "../workspace/NewWorkspaceSplitButton";
import { WorkspaceSettingsDrawer } from "../workspace/WorkspaceSettingsDrawer";
import { RenameSessionDialog } from "../session/RenameSessionDialog";
import { KebabMenu } from "../ui/KebabMenu";
import type { KebabMenuItem } from "../ui/KebabMenu";
import { ConfirmDialog } from "../ui/ConfirmDialog";
import {
  Settings,
  LogOut,
  Building2,
  Plus,
  Circle,
  MessageSquare,
  MessageSquareText,
  OctagonX,
  HelpCircle,
  ChevronRight,
  ChevronDown,
  Play,
  Shield,
  Workflow,
  Zap,
  Archive,
  KanbanSquare,
  Bot,
} from "lucide-react";
import { Spinner } from "../ui/Spinner";
import { BusyIndicator } from "../ui/BusyIndicator";
import type { SessionListItem, WorkspaceListItem } from "../../api/types";
import { sessionDisplayTitle } from "../../lib/names";
import { formatRelativeTime } from "../../lib/time";
import { useNow } from "../../hooks/useNow";
import { cn } from "../../lib/utils";
import { buildSessionTree, ancestorChain } from "../../lib/sessionTree";
import type { SessionTreeNode } from "../../lib/sessionTree";
import { isWorkerSession, sortByRecentActivity } from "../../lib/sessionWorker";
import { useUserSetting } from "../../hooks/useUserSettings";

interface Props {
  onNavigate?: () => void;
}

export function Sidebar({ onNavigate }: Props) {
  const { logout, user } = useAuth();
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const { workspaceId, sessionId } = useParams();
  const { confirm: confirmAction, dialog: confirmDialog } = useConfirmDialog();
  const [expandedWs, setExpandedWs] = useState<Set<string>>(() =>
    workspaceId ? new Set([workspaceId]) : new Set(),
  );
  const [renamingWs, setRenamingWs] = useState<string | null>(null);
  const [renamingSession, setRenamingSession] = useState<{ wsId: string; sessionId: string; title: string } | null>(null);

  const { data: workspaces } = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => workspacesApi.list(),
  });

  const { data: userOrgs } = useQuery({
    queryKey: ["user-orgs"],
    queryFn: () => orgsApi.list(),
    staleTime: 60_000,
  });
  const orgAdminLink = userOrgs && userOrgs.length > 0 && userOrgs[0]!.userRole === "admin"
    ? `/orgs/${userOrgs[0]!.id}`
    : null;

  useEffect(() => {
    if (workspaces?.items) {
      setExpandedWs((prev) => {
        const next = new Set(prev);
        workspaces.items.forEach((w) => next.add(w.id));
        return next;
      });
    }
  }, [workspaces?.items]);

  const activateMutation = useMutation({
    mutationFn: (wsId: string) => workspacesApi.activate(wsId),
    onSuccess: (_data, wsId) => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      queryClient.invalidateQueries({ queryKey: ["workspace-status"] });
      queryClient.invalidateQueries({ queryKey: ["sessions", wsId] });
    },
  });

  const suspendMutation = useMutation({
    mutationFn: (wsId: string) => workspacesApi.suspend(wsId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      queryClient.invalidateQueries({ queryKey: ["workspace-status"] });
    },
  });

  const refreshComputeMutation = useMutation({
    mutationFn: (wsId: string) => workspacesApi.refreshCompute(wsId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      queryClient.invalidateQueries({ queryKey: ["workspace-status"] });
    },
  });

  const newSessionMutation = useMutation({
    mutationFn: (wsId: string) => workspacesApi.ensureSession(wsId),
    onSuccess: (data, wsId) => {
      queryClient.invalidateQueries({ queryKey: ["sessions", wsId] });
      navigate(`/chat/${wsId}/${data.sessionId}`);
      onNavigate?.();
    },
  });

  const renameWsMutation = useMutation({
    mutationFn: ({ wsId, name }: { wsId: string; name: string }) =>
      workspacesApi.renameWorkspace(wsId, name),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      setRenamingWs(null);
    },
  });

  const deleteWsMutation = useMutation({
    mutationFn: (wsId: string) => workspacesApi.deleteWorkspace(wsId),
    onSuccess: (_data, wsId) => {
      queryClient.invalidateQueries({ queryKey: ["workspaces"] });
      if (workspaceId === wsId) {
        navigate("/chat");
      }
    },
  });

  const renameSessionMutation = useMutation({
    mutationFn: ({ wsId, sessionId, title }: { wsId: string; sessionId: string; title: string }) =>
      workspacesApi.renameSession(wsId, sessionId, title),
    onSuccess: (_data, vars) => {
      queryClient.invalidateQueries({ queryKey: ["sessions", vars.wsId] });
      setRenamingSession(null);
    },
  });

  // #1627: archive/unarchive. The listing refetch moves the session
  // into/out of the Archived group; the SSE archived/unarchived event
  // converges every other tab's cache.
  const archiveSessionMutation = useMutation({
    mutationFn: ({ wsId, sessionId, archived }: { wsId: string; sessionId: string; archived: boolean }) =>
      workspacesApi.setSessionArchived(wsId, sessionId, archived),
    onSuccess: (_data, vars) => {
      queryClient.invalidateQueries({ queryKey: ["sessions", vars.wsId] });
    },
    // Same feedback contract as the adjacent kebab siblings (force stop,
    // delete): a failed call alerts instead of closing the menu silently.
    onError: () => {
      try { window.alert("Failed to change archive state."); } catch { /* blocked */ }
    },
  });

  const handleWorkspaceClick = (ws: WorkspaceListItem) => {
    const isExpanded = expandedWs.has(ws.id);

    setExpandedWs((prev) => {
      const next = new Set(prev);
      if (isExpanded) {
        next.delete(ws.id);
      } else {
        next.add(ws.id);
      }
      return next;
    });
  };

  const handleSessionClick = (wsId: string, sid: string) => {
    navigate(`/chat/${wsId}/${sid}`);
    onNavigate?.();
  };

  return (
    <aside className="flex h-full flex-col border-r border-border bg-card resize-x overflow-auto min-w-48 max-w-96" style={{ width: "16rem" }} aria-label="Navigation">
      <div className="flex items-center justify-between border-b border-border px-4 pb-3 pt-[calc(0.75rem+env(safe-area-inset-top,0px))]">
        <h1 className="text-sm font-semibold">Safe Space</h1>
        <NewWorkspaceSplitButton
          onCreated={(wsId) => {
            queryClient.invalidateQueries({ queryKey: ["workspaces"] });
            setExpandedWs((prev) => new Set(prev).add(wsId));
            navigate(`/chat/${wsId}`);
            onNavigate?.();
          }}
        />
      </div>

      <div className="flex-1 overflow-y-auto overflow-x-hidden scrollbar-thin scrollbar-track-transparent scrollbar-thumb-muted-foreground/20 hover:scrollbar-thumb-muted-foreground/40">
        <div className="flex flex-col gap-0.5 px-2 pt-2 pb-1">
          <button
            onClick={() => { navigate("/workflows"); onNavigate?.(); }}
            className={cn(
              "flex items-center gap-2 rounded-md px-2 py-1.5 text-sm font-medium transition-colors",
              "hover:bg-accent",
              location.pathname.startsWith("/workflows") && "bg-accent text-accent-foreground",
            )}
          >
            <Workflow className="h-4 w-4 shrink-0 text-muted-foreground" />
            <span>Workflows</span>
          </button>
          <button
            onClick={() => { navigate("/triggers"); onNavigate?.(); }}
            className={cn(
              "flex items-center gap-2 rounded-md px-2 py-1.5 text-sm font-medium transition-colors",
              "hover:bg-accent",
              location.pathname.startsWith("/triggers") && "bg-accent text-accent-foreground",
            )}
          >
            <Zap className="h-4 w-4 shrink-0 text-muted-foreground" />
            <span>Triggers</span>
          </button>
          <button
            onClick={() => { navigate("/board"); onNavigate?.(); }}
            className={cn(
              "flex items-center gap-2 rounded-md px-2 py-1.5 text-sm font-medium transition-colors",
              "hover:bg-accent",
              location.pathname.startsWith("/board") && "bg-accent text-accent-foreground",
            )}
          >
            <KanbanSquare className="h-4 w-4 shrink-0 text-muted-foreground" />
            <span>Board</span>
          </button>
        </div>
        <div className="border-t border-border mx-2 my-1" />
        <nav className="flex flex-col gap-0.5 p-2" aria-label="Workspaces">
          {(workspaces?.items ?? []).map((ws) => (
            <WorkspaceGroup
              key={ws.id}
              workspace={ws}
              expanded={expandedWs.has(ws.id)}
              selectedWorkspaceId={workspaceId}
              selectedSessionId={sessionId}
              activating={activateMutation.isPending && activateMutation.variables === ws.id}
              creatingSession={newSessionMutation.isPending && newSessionMutation.variables === ws.id}
              onToggle={() => handleWorkspaceClick(ws)}
              onSelectSession={(sid) => handleSessionClick(ws.id, sid)}
              onNewSession={() => newSessionMutation.mutate(ws.id)}
              isRenaming={renamingWs === ws.id}
              onRenameClick={() => setRenamingWs(ws.id)}
              onRenameCancel={() => setRenamingWs(null)}
              onRenameConfirm={(name) => renameWsMutation.mutate({ wsId: ws.id, name })}
              onDelete={() => {
                confirmAction({
                  title: `Delete workspace "${ws.name}"?`,
                  description: "This action cannot be undone. All sessions and data will be lost.",
                  confirmLabel: "Delete",
                  destructive: true,
                  onConfirm: () => deleteWsMutation.mutate(ws.id),
                });
              }}
              onSuspend={() => suspendMutation.mutate(ws.id)}
              onResume={() => activateMutation.mutate(ws.id)}
              onRefreshCompute={() => refreshComputeMutation.mutate(ws.id)}
              refreshingCompute={refreshComputeMutation.isPending && refreshComputeMutation.variables === ws.id}
              onArchiveSession={(sessionId, archived) =>
                archiveSessionMutation.mutate({ wsId: ws.id, sessionId, archived })
              }
              onRenameSession={(sessionId, title) => setRenamingSession({ wsId: ws.id, sessionId, title })}
              onAbortSession={(sid) => {
                workspacesApi.abortSession(ws.id, sid)
                  .catch(() => {
                    try { window.alert("Failed to force stop session."); } catch { /* blocked */ }
                  });
              }}
              onDeleteSession={(sid) => {
                confirmAction({
                  title: "Delete this session?",
                  description: "This action cannot be undone.",
                  confirmLabel: "Delete",
                  destructive: true,
                  onConfirm: () => {
                    workspacesApi.deleteSession(ws.id, sid)
                      .catch((err: unknown) => {
                        if (err instanceof ApiClientError && err.status === 404) return;
                        throw err;
                      })
                      .then(() => {
                        queryClient.invalidateQueries({ queryKey: ["sessions", ws.id] });
                        if (sid === sessionId) {
                          navigate(`/chat/${ws.id}`);
                        }
                      })
                      .catch(() => {
                        try { window.alert("Failed to delete session."); } catch { /* blocked */ }
                      });
                  },
                });
              }}
              renamingSession={renamingSession?.wsId === ws.id ? renamingSession : null}
              onRenameSessionCancel={() => setRenamingSession(null)}
              onRenameSessionConfirm={(sessionId, title) =>
                renameSessionMutation.mutate({ wsId: ws.id, sessionId, title })
              }
            />
          ))}
          {((workspaces?.items ?? []).length === 0) && (
            <div className="px-4 py-8 text-center text-sm text-muted-foreground">
              No workspaces yet
            </div>
          )}
        </nav>
      </div>

      <div className="border-t border-border p-2">
        <div className="flex items-center justify-between">
          <span className="truncate px-2 text-xs text-muted-foreground">{user?.username}</span>
          <div className="flex gap-1">
            {user?.role === "admin" && (
              <button onClick={() => { navigate("/admin"); onNavigate?.(); }} className="rounded p-1.5 hover:bg-accent" aria-label="Platform Administration" title="Platform Administration">
                <Shield className="h-4 w-4" />
              </button>
            )}
            {orgAdminLink && (
              <button onClick={() => { navigate(orgAdminLink); onNavigate?.(); }} className="rounded p-1.5 hover:bg-accent" aria-label="Organisation" title="Organisation">
                <Building2 className="h-4 w-4" />
              </button>
            )}
            <button onClick={() => { navigate("/settings"); onNavigate?.(); }} className="rounded p-1.5 hover:bg-accent" aria-label="Settings">
              <Settings className="h-4 w-4" />
            </button>
            <button onClick={logout} className="rounded p-1.5 hover:bg-accent" aria-label="Log out">
              <LogOut className="h-4 w-4" />
            </button>
          </div>
        </div>
      </div>
      {confirmDialog}
    </aside>
  );
}

interface WorkspaceGroupProps {
  workspace: WorkspaceListItem;
  expanded: boolean;
  selectedWorkspaceId?: string;
  selectedSessionId?: string;
  activating: boolean;
  creatingSession: boolean;
  onToggle: () => void;
  onSelectSession: (sessionId: string) => void;
  onNewSession: () => void;
  isRenaming: boolean;
  onRenameClick: () => void;
  onRenameCancel: () => void;
  onRenameConfirm: (name: string) => void;
  onDelete: () => void;
  onSuspend: () => void;
  onResume: () => void;
  onRefreshCompute: () => void;
  refreshingCompute: boolean;
  onRenameSession: (sessionId: string, title: string) => void;
  onArchiveSession: (sessionId: string, archived: boolean) => void;
  onDeleteSession: (sessionId: string) => void;
  onAbortSession: (sessionId: string) => void;
  renamingSession: { wsId: string; sessionId: string; title: string } | null;
  onRenameSessionCancel: () => void;
  onRenameSessionConfirm: (sessionId: string, title: string) => void;
}

function WorkspaceGroup({
  workspace,
  expanded,
  selectedWorkspaceId,
  selectedSessionId,
  activating,
  creatingSession,
  onToggle,
  onSelectSession,
  onNewSession,
  isRenaming,
  onRenameClick,
  onRenameCancel,
  onRenameConfirm,
  onDelete,
  onSuspend,
  onResume,
  onRefreshCompute,
  refreshingCompute,
  onRenameSession,
  onArchiveSession,
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
}: WorkspaceGroupProps) {
  const isSelected = workspace.id === selectedWorkspaceId;
  const isSuspended = workspace.phase === "Suspended";
  const isResuming = workspace.phase === "Resuming";
  const isActive = workspace.phase === "Active";
  const busyCount = useWorkspaceBusyCount(workspace.id);
  const hung = useWorkspaceHung(workspace.id);

  const [showSettings, setShowSettings] = useState(false);
  const [showRefreshConfirm, setShowRefreshConfirm] = useState(false);

  const versionFooter: string[] = [
    ...(workspace.agentVersion ? [`opencode v${workspace.agentVersion}`] : []),
    ...(workspace.imageTag ? [`image: ${workspace.imageTag}`] : []),
  ];

  const kebabItems: KebabMenuItem[] = [
    { label: "Settings", onClick: () => setShowSettings(true) },
    { label: "Copy new session link", onClick: () => navigator.clipboard.writeText(`${window.location.origin}/chat/${workspace.id}`) },
    { label: "Rename", onClick: onRenameClick },
    {
      label: refreshingCompute ? "Refreshing compute…" : "Refresh compute",
      onClick: () => setShowRefreshConfirm(true),
      disabled: refreshingCompute,
      section: "Lifecycle",
    },
    ...(isActive ? [{ label: "Suspend", onClick: onSuspend, section: "Lifecycle" }] : []),
    ...(isSuspended ? [{ label: "Resume", onClick: onResume, section: "Lifecycle" }] : []),
    { label: "Delete", onClick: onDelete, destructive: true, section: "Lifecycle" },
  ];

  return (
    <div>
      {isRenaming ? (
        <div className="border-b border-border">
          <RenameWorkspaceDialog
            currentName={workspace.name}
            onRename={onRenameConfirm}
            onCancel={onRenameCancel}
          />
        </div>
      ) : (
        <div className={cn(
          "group flex items-center rounded-md transition-colors hover:bg-accent/50",
          isSelected && "bg-accent",
        )}>
          <button
            onClick={onToggle}
            className={cn(
              "flex flex-1 min-w-0 items-center gap-1.5 rounded-md px-3 py-2 text-left text-sm transition-colors overflow-hidden",
              isSelected ? "text-accent-foreground" : "",
            )}
          >
            {expanded ? (
              <ChevronDown className="h-3.5 w-3.5 flex-shrink-0 text-muted-foreground" />
            ) : (
              <ChevronRight className="h-3.5 w-3.5 flex-shrink-0 text-muted-foreground" />
            )}
            <Circle
              className={cn(
                "h-2 w-2 flex-shrink-0",
                isActive
                  ? "fill-green-500 text-green-500"
                  : isSuspended
                    ? "fill-yellow-500 text-yellow-500"
                    : isResuming
                      ? "fill-yellow-500 text-yellow-500 animate-pulse"
                      : "fill-muted-foreground/40 text-muted-foreground/40",
              )}
            />
            <span className="flex-1 truncate">{workspace.name}</span>
            {activating && <Spinner size="sm" />}
            {!activating && workspace.phase && !isActive && !isSuspended && (
              <span className="text-xs text-muted-foreground">{workspace.phase}</span>
            )}
            {!expanded && busyCount > 0 && !hung && (
              <BusyIndicator size="sm" />
            )}
            {!expanded && hung && (
              <span
                title="A session has been busy without progress — possibly hung (nothing was stopped automatically)"
                aria-label="session possibly hung"
                className="h-2 w-2 shrink-0 rounded-full bg-amber-500"
                data-testid="hung-badge"
              />
            )}
          </button>
          {isSuspended && !activating && (
            <button
              onClick={onResume}
              className="rounded p-1 text-yellow-500 hover:bg-accent hover:text-yellow-400 transition-colors"
              aria-label="Resume workspace"
              title="Resume workspace"
            >
              <Play className="h-3 w-3" />
            </button>
          )}
          {isActive && (
            <button
              onClick={onNewSession}
              disabled={creatingSession}
              className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-50 transition-opacity"
              aria-label="New chat"
              title="New chat"
            >
              {creatingSession ? <Spinner size="sm" /> : <Plus className="h-3 w-3" />}
            </button>
          )}
          <div className="mr-1">
            <KebabMenu items={kebabItems} align="left" footer={versionFooter.length > 0 ? versionFooter : undefined} />
          </div>
        </div>
      )}

      {expanded && (
        <WorkspaceSessionList
          workspaceId={workspace.id}
          selectedSessionId={selectedSessionId}
          onSelectSession={onSelectSession}
          onNewSession={onNewSession}
          creatingSession={creatingSession}
          isSuspended={isSuspended || isResuming}
          onRenameSession={onRenameSession}
          onArchiveSession={onArchiveSession}
          onDeleteSession={onDeleteSession}
          onAbortSession={onAbortSession}
          renamingSession={renamingSession}
          onRenameSessionCancel={onRenameSessionCancel}
          onRenameSessionConfirm={onRenameSessionConfirm}
        />
      )}
      <WorkspaceSettingsDrawer
        workspace={workspace}
        open={showSettings}
        onOpenChange={setShowSettings}
      />
      <ConfirmDialog
        open={showRefreshConfirm}
        onOpenChange={setShowRefreshConfirm}
        title="Refresh compute?"
        description="This will halt all current work in the workspace and reprovision it from scratch with the latest image version and current resource defaults. Any running agent sessions will be interrupted."
        note="Your files and workspace data are preserved."
        confirmLabel="Refresh compute"
        onConfirm={() => {
          setShowRefreshConfirm(false);
          onRefreshCompute();
        }}
      />
    </div>
  );
}

interface SessionListProps {
  workspaceId: string;
  selectedSessionId?: string;
  onSelectSession: (sessionId: string) => void;
  onNewSession: () => void;
  creatingSession: boolean;
  isSuspended: boolean;
  onRenameSession: (sessionId: string, title: string) => void;
  onArchiveSession: (sessionId: string, archived: boolean) => void;
  onDeleteSession: (sessionId: string) => void;
  onAbortSession: (sessionId: string) => void;
  renamingSession: { wsId: string; sessionId: string; title: string } | null;
  onRenameSessionCancel: () => void;
  onRenameSessionConfirm: (sessionId: string, title: string) => void;
}

function WorkspaceSessionList({
  workspaceId,
  selectedSessionId,
  onSelectSession,
  onNewSession: _onNewSession,
  creatingSession: _creatingSession,
  isSuspended,
  onRenameSession,
  onArchiveSession,
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
}: SessionListProps) {
  // #1966: the sessions cache key is SHARED with ChatPage (same key,
  // same plain queryFn — no merge in either). The previous shape
  // (origins merged inside this component's queryFn) raced ChatPage's
  // merge-less queryFn for whose fetch defined the cache: when
  // ChatPage mounted first, worker origins never merged and the
  // Workers folder miscounted. The origins now ride their own cache
  // and merge at this consumer — deterministic by construction.
  const { data: sessions, isLoading } = useQuery({
    queryKey: ["sessions", workspaceId],
    queryFn: () => workspacesApi.getSessions(workspaceId, { includeArchived: true }),
    enabled: !!workspaceId,
    // #1646: reconciliation floor for the session LIST (and its unread
    // reconcile, which runs on every cache update). SSE events converge
    // it live, but a stream that died silently leaves the list frozen
    // at last-known state; a 60s interval bounds the staleness with
    // zero events and zero focus. Explicit refetchOnWindowFocus (the
    // default) documents the same intent for tab switches. Busy state
    // is NOT re-derived here by design — the provider's seedBusy stays
    // gated (SSE-tracked busy must survive a stale REST refetch); busy
    // convergence rides the reconnect reseed + liveness watchdog.
    refetchInterval: 60_000,
    refetchOnWindowFocus: true,
  });

  // #1915/#1966 origin marks on their own cache; the sidebar merges
  // them into the (shared) session rows at the consumer seam.
  const { data: sessionOrigins } = useQuery({
    queryKey: ["session-origins", workspaceId],
    queryFn: () => workspaceWorkflowApi.sessionOrigins(workspaceId),
    enabled: !!workspaceId,
    staleTime: 60_000,
    refetchInterval: 60_000,
    refetchOnWindowFocus: true,
  });

  const mergedSessions = useMemo(() => {
    if (!sessions || !sessionOrigins || sessionOrigins.length === 0) {
      return sessions;
    }
    const originMap = new Map(sessionOrigins.map((o) => [o.sessionId, o.origin]));
    return sessions.map((s) => ({
      ...s,
      origin: originMap.get(s.id) || s.origin,
    }));
  }, [sessions, sessionOrigins]);

  // S36.5: Subscribe to workspace status cache (populated by ChatPage poller)
  // to show per-session context usage indicators. staleTime:Infinity + enabled:false
  // means: read from cache only, never trigger a fetch from here.
  // Build contextBySessionId from the sessions list (already fetched, no extra query needed).
  // context_used is persisted to session_index by the proxy on every step.ended event,
  // so it is available for all sessions regardless of workspace pod state.
  const contextBySessionId = useMemo(() => {
    const m = new Map<string, number>();
    for (const s of mergedSessions ?? []) {
      if (s.contextUsed != null) {
        m.set(s.id, s.contextUsed);
      }
    }
    return m;
  }, [mergedSessions]);

  // #1627: archived sessions never render in the live tree — they
  // live in the collapsed-by-default Archived group below it.
  // Archived filters FIRST everywhere below: an archived worker rows
  // under Archived (retirement outranks the worker class).
  const archivedSessions = useMemo(
    () => (mergedSessions ?? []).filter((s) => s.archived),
    [mergedSessions],
  );

  // Rulings 6+7 (2026-10-28): the worker subtree is for board workers
  // (the EXISTING Workers group — no new group), and parentage no
  // longer banishes — subtasks nest under their top-level parent.
  // ONE tree is built over the full live set; its forest is then
  // partitioned by PROVENANCE — origin-class 'worker' roots leave the
  // flat main list for the Workers group (their genuine
  // subagent children ride their subtrees INTO the group), everything
  // else stays main-list. Parentage alone no longer folders a session:
  // a Task-tool subagent nests beneath its own parent wherever that
  // parent renders.
  const liveSessions = useMemo(
    () => (mergedSessions ?? []).filter((s) => !s.archived),
    [mergedSessions],
  );
  const tree = useMemo(() => buildSessionTree(liveSessions), [liveSessions]);

  // Workers (current AND past — idle included), sorted by recent
  // activity. Empty list hides the group entirely (ruling 6). Roots
  // are the origin-class worker forest nodes PLUS orphaned workers
  // (parentId pointing at a deleted session — provenance still roots
  // the group; an orphaned worker is never a flat main-list row).
  const workerRootNodes = useMemo(() => {
    const workers = [
      ...tree.roots.filter((n) => isWorkerSession(n.session)),
      ...tree.orphans.filter((n) => isWorkerSession(n.session)),
    ];
    const byId = new Map(workers.map((n) => [n.session.id, n]));
    return sortByRecentActivity(workers.map((n) => n.session))
      .map((s) => byId.get(s.id))
      .filter((n): n is SessionTreeNode => !!n);
  }, [tree]);

  // Main list: non-worker roots, plus NON-WORKER orphan-by-parentage
  // rows promoted to top-level so they stay visible (their parent is
  // gone; pre-ruling they foldered into the worker group by their
  // parentId — the synthetic "Orphaned subtasks" group stays dead).
  // Orphaned WORKERS follow the provenance split like every other
  // worker: they root the Workers group (below), never a flat
  // main-list row.
  const mainRootNodes = useMemo(
    () => [
      ...tree.roots.filter((n) => !isWorkerSession(n.session)),
      ...tree.orphans.filter((n) => !isWorkerSession(n.session)),
    ],
    [tree],
  );

  const pendingActionIds = useSessionPendingActions();

  // Sessions that should show the pending-action indicator (HelpCircle with
  // pulse). A session shows the indicator when it or any descendant has a
  // pending question/permission — the indicator bubbles up so the top-level
  // parent session catches the user's attention.
  const pendingIndicatorIds = useMemo(() => {
    const pending = new Set<string>();
    function walk(node: SessionTreeNode): boolean {
      let found = pendingActionIds.has(node.session.id);
      for (const child of node.children) {
        if (walk(child)) found = true;
      }
      if (found) pending.add(node.session.id);
      return found;
    }
    for (const root of tree.roots) walk(root);
    for (const orphan of tree.orphans) walk(orphan);
    return pending;
  }, [tree, pendingActionIds]);

  // Collapsed-by-default; auto-expand the chain of ancestors from the
  // active session so the user always sees where they are in the tree.
  // Per-workspace state so collapsing one workspace doesn't bleed into
  // another, but in practice this component instance is per-workspace.
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  // Whether to auto-expand/collapse children when navigating between sessions.
  const autoExpandChildren = useUserSetting<boolean>("autoExpandChildren", true);

  // Track the previously selected session so we can collapse its subtree
  // when the user navigates away to a session outside that subtree.
  const prevSelectedRef = useRef<string | undefined>(undefined);

  // Whenever the selected session changes (or sessions load), apply
  // auto-expand/collapse logic when the setting is enabled.
  // When disabled, still auto-expand ancestors so the active session is
  // always visible (existing behaviour).
  useEffect(() => {
    if (!mergedSessions) return;

    const prev = prevSelectedRef.current;
    const next = selectedSessionId;

    if (autoExpandChildren) {
      // --- Auto-collapse previous subtree root when leaving it ---
      if (prev && prev !== next) {
        const prevChain = ancestorChain(mergedSessions, prev);
        const nextChain = next ? ancestorChain(mergedSessions, next) : [];
        const prevRoot = prevChain[0];
        const nextRoot = nextChain[0];

        // Only collapse when the user actually moved to a different subtree.
        if (prevRoot && prevRoot !== nextRoot) {
          setExpanded((current) => {
            const updated = new Set(current);
            // Remove every node in the previous subtree from expanded set.
            // We do this by walking the prevChain ancestors and any session
            // whose root ancestor is prevRoot.
            for (const id of prevChain) {
              updated.delete(id);
            }
            return updated;
          });
        }
      }

      // --- Auto-expand the active session if it has children ---
      if (next) {
        const chain = ancestorChain(mergedSessions, next);
        setExpanded((current) => {
          const updated = new Set(current);
          // Expand the full ancestor chain (existing behaviour).
          for (let i = 0; i < chain.length - 1; i++) {
            updated.add(chain[i]!);
          }
          // Also expand the active session itself if it has children.
          updated.add(next);
          return updated;
        });
      }
    } else {
      // Setting off: only ensure ancestor chain is visible (existing behaviour).
      if (!next) return;
      const chain = ancestorChain(mergedSessions, next);
      if (chain.length <= 1) return;
      setExpanded((prev) => {
        const next2 = new Set(prev);
        for (let i = 0; i < chain.length - 1; i++) {
          next2.add(chain[i]!);
        }
        return next2;
      });
    }

    prevSelectedRef.current = next;
  }, [mergedSessions, selectedSessionId, autoExpandChildren]);

  // The synthetic "Orphaned subtasks" group stays dead (#1966):
  // orphan-by-parentage rows are promoted to top-level rows of the
  // main list (see mainRootNodes) — visible, but never a group.

  // #1627 ruling 3: the Archived group is ALWAYS collapsed by default —
  // deliberate friction. Expanding requires a human click; nothing
  // auto-expands it (not even selecting an archived session).
  const [archivedExpanded, setArchivedExpanded] = useState(false);

  // #1966/rulings 6+7: the Workers group is collapsed by default
  // (the group IS the decluttering), but — unlike Archived — a live
  // worker CAN be the session the user is reading, so selecting a
  // worker (or one of its nested subagents) expands it (the tree's
  // auto-expand-the-active-chain precedent, not the Archived group's
  // deliberate friction).
  const [workersExpanded, setWorkersExpanded] = useState(false);
  const workerIds = useMemo(
    () => new Set(workerRootNodes.map((n) => n.session.id)),
    [workerRootNodes],
  );
  const selectedIsWorker = useMemo(() => {
    if (!selectedSessionId || !mergedSessions) return false;
    // The selected session renders inside the group when it IS a board
    // worker or descends from one (its ancestor chain crosses the
    // group boundary).
    return ancestorChain(mergedSessions, selectedSessionId).some((id) => workerIds.has(id));
  }, [mergedSessions, selectedSessionId, workerIds]);
  useEffect(() => {
    if (selectedIsWorker) setWorkersExpanded(true);
  }, [selectedIsWorker]);

  const toggleExpanded = (sessionId: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(sessionId)) {
        next.delete(sessionId);
      } else {
        next.add(sessionId);
      }
      return next;
    });
  };

  if (isSuspended && isLoading) {
    return (
      <div className="ml-7 px-2 py-1 text-xs text-muted-foreground">
        Loading...
      </div>
    );
  }

  return (
    <div className="ml-5 pl-2 border-l border-border">
      {isLoading && (
        <div className="px-2 py-1 text-xs text-muted-foreground">Loading...</div>
      )}

      {!isLoading && (!mergedSessions || mergedSessions.length === 0) && (
        <div className="px-2 py-1 text-xs text-muted-foreground">No sessions yet</div>
      )}

      {mergedSessions && mergedSessions.length > 0 && (
        <div className="flex flex-col gap-0.5">
          {mainRootNodes.map((node) => (
            <SessionTreeRow
              key={node.session.id}
              node={node}
              depth={0}
              workspaceId={workspaceId}
              selectedSessionId={selectedSessionId}
              expanded={expanded}
              onToggleExpand={toggleExpanded}
              onSelectSession={onSelectSession}
              onRenameSession={onRenameSession}
              onArchiveSession={onArchiveSession}
              onDeleteSession={onDeleteSession}
              onAbortSession={onAbortSession}
              renamingSession={renamingSession}
              onRenameSessionCancel={onRenameSessionCancel}
              onRenameSessionConfirm={onRenameSessionConfirm}
              contextBySessionId={contextBySessionId}
              pendingIndicatorIds={pendingIndicatorIds}
            />
          ))}

          {workerRootNodes.length > 0 && (
            <WorkersGroup
              nodes={workerRootNodes}
              expanded={workersExpanded}
              onToggleExpand={() => setWorkersExpanded((v) => !v)}
              nodeExpanded={expanded}
              onToggleNodeExpand={toggleExpanded}
              workspaceId={workspaceId}
              selectedSessionId={selectedSessionId}
              onSelectSession={onSelectSession}
              onRenameSession={onRenameSession}
              onArchiveSession={onArchiveSession}
              onDeleteSession={onDeleteSession}
              onAbortSession={onAbortSession}
              renamingSession={renamingSession}
              onRenameSessionCancel={onRenameSessionCancel}
              onRenameSessionConfirm={onRenameSessionConfirm}
              contextBySessionId={contextBySessionId}
              pendingIndicatorIds={pendingIndicatorIds}
            />
          )}

          {archivedSessions.length > 0 && (
            <ArchivedGroup
              sessions={archivedSessions}
              expanded={archivedExpanded}
              onToggleExpand={() => setArchivedExpanded((v) => !v)}
              workspaceId={workspaceId}
              selectedSessionId={selectedSessionId}
              onSelectSession={onSelectSession}
              onRenameSession={onRenameSession}
              onArchiveSession={onArchiveSession}
              onDeleteSession={onDeleteSession}
              onAbortSession={onAbortSession}
              renamingSession={renamingSession}
              onRenameSessionCancel={onRenameSessionCancel}
              onRenameSessionConfirm={onRenameSessionConfirm}
              contextBySessionId={contextBySessionId}
            />
          )}
        </div>
      )}
    </div>
  );
}

interface SessionTreeRowProps {
  node: SessionTreeNode;
  depth: number;
  /**
   * True for rows rendered as ROOTS inside a collapsed group (the
   * Workers group renders its roots at depth 1 for indentation). The
   * depth===0 gates below express "is this a top-level row" — a
   * group root is top-level in that sense (pending-input indicators
   * and the unread pulse must fire for workers awaiting input inside
   * the group, exactly as they would on a main-list root).
   */
  rootLevel?: boolean;
  /**
   * True for rows rendered inside the Workers group (roots AND their
   * nested subtrees): the group header already classifies every row
   * within it, so the per-row origin pill is suppressed (2026-10-29
   * owner ruling — the pill ate the title's width, truncating titles
   * to a character or two on narrow viewports). The origin text rides
   * a native title tooltip on the row instead (zero horizontal cost).
   * Rows outside the group keep their pill.
   */
  suppressOriginBadge?: boolean;
  workspaceId: string;
  selectedSessionId?: string;
  expanded: Set<string>;
  onToggleExpand: (sessionId: string) => void;
  onSelectSession: (sessionId: string) => void;
  onRenameSession: (sessionId: string, title: string) => void;
  onArchiveSession: (sessionId: string, archived: boolean) => void;
  onDeleteSession: (sessionId: string) => void;
  onAbortSession: (sessionId: string) => void;
  renamingSession: { wsId: string; sessionId: string; title: string } | null;
  onRenameSessionCancel: () => void;
  onRenameSessionConfirm: (sessionId: string, title: string) => void;
  /** Per-session context token count from workspace status (S36.5). */
  contextBySessionId: Map<string, number>;
  /** Sessions (and ancestors) whose subtree has a pending question/permission request. */
  pendingIndicatorIds: Set<string>;
}

/** Single row in the session tree. Renders its children recursively when
 *  expanded, with progressive left padding to show depth. */
function SessionTreeRow({
  node,
  depth,
  rootLevel = false,
  suppressOriginBadge = false,
  workspaceId,
  selectedSessionId,
  expanded,
  onToggleExpand,
  onSelectSession,
  onRenameSession,
  onArchiveSession,
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
  contextBySessionId,
  pendingIndicatorIds,
}: SessionTreeRowProps) {
  const s = node.session;
  const isRenaming = renamingSession?.sessionId === s.id;
  const hasChildren = node.children.length > 0;
  const isExpanded = expanded.has(s.id);
  const now = useNow();
  const title = sessionDisplayTitle(s.title, s.lastMessageAt);
  const status = useSessionStatus(s.id);
  // #786: a force-stopped session renders interrupted until its next
  // turn — distinct from plain idle (something happened; nothing is
  // running).
  const aborted = useIsSessionAborted(s.id);
  const isSelected = s.id === selectedSessionId;
  // A group root (rootLevel) is top-level in the indicator sense even
  // though it renders indented at depth 1 — see SessionTreeRowProps.
  const isTopLevel = depth === 0 || rootLevel;
  const rowStatus: SessionDisplayStatus =
    isTopLevel && pendingIndicatorIds.has(s.id) ? "pending_input"
    : status === "pending_input" ? "busy"
    : status;
  const showPulse = rowStatus === "unread" && !isSelected && isTopLevel;
  const contextUsed = contextBySessionId.get(s.id);

  if (isRenaming) {
    return (
      <div className="border rounded-md border-border ml-2">
        <RenameSessionDialog
          currentTitle={s.title ?? ""}
          onRename={(newTitle) => onRenameSessionConfirm(s.id, newTitle)}
          onCancel={onRenameSessionCancel}
        />
      </div>
    );
  }

  const kebabItems: KebabMenuItem[] = [
    {
      label: "Copy link",
      onClick: () => navigator.clipboard.writeText(`${window.location.origin}/chat/${workspaceId}/${s.id}`),
    },
    {
      label: "Rename",
      onClick: () => onRenameSession(s.id, s.title ?? ""),
    },
    // #1627: the reversible retirement action sits above the
    // destructive one; the label names the direction.
    {
      label: s.archived ? "Unarchive" : "Archive",
      onClick: () => onArchiveSession(s.id, !s.archived),
    },
    {
      label: "Force Stop",
      onClick: () => onAbortSession(s.id),
    },
    {
      label: "Delete",
      onClick: () => onDeleteSession(s.id),
      destructive: true,
    },
  ];

  // Each depth level adds a small indent so the tree shape is visually
  // obvious even with long titles. Capped implicitly by MAX_DEPTH in the
  // tree builder.
  const indentStyle = { paddingLeft: `${depth * 0.75}rem` };

  return (
    <>
      <div
        className={cn(
          "group flex items-center rounded-md transition-colors hover:bg-accent/50",
          s.id === selectedSessionId && "bg-accent",
        )}
        style={indentStyle}
      >
        {hasChildren ? (
          <button
            onClick={() => onToggleExpand(s.id)}
            className="rounded p-0.5 text-muted-foreground hover:bg-accent flex-shrink-0"
            aria-label={isExpanded ? "Collapse subtasks" : "Expand subtasks"}
            aria-expanded={isExpanded}
          >
            {isExpanded ? (
              <ChevronDown className="h-3 w-3" />
            ) : (
              <ChevronRight className="h-3 w-3" />
            )}
          </button>
        ) : (
          // Spacer so titles align between rows that do/don't have children.
          <span className="inline-block w-4 flex-shrink-0" aria-hidden="true" />
        )}
        <button
          onClick={() => onSelectSession(s.id)}
          className={cn(
            "flex flex-1 items-center gap-2 rounded-md px-2 py-1.5 text-left text-sm transition-colors overflow-hidden",
            s.id === selectedSessionId
              ? "text-accent-foreground"
              : "text-muted-foreground",
          )}
        >
          {rowStatus === "pending_input" ? (
            <HelpCircle className="h-3.5 w-3.5 flex-shrink-0 animate-unread-pulse text-amber-500" />
          ) : rowStatus === "busy" ? (
            <BusyIndicator />
          ) : rowStatus === "unread" ? (
            <MessageSquareText className="h-3.5 w-3.5 flex-shrink-0 animate-unread-pulse" />
          ) : aborted ? (
            <OctagonX className="h-3.5 w-3.5 flex-shrink-0 text-destructive" aria-label="Session was interrupted" />
          ) : (
            <MessageSquare className="h-3.5 w-3.5 flex-shrink-0" />
          )}
          <span
            className={cn(
              "flex-1 truncate",
              (showPulse || rowStatus === "pending_input") && "animate-unread-pulse",
              s.archived && "italic text-muted-foreground/70",
            )}
            data-testid={s.archived ? "archived-session-title" : undefined}
            // Inside the Workers group the origin pill is suppressed
            // (the group IS the classification) — the origin survives
            // as this native tooltip, costing no horizontal space.
            title={suppressOriginBadge && s.origin ? `Origin: ${s.origin}` : undefined}
          >
            {title}
          </span>
          {s.origin && s.origin !== "manual" && !suppressOriginBadge && (
            <span
              data-testid="origin-badge"
              className="flex-shrink-0 rounded px-1 text-[9px] font-medium uppercase"
              style={{
                color: s.origin === "routine" ? "#8b5cf6" : s.origin === "workflow" ? "#3b82f6" : s.origin === "worker" ? "#d97706" : "#6b7280",
                backgroundColor: s.origin === "routine" ? "#8b5cf620" : s.origin === "workflow" ? "#3b82f620" : s.origin === "worker" ? "#d9770620" : "#6b728020",
              }}
              title={`Origin: ${s.origin}`}
            >
              {s.origin}
            </span>
          )}
          {contextUsed != null && (
            <span
              className="flex-shrink-0 text-[10px] tabular-nums text-muted-foreground/50"
              aria-label={`Context: ${contextUsed} tokens`}
            >
              {contextUsed < 1000 ? String(contextUsed) : contextUsed < 1_000_000 ? `${Math.round(contextUsed / 1000)}K` : `${(contextUsed / 1_000_000).toFixed(1)}M`}
            </span>
          )}
          {s.lastMessageAt && (
            <span className="flex-shrink-0 text-xs text-muted-foreground/60">{formatRelativeTime(s.lastMessageAt, now)}</span>
          )}
        </button>
        <div className="mr-1 flex-shrink-0">
          <KebabMenu items={kebabItems} align="left" />
        </div>
      </div>
      {hasChildren && isExpanded &&
        node.children.map((child) => (
          <SessionTreeRow
            key={child.session.id}
            node={child}
            depth={depth + 1}
            suppressOriginBadge={suppressOriginBadge}
            workspaceId={workspaceId}
            selectedSessionId={selectedSessionId}
            expanded={expanded}
            onToggleExpand={onToggleExpand}
            onSelectSession={onSelectSession}
            onRenameSession={onRenameSession}
            onArchiveSession={onArchiveSession}
            onDeleteSession={onDeleteSession}
            onAbortSession={onAbortSession}
            renamingSession={renamingSession}
            onRenameSessionCancel={onRenameSessionCancel}
            onRenameSessionConfirm={onRenameSessionConfirm}
            contextBySessionId={contextBySessionId}
            pendingIndicatorIds={pendingIndicatorIds}
          />
        ))}
    </>
  );
}

interface ArchivedGroupProps {
  sessions: SessionListItem[];
  expanded: boolean;
  onToggleExpand: () => void;
  workspaceId: string;
  selectedSessionId?: string;
  onSelectSession: (sessionId: string) => void;
  onRenameSession: (sessionId: string, title: string) => void;
  onArchiveSession: (sessionId: string, archived: boolean) => void;
  onDeleteSession: (sessionId: string) => void;
  onAbortSession: (sessionId: string) => void;
  renamingSession: { wsId: string; sessionId: string; title: string } | null;
  onRenameSessionCancel: () => void;
  onRenameSessionConfirm: (sessionId: string, title: string) => void;
  /** Per-session context token count from workspace status (S36.5). */
  contextBySessionId: Map<string, number>;
}

/**
 * #1627: the Archived group — every archived session of this workspace,
 * rendered as a top-level entry INSIDE the workspace's session list.
 * Rendered only when at least one session is archived, and ALWAYS
 * collapsed by default: expanding requires a deliberate human click
 * (the owner's deliberate-friction ruling — nothing auto-expands it,
 * and selection never opens it). Rows stay fully usable when expanded:
 * open (history is viewable), unarchive, rename, delete.
 */
function ArchivedGroup({
  sessions,
  expanded,
  onToggleExpand,
  workspaceId,
  selectedSessionId,
  onSelectSession,
  onRenameSession,
  onArchiveSession,
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
  contextBySessionId,
}: ArchivedGroupProps) {
  return (
    <>
      <div className="group flex items-center rounded-md transition-colors hover:bg-accent/50" data-testid="archived-group">
        <button
          onClick={onToggleExpand}
          className="flex flex-1 items-center gap-1 rounded-md px-2 py-1.5 text-left text-sm text-muted-foreground"
          aria-label={expanded ? "Collapse archived sessions" : "Expand archived sessions"}
          aria-expanded={expanded}
        >
          {expanded ? (
            <ChevronDown className="h-3 w-3 flex-shrink-0" />
          ) : (
            <ChevronRight className="h-3 w-3 flex-shrink-0" />
          )}
          <Archive className="h-3 w-3 flex-shrink-0" aria-hidden="true" />
          <span className="flex-1 truncate text-muted-foreground/80">Archived</span>
          <span className="flex-shrink-0 text-xs text-muted-foreground/60">{sessions.length}</span>
        </button>
      </div>
      {expanded &&
        sessions.map((session) => (
          <SessionTreeRow
            key={session.id}
            node={{ session, children: [] }}
            depth={1}
            workspaceId={workspaceId}
            selectedSessionId={selectedSessionId}
            expanded={new Set()}
            onToggleExpand={() => {}}
            onSelectSession={onSelectSession}
            onRenameSession={onRenameSession}
            onArchiveSession={onArchiveSession}
            onDeleteSession={onDeleteSession}
            onAbortSession={onAbortSession}
            renamingSession={renamingSession}
            onRenameSessionCancel={onRenameSessionCancel}
            onRenameSessionConfirm={onRenameSessionConfirm}
            contextBySessionId={contextBySessionId}
            pendingIndicatorIds={new Set()}
          />
        ))}
    </>
  );
}

interface WorkersGroupProps {
  /** Worker-provenance root nodes, activity-sorted (their subagent subtrees attached). */
  nodes: SessionTreeNode[];
  expanded: boolean;
  onToggleExpand: () => void;
  /** Per-node subtree expansion — shared with the main tree so chevrons work inside the group. */
  nodeExpanded: Set<string>;
  onToggleNodeExpand: (sessionId: string) => void;
  workspaceId: string;
  selectedSessionId?: string;
  onSelectSession: (sessionId: string) => void;
  onRenameSession: (sessionId: string, title: string) => void;
  onArchiveSession: (sessionId: string, archived: boolean) => void;
  onDeleteSession: (sessionId: string) => void;
  onAbortSession: (sessionId: string) => void;
  renamingSession: { wsId: string; sessionId: string; title: string } | null;
  onRenameSessionCancel: () => void;
  onRenameSessionConfirm: (sessionId: string, title: string) => void;
  /** Per-session context token count from workspace status (S36.5). */
  contextBySessionId: Map<string, number>;
  /** Sessions (or ancestors of sessions) with a pending question/permission. */
  pendingIndicatorIds: Set<string>;
}

/**
 * #1966/rulings 6+7 (2026-10-28): the EXISTING Workers folder — no
 * new group — now populated by worker PROVENANCE with real nesting.
 * Roots are origin-class 'worker' sessions (board/engine fresh-worker
 * spawns, current AND past — idle included): parentage no longer
 * auto-banishes (ruling 7 — subtasks nest under their top-level
 * parent wherever it renders), so a user session's Task-tool subagents
 * nest beneath it in the main tree, and a worker's genuine subagent
 * children nest BENEATH it here (the folder is a real tree: shared
 * SessionTreeRow renderer, working expand state). Rows are sorted by
 * recent activity and fully usable when expanded: open, rename,
 * archive, delete, force stop — grouping is presentation-only.
 * Rendered only when at least one board worker exists (hidden
 * entirely otherwise); collapsed by default with a count badge so
 * activity stays visible without clutter. Rows inside the group
 * carry NO origin pill (2026-10-29 owner ruling: the group header
 * already says "Workers" — the pill only ate the title's width);
 * the origin rides a title tooltip on each row instead.
 */
function WorkersGroup({
  nodes,
  expanded,
  onToggleExpand,
  nodeExpanded,
  onToggleNodeExpand,
  workspaceId,
  selectedSessionId,
  onSelectSession,
  onRenameSession,
  onArchiveSession,
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
  contextBySessionId,
  pendingIndicatorIds,
}: WorkersGroupProps) {
  return (
    <>
      <div className="group flex items-center rounded-md transition-colors hover:bg-accent/50" data-testid="workers-group">
        <button
          onClick={onToggleExpand}
          className="flex flex-1 items-center gap-1 rounded-md px-2 py-1.5 text-left text-sm text-muted-foreground"
          aria-label={expanded ? "Collapse worker sessions" : "Expand worker sessions"}
          aria-expanded={expanded}
        >
          {expanded ? (
            <ChevronDown className="h-3 w-3 flex-shrink-0" />
          ) : (
            <ChevronRight className="h-3 w-3 flex-shrink-0" />
          )}
          <Bot className="h-3 w-3 flex-shrink-0" aria-hidden="true" />
          <span className="flex-1 truncate text-muted-foreground/80">Workers</span>
          <span className="flex-shrink-0 text-xs text-muted-foreground/60">{nodes.length}</span>
        </button>
      </div>
      {expanded &&
        nodes.map((node) => (
          <SessionTreeRow
            key={node.session.id}
            node={node}
            depth={1}
            rootLevel
            // 2026-10-29 owner ruling: no per-row worker pill inside
            // the group — the group header IS the classification, and
            // the pill ate the title's width. Covers the whole subtree
            // (nested subagent rows inherit the suppression).
            suppressOriginBadge
            workspaceId={workspaceId}
            selectedSessionId={selectedSessionId}
            expanded={nodeExpanded}
            onToggleExpand={onToggleNodeExpand}
            onSelectSession={onSelectSession}
            onRenameSession={onRenameSession}
            onArchiveSession={onArchiveSession}
            onDeleteSession={onDeleteSession}
            onAbortSession={onAbortSession}
            renamingSession={renamingSession}
            onRenameSessionCancel={onRenameSessionCancel}
            onRenameSessionConfirm={onRenameSessionConfirm}
            contextBySessionId={contextBySessionId}
            pendingIndicatorIds={pendingIndicatorIds}
          />
        ))}
    </>
  );
}
