import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { triggerApi, workflowApi, type Trigger, type Workflow } from "../api/workflows";
import { workspacesApi } from "../api/workspaces";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import { cn } from "../lib/utils";
import {
  Plus, Clock, Link as LinkIcon, Copy, Eye, EyeOff, AlertTriangle,
  Shield, Activity, ArrowLeft,
} from "lucide-react";
import {
  type FriendlyCron, type CronFrequency, friendlyToCron, cronToFriendly,
  describeCron, COMMON_TIMEZONES,
} from "../components/workflows/cronUtils";

export function TriggersPage() {
  const { triggerId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);

  const { data: triggers, isLoading } = useQuery({
    queryKey: ["triggers"],
    queryFn: () => triggerApi.list(),
  }) as { data: Trigger[] | undefined; isLoading: boolean };

  const { data: workflows } = useQuery({
    queryKey: ["workflows"],
    queryFn: () => workflowApi.list(),
  }) as { data: Workflow[] | undefined };

  const selected = triggers?.find((t) => t.id === triggerId) ?? null;

  const deleteMutation = useMutation({
    mutationFn: (id: string) => triggerApi.delete(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["triggers"] });
      navigate("/triggers");
    },
  });

  if (isLoading) {
    return (
      <div className="flex h-full items-center justify-center"><Spinner /></div>
    );
  }

  return (
    <div className="flex h-full overflow-hidden">
      {/* List pane — full width on mobile when no selection, hidden when detail open on mobile */}
      <div className={cn(
        "w-full md:w-72 shrink-0 border-r border-border overflow-y-auto scrollbar-thin",
        selected && "hidden md:block",
      )}>
        <div className="flex items-center justify-between p-3 border-b border-border">
          <h2 className="text-sm font-semibold">Triggers</h2>
          <button
            onClick={() => setShowCreate(true)}
            className="rounded-md p-1 hover:bg-accent"
            aria-label="New trigger"
          >
            <Plus className="h-4 w-4" />
          </button>
        </div>

        {showCreate && (
          <TriggerCreateForm
            workflows={workflows || []}
            onSave={async (data) => {
              await triggerApi.create(data);
              setShowCreate(false);
              queryClient.invalidateQueries({ queryKey: ["triggers"] });
            }}
            onCancel={() => setShowCreate(false)}
          />
        )}

        <div className="flex flex-col gap-0.5 p-1">
          {(triggers ?? []).map((trig) => (
            <button
              key={trig.id}
              onClick={() => navigate(`/triggers/${trig.id}`)}
              className={cn(
                "flex flex-col gap-1 rounded-md p-2 text-left transition-colors hover:bg-accent",
                selected?.id === trig.id && "bg-accent",
              )}
            >
              <div className="flex items-center justify-between gap-2">
                <div className="flex items-center gap-2">
                  {trig.sourceType === "cron" ? (
                    <Clock className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  ) : (
                    <LinkIcon className="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
                  )}
                  <span className="truncate text-sm font-medium">{trig.name}</span>
                </div>
                {!trig.enabled && (
                  <Badge variant="destructive" className="text-[10px]">disabled</Badge>
                )}
              </div>
              {trig.sourceType === "cron" && trig.enabled && trig.nextFireAt && (
                <span className="text-xs text-muted-foreground">
                  Next: {describeCron(cronToFriendly(trig.sourceConfig))}
                </span>
              )}
              {trig.consecutiveFailures > 0 && (
                <span className="text-xs text-destructive flex items-center gap-1">
                  <AlertTriangle className="h-3 w-3" />
                  {trig.consecutiveFailures} failures
                  {trig.consecutiveFailures >= trig.autoDisableAfter && " (auto-disabled)"}
                </span>
              )}
            </button>
          ))}
          {(!triggers || triggers.length === 0) && !showCreate && (
            <div className="px-3 py-8 text-center text-sm text-muted-foreground">
              No triggers yet. Click + to create one.
            </div>
          )}
        </div>
      </div>

      {/* Detail pane — full width on mobile, hidden when no selection on desktop empty state */}
      <div className={cn(
        "flex-1 overflow-auto",
        !selected && !showCreate && "hidden md:block",
      )}>
        {selected ? (
          <>
            <button
              onClick={() => navigate("/triggers", { replace: true })}
              className="flex items-center gap-1 border-b border-border px-3 py-2 text-sm text-muted-foreground hover:bg-accent md:hidden"
            >
              <ArrowLeft className="h-4 w-4" /> Triggers
            </button>
            <TriggerEditor
              trigger={selected}
            workflows={workflows || []}
            onUpdate={async (updates) => {
              await triggerApi.update(selected.id, updates);
              queryClient.invalidateQueries({ queryKey: ["triggers"] });
            }}
            onDelete={() => {
              if (confirm(`Delete trigger "${selected.name}"?`)) {
                deleteMutation.mutate(selected.id);
              }
            }}
            onRunWorkflow={async (wfId) => {
              const run = await workflowApi.run(wfId);
              navigate(`/workflows/${wfId}/runs/${run.id}`);
            }}
          />
          </>
        ) : (
          <div className="flex h-full items-center justify-center text-muted-foreground">
            <p className="text-sm">Select a trigger from the list, or create a new one.</p>
          </div>
        )}
      </div>
    </div>
  );
}

