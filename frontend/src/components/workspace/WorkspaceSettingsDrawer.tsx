import * as Dialog from "@radix-ui/react-dialog";
import { X, Lock, ExternalLink } from "lucide-react";
import { useState, useEffect } from "react";
import type { WorkspaceListItem } from "../../api/types";
import { secretsApi, type SecretResponse } from "../../api/secrets";
import { api } from "../../api/client";
import { promptsApi } from "../../api/prompts";

interface Props {
  workspace: WorkspaceListItem;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

const SECRET_TYPE_LABELS: Record<string, { label: string; icon: string }> = {
  "llm-provider": { label: "LLM Providers", icon: "🤖" },
  "ssh-key": { label: "SSH Keys", icon: "🔑" },
  "git-credential": { label: "Git Credentials", icon: "📦" },
  "secret-file": { label: "Secret Files", icon: "📄" },
  "env-secret": { label: "Environment Variables", icon: "⚙️" },
  "api-key": { label: "API Keys (legacy)", icon: "🗝️" },
};

export function WorkspaceSettingsDrawer({ workspace, open, onOpenChange }: Props) {
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [allSecrets, setAllSecrets] = useState<SecretResponse[]>([]);
  const [boundIds, setBoundIds] = useState<Set<string>>(new Set());
  const [bindingsChanged, setBindingsChanged] = useState(false);
  const [customPrompt, setCustomPrompt] = useState("");
  const [promptLocked, setPromptLocked] = useState(false);
  const [devPreview, setDevPreview] = useState(false);
  const [previewPort, setPreviewPort] = useState("5173");

  useEffect(() => {
    if (!open) return;
    Promise.all([
      secretsApi.list(),
      api.get<{ bindings: { secretId: string }[]; devPreviewEnabled?: boolean }>(`/workspaces/${workspace.id}/bindings`).catch(() => ({ bindings: [], devPreviewEnabled: undefined })),
      api.get<{ devPreviewEnabled?: boolean }>(`/workspaces/${workspace.id}`).catch(() => ({ devPreviewEnabled: undefined })),
    ]).then(([secretsRes, bindingsRes, wsRes]) => {
      setAllSecrets(secretsRes.secrets || []);
      setBoundIds(new Set((bindingsRes.bindings || []).map((b) => b.secretId)));
      setDevPreview(!!(wsRes as { devPreviewEnabled?: boolean }).devPreviewEnabled);
    }).catch(() => {});

    const orgId = workspace.orgId;
    if (orgId) {
      promptsApi.getOrg(orgId).then((data) => {
        setPromptLocked(!data.allowUserPrompt);
      }).catch(() => {
        setPromptLocked(true);
      });
    }
    api.get<{ prompt?: string }>(`/workspaces/${workspace.id}/prompt`).then((data) => {
      setCustomPrompt(data.prompt ?? "");
    }).catch(() => {});
  }, [open, workspace]);

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      if (bindingsChanged) {
        await api.put(`/workspaces/${workspace.id}/bindings`, { secretIds: Array.from(boundIds) });
      }
      if (!promptLocked) {
        await api.put(`/workspaces/${workspace.id}/prompt`, { prompt: customPrompt });
      }
      await api.put(`/workspaces/${workspace.id}/dev-preview`, { enabled: devPreview });
      onOpenChange(false);
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Save failed");
    } finally {
      setSaving(false);
    }
  };

  const toggleBinding = (secretId: string) => {
    setBoundIds((prev) => {
      const next = new Set(prev);
      if (next.has(secretId)) next.delete(secretId);
      else next.add(secretId);
      return next;
    });
    setBindingsChanged(true);
  };

  const grouped = Object.entries(SECRET_TYPE_LABELS)
    .map(([type, meta]) => ({
      type,
      ...meta,
      secrets: allSecrets.filter((s) => s.type === type),
    }))
    .filter((g) => g.secrets.length > 0);

  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50 data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=closed]:animate-out data-[state=closed]:fade-out-0" />
        <Dialog.Content className="fixed right-0 top-0 z-50 h-full w-80 max-w-full bg-background border-l border-border shadow-xl p-6 overflow-y-auto data-[state=open]:animate-in data-[state=open]:slide-in-from-right data-[state=closed]:animate-out data-[state=closed]:slide-out-to-right duration-200">
          <div className="flex items-center justify-between mb-6">
            <Dialog.Title className="text-sm font-semibold">
              Workspace Settings
            </Dialog.Title>
            <Dialog.Close className="rounded p-1 hover:bg-accent">
              <X className="h-4 w-4" />
            </Dialog.Close>
          </div>
          <Dialog.Description className="sr-only">
            Configure settings for this workspace
          </Dialog.Description>

          <p className="text-xs text-muted-foreground mb-6 truncate">{workspace.name}</p>

          <div className="space-y-5">
            {/* Custom Agent Instructions */}
            <div className="border-t border-border pt-4">
              <label className="text-sm font-medium">Custom Instructions</label>
              <p className="text-xs text-muted-foreground mb-2">
                Instructions specific to this workspace — e.g. "focus on test coverage this session", "use Tailwind for new components".
              </p>
              {promptLocked ? (
                <div className="flex items-center gap-2 rounded-md bg-muted/50 px-3 py-2 text-xs text-muted-foreground">
                  <Lock className="h-3 w-3" />
                  Managed by your organization. Contact your admin to request changes.
                </div>
              ) : (
                <textarea
                  className="w-full min-h-[80px] rounded-md border border-border bg-background px-3 py-2 text-sm font-mono"
                  placeholder="Focus on test coverage this session..."
                  value={customPrompt}
                  onChange={(e) => setCustomPrompt(e.target.value)}
                  maxLength={10000}
                />
              )}
            </div>

            {allSecrets.length > 0 && (
              <div className="border-t border-border pt-4">
                <label className="text-sm font-medium">Attached Secrets</label>
                <p className="text-xs text-muted-foreground mb-3">Secrets injected when this workspace starts</p>
                <div className="space-y-3 max-h-64 overflow-y-auto">
                  {grouped.map((group) => (
                    <div key={group.type}>
                      <p className="text-xs font-medium text-muted-foreground mb-1">
                        {group.icon} {group.label}
                      </p>
                      <div className="space-y-0.5 ml-1">
                        {group.secrets.map((s) => (
                          <label key={s.id} className="flex items-center gap-2 rounded px-2 py-1 hover:bg-accent/50 cursor-pointer">
                            <input
                              type="checkbox"
                              checked={boundIds.has(s.id)}
                              onChange={() => toggleBinding(s.id)}
                              className="rounded border-border"
                            />
                            <span className="text-sm">{s.name}</span>
                          </label>
                        ))}
                      </div>
                    </div>
                  ))}
                </div>
              </div>
            )}

            {/* Dev Preview (Epic 66) */}
            <div className="border-t border-border pt-4">
              <label className="text-sm font-medium">Dev Preview</label>
              <p className="text-xs text-muted-foreground mb-3">
                Tunnel HTTP/WebSocket from your browser to a dev server (Vite, Next, etc.) running in this workspace.
              </p>
              <label className="flex items-center gap-2 rounded px-2 py-1 hover:bg-accent/50 cursor-pointer">
                <input
                  type="checkbox"
                  checked={devPreview}
                  onChange={(e) => setDevPreview(e.target.checked)}
                  className="rounded border-border"
                />
                <span className="text-sm">Enable dev preview tunnel</span>
              </label>
              {devPreview && workspace.phase === "Active" && (
                <div className="mt-3 flex items-center gap-2">
                  <input
                    type="text"
                    inputMode="numeric"
                    placeholder="5173"
                    value={previewPort}
                    onChange={(e) => setPreviewPort(e.target.value.replace(/[^0-9]/g, ""))}
                    className="w-20 rounded-md border border-border bg-background px-2 py-1 text-sm"
                  />
                  <a
                    href={`/api/v1/workspaces/${workspace.id}/dev-preview/${previewPort || "5173"}/`}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="inline-flex items-center gap-1 rounded-md border border-border px-2 py-1 text-xs hover:bg-accent"
                  >
                    <ExternalLink className="h-3 w-3" />
                    Open preview
                  </a>
                </div>
              )}
              {devPreview && workspace.phase !== "Active" && (
                <p className="text-xs text-muted-foreground mt-2">
                  Workspace must be Active to open the preview.
                </p>
              )}
            </div>
          </div>

          {error && <p className="text-xs text-destructive mt-4">{error}</p>}

          <div className="mt-8 flex gap-2">
            <button
              onClick={handleSave}
              disabled={saving}
              className="flex-1 rounded-md bg-primary px-3 py-1.5 text-sm text-primary-foreground hover:bg-primary/90 disabled:opacity-50"
            >
              {saving ? "Saving..." : "Save"}
            </button>
            <Dialog.Close className="flex-1 rounded-md border border-border px-3 py-1.5 text-sm hover:bg-accent">
              Cancel
            </Dialog.Close>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
