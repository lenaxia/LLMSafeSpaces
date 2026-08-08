import { useState } from "react";
import { X, Plus, Trash2 } from "lucide-react";
import type { FlowNode, NodeType, ScriptNodeData, AgentNodeData, HTTPNodeData, ConditionNodeData } from "./dagTypes";

interface NodeEditPanelProps {
  node: FlowNode | null;
  onClose: () => void;
  onChange: (id: string, data: Record<string, unknown>) => void;
  onDelete: (id: string) => void;
}

export function NodeEditPanel({ node, onClose, onChange, onDelete }: NodeEditPanelProps) {
  if (!node) return null;

  return (
    <div className="flex h-full w-80 flex-col border-l border-border bg-background">
      <div className="flex items-center justify-between border-b border-border px-3 py-2">
        <h3 className="text-sm font-semibold capitalize">{node.type} Node</h3>
        <div className="flex gap-1">
          <button
            onClick={() => { onDelete(node.id); onClose(); }}
            className="rounded p-1 text-destructive hover:bg-destructive/10"
            title="Delete node"
          >
            <Trash2 className="h-3.5 w-3.5" />
          </button>
          <button onClick={onClose} className="rounded p-1 hover:bg-accent">
            <X className="h-3.5 w-3.5" />
          </button>
        </div>
      </div>

      <div className="flex-1 space-y-3 overflow-y-auto p-3">
        <div>
          <label className="mb-1 block text-xs font-medium text-muted-foreground">Node ID</label>
          <input
            className="w-full rounded border border-border px-2 py-1 font-mono text-xs bg-background"
            value={node.id}
            readOnly
          />
        </div>

        <div>
          <label className="mb-1 block text-xs font-medium text-muted-foreground">Label</label>
          <input
            className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
            value={(node.data as any).label || ""}
            onChange={(e) => onChange(node.id, { ...node.data, label: e.target.value })}
            placeholder="Display name"
          />
        </div>

        <NodeFields node={node} onChange={onChange} />

        <div className="grid grid-cols-2 gap-2 border-t border-border pt-3">
          <div>
            <label className="mb-1 block text-xs font-medium text-muted-foreground">Max Attempts</label>
            <input
              type="number"
              min={1}
              max={10}
              className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
              value={(node.data as any).maxAttempts || ""}
              onChange={(e) => onChange(node.id, { ...node.data, maxAttempts: e.target.value ? parseInt(e.target.value) : undefined })}
              placeholder="1"
            />
          </div>
          <div>
            <label className="mb-1 block text-xs font-medium text-muted-foreground">Timeout</label>
            <input
              className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
              value={(node.data as any).timeout || ""}
              onChange={(e) => onChange(node.id, { ...node.data, timeout: e.target.value })}
              placeholder="10m"
            />
          </div>
        </div>
      </div>
    </div>
  );
}

function NodeFields({ node, onChange }: { node: FlowNode; onChange: (id: string, data: Record<string, unknown>) => void }) {
  const type = node.type as NodeType;
  const data = node.data;

  switch (type) {
    case "script":
      return <ScriptFields data={data as unknown as ScriptNodeData} onChange={(d) => onChange(node.id, { ...node.data, ...d })} />;
    case "agent":
      return <AgentFields data={data as unknown as AgentNodeData} onChange={(d) => onChange(node.id, { ...node.data, ...d })} />;
    case "http":
      return <HTTPFields data={data as unknown as HTTPNodeData} onChange={(d) => onChange(node.id, { ...node.data, ...d })} />;
    case "condition":
      return <ConditionFields data={data as unknown as ConditionNodeData} onChange={(d) => onChange(node.id, { ...node.data, ...d })} />;
    default:
      return null;
  }
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <label className="mb-1 block text-xs font-medium text-muted-foreground">{label}</label>
      {children}
    </div>
  );
}

const inputCls = "w-full rounded border border-border px-2 py-1 text-sm bg-background";
const monoCls = "w-full rounded border border-border px-2 py-1 font-mono text-xs bg-background";