function TriggerCreateForm({ workflows, onSave, onCancel }: {
  workflows: Workflow[];
  onSave: (data: any) => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState("");
  const [sourceType, setSourceType] = useState<"cron" | "webhook">("cron");
  const [mode, setMode] = useState<"workflow" | "routine">("routine");

  const [workflowId, setWorkflowId] = useState("");
  const [workspaceId, setWorkspaceId] = useState("");
  const [prompt, setPrompt] = useState("");
  const [agentProfile, setAgentProfile] = useState("");
  const [scriptPath, setScriptPath] = useState("");
  const [scriptArgs, setScriptArgs] = useState("");
  const [scriptEnv, setScriptEnv] = useState("");
  const [memoryMode, setMemoryMode] = useState("none");
  const [captureMode, setCaptureMode] = useState("errors_only");
  const [preserveSession, setPreserveSession] = useState("never");

  const { data: workspaces } = useQuery({
    queryKey: ["workspaces"],
    queryFn: () => workspacesApi.list(),
  }) as { data: { items?: { id: string; name: string; phase: string }[] } | undefined };

  const [friendly, setFriendly] = useState<FriendlyCron>({
    frequency: "daily", hour: 9, minute: 0, tz: "UTC",
  });
  const [rawMode, setRawMode] = useState(false);
  const [rawExpr, setRawExpr] = useState("0 9 * * *");

  const [webhookAllowedIps, setWebhookAllowedIps] = useState("");
  const [webhookIdempotencyMode, setWebhookIdempotencyMode] = useState("header");
  const [createdSecret, setCreatedSecret] = useState<string | null>(null);
  const [createdUrl, setCreatedUrl] = useState<string | null>(null);
  const [showSecret, setShowSecret] = useState(false);
  const [copied, setCopied] = useState(false);

  const cronConfig = sourceType === "cron"
    ? (rawMode ? { expr: rawExpr, tz: friendly.tz } : friendlyToCron(friendly))
    : { expr: "", tz: "UTC" };

  const handleCreate = async () => {
    const data: any = {
      name,
      sourceType,
      sourceConfig: sourceType === "cron" ? cronConfig : {},
      memoryMode,
      captureMode,
      preserveSession,
    };

    if (mode === "workflow") {
      data.workflowId = workflowId;
    } else {
      data.workspaceId = workspaceId;
      data.prompt = prompt;
      if (agentProfile) data.agent = agentProfile;
      if (scriptPath) {
        data.scriptPath = scriptPath;
        data.scriptArgs = scriptArgs.trim() ? scriptArgs.split(/\s+/) : [];
        if (scriptEnv.trim()) {
          const env: Record<string, string> = {};
          scriptEnv.split("\n").forEach((line) => {
            const idx = line.indexOf("=");
            if (idx > 0) env[line.slice(0, idx).trim()] = line.slice(idx + 1).trim();
          });
          data.scriptEnv = env;
        }
      }
    }

    if (sourceType === "webhook" && webhookAllowedIps.trim()) {
      data.webhookAllowedIps = webhookAllowedIps.split(",").map((s) => s.trim()).filter(Boolean);
    }
    if (sourceType === "webhook") {
      data.webhookIdempotencyMode = webhookIdempotencyMode;
    }
    try {
      const result = await triggerApi.create(data);
      if (result.webhookSecret) {
        setCreatedSecret(result.webhookSecret);
        setCreatedUrl(result.webhookUrl || `/api/v1/hooks/${result.trigger.id}`);
      } else {
        onSave(data);
      }
    } catch (e: any) {
      alert(e?.message || "Failed to create trigger");
    }
  };

  if (createdSecret) {
    return (
      <div className="m-2 rounded-md border border-border p-4 space-y-3">
        <div className="flex items-center gap-2 text-sm font-semibold text-yellow-500">
          <AlertTriangle className="h-4 w-4" /> Save Your Webhook Secret
        </div>
        <p className="text-xs text-muted-foreground">This secret is shown only once. Store it securely.</p>
        <div className="space-y-2">
          <div>
            <label className="text-xs text-muted-foreground">Webhook URL</label>
            <div className="flex gap-1">
              <code className="flex-1 rounded border border-border p-2 text-xs font-mono break-all">
                {window.location.origin}{createdUrl}
              </code>
              <button
                onClick={() => {
                  navigator.clipboard.writeText(`${window.location.origin}${createdUrl}`);
                  setCopied(true);
                  setTimeout(() => setCopied(false), 2000);
                }}
                className="rounded border border-border p-2 hover:bg-accent"
              >
                <Copy className="h-3.5 w-3.5" />
              </button>
            </div>
            {copied && <span className="text-xs text-green-500">Copied!</span>}
          </div>
          <div>
            <label className="text-xs text-muted-foreground">HMAC Secret</label>
            <div className="flex gap-1">
              <code className="flex-1 rounded border border-border p-2 text-xs font-mono break-all">
                {showSecret ? createdSecret : "••••••••••••••••••••••••"}
              </code>
              <button onClick={() => setShowSecret(!showSecret)} className="rounded border border-border p-2 hover:bg-accent">
                {showSecret ? <EyeOff className="h-3.5 w-3.5" /> : <Eye className="h-3.5 w-3.5" />}
              </button>
              <button onClick={() => navigator.clipboard.writeText(createdSecret)} className="rounded border border-border p-2 hover:bg-accent">
                <Copy className="h-3.5 w-3.5" />
              </button>
            </div>
          </div>
        </div>
        <button onClick={onCancel} className="w-full rounded bg-primary py-2 text-sm text-primary-foreground">Done</button>
      </div>
    );
  }

  const canCreate = name && (
    mode === "workflow" ? workflowId :
    workspaceId && (prompt || scriptPath)
  );

  return (
    <div className="m-2 rounded-md border border-border p-3 space-y-3">
      <input className="w-full rounded border border-border px-2 py-1 text-sm bg-background" placeholder="Trigger name" value={name} onChange={(e) => setName(e.target.value)} />

      <div>
        <label className="mb-1 block text-xs text-muted-foreground">Trigger Type</label>
        <div className="flex gap-2">
          <button onClick={() => setSourceType("cron")} className={cn("flex-1 rounded border px-2 py-1.5 text-sm flex items-center justify-center gap-1", sourceType === "cron" ? "border-primary bg-primary/10" : "border-border")}>
            <Clock className="h-3.5 w-3.5" /> Schedule
          </button>
          <button onClick={() => setSourceType("webhook")} className={cn("flex-1 rounded border px-2 py-1.5 text-sm flex items-center justify-center gap-1", sourceType === "webhook" ? "border-primary bg-primary/10" : "border-border")}>
            <LinkIcon className="h-3.5 w-3.5" /> Webhook
          </button>
        </div>
      </div>

      {sourceType === "cron" && (
        <div className="space-y-2">
          {rawMode ? (
            <input className="w-full rounded border border-border px-2 py-1 text-sm font-mono bg-background" placeholder="Cron expression" value={rawExpr} onChange={(e) => setRawExpr(e.target.value)} />
          ) : (
            <CronBuilder friendly={friendly} onChange={setFriendly} />
          )}
          <button onClick={() => setRawMode(!rawMode)} className="text-xs text-blue-500 hover:underline">
            {rawMode ? "← Friendly mode" : "Raw cron →"}
          </button>
          <div className="rounded bg-muted p-2 text-xs text-muted-foreground">Schedule: {describeCron(cronToFriendly(cronConfig))}</div>
        </div>
      )}

      {sourceType === "webhook" && (
        <div className="space-y-2">
          <div>
            <label className="mb-1 block text-xs text-muted-foreground">IP Allowlist (optional)</label>
            <input className="w-full rounded border border-border px-2 py-1 text-sm font-mono bg-background" placeholder="10.0.0.0/8" value={webhookAllowedIps} onChange={(e) => setWebhookAllowedIps(e.target.value)} />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted-foreground">Idempotency</label>
            <select className="w-full rounded border border-border px-2 py-1 text-sm bg-background" value={webhookIdempotencyMode} onChange={(e) => setWebhookIdempotencyMode(e.target.value)}>
              <option value="header">Header (X-Request-ID)</option>
              <option value="hash">Hash (body + timestamp)</option>
              <option value="disabled">Disabled</option>
            </select>
          </div>
        </div>
      )}

      <div>
        <label className="mb-1 block text-xs text-muted-foreground">Target</label>
        <div className="flex gap-2">
          <button onClick={() => setMode("routine")} className={cn("flex-1 rounded border px-2 py-1.5 text-sm", mode === "routine" ? "border-primary bg-primary/10" : "border-border")}>
            Agent Routine
          </button>
          <button onClick={() => setMode("workflow")} className={cn("flex-1 rounded border px-2 py-1.5 text-sm", mode === "workflow" ? "border-primary bg-primary/10" : "border-border")}>
            DAG Workflow
          </button>
        </div>
      </div>

      {mode === "workflow" ? (
        <div>
          <label className="mb-1 block text-xs text-muted-foreground">Workflow</label>
          <select className="w-full rounded border border-border px-2 py-1 text-sm bg-background" value={workflowId} onChange={(e) => setWorkflowId(e.target.value)}>
            <option value="">Select a workflow...</option>
            {workflows.map((wf) => (<option key={wf.id} value={wf.id}>{wf.name}</option>))}
          </select>
        </div>
      ) : (
        <div className="space-y-2">
          <div>
            <label className="mb-1 block text-xs text-muted-foreground">Workspace</label>
            <select className="w-full rounded border border-border px-2 py-1 text-sm bg-background" value={workspaceId} onChange={(e) => setWorkspaceId(e.target.value)}>
              <option value="">Select workspace...</option>
              {(workspaces?.items || []).map((ws) => (<option key={ws.id} value={ws.id}>{ws.name} ({ws.phase})</option>))}
            </select>
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted-foreground">Prompt (text/template)</label>
            <textarea className="w-full rounded border border-border px-2 py-1 text-xs bg-background h-20 resize-y" spellCheck={false} value={prompt} onChange={(e) => setPrompt(e.target.value)} placeholder={"Check for new action items.\n\nPrevious result:\n{{.prevResult}}"} />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted-foreground">Agent Profile (optional)</label>
            <input className="w-full rounded border border-border px-2 py-1 text-sm bg-background" value={agentProfile} onChange={(e) => setAgentProfile(e.target.value)} placeholder="default" />
          </div>
          <div>
            <label className="mb-1 block text-xs text-muted-foreground">Script Path (optional — runs before agent)</label>
            <input className="w-full rounded border border-border px-2 py-1 font-mono text-sm bg-background" placeholder="/workspace/scripts/fetch.sh" value={scriptPath} onChange={(e) => setScriptPath(e.target.value)} />
          </div>
          {scriptPath && (
            <>
              <div>
                <label className="mb-1 block text-xs text-muted-foreground">Script Args</label>
                <input className="w-full rounded border border-border px-2 py-1 font-mono text-sm bg-background" value={scriptArgs} onChange={(e) => setScriptArgs(e.target.value)} />
              </div>
              <div>
                <label className="mb-1 block text-xs text-muted-foreground">Script Env (KEY=value per line)</label>
                <textarea className="w-full rounded border border-border px-2 py-1 font-mono text-xs bg-background h-12 resize-y" value={scriptEnv} onChange={(e) => setScriptEnv(e.target.value)} />
              </div>
            </>
          )}
          <div className="grid grid-cols-3 gap-2">
            <div>
              <label className="mb-1 block text-xs text-muted-foreground">Memory</label>
              <select className="w-full rounded border border-border px-2 py-1 text-xs bg-background" value={memoryMode} onChange={(e) => setMemoryMode(e.target.value)}>
                <option value="none">None</option>
                <option value="last_result">Last result</option>
              </select>
            </div>
            <div>
              <label className="mb-1 block text-xs text-muted-foreground">Capture</label>
              <select className="w-full rounded border border-border px-2 py-1 text-xs bg-background" value={captureMode} onChange={(e) => setCaptureMode(e.target.value)}>
                <option value="errors_only">Errors only</option>
                <option value="full">Full result</option>
              </select>
            </div>
            <div>
              <label className="mb-1 block text-xs text-muted-foreground">Session</label>
              <select className="w-full rounded border border-border px-2 py-1 text-xs bg-background" value={preserveSession} onChange={(e) => setPreserveSession(e.target.value)}>
                <option value="never">Delete after</option>
                <option value="on_failure">Keep on failure</option>
                <option value="always">Always keep</option>
              </select>
            </div>
          </div>
        </div>
      )}

      <div className="flex gap-2">
        <button className="flex-1 rounded bg-primary px-2 py-1 text-sm text-primary-foreground disabled:opacity-50" disabled={!canCreate} onClick={handleCreate}>Create</button>
        <button className="rounded border border-border px-2 py-1 text-sm" onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}

function CronBuilder({ friendly, onChange }: { friendly: FriendlyCron; onChange: (f: FriendlyCron) => void }) {
  const frequencies: { value: CronFrequency; label: string }[] = [
    { value: "every-n-minutes", label: "Every N minutes" },
    { value: "every-n-hours", label: "Every N hours" },
    { value: "daily", label: "Daily" },
    { value: "weekdays", label: "Weekdays" },
  ];

  return (
    <div className="space-y-2">
      <select
        className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
        value={friendly.frequency === "custom" ? "daily" : friendly.frequency}
        onChange={(e) => onChange({ ...friendly, frequency: e.target.value as CronFrequency })}
      >
        {frequencies.map((f) => (
          <option key={f.value} value={f.value}>{f.label}</option>
        ))}
      </select>

      {(friendly.frequency === "every-n-minutes" || friendly.frequency === "every-n-hours") && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">Every</span>
          <input
            type="number"
            min={1}
            max={59}
            className="w-16 rounded border border-border px-2 py-1 text-sm bg-background"
            value={friendly.interval || 1}
            onChange={(e) => onChange({ ...friendly, interval: parseInt(e.target.value) || 1 })}
          />
          <span className="text-xs text-muted-foreground">
            {friendly.frequency === "every-n-minutes" ? "minute(s)" : "hour(s)"}
          </span>
        </div>
      )}

      {(friendly.frequency === "daily" || friendly.frequency === "weekdays") && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">At</span>
          <input
            type="number"
            min={0}
            max={23}
            className="w-16 rounded border border-border px-2 py-1 text-sm bg-background"
            value={friendly.hour ?? 9}
            onChange={(e) => onChange({ ...friendly, hour: parseInt(e.target.value) || 0 })}
          />
          <span className="text-xs text-muted-foreground">:</span>
          <input
            type="number"
            min={0}
            max={59}
            className="w-16 rounded border border-border px-2 py-1 text-sm bg-background"
            value={friendly.minute ?? 0}
            onChange={(e) => onChange({ ...friendly, minute: parseInt(e.target.value) || 0 })}
          />
        </div>
      )}

      <div>
        <label className="mb-1 block text-xs text-muted-foreground">Timezone</label>
        <select
          className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
          value={friendly.tz}
          onChange={(e) => onChange({ ...friendly, tz: e.target.value })}
        >
          {COMMON_TIMEZONES.map((tz) => (
            <option key={tz} value={tz}>{tz}</option>
          ))}
        </select>
      </div>
    </div>
  );
}

