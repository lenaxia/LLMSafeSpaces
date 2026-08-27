import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useParams, useLocation } from "react-router-dom";
import { workspacesApi } from "../../api/workspaces";
import { workspaceWorkflowApi } from "../../api/workflows";
import { useConfirmDialog } from "../../hooks/useConfirmDialog";
import { orgsApi } from "../../api/orgs";
import { ApiClientError } from "../../api/client";
import { useAuth } from "../../providers/AuthProvider";
import { useSessionStatus, useWorkspaceBusyCount, useSessionPendingActions, useWorkspaceHung } from "../../providers/SessionActivityProvider";
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
  HelpCircle,
  ChevronRight,
  ChevronDown,
  Play,
  Shield,
  Workflow,
  Zap,
} from "lucide-react";
import { Spinner } from "../ui/Spinner";
import { BusyIndicator } from "../ui/BusyIndicator";
import type { WorkspaceListItem } from "../../api/types";
import { sessionDisplayTitle } from "../../lib/names";
import { formatRelativeTime } from "../../lib/time";
import { useNow } from "../../hooks/useNow";
import { cn } from "../../lib/utils";
import { buildSessionTree, ancestorChain } from "../../lib/sessionTree";
import type { SessionTreeNode } from "../../lib/sessionTree";
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
      <div className="flex items-center justify-between border-b border-border px-4 py-3">
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
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
}: SessionListProps) {
  const { data: sessions, isLoading } = useQuery({
    queryKey: ["sessions", workspaceId],
    queryFn: async () => {
      const result = await workspacesApi.getSessions(workspaceId);
      const origins = await workspaceWorkflowApi.sessionOrigins(workspaceId);
      if (origins.length > 0) {
        const originMap = new Map(origins.map((o) => [o.sessionId, o.origin]));
        return result.map((s) => ({
          ...s,
          origin: originMap.get(s.id) || s.origin,
        }));
      }
      return result;
    },
    enabled: !!workspaceId,
  });

  // S36.5: Subscribe to workspace status cache (populated by ChatPage poller)
  // to show per-session context usage indicators. staleTime:Infinity + enabled:false
  // means: read from cache only, never trigger a fetch from here.
  // Build contextBySessionId from the sessions list (already fetched, no extra query needed).
  // context_used is persisted to session_index by the proxy on every step.ended event,
  // so it is available for all sessions regardless of workspace pod state.
  const contextBySessionId = useMemo(() => {
    const m = new Map<string, number>();
    for (const s of sessions ?? []) {
      if (s.contextUsed != null) {
        m.set(s.id, s.contextUsed);
      }
    }
    return m;
  }, [sessions]);

  // Tree shape: roots + orphans, where roots/orphans contain children of
  // arbitrary depth. Recomputed only when the session list changes.
  const tree = useMemo(() => buildSessionTree(sessions ?? []), [sessions]);

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
    if (!sessions) return;

    const prev = prevSelectedRef.current;
    const next = selectedSessionId;

    if (autoExpandChildren) {
      // --- Auto-collapse previous subtree root when leaving it ---
      if (prev && prev !== next) {
        const prevChain = ancestorChain(sessions, prev);
        const nextChain = next ? ancestorChain(sessions, next) : [];
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
        const chain = ancestorChain(sessions, next);
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
      const chain = ancestorChain(sessions, next);
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
  }, [sessions, selectedSessionId, autoExpandChildren]);

  // The synthetic "Orphaned subtasks" group is also collapsible, but
  // tracked separately from real session IDs to avoid any chance of
  // collision with a real ses_orphans-like ID.
  const [orphansExpanded, setOrphansExpanded] = useState(false);

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

      {!isLoading && (!sessions || sessions.length === 0) && (
        <div className="px-2 py-1 text-xs text-muted-foreground">No sessions yet</div>
      )}

      {sessions && sessions.length > 0 && (
        <div className="flex flex-col gap-0.5">
          {tree.roots.map((node) => (
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
              onDeleteSession={onDeleteSession}
              onAbortSession={onAbortSession}
              renamingSession={renamingSession}
              onRenameSessionCancel={onRenameSessionCancel}
              onRenameSessionConfirm={onRenameSessionConfirm}
              contextBySessionId={contextBySessionId}
              pendingIndicatorIds={pendingIndicatorIds}
            />
          ))}

          {tree.orphans.length > 0 && (
            <OrphansGroup
              orphans={tree.orphans}
              expanded={orphansExpanded}
              onToggleExpand={() => setOrphansExpanded((v) => !v)}
              workspaceId={workspaceId}
              selectedSessionId={selectedSessionId}
              childExpanded={expanded}
              onChildToggleExpand={toggleExpanded}
              onSelectSession={onSelectSession}
              onRenameSession={onRenameSession}
              onDeleteSession={onDeleteSession}
              onAbortSession={onAbortSession}
              renamingSession={renamingSession}
              onRenameSessionCancel={onRenameSessionCancel}
              onRenameSessionConfirm={onRenameSessionConfirm}
              contextBySessionId={contextBySessionId}
              pendingIndicatorIds={pendingIndicatorIds}
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
  workspaceId: string;
  selectedSessionId?: string;
  expanded: Set<string>;
  onToggleExpand: (sessionId: string) => void;
  onSelectSession: (sessionId: string) => void;
  onRenameSession: (sessionId: string, title: string) => void;
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
  workspaceId,
  selectedSessionId,
  expanded,
  onToggleExpand,
  onSelectSession,
  onRenameSession,
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
  const isSelected = s.id === selectedSessionId;
  const rowStatus: SessionDisplayStatus =
    depth === 0 && pendingIndicatorIds.has(s.id) ? "pending_input"
    : status === "pending_input" ? "busy"
    : status;
  const showPulse = rowStatus === "unread" && !isSelected && depth === 0;
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
          ) : (
            <MessageSquare className="h-3.5 w-3.5 flex-shrink-0" />
          )}
          <span className={cn("flex-1 truncate", (showPulse || rowStatus === "pending_input") && "animate-unread-pulse")}>{title}</span>
          {s.origin && s.origin !== "manual" && (
            <span
              className="flex-shrink-0 rounded px-1 text-[9px] font-medium uppercase"
              style={{
                color: s.origin === "routine" ? "#8b5cf6" : s.origin === "workflow" ? "#3b82f6" : "#6b7280",
                backgroundColor: s.origin === "routine" ? "#8b5cf620" : s.origin === "workflow" ? "#3b82f620" : "#6b728020",
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
            workspaceId={workspaceId}
            selectedSessionId={selectedSessionId}
            expanded={expanded}
            onToggleExpand={onToggleExpand}
            onSelectSession={onSelectSession}
            onRenameSession={onRenameSession}
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

interface OrphansGroupProps {
  orphans: SessionTreeNode[];
  expanded: boolean;
  onToggleExpand: () => void;
  workspaceId: string;
  selectedSessionId?: string;
  childExpanded: Set<string>;
  onChildToggleExpand: (sessionId: string) => void;
  onSelectSession: (sessionId: string) => void;
  onRenameSession: (sessionId: string, title: string) => void;
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

/** Synthetic top-level entry that collects all sessions whose parent is
 *  no longer in the workspace (e.g. the parent session was deleted).
 *  Renders as a non-clickable header row with a chevron, mirroring the
 *  collapsed/expanded behaviour of a real parent. */
function OrphansGroup({
  orphans,
  expanded,
  onToggleExpand,
  workspaceId,
  selectedSessionId,
  childExpanded,
  onChildToggleExpand,
  onSelectSession,
  onRenameSession,
  onDeleteSession,
  onAbortSession,
  renamingSession,
  onRenameSessionCancel,
  onRenameSessionConfirm,
  contextBySessionId,
  pendingIndicatorIds,
}: OrphansGroupProps) {
  return (
    <>
      <div className="group flex items-center rounded-md transition-colors hover:bg-accent/50">
        <button
          onClick={onToggleExpand}
          className="flex flex-1 items-center gap-1 rounded-md px-2 py-1.5 text-left text-sm text-muted-foreground italic"
          aria-label={expanded ? "Collapse orphaned subtasks" : "Expand orphaned subtasks"}
          aria-expanded={expanded}
        >
          {expanded ? (
            <ChevronDown className="h-3 w-3 flex-shrink-0" />
          ) : (
            <ChevronRight className="h-3 w-3 flex-shrink-0" />
          )}
          <span className="flex-1 truncate">Orphaned subtasks</span>
          <span className="flex-shrink-0 text-xs text-muted-foreground/60">{orphans.length}</span>
        </button>
      </div>
      {expanded &&
        orphans.map((node) => (
          <SessionTreeRow
            key={node.session.id}
            node={node}
            depth={1}
            workspaceId={workspaceId}
            selectedSessionId={selectedSessionId}
            expanded={childExpanded}
            onToggleExpand={onChildToggleExpand}
            onSelectSession={onSelectSession}
            onRenameSession={onRenameSession}
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
