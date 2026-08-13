import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { workflowApi, runApi, type Workflow, type WorkflowRun } from "../api/workflows";
import { WorkflowEditor } from "../components/workflows/WorkflowEditor";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import { cn } from "../lib/utils";
import { Plus, History, ArrowLeft } from "lucide-react";
import { safeConfirm } from "../lib/safeConfirm";

export function WorkflowsPage() {
  const { workflowId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [showHistory, setShowHistory] = useState(false);

  const { data: workflows, isLoading } = useQuery({
    queryKey: ["workflows"],
    queryFn: () => workflowApi.list(),
  }) as { data: Workflow[] | undefined; isLoading: boolean };

  const selected = workflows?.find((w) => w.id === workflowId) ?? null;

  const { data: runs } = useQuery({
    queryKey: ["workflow-runs", workflowId],
    queryFn: () => runApi.listForWorkflow(workflowId!),
    enabled: !!workflowId && showHistory,
  }) as { data: WorkflowRun[] | undefined };

  const deleteMutation = useMutation({
    mutationFn: (id: string) => workflowApi.delete(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workflows"] });
      navigate("/workflows");
    },
  });

  const handleSelect = (wf: Workflow) => navigate(`/workflows/${wf.id}`);

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
        (selected || showCreate) && "hidden md:block",
      )}>
        <div className="flex items-center justify-between p-3 border-b border-border">
          <h2 className="text-sm font-semibold">Workflows</h2>
          <button
            onClick={() => setShowCreate(true)}
            className="rounded-md p-1 hover:bg-accent"
            aria-label="New workflow"
          >
            <Plus className="h-4 w-4" />
          </button>
        </div>

        <div className="flex flex-col gap-0.5 p-1">
          {(workflows ?? []).map((wf) => (
            <button
              key={wf.id}
              onClick={() => handleSelect(wf)}
              className={cn(
                "flex flex-col gap-1 rounded-md p-2 text-left transition-colors hover:bg-accent",
                selected?.id === wf.id && "bg-accent",
              )}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm font-medium">{wf.name}</span>
                <StatusBadge status={wf.status} />
              </div>
              {wf.description && (
                <span className="truncate text-xs text-muted-foreground">{wf.description}</span>
              )}
            </button>
          ))}
          {(!workflows || workflows.length === 0) && !showCreate && (
            <div className="px-3 py-8 text-center text-sm text-muted-foreground">
              No workflows yet. Click + to create one.
            </div>
          )}
        </div>
      </div>

      {/* Detail pane — full width on mobile, hidden when nothing to show */}
      <div className={cn(
        "flex flex-1 flex-col overflow-hidden",
        !selected && !showCreate && "hidden md:flex",
      )}>
        {showCreate ? (
          <>
            <button
              onClick={() => { setShowCreate(false); navigate("/workflows", { replace: true }); }}
              className="flex items-center gap-1 border-b border-border px-3 py-2 text-sm text-muted-foreground hover:bg-accent md:hidden"
            >
              <ArrowLeft className="h-4 w-4" /> Workflows
            </button>
            <div className="flex-1 overflow-hidden">
              <WorkflowEditor
                mode="create"
                onSave={async (name, spec, status, opts) => {
                  const created = await workflowApi.create({
                    name, specYaml: spec, status,
                    targetWorkspaceId: opts?.targetWorkspaceId,
                    onMissingWorkspace: opts?.onMissingWorkspace,
                    inputSchema: opts?.inputSchema as any,
                    defaults: opts?.defaults as any,
                  }) as Workflow;
                  setShowCreate(false);
                  queryClient.invalidateQueries({ queryKey: ["workflows"] });
                  navigate(`/workflows/${created.id}`);
                }}
                onCancel={() => setShowCreate(false)}
              />
            </div>
          </>
        ) : selected ? (
          <>
            <button
              onClick={() => navigate("/workflows", { replace: true })}
              className="flex items-center gap-1 border-b border-border px-3 py-2 text-sm text-muted-foreground hover:bg-accent md:hidden"
            >
              <ArrowLeft className="h-4 w-4" /> Workflows
            </button>
            <div className="flex-1 overflow-hidden">
              <WorkflowEditor
                mode="edit"
                workflow={selected}
                onSave={async (name, spec, status, opts) => {
                  await workflowApi.update(selected.id, {
                    name, status, specYaml: spec,
                    targetWorkspaceId: opts?.targetWorkspaceId,
                    onMissingWorkspace: opts?.onMissingWorkspace,
                    inputSchema: opts?.inputSchema as any,
                    defaults: opts?.defaults as any,
                  });
                  queryClient.invalidateQueries({ queryKey: ["workflows"] });
                }}
                onDelete={async () => {
                  if (safeConfirm(`Delete workflow "${selected.name}"?`)) {
                    deleteMutation.mutate(selected.id);
                  }
                }}
                onRun={async (input?: string, workspaceId?: string) => {
                  const run = await workflowApi.run(selected.id, input || undefined, workspaceId);
                  queryClient.invalidateQueries({ queryKey: ["workflow-runs", selected.id] });
                  navigate(`/workflows/${selected.id}/runs/${run.id}`);
                }}
              />
            </div>
            <div className="border-t border-border">
              <button
                onClick={() => setShowHistory(!showHistory)}
                className="flex w-full items-center gap-2 px-4 py-2 text-sm text-muted-foreground hover:bg-accent"
              >
                <History className="h-3.5 w-3.5" />
                {showHistory ? "Hide" : "Show"} run history
              </button>
              {showHistory && (
                <div className="max-h-48 overflow-y-auto border-t border-border p-2 space-y-1">
                  {(runs || []).length === 0 ? (
                    <div className="px-3 py-4 text-center text-xs text-muted-foreground">No runs yet.</div>
                  ) : (
                    (runs || []).map((run) => (
                      <button
                        key={run.id}
                        onClick={() => navigate(`/workflows/${selected.id}/runs/${run.id}`)}
                        className="flex w-full items-center justify-between rounded p-2 text-left text-sm hover:bg-accent"
                      >
                        <div className="flex items-center gap-2">
                          <RunStatusDot status={run.status} />
                          <span className="font-mono text-xs">{run.id.slice(0, 8)}</span>
                          {run.errorCode && (
                            <span className="text-xs text-destructive">{run.errorCode}</span>
                          )}
                        </div>
                        <span className="text-xs text-muted-foreground">
                          {new Date(run.createdAt).toLocaleString()}
                        </span>
                      </button>
                    ))
                  )}
                </div>
              )}
            </div>
          </>
        ) : (
          <div className="flex h-full items-center justify-center text-muted-foreground">
            <div className="text-center">
              <p className="text-sm">Select a workflow from the list, or create a new one.</p>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const variant =
    status === "active" ? "success" :
    status === "archived" ? "warning" :
    "default";
  return <Badge variant={variant as any} className="text-[10px]">{status}</Badge>;
}

function RunStatusDot({ status }: { status: string }) {
  const color =
    status === "succeeded" ? "bg-green-500" :
    status === "failed" || status === "timed_out" ? "bg-red-500" :
    status === "running" ? "bg-blue-500 animate-pulse" :
    status === "canceled" ? "bg-orange-500" :
    "bg-muted-foreground";
  return <span className={cn("inline-block h-2 w-2 rounded-full", color)} />;
}