function TriggerEditor({ trigger, workflows, onUpdate, onDelete, onRunWorkflow }: {
  trigger: Trigger;
  workflows: Workflow[];
  onUpdate: (updates: any) => void;
  onDelete: () => void;
  onRunWorkflow: (workflowId: string) => void;
}) {
  const [showDeliveryLog, setShowDeliveryLog] = useState(false);
  const [copied, setCopied] = useState(false);
  const [editingSchedule, setEditingSchedule] = useState(false);
  const [editingTarget, setEditingTarget] = useState(false);
  const [editingTemplate, setEditingTemplate] = useState(false);

  const [friendly, setFriendly] = useState<FriendlyCron>(() => cronToFriendly(trigger.sourceConfig || {}));
  const [rawMode, setRawMode] = useState(false);
  const [rawExpr, setRawExpr] = useState(trigger.sourceConfig?.expr || "0 * * * *");
  const [selectedWorkflowId, setSelectedWorkflowId] = useState(trigger.workflowId || "");
  const [templateStr, setTemplateStr] = useState(() => {
    return "";
  });

  const targetWorkflowId = trigger.workflowId || "";
  const targetWorkflow = workflows.find((w) => w.id === targetWorkflowId);
  const isAutoDisabled = trigger.consecutiveFailures >= trigger.autoDisableAfter && !trigger.enabled;
  const isCron = trigger.sourceType === "cron";

  const cronConfig = rawMode
    ? { expr: rawExpr, tz: friendly.tz }
    : friendlyToCron(friendly);

  const handleSaveSchedule = () => {
    onUpdate({ sourceConfig: cronConfig });
    setEditingSchedule(false);
  };

  const handleSaveTarget = () => {
    onUpdate({ workflowId: selectedWorkflowId });
    setEditingTarget(false);
  };

  const handleSavePrompt = () => {
    onUpdate({ prompt });
    setEditingTemplate(false);
  };

  return (
    <div className="mx-auto max-w-2xl space-y-4 overflow-y-auto p-6">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">{trigger.name}</h2>
        <div className="flex gap-2">
          <button
            className={cn(
              "rounded-md border px-3 py-1 text-sm",
              trigger.enabled ? "bg-primary text-primary-foreground" : "bg-background",
            )}
            onClick={() => onUpdate({ enabled: !trigger.enabled })}
          >
            {trigger.enabled ? "Enabled" : "Disabled"}
          </button>
          <button
            className="rounded-md border border-destructive px-3 py-1 text-sm text-destructive hover:bg-destructive/10"
            onClick={onDelete}
          >
            Delete
          </button>
        </div>
      </div>

      {isAutoDisabled && (
        <div className="flex items-center gap-2 rounded-lg border border-yellow-500/30 bg-yellow-500/10 p-3">
          <AlertTriangle className="h-4 w-4 shrink-0 text-yellow-500" />
          <div className="text-sm">
            <span className="font-medium text-yellow-500">Auto-disabled</span> after {trigger.consecutiveFailures} consecutive failures.
            Fix the workflow and re-enable to resume.
          </div>
        </div>
      )}

      {/* Schedule section (cron only) */}
      {isCron && (
        <div className="rounded-lg border border-border p-4 space-y-2">
          <div className="flex items-center justify-between">
            <h3 className="flex items-center gap-1 text-sm font-semibold">
              <Clock className="h-3.5 w-3.5" /> Schedule
            </h3>
            {!editingSchedule ? (
              <button onClick={() => setEditingSchedule(true)} className="text-xs text-blue-500 hover:underline">Edit</button>
            ) : (
              <div className="flex gap-2">
                <button onClick={() => setEditingSchedule(false)} className="text-xs text-muted-foreground hover:underline">Cancel</button>
                <button onClick={handleSaveSchedule} className="text-xs text-blue-500 hover:underline">Save</button>
              </div>
            )}
          </div>
          {!editingSchedule ? (
            <>
              <code className="rounded bg-muted px-2 py-1 text-xs font-mono">
                {describeCron(cronToFriendly(trigger.sourceConfig || {}))}
              </code>
              {trigger.nextFireAt && trigger.enabled && (
                <div className="text-xs text-muted-foreground">
                  Next fire: {new Date(trigger.nextFireAt).toLocaleString()}
                </div>
              )}
            </>
          ) : (
            <div className="space-y-2">
              {rawMode ? (
                <input
                  className="w-full rounded border border-border px-2 py-1 font-mono text-xs bg-background"
                  value={rawExpr}
                  onChange={(e) => setRawExpr(e.target.value)}
                />
              ) : (
                <CronBuilder friendly={friendly} onChange={setFriendly} />
              )}
              <button onClick={() => setRawMode(!rawMode)} className="text-xs text-blue-500 hover:underline">
                {rawMode ? "← Friendly mode" : "Raw cron →"}
              </button>
              <div className="rounded bg-muted p-2 text-xs text-muted-foreground">
                Preview: {describeCron(cronToFriendly(cronConfig))}
              </div>
            </div>
          )}
        </div>
      )}

      {/* Webhook URL section */}
      {!isCron && (
        <div className="rounded-lg border border-border p-4 space-y-2">
          <div className="flex items-center justify-between">
            <h3 className="flex items-center gap-1 text-sm font-semibold">
              <LinkIcon className="h-3.5 w-3.5" /> Webhook
            </h3>
            <RotateSecretButton triggerId={trigger.id} />
          </div>
          <div className="flex gap-1">
            <code className="flex-1 rounded bg-muted px-2 py-1 text-xs font-mono break-all">
              {window.location.origin}/api/v1/hooks/{trigger.id}
            </code>
            <button
              onClick={() => {
                navigator.clipboard.writeText(`${window.location.origin}/api/v1/hooks/${trigger.id}`);
                setCopied(true);
                setTimeout(() => setCopied(false), 2000);
              }}
              className="rounded border border-border p-1 hover:bg-accent"
            >
              <Copy className="h-3.5 w-3.5" />
            </button>
          </div>
          {copied && <span className="text-xs text-green-500">Copied!</span>}
          <div className="flex items-center gap-1 text-xs text-muted-foreground">
            <Shield className="h-3 w-3" /> HMAC-SHA256 verified
          </div>
        </div>
      )}

      {/* Target workflow section */}
      <div className="rounded-lg border border-border p-4 space-y-2">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold">Target Workflow</h3>
          {!editingTarget ? (
            <button onClick={() => { setSelectedWorkflowId(targetWorkflowId); setEditingTarget(true); }} className="text-xs text-blue-500 hover:underline">Edit</button>
          ) : (
            <div className="flex gap-2">
              <button onClick={() => setEditingTarget(false)} className="text-xs text-muted-foreground hover:underline">Cancel</button>
              <button onClick={handleSaveTarget} className="text-xs text-blue-500 hover:underline">Save</button>
            </div>
          )}
        </div>
        {!editingTarget ? (
          <div className="flex items-center gap-2 text-sm">
            {targetWorkflow ? (
              <>
                <span className="font-medium">{targetWorkflow.name}</span>
                <button
                  onClick={() => onRunWorkflow(targetWorkflowId)}
                  className="rounded border border-border px-2 py-0.5 text-xs hover:bg-accent"
                >
                  Run now →
                </button>
              </>
            ) : (
              <span className="text-muted-foreground">No workflow set</span>
            )}
          </div>
        ) : (
          <select
            className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
            value={selectedWorkflowId}
            onChange={(e) => setSelectedWorkflowId(e.target.value)}
          >
            <option value="">Select workflow...</option>
            {workflows.map((wf) => (
              <option key={wf.id} value={wf.id}>{wf.name}</option>
            ))}
          </select>
        )}
      </div>

      {/* Input template section */}
      <div className="rounded-lg border border-border p-4 space-y-2">
        <div className="flex items-center justify-between">
          <h3 className="text-sm font-semibold">Input Template</h3>
          {!editingTemplate ? (
            <button onClick={() => setEditingTemplate(true)} className="text-xs text-blue-500 hover:underline">Edit</button>
          ) : (
            <div className="flex gap-2">
              <button onClick={() => setEditingTemplate(false)} className="text-xs text-muted-foreground hover:underline">Cancel</button>
              <button onClick={handleSavePrompt} className="text-xs text-blue-500 hover:underline">Save</button>
            </div>
          )}
        </div>
        {!editingTemplate ? (
          trigger.prompt ? (
            <pre className="overflow-x-auto rounded bg-muted p-2 text-xs font-mono">
              {trigger.prompt}
            </pre>
          ) : (
            <p className="text-xs text-muted-foreground">No prompt set.</p>
          )
        ) : (
          <>
            <textarea
              className="h-32 w-full rounded border border-border p-2 font-mono text-xs bg-background"
              spellCheck={false}
              value={templateStr}
              onChange={(e) => setTemplateStr(e.target.value)}
              placeholder={'{"repo": "{{.body.repository.full_name}}", "issue": "{{.body.issue.number}}"}'}
            />
            <p className="text-xs text-muted-foreground">
              Use <code>{"{{.body.field}}"}</code> to extract from webhook payload. Leave empty to pass full envelope.
            </p>
          </>
        )}
      </div>

      {/* Circuit breaker section */}
      <div className="space-y-2 rounded-lg border border-border p-4">
        <div className="flex items-center justify-between">
          <h3 className="flex items-center gap-1 text-sm font-semibold">
            <Activity className="h-3.5 w-3.5" /> Circuit Breaker
          </h3>
          <label className="flex items-center gap-1 text-xs text-muted-foreground">
            Disable after:
            <input
              type="number"
              min={1}
              max={100}
              className="w-14 rounded border border-border px-1 py-0.5 text-xs bg-background"
              value={trigger.autoDisableAfter}
              onChange={(e) => onUpdate({ autoDisableAfter: parseInt(e.target.value) || 10 })}
            />
            failures
          </label>
        </div>
        <div className="flex items-center gap-2 text-sm">
          <div className="h-2 flex-1 rounded-full bg-muted">
            <div
              className={cn(
                "h-2 rounded-full transition-all",
                trigger.consecutiveFailures >= trigger.autoDisableAfter ? "bg-destructive" :
                trigger.consecutiveFailures > 0 ? "bg-yellow-500" : "bg-green-500",
              )}
              style={{ width: `${Math.min(100, (trigger.consecutiveFailures / trigger.autoDisableAfter) * 100)}%` }}
            />
          </div>
          <span className="whitespace-nowrap text-xs text-muted-foreground">
            {trigger.consecutiveFailures}/{trigger.autoDisableAfter}
          </span>
        </div>
        {trigger.lastFiredAt && (
          <div className="text-xs text-muted-foreground">
            Last fired: {new Date(trigger.lastFiredAt).toLocaleString()}
          </div>
        )}
      </div>

      {!isCron && (
        <div>
          <button
            onClick={() => setShowDeliveryLog(!showDeliveryLog)}
            className="flex items-center gap-1 text-sm text-blue-500 hover:underline"
          >
            <Activity className="h-3.5 w-3.5" />
            {showDeliveryLog ? "Hide" : "Show"} delivery log
          </button>
          {showDeliveryLog && <DeliveryLog triggerId={trigger.id} />}
        </div>
      )}
    </div>
  );
}