function ScriptFields({ data, onChange }: { data: ScriptNodeData; onChange: (d: Partial<ScriptNodeData>) => void }) {
  return (
    <>
      <Field label="Language">
        <select
          className={inputCls}
          value={data.language || "python"}
          onChange={(e) => onChange({ language: e.target.value as "python" | "node" })}
        >
          <option value="python">Python</option>
          <option value="node">Node.js</option>
        </select>
      </Field>
      <Field label="Handler (function source)">
        <textarea
          className={`${monoCls} h-48 resize-y`}
          spellCheck={false}
          value={data.handler || ""}
          onChange={(e) => onChange({ handler: e.target.value })}
          placeholder={data.language === "node" ? "function handler(input) {\n  return { result: input.value };\n}" : "def handler(input):\n    return {'result': input['value']}"}
        />
      </Field>
    </>
  );
}

function AgentFields({ data, onChange }: { data: AgentNodeData; onChange: (d: Partial<AgentNodeData>) => void }) {
  return (
    <>
      <Field label="Agent Profile (named opencode agent)">
        <input
          className={inputCls}
          value={data.agent || ""}
          onChange={(e) => onChange({ agent: e.target.value })}
          placeholder="default (or named profile)"
        />
      </Field>
      <Field label="Prompt Template (text/template — {{.input.field}})">
        <textarea
          className={`${monoCls} h-32 resize-y`}
          spellCheck={false}
          value={data.prompt || ""}
          onChange={(e) => onChange({ prompt: e.target.value })}
          placeholder="Process {{.input.meetingId}}:&#10;{{.input.rawSummary}}"
        />
      </Field>
      <Field label="System Prompt Override (optional)">
        <textarea
          className={`${monoCls} h-16 resize-y`}
          spellCheck={false}
          value={data.system || ""}
          onChange={(e) => onChange({ system: e.target.value })}
        />
      </Field>
      <Field label="Session Lifecycle">
        <select
          className={inputCls}
          value={data.session || "ephemeral"}
          onChange={(e) => onChange({ session: e.target.value as "ephemeral" | "new" | "existing" })}
        >
          <option value="ephemeral">Ephemeral (create + destroy)</option>
          <option value="new">New (create + persist)</option>
          <option value="existing">Existing (reuse sessionId)</option>
        </select>
      </Field>
      {data.session === "existing" && (
        <Field label="Session ID">
          <input
            className={inputCls}
            value={data.sessionId || ""}
            onChange={(e) => onChange({ sessionId: e.target.value })}
            placeholder="opencode session ID"
          />
        </Field>
      )}
      <div className="flex items-center gap-2">
        <input
          type="checkbox"
          id="enforce-structured"
          checked={data.enforceStructuredOutput || false}
          onChange={(e) => onChange({ enforceStructuredOutput: e.target.checked })}
        />
        <label htmlFor="enforce-structured" className="text-xs text-muted-foreground">
          Enforce structured output (JSON Schema)
        </label>
      </div>
      {data.enforceStructuredOutput && (
        <Field label="Output Schema (JSON Schema)">
          <textarea
            className={`${monoCls} h-32 resize-y`}
            spellCheck={false}
            value={typeof data.outputSchema === "string" ? data.outputSchema : JSON.stringify(data.outputSchema || {}, null, 2)}
            onChange={(e) => {
              try {
                onChange({ outputSchema: JSON.parse(e.target.value) });
              } catch {
                onChange({ outputSchema: e.target.value as any });
              }
            }}
            placeholder='{"type":"object","properties":{"result":{"type":"string"}}}'
          />
        </Field>
      )}
    </>
  );
}

