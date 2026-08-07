import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { workflowApi, type Workflow } from "../api/workflows";
import { WorkflowEditor } from "../components/workflows/WorkflowEditor";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import { cn } from "../lib/utils";
import { Plus } from "lucide-react";

export function WorkflowsPage() {
  const { workflowId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);

  const { data: workflows, isLoading } = useQuery({
    queryKey: ["workflows"],
    queryFn: () => workflowApi.list(),
  }) as { data: Workflow[] | undefined; isLoading: boolean };

  const selected = workflows?.find((w) => w.id === workflowId) ?? null;

  const deleteMutation = useMutation({
    mutationFn: (id: string) => workflowApi.delete(id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["workflows"] });
      navigate("/workflows");
    },
  });

  const handleSelect = (wf: Workflow) => {
    navigate(`/workflows/${wf.id}`);
  };

  if (isLoading) {
    return (
      <div className="flex h-full items-center justify-center">
        <Spinner />
      </div>
    );
  }

  return (
    <div className="flex h-full overflow-hidden">
      {/* Left panel — workflow list */}
      <div className="w-72 shrink-0 border-r border-border overflow-y-auto scrollbar-thin">
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

        {showCreate && (
          <WorkflowEditor
            mode="create"
            onSave={async (name, spec, status) => {
              const created = await workflowApi.create({ name, specYaml: spec, status }) as Workflow;
              setShowCreate(false);
              queryClient.invalidateQueries({ queryKey: ["workflows"] });
              navigate(`/workflows/${created.id}`);
            }}
            onCancel={() => setShowCreate(false)}
          />
        )}

        <div className="flex flex-col gap-0.5 p-1">
          {(workflows ?? []).map((wf) => (
            <button
              key={wf.id}
              onClick={() => handleSelect(wf)}
              className={cn(
                "flex flex-col gap-1 rounded-md p-2 text-left transition-colors",
                "hover:bg-accent",
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

      {/* Right panel — editor / empty state */}
      <div className="flex-1 overflow-hidden">
        {selected ? (
          <WorkflowEditor
            mode="edit"
            workflow={selected}
            onSave={async (name, spec, status) => {
              await workflowApi.update(selected.id, { name, status, specYaml: spec });
              queryClient.invalidateQueries({ queryKey: ["workflows"] });
            }}
            onDelete={async () => {
              if (confirm(`Delete workflow "${selected.name}"?`)) {
                deleteMutation.mutate(selected.id);
              }
            }}
            onRun={async (input?: string) => {
              await workflowApi.run(selected.id, input);
            }}
          />
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
  return (
    <Badge variant={variant as any} className="text-[10px]">
      {status}
    </Badge>
  );
}