function RotateSecretButton({ triggerId }: { triggerId: string }) {
  const [rotating, setRotating] = useState(false);
  const [newSecret, setNewSecret] = useState<string | null>(null);
  const [showSecret, setShowSecret] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleRotate = async () => {
    if (!confirm("Rotate webhook secret? The old secret will stop working immediately.")) return;
    setRotating(true);
    setError(null);
    try {
      const result = await triggerApi.rotateSecret(triggerId);
      setNewSecret(result.webhookSecret);
    } catch (e: any) {
      setError(e?.message || "Failed to rotate secret");
    } finally {
      setRotating(false);
    }
  };

  if (newSecret) {
    return (
      <div className="space-y-1">
        <div className="flex items-center gap-1">
          <code className="rounded bg-yellow-500/10 border border-yellow-500/30 px-2 py-1 text-xs font-mono break-all">
            {showSecret ? newSecret : "••••••••••••••••••••••••"}
          </code>
          <button onClick={() => setShowSecret(!showSecret)} className="rounded border border-border p-1 hover:bg-accent">
            {showSecret ? <EyeOff className="h-3 w-3" /> : <Eye className="h-3 w-3" />}
          </button>
          <button
            onClick={() => navigator.clipboard.writeText(newSecret)}
            className="rounded border border-border p-1 hover:bg-accent"
          >
            <Copy className="h-3 w-3" />
          </button>
          <button onClick={() => setNewSecret(null)} className="text-xs text-muted-foreground hover:underline">Done</button>
        </div>
        <p className="text-[10px] text-yellow-500">Save this secret — it won't be shown again.</p>
      </div>
    );
  }

  return (
    <>
      <button
        onClick={handleRotate}
        disabled={rotating}
        className="text-xs text-yellow-500 hover:underline disabled:opacity-50"
      >
        {rotating ? "Rotating..." : "Rotate secret"}
      </button>
      {error && <p className="text-xs text-destructive">{error}</p>}
    </>
  );
}