function HTTPFields({ data, onChange }: { data: HTTPNodeData; onChange: (d: Partial<HTTPNodeData>) => void }) {
  const [headerKey, setHeaderKey] = useState("");
  const [headerVal, setHeaderVal] = useState("");
  const headers = data.headers || {};

  return (
    <>
      <Field label="Method">
        <select
          className={inputCls}
          value={data.method || "GET"}
          onChange={(e) => onChange({ method: e.target.value })}
        >
          {["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => (
            <option key={m} value={m}>{m}</option>
          ))}
        </select>
      </Field>
      <Field label="URL (use {{secrets.NAME}} for credentials)">
        <input
          className={monoCls}
          value={data.url || ""}
          onChange={(e) => onChange({ url: e.target.value })}
          placeholder="https://api.example.com/widgets"
        />
      </Field>
      <Field label="Headers">
        <div className="space-y-1">
          {Object.entries(headers).map(([k, v]) => (
            <div key={k} className="flex items-center gap-1">
              <span className="truncate text-xs font-mono text-muted-foreground">{k}</span>
              <span className="truncate text-xs font-mono">: {v}</span>
              <button
                onClick={() => {
                  const next = { ...headers };
                  delete next[k];
                  onChange({ headers: next });
                }}
                className="ml-auto rounded p-0.5 text-destructive hover:bg-destructive/10"
              >
                <X className="h-3 w-3" />
              </button>
            </div>
          ))}
          <div className="flex gap-1">
            <input
              className={`${inputCls} flex-1`}
              placeholder="Header name"
              value={headerKey}
              onChange={(e) => setHeaderKey(e.target.value)}
            />
            <input
              className={`${inputCls} flex-1`}
              placeholder="Value or {{secrets.NAME}}"
              value={headerVal}
              onChange={(e) => setHeaderVal(e.target.value)}
            />
            <button
              onClick={() => {
                if (headerKey) {
                  onChange({ headers: { ...headers, [headerKey]: headerVal } });
                  setHeaderKey("");
                  setHeaderVal("");
                }
              }}
              className="rounded border border-border p-1 hover:bg-accent"
            >
              <Plus className="h-3 w-3" />
            </button>
          </div>
        </div>
      </Field>
      <Field label="Body (text/template — {{.input.field}})">
        <textarea
          className={`${monoCls} h-24 resize-y`}
          spellCheck={false}
          value={data.body || ""}
          onChange={(e) => onChange({ body: e.target.value })}
          placeholder="{{.input.payload}}"
        />
      </Field>
      <Field label="Timeout">
        <input
          className={inputCls}
          value={data.timeout || ""}
          onChange={(e) => onChange({ timeout: e.target.value })}
          placeholder="30s"
        />
      </Field>
    </>
  );
}

function ConditionFields({ data, onChange }: { data: ConditionNodeData; onChange: (d: Partial<ConditionNodeData>) => void }) {
  const conditions = data.conditions || [];

  return (
    <div className="space-y-2">
      <p className="text-xs text-muted-foreground">
        Expressions are evaluated against <code className="rounded bg-muted px-1">input</code> in order.
        First match wins. Implicit <code className="rounded bg-muted px-1">otherwise</code> branch is always available.
      </p>
      {conditions.map((c, i) => (
        <div key={i} className="flex gap-1">
          <input
            className="w-20 rounded border border-border px-1.5 py-1 text-xs bg-background"
            placeholder="branch-id"
            value={c.id}
            onChange={(e) => {
              const next = [...conditions];
              next[i] = { ...c, id: e.target.value };
              onChange({ conditions: next });
            }}
          />
          <input
            className="flex-1 rounded border border-border px-1.5 py-1 font-mono text-xs bg-background"
            placeholder="input.Field == 'value'"
            value={c.expression}
            onChange={(e) => {
              const next = [...conditions];
              next[i] = { ...c, expression: e.target.value };
              onChange({ conditions: next });
            }}
          />
          <button
            onClick={() => onChange({ conditions: conditions.filter((_, idx) => idx !== i) })}
            className="rounded p-1 text-destructive hover:bg-destructive/10"
          >
            <X className="h-3 w-3" />
          </button>
        </div>
      ))}
      <button
        onClick={() => onChange({ conditions: [...conditions, { id: `branch-${conditions.length + 1}`, expression: "" }] })}
        className="flex w-full items-center justify-center gap-1 rounded border border-dashed border-border py-1 text-xs text-muted-foreground hover:bg-accent"
      >
        <Plus className="h-3 w-3" /> Add branch
      </button>
    </div>
  );
}
