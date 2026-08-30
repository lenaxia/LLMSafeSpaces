import { useState } from "react";
import { createPortal } from "react-dom";
import { useQuery, useMutation, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { agentRolesApi, type AgentRole } from "../../api/agentRoles";
import { promptsApi } from "../../api/prompts";
import { ChevronDown, Lock } from "lucide-react";
import { useMenuPosition } from "../ui/menuPosition";

interface Props {
  workspaceId: string;
  orgId?: string;
  disabled?: boolean;
}

export function RoleSelector({ workspaceId, orgId, disabled }: Props) {
  const [open, setOpen] = useState(false);
  const [optimisticRole, setOptimisticRole] = useState<string | null>(null);
  const queryClient = useQueryClient();

  const { data: currentRole } = useQuery({
    queryKey: ["workspace-role", workspaceId],
    queryFn: () => agentRolesApi.getWorkspaceRole(workspaceId),
    enabled: !!workspaceId,
    staleTime: 30_000,
    placeholderData: keepPreviousData,
  });

  const { data: allowUserPrompt } = useQuery({
    queryKey: ["org-allow-prompt", orgId],
    queryFn: async () => {
      if (!orgId) return true;
      try {
        const data = await promptsApi.getOrg(orgId);
        return data.allowUserPrompt;
      } catch {
        return false;
      }
    },
    enabled: !!orgId,
    staleTime: 30_000,
  });

  const { data: platformRoles = [] } = useQuery({
    queryKey: ["platform-roles"],
    queryFn: () => agentRolesApi.listPlatform(),
    staleTime: 60_000,
  });

  const { data: orgRoles = [] } = useQuery({
    queryKey: ["org-roles", orgId],
    queryFn: () => agentRolesApi.listOrg(orgId!),
    enabled: !!orgId,
    staleTime: 60_000,
  });

  const setRoleMutation = useMutation({
    mutationFn: (roleId: string) => agentRolesApi.setWorkspaceRole(workspaceId, roleId),
    onSuccess: () => {
      setOptimisticRole(null);
      queryClient.invalidateQueries({ queryKey: ["workspace-role", workspaceId] });
    },
    onError: () => {
      setOptimisticRole(null);
    },
  });

  const clearRoleMutation = useMutation({
    mutationFn: () => agentRolesApi.clearWorkspaceRole(workspaceId),
    onSuccess: () => {
      setOptimisticRole(null);
      queryClient.invalidateQueries({ queryKey: ["workspace-role", workspaceId] });
    },
    onError: () => {
      setOptimisticRole(null);
    },
  });

  const isLocked = orgId !== undefined && allowUserPrompt === false;
  const allRoles = [...orgRoles, ...platformRoles.filter((pr) => !orgRoles.some((or) => or.extends === pr.id))];
  const roleId = optimisticRole ?? currentRole?.id ?? null;
  const currentName = roleId
    ? allRoles.find((r) => r.id === roleId)?.name ?? "Custom"
    : "Default";

  // Viewport-aware positioning: the selector sits in the composer options
  // drawer at the bottom of the screen, so the dropdown must flip above /
  // clamp horizontally / cap height instead of opening blindly downward
  // off-screen.
  const { triggerRef, menuRef, pos } = useMenuPosition(open, "right", 224);

  if (isLocked) {
    return (
      <span
        className="flex items-center gap-1 text-xs text-muted-foreground px-2 py-1"
        title="Your org admin manages agent roles"
      >
        <Lock className="h-3 w-3" />
        <span className="max-w-[120px] truncate">{currentName}</span>
      </span>
    );
  }

  if (allRoles.length === 0 && !currentRole) {
    return null;
  }

  return (
    <div className="relative">
      <button
        ref={triggerRef}
        type="button"
        onClick={() => setOpen(!open)}
        disabled={disabled}
        className="flex items-center gap-1 rounded-md border border-input bg-background px-2 py-1 text-xs hover:bg-accent disabled:opacity-50"
      >
        <span className="max-w-[120px] truncate">{currentName}</span>
        <ChevronDown className="h-3 w-3 shrink-0" />
      </button>

      {open &&
        createPortal(
          <>
            <div className="fixed inset-0 z-40" onClick={() => setOpen(false)} />
            <div
              ref={menuRef}
              role="menu"
              className="fixed z-50 max-h-64 w-56 overflow-y-auto rounded-md border border-border bg-popover shadow-md"
              style={{ top: pos.top, left: pos.left, maxHeight: pos.maxHeight }}
            >
              {roleId && (
                <button
                  role="menuitem"
                  type="button"
                  onClick={() => {
                    setOpen(false);
                    setOptimisticRole(null);
                    clearRoleMutation.mutate();
                  }}
                  className="flex w-full items-center justify-between px-3 py-2 text-left text-xs hover:bg-accent text-muted-foreground"
                >
                  <span>Use platform default</span>
                </button>
              )}
              {allRoles.map((role: AgentRole) => (
                <button
                  key={role.id}
                  role="menuitem"
                  type="button"
                  onClick={() => {
                    setOptimisticRole(role.id);
                    setOpen(false);
                    setRoleMutation.mutate(role.id);
                  }}
                  className={`flex w-full flex-col px-3 py-2 text-left text-xs hover:bg-accent ${
                    role.id === roleId ? "bg-accent/50" : ""
                  }`}
                >
                  <span className="font-medium">{role.name}</span>
                  {role.description && (
                    <span className="text-muted-foreground line-clamp-1">{role.description}</span>
                  )}
                </button>
              ))}
            </div>
          </>,
          document.body,
        )}
    </div>
  );
}
