import { useState } from "react";
import type { Workflow } from "../../api/workflows";
import { Badge } from "../ui/Badge";
import { Play, Trash2 } from "lucide-react";

interface WorkflowEditorProps {
  mode: "create" | "edit";
  workflow?: Workflow;
  onSave: (name: string, spec: string, status: string) => Promise<void>;
  onCancel?: () => void;
  onDelete?: () => void;
  onRun?: (input?: string) => Promise<void>;
}

export function WorkflowEditor({ mode, workflow, onSave, onCancel, onDelete, onRun }: WorkflowEditorProps) {
  const [name, setName] = useState(workflow?.name || "");
  const [spec, setSpec] = useState(workflow?.specYaml || defaultSpec);
  const [status, setStatus] = useState(workflow?.status || "draft");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showRunDialog, setShowRunDialog] = useState(false);
  const [runInput, setRunInput] = useState("");

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      await onSave(name, spec, status);
    } catch (e: any) {
      const msg = e?.message || String(e);
      setError(msg);
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex h-full flex-col overflow-hidden">
      {/* Toolbar */}
      <div className="flex items-center justify-between border-b border-border px-4 py-2">
        <div className="flex items-center gap-3">
          <input
            className="rounded border border-border px-2 py-1 text-sm font-medium bg-background"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Workflow name"
          />
          <select
            className="rounded border border-border px-2 py-1 text-sm bg-background"
            value={status}
            onChange={(e) => setStatus(e.target.value)}
          >
            <option value="draft">Draft</option>
            <option value="active">Active</option>
            <option value="archived">Archived</option>
          </select>
          {workflow && <Badge variant="default" className="text-xs">{workflow.slug}</Badge>}
        </div>
        <div className="flex items-center gap-2">
          {mode === "edit" && onRun && (
            <button
              onClick={() => setShowRunDialog(true)}
              className="flex items-center gap-1 rounded-md px-3 py-1 text-sm border hover:bg-accent"
            >
              <Play className="h-3.5 w-3.5" /> Run
            </button>
          )}
          {mode === "edit" && onDelete && (
            <button
              onClick={onDelete}
              className="flex items-center gap-1 rounded-md px-3 py-1 text-sm border border-destructive text-destructive hover:bg-destructive/10"
            >
              <Trash2 className="h-3.5 w-3.5" /> Delete
            </button>
          )}
          <button
            onClick={handleSave}
            disabled={saving || !name || !spec}
            className="rounded-md bg-primary px-3 py-1 text-sm text-primary-foreground disabled:opacity-50"
          >
            {saving ? "Saving..." : mode === "create" ? "Create" : "Save"}
          </button>
          {mode === "create" && onCancel && (
            <button onClick={onCancel} className="rounded-md border px-3 py-1 text-sm">
              Cancel
            </button>
          )}
        </div>
      </div>

      {/* Error banner */}
      {error && (
        <div className="border-b border-destructive/30 bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {error}
        </div>
      )}

      {/* Spec editor — textarea for now; @xyflow/react canvas will replace this */}
      <div className="flex-1 overflow-auto p-4">
        <div className="mb-2 flex items-center justify-between">
          <span className="text-xs text-muted-foreground">
            DAG Spec (JSON) — visual editor coming soon
          </span>
        </div>
        <textarea
          className="h-full w-full rounded border border-border p-3 font-mono text-sm bg-background resize-none"
          value={spec}
          onChange={(e) => setSpec(e.target.value)}
          spellCheck={false}
        />
      </div>

      {/* Run dialog */}
      {showRunDialog && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={() => setShowRunDialog(false)}>
          <div className="w-96 rounded-lg bg-background p-6 shadow-lg" onClick={(e) => e.stopPropagation()}>
            <h3 className="mb-3 text-sm font-semibold">Run Workflow</h3>
            <textarea
              className="mb-3 h-32 w-full rounded border border-border p-2 text-sm font-mono"
              placeholder='{"key": "value"} (optional input JSON)'
              value={runInput}
              onChange={(e) => setRunInput(e.target.value)}
              spellCheck={false}
            />
            <div className="flex justify-end gap-2">
              <button onClick={() => setShowRunDialog(false)} className="rounded border px-3 py-1 text-sm">Cancel</button>
              <button
                onClick={async () => {
                  setShowRunDialog(false);
                  await onRun?.(runInput || undefined);
                }}
                className="rounded bg-primary px-3 py-1 text-sm text-primary-foreground"
              >
                Start Run
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

const defaultSpec = JSON.stringify({
  nodes: [
    {
      id: "start",
      type: "script",
      position: { x: 100, y: 100 },
      data: {
        language: "python",
        handler: "def handler(input):\n    return {\"result\": \"hello from workflow\"}",
      },
    },
  ],
  edges: [],
}, null, 2);
