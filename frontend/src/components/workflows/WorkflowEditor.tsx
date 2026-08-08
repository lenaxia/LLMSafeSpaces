import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { Workflow } from "../../api/workflows";
import { workspacesApi } from "../../api/workspaces";
import { Badge } from "../ui/Badge";
import { Play, Trash2, Code2, Workflow as WorkflowIcon } from "lucide-react";
import { DAGCanvas } from "./DAGCanvas";
import { type WorkflowSpec, parseSpec, serializeSpec } from "./dagTypes";

interface WorkflowEditorProps {
  mode: "create" | "edit";
  workflow?: Workflow;
  onSave: (name: string, spec: string, status: string, opts?: {
    targetWorkspaceId?: string; onMissingWorkspace?: string;
    inputSchema?: unknown; defaults?: unknown;
  }) => Promise<void>;
  onCancel?: () => void;
  onDelete?: () => void;
  onRun?: (input?: string, workspaceId?: string) => Promise<void>;
}

export function WorkflowEditor({ mode, workflow, onSave, onCancel, onDelete, onRun }: WorkflowEditorProps) {
  const [name, setName] = useState(workflow?.name || "");
  const [spec, setSpec] = useState<WorkflowSpec>(parseSpec(workflow?.specYaml || defaultSpec));
  const [status, setStatus] = useState(workflow?.status || "draft");
  const [targetWorkspaceId, setTargetWorkspaceId] = useState(workflow?.targetWorkspaceId || "");
  const [onMissingWorkspace, setOnMissingWorkspace] = useState(workflow?.onMissingWorkspace || "abort");
  const [inputSchemaStr, setInputSchemaStr] = useState(() => {
    const s = workflow?.inputSchema;
    if (!s) return "";
    return typeof s === "string" ? s : JSON.stringify(s, null, 2);
  });
  const [defaultsStr, setDefaultsStr] = useState(() => {
    const d = workflow?.defaults;
    if (!d) return "";
    return typeof d === "string" ? d : JSON.stringify(d, null, 2);
  });
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [showRunDialog, setShowRunDialog] = useState(false);
  const [runInput, setRunInput] = useState("");
  const [runWorkspaceId, setRunWorkspaceId] = useState("");
  const [viewMode, setViewMode] = useState<"visual" | "json">("visual");

  const { data: workspaces } = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => workspacesApi.list(),
  }) as { data: { items?: { id: string; name: string; phase: string }[] } | undefined };

  const handleSave = async () => {
    setSaving(true);
    setError(null);
    try {
      let parsedSchema: unknown;
      if (inputSchemaStr.trim()) {
        try {
          parsedSchema = JSON.parse(inputSchemaStr);
        } catch {
          setError("Input Schema is not valid JSON");
          setSaving(false);
          return;
        }
      }
      let parsedDefaults: unknown;
      if (defaultsStr.trim()) {
        try {
          parsedDefaults = JSON.parse(defaultsStr);
        } catch {
          setError("Defaults block is not valid JSON");
          setSaving(false);
          return;
        }
      }
      await onSave(name, serializeSpec(spec), status, {
        targetWorkspaceId: targetWorkspaceId || undefined,
        onMissingWorkspace,
        inputSchema: parsedSchema,
        defaults: parsedDefaults,
      });
    } catch (e: any) {
      const msg = e?.message || String(e);
      setError(msg);
    } finally {
      setSaving(false);
    }
  };

  const specJson = serializeSpec(spec);

  return (
    <div className="flex h-full flex-col overflow-hidden">
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
          <div className="flex rounded-md border border-border">
            <button
              onClick={() => setViewMode("visual")}
              className={`flex items-center gap-1 rounded-l-md px-2 py-1 text-xs ${viewMode === "visual" ? "bg-accent" : ""}`}
            >
              <WorkflowIcon className="h-3.5 w-3.5" /> Visual
            </button>
            <button
              onClick={() => setViewMode("json")}
              className={`flex items-center gap-1 rounded-r-md border-l border-border px-2 py-1 text-xs ${viewMode === "json" ? "bg-accent" : ""}`}
            >
              <Code2 className="h-3.5 w-3.5" /> JSON
            </button>
          </div>
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
            disabled={saving || !name}
            className="rounded-md bg-primary px-3 py-1 text-sm text-primary-foreground disabled:opacity-50"
          >
            {saving ? "Saving..." : mode === "create" ? "Create" : "Save"}
          </button>
          {mode === "create" && onCancel && (
            <button onClick={onCancel} className="rounded-md border px-3 py-1 text-sm">Cancel</button>
          )}
        </div>
      </div>

      <div className="flex items-center gap-4 border-b border-border px-4 py-2">
        <div className="flex items-center gap-2">
          <label className="text-xs text-muted-foreground whitespace-nowrap">Target Workspace:</label>
          <select
            className="rounded border border-border px-2 py-1 text-sm bg-background max-w-[200px]"
            value={targetWorkspaceId}
            onChange={(e) => setTargetWorkspaceId(e.target.value)}
          >
            <option value="">Caller picks at run time</option>
            {(workspaces?.items || []).map((ws) => (
              <option key={ws.id} value={ws.id}>{ws.name} ({ws.phase})</option>
            ))}
          </select>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-xs text-muted-foreground whitespace-nowrap">If missing:</label>
          <select
            className="rounded border border-border px-2 py-1 text-sm bg-background"
            value={onMissingWorkspace}
            onChange={(e) => setOnMissingWorkspace(e.target.value)}
            disabled={!targetWorkspaceId}
          >
            <option value="abort">Abort (fail run)</option>
            <option value="create">Create new workspace</option>
          </select>
          {!targetWorkspaceId && (
            <span className="text-xs text-muted-foreground">(pick a workspace to enable)</span>
          )}
        </div>
      </div>

      {error && (
        <div className="border-b border-destructive/30 bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {error}
        </div>
      )}

      <div className="border-b border-border">
        <button
          onClick={() => setShowAdvanced(!showAdvanced)}
          className="flex w-full items-center gap-1 px-4 py-1.5 text-xs text-muted-foreground hover:bg-accent"
        >
          {showAdvanced ? "▼" : "▶"} Advanced (input schema, defaults)
        </button>
        {showAdvanced && (
          <div className="grid grid-cols-2 gap-3 px-4 pb-3">
            <div>
              <label className="mb-1 block text-xs text-muted-foreground">Input Schema (JSON Schema — validates manual run input)</label>
              <textarea
                className="h-24 w-full rounded border border-border p-2 font-mono text-xs bg-background resize-y"
                spellCheck={false}
                value={inputSchemaStr}
                onChange={(e) => setInputSchemaStr(e.target.value)}
                placeholder={'{"type":"object","properties":{"meetingId":{"type":"string"}}}'}
              />
            </div>
            <div>
              <label className="mb-1 block text-xs text-muted-foreground">Defaults (maxAttempts, timeout — applied to all nodes)</label>
              <textarea
                className="h-24 w-full rounded border border-border p-2 font-mono text-xs bg-background resize-y"
                spellCheck={false}
                value={defaultsStr}
                onChange={(e) => setDefaultsStr(e.target.value)}
                placeholder={'{"maxAttempts": 2, "timeout": "10m"}'}
              />
            </div>
          </div>
        )}
      </div>

      {viewMode === "visual" ? (
        <DAGCanvas spec={spec} onSpecChange={setSpec} />
      ) : (
        <div className="flex-1 overflow-auto p-4">
          <textarea
            className="h-full w-full rounded border border-border p-3 font-mono text-sm bg-background resize-none"
            spellCheck={false}
            value={specJson}
            onChange={(e) => {
              const parsed = parseSpec(e.target.value);
              setSpec(parsed);
            }}
          />
        </div>
      )}

      {showRunDialog && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50" onClick={() => setShowRunDialog(false)}>
          <div className="w-96 rounded-lg bg-background p-6 shadow-lg" onClick={(e) => e.stopPropagation()}>
            <h3 className="mb-3 text-sm font-semibold">Run Workflow</h3>
            {!targetWorkspaceId && (
              <div className="mb-3">
                <label className="mb-1 block text-xs text-muted-foreground">Workspace (no default set):</label>
                <select
                  className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
                  value={runWorkspaceId}
                  onChange={(e) => setRunWorkspaceId(e.target.value)}
                >
                  <option value="">Select workspace...</option>
                  {(workspaces?.items || []).map((ws) => (
                    <option key={ws.id} value={ws.id}>{ws.name} ({ws.phase})</option>
                  ))}
                </select>
              </div>
            )}
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
                  await onRun?.(runInput || undefined, !targetWorkspaceId ? runWorkspaceId || undefined : undefined);
                }}
                disabled={!targetWorkspaceId && !runWorkspaceId}
                className="rounded bg-primary px-3 py-1 text-sm text-primary-foreground disabled:opacity-50"
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
