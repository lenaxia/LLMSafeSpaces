import { useEffect, useState } from "react";
import { createPortal } from "react-dom";
import { useQuery, useMutation, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { workspacesApi } from "../../api/workspaces";
import type { ModelInfo } from "../../api/workspaces";
import { useUserSetting } from "../../hooks/useUserSettings";
import { ChevronDown } from "lucide-react";
import { useMenuPosition } from "../ui/menuPosition";

interface Props {
  workspaceId: string;
  disabled?: boolean;
}

export function ModelSelector({ workspaceId, disabled }: Props) {
  const [open, setOpen] = useState(false);
  const [toast, setToast] = useState<string | null>(null);
  const [optimisticModel, setOptimisticModel] = useState<string | null>(null);
  const queryClient = useQueryClient();
  const preferredModel = useUserSetting<string>("preferredModel", "");

  // Viewport-aware positioning: the selector sits in the composer options
  // drawer at the bottom of the screen, so the dropdown (and the transient
  // toast) must flip above / clamp horizontally / cap height instead of
  // opening blindly downward off-screen. The toast shares the dropdown's
  // anchor via triggerRef.
  const { triggerRef, menuRef, pos } = useMenuPosition(open, "right", 256);
  const { menuRef: toastMenuRef, pos: toastPos } = useMenuPosition<HTMLButtonElement>(
    toast !== null,
    "right",
    224,
    triggerRef,
  );

  const { data, isLoading, isError } = useQuery({
    queryKey: ["models", workspaceId],
    queryFn: () => workspacesApi.listModels(workspaceId),
    enabled: !!workspaceId,
    staleTime: 10_000,
    retry: 1,
    placeholderData: keepPreviousData,
  });

  const setModelMutation = useMutation({
    mutationFn: (model: string) => workspacesApi.setModel(workspaceId, model),
    onSuccess: (data) => {
      setOptimisticModel(null);
      queryClient.invalidateQueries({ queryKey: ["models", workspaceId] });
      if (data && !data.applied) {
        setToast("Model saved — takes effect on your next message.");
      }
    },
    onError: () => {
      setOptimisticModel(null);
      setToast("Failed to set model. Please try again.");
    },
  });

  const handleSelectModel = (modelId: string) => {
    setOptimisticModel(modelId);
    setOpen(false);
    setModelMutation.mutate(modelId);
  };

  const models = data?.models ?? [];
  const serverModel = data?.currentModel || "";
  const currentModel = optimisticModel ?? serverModel;
  const currentDisplay = currentModel
    ? models.find((m) => m.id === currentModel)?.name || currentModel.split("/").pop()
    : "Select model";

  useEffect(() => {
    if (!workspaceId || !preferredModel || serverModel || models.length === 0) return;
    if (setModelMutation.isPending) return;
    const available = models.some((m) => m.id === preferredModel);
    if (!available) return;
    setModelMutation.mutate(preferredModel);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [workspaceId, preferredModel, serverModel, models]);

  useEffect(() => {
    if (!toast) return;
    const id = setTimeout(() => setToast(null), 4000);
    return () => clearTimeout(id);
  }, [toast]);

  // Show spinner only on the very first load (no prior data in cache).
  if (isLoading && models.length === 0) {
    return (
      <span className="text-xs text-muted-foreground px-2 py-1">
        Loading models...
      </span>
    );
  }

  if (isError && models.length === 0) {
    return (
      <span className="text-xs text-destructive px-2 py-1" title="Could not load models">
        ⚠ Models
      </span>
    );
  }

  // Return null only when we have a confirmed empty result (not a loading race).
  if (!isLoading && models.length === 0) {
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
        <span className="max-w-[160px] truncate">{currentDisplay}</span>
        <ChevronDown className="h-3 w-3 shrink-0" />
      </button>

      {open &&
        createPortal(
          <>
            {/* Backdrop to close on click outside */}
            <div className="fixed inset-0 z-40" onClick={() => setOpen(false)} />
            <div
              ref={menuRef}
              role="menu"
              className="fixed z-50 max-h-64 w-64 overflow-y-auto rounded-md border border-border bg-popover shadow-md"
              style={{ top: pos.top, left: pos.left, maxHeight: pos.maxHeight }}
            >
              {/* Backend already filters out unavailable models; no frontend re-filter needed */}
              {models.map((m: ModelInfo) => (
                <button
                  key={m.id}
                  role="menuitem"
                  type="button"
                  onClick={() => handleSelectModel(m.id)}
                  className={`flex w-full items-center justify-between px-3 py-2 text-left text-xs hover:bg-accent ${
                    m.id === currentModel ? "bg-accent/50 font-medium" : ""
                  }`}
                >
                  <span className="truncate">{m.name || m.id}</span>
                  <span className={`ml-2 shrink-0 rounded px-1 py-0.5 text-[10px] ${
                    m.freeTier ? "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-200" : "bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200"
                  }`}>
                    {m.tier}
                  </span>
                </button>
              ))}
            </div>
          </>,
          document.body,
        )}
      {toast &&
        createPortal(
          <div
            ref={toastMenuRef}
            className="fixed z-50 w-max max-w-64 rounded-md border border-border bg-popover px-3 py-2 text-xs shadow-md"
            style={{ top: toastPos.top, left: toastPos.left, maxHeight: toastPos.maxHeight }}
          >
            {toast}
          </div>,
          document.body,
        )}
    </div>
  );
}
