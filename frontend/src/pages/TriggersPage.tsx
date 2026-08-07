import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { triggerApi, type Trigger } from "../api/workflows";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import { cn } from "../lib/utils";
import { Plus, Clock, Link as LinkIcon } from "lucide-react";

export function TriggersPage() {
  const { triggerId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);

  const { data: triggers, isLoading } = useQuery({
    queryKey: ["triggers"],
    queryFn: () => triggerApi.list(),
  }) as { data: Trigger[] | undefined; isLoading: boolean };

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
      <div className="flex h-full items-center justify-center">
        <Spinner />
      </div>
    );
  }

  return (
    <div className="flex h-full overflow-hidden">
      {/* Left panel — trigger list */}
      <div className="w-72 shrink-0 border-r border-border overflow-y-auto scrollbar-thin">
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
                "flex flex-col gap-1 rounded-md p-2 text-left transition-colors",
                "hover:bg-accent",
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
              {trig.nextFireAt && trig.enabled && (
                <span className="text-xs text-muted-foreground">
                  Next: {new Date(trig.nextFireAt).toLocaleString()}
                </span>
              )}
              {trig.consecutiveFailures > 0 && (
                <span className="text-xs text-destructive">
                  {trig.consecutiveFailures} consecutive failures
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

      {/* Right panel — editor / empty state */}
      <div className="flex-1 overflow-auto">
        {selected ? (
          <TriggerEditor
            trigger={selected}
            onUpdate={async (updates) => {
              await triggerApi.update(selected.id, updates);
              queryClient.invalidateQueries({ queryKey: ["triggers"] });
            }}
            onDelete={() => {
              if (confirm(`Delete trigger "${selected.name}"?`)) {
                deleteMutation.mutate(selected.id);
              }
            }}
          />
        ) : (
          <div className="flex h-full items-center justify-center text-muted-foreground">
            <p className="text-sm">Select a trigger from the list, or create a new one.</p>
          </div>
        )}
      </div>
    </div>
  );
}

function TriggerCreateForm({ onSave, onCancel }: {
  onSave: (data: { name: string; sourceType: string; targetType: string; sourceConfig: any; targetConfig: any }) => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState("");
  const [sourceType, setSourceType] = useState("cron");
  const [cronExpr, setCronExpr] = useState("0 * * * *");
  const [workflowId, setWorkflowId] = useState("");

  return (
    <div className="m-2 rounded-md border border-border p-3 space-y-2">
      <input
        className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
        placeholder="Trigger name"
        value={name}
        onChange={(e) => setName(e.target.value)}
      />
      <select
        className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
        value={sourceType}
        onChange={(e) => setSourceType(e.target.value)}
      >
        <option value="cron">Cron</option>
        <option value="webhook">Webhook</option>
      </select>
      {sourceType === "cron" && (
        <input
          className="w-full rounded border border-border px-2 py-1 text-sm font-mono bg-background"
          placeholder="Cron expression (e.g. 0 * * * *)"
          value={cronExpr}
          onChange={(e) => setCronExpr(e.target.value)}
        />
      )}
      <input
        className="w-full rounded border border-border px-2 py-1 text-sm bg-background"
        placeholder="Workflow ID"
        value={workflowId}
        onChange={(e) => setWorkflowId(e.target.value)}
      />
      <div className="flex gap-2">
        <button
          className="flex-1 rounded bg-primary px-2 py-1 text-sm text-primary-foreground disabled:opacity-50"
          disabled={!name || !workflowId}
          onClick={() => onSave({
            name,
            sourceType,
            targetType: "run_workflow",
            sourceConfig: sourceType === "cron" ? { expr: cronExpr, tz: "UTC" } : {},
            targetConfig: { workflowId },
          })}
        >
          Create
        </button>
        <button
          className="rounded border border-border px-2 py-1 text-sm"
          onClick={onCancel}
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

function TriggerEditor({ trigger, onUpdate, onDelete }: {
  trigger: Trigger;
  onUpdate: (updates: Partial<{ enabled: boolean; autoDisableAfter: number }>) => void;
  onDelete: () => void;
}) {
  return (
    <div className="mx-auto max-w-2xl p-6 space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">{trigger.name}</h2>
        <div className="flex gap-2">
          <button
            className={cn(
              "rounded-md px-3 py-1 text-sm border",
              trigger.enabled ? "bg-primary text-primary-foreground" : "bg-background",
            )}
            onClick={() => onUpdate({ enabled: !trigger.enabled })}
          >
            {trigger.enabled ? "Enabled" : "Disabled"}
          </button>
          <button
            className="rounded-md px-3 py-1 text-sm border border-destructive text-destructive hover:bg-destructive/10"
            onClick={onDelete}
          >
            Delete
          </button>
        </div>
      </div>

      <div className="rounded-lg border border-border p-4 space-y-3">
        <div className="grid grid-cols-2 gap-3 text-sm">
          <div>
            <span className="text-muted-foreground">Source:</span>{" "}
            <span className="font-medium">{trigger.sourceType}</span>
          </div>
          <div>
            <span className="text-muted-foreground">Target:</span>{" "}
            <span className="font-medium">{trigger.targetType}</span>
          </div>
          {trigger.sourceType === "cron" && (
            <div className="col-span-2">
              <span className="text-muted-foreground">Expression:</span>{" "}
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                {JSON.stringify(trigger.sourceConfig)}
              </code>
            </div>
          )}
          {trigger.sourceType === "webhook" && (
            <div className="col-span-2">
              <span className="text-muted-foreground">Webhook URL:</span>{" "}
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                /api/v1/hooks/{trigger.id}
              </code>
            </div>
          )}
        </div>
      </div>

      <div className="rounded-lg border border-border p-4 space-y-2">
        <h3 className="text-sm font-semibold">Circuit Breaker</h3>
        <div className="text-sm text-muted-foreground">
          Auto-disable after {trigger.autoDisableAfter} consecutive failures.
          Current: {trigger.consecutiveFailures} failures.
        </div>
      </div>

      {trigger.lastFiredAt && (
        <div className="text-xs text-muted-foreground">
          Last fired: {new Date(trigger.lastFiredAt).toLocaleString()}
        </div>
      )}
    </div>
  );
}