function DeliveryLog({ triggerId }: { triggerId: string }) {
  const { data: fires, isLoading } = useQuery({
    queryKey: ["trigger-fires", triggerId],
    queryFn: () => triggerApi.fires(triggerId),
    refetchInterval: 10000,
  });
  const [expanded, setExpanded] = useState<Set<string>>(new Set());

  if (isLoading) return <div className="text-xs text-muted-foreground">Loading...</div>;
  if (!fires || fires.length === 0) {
    return <div className="rounded-lg border border-border p-3 text-xs text-muted-foreground">No deliveries recorded yet.</div>;
  }

  const statusColors: Record<string, string> = {
    fired: "text-blue-500",
    delivered: "text-green-500",
    failed: "text-red-500",
    skipped: "text-muted-foreground",
    rate_limited: "text-orange-500",
    validation_error: "text-yellow-500",
    auto_disabled: "text-red-500",
  };

  const toggle = (id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <div className="max-h-64 space-y-1 overflow-y-auto rounded-lg border border-border p-2">
      {fires.map((f) => (
        <div key={f.id}>
          <button
            onClick={() => toggle(f.id)}
            className="flex w-full items-center justify-between rounded p-2 text-xs hover:bg-accent"
          >
            <div className="flex items-center gap-2">
              <span className={`font-medium ${statusColors[f.status] || "text-muted-foreground"}`}>{f.status}</span>
              <span className="font-mono text-muted-foreground">{f.actionType}</span>
              {(f.inputEnvelope || f.actionResult) && (
                <span className="text-muted-foreground">{expanded.has(f.id) ? "▼" : "▶"}</span>
              )}
            </div>
            <span className="text-muted-foreground">
              {new Date(f.firedAt).toLocaleString()}
            </span>
          </button>
          {expanded.has(f.id) && (f.inputEnvelope || f.actionResult) && (
            <div className="space-y-1 px-4 pb-2">
              {f.inputEnvelope && (
                <div>
                  <div className="text-[10px] text-muted-foreground">Envelope:</div>
                  <pre className="overflow-x-auto rounded bg-muted p-1.5 text-[10px] font-mono">
                    {typeof f.inputEnvelope === "string" ? f.inputEnvelope : JSON.stringify(f.inputEnvelope, null, 2)}
                  </pre>
                </div>
              )}
              {f.actionResult && (
                <div>
                  <div className="text-[10px] text-muted-foreground">Result:</div>
                  <pre className="overflow-x-auto rounded bg-muted p-1.5 text-[10px] font-mono">
                    {typeof f.actionResult === "string" ? f.actionResult : JSON.stringify(f.actionResult, null, 2)}
                  </pre>
                </div>
              )}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}
