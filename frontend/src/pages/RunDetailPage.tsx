import { useState } from "react";
import { useParams, useNavigate } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { runApi } from "../api/workflows";
import { Badge } from "../components/ui/Badge";
import { Spinner } from "../components/ui/Spinner";
import {
  CheckCircle2, XCircle, Clock, Loader2, SkipForward,
  Ban, AlertTriangle, ChevronDown, ChevronRight,
} from "lucide-react";

const STATUS_ICONS: Record<string, typeof CheckCircle2> = {
  succeeded: CheckCircle2,
  failed: XCircle,
  running: Loader2,
  pending: Clock,
  skipped: SkipForward,
  canceled: Ban,
  queued: Clock,
  timed_out: AlertTriangle,
};

const STATUS_COLORS: Record<string, string> = {
  succeeded: "text-green-500",
  failed: "text-red-500",
  running: "text-blue-500",
  pending: "text-muted-foreground",
  skipped: "text-muted-foreground",
  canceled: "text-orange-500",
  queued: "text-muted-foreground",
  timed_out: "text-yellow-500",
};

export function RunDetailPage() {
  const { runId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [expandedNodes, setExpandedNodes] = useState<Set<string>>(new Set());

  const { data: run, isLoading: runLoading } = useQuery({
    queryKey: ["run", runId],
    queryFn: () => runApi.get(runId!),
    refetchInterval: (q) => {
      const status = (q.state.data as any)?.status;
      return status === "running" || status === "queued" ? 3000 : false;
    },
  });

  const { data: nodes, isLoading: nodesLoading } = useQuery({
    queryKey: ["run-nodes", runId],
    queryFn: () => runApi.nodes(runId!),
    refetchInterval: (q) => {
      const arr = q.state.data as any[];
      const anyRunning = arr?.some((n) => n.status === "running" || n.status === "pending");
      return anyRunning ? 3000 : false;
    },
  });

  const cancelMutation = useMutation({
    mutationFn: () => runApi.cancel(runId!),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["run", runId] }),
  });

  if (runLoading || !run) {
    return (
      <div className="flex h-full items-center justify-center"><Spinner /></div>
    );
  }

  const runData = run as any;
  const isRunning = runData.status === "running" || runData.status === "queued";

  const toggleNode = (id: string) => {
    setExpandedNodes((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <div className="flex h-full flex-col overflow-hidden">
      <div className="border-b border-border px-4 py-3">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-3">
            <button
              onClick={() => navigate(-1)}
              className="rounded border border-border px-2 py-1 text-xs hover:bg-accent"
            >
              ← Back
            </button>
            <h2 className="text-sm font-semibold">Run {runData.id.slice(0, 8)}</h2>
            <StatusBadge status={runData.status} />
            {runData.errorCode && (
              <Badge variant="destructive" className="text-xs font-mono">{runData.errorCode}</Badge>
            )}
          </div>
          {isRunning && (
            <button
              onClick={() => cancelMutation.mutate()}
              disabled={cancelMutation.isPending}
              className="flex items-center gap-1 rounded-md border border-destructive px-3 py-1 text-sm text-destructive hover:bg-destructive/10"
            >
              <Ban className="h-3.5 w-3.5" /> Cancel Run
            </button>
          )}
        </div>

        <div className="mt-2 flex gap-4 text-xs text-muted-foreground">
          {runData.startedAt && (
            <span>Started: {new Date(runData.startedAt).toLocaleString()}</span>
          )}
          {runData.finishedAt && (
            <span>Finished: {new Date(runData.finishedAt).toLocaleString()}</span>
          )}
          {runData.startedAt && runData.finishedAt && (
            <span>
              Duration: {Math.round((new Date(runData.finishedAt).getTime() - new Date(runData.startedAt).getTime()) / 1000)}s
            </span>
          )}
          {runData.triggerId && (
            <span>Trigger: {runData.triggerId.slice(0, 8)}</span>
          )}
        </div>

        {runData.error && (
          <div className="mt-2 rounded-md border border-destructive/30 bg-destructive/10 p-2 text-xs text-destructive">
            <pre className="whitespace-pre-wrap font-mono">{JSON.stringify(runData.error, null, 2)}</pre>
          </div>
        )}
      </div>

      <div className="flex-1 overflow-y-auto p-4">
        {nodesLoading ? (
          <Spinner />
        ) : (nodes as any[])?.length === 0 ? (
          <div className="py-8 text-center text-sm text-muted-foreground">
            {isRunning ? "Waiting for node execution to start..." : "No node runs recorded."}
          </div>
        ) : (
          <div className="space-y-2">
            {(nodes as any[]).map((node) => {
              const Icon = STATUS_ICONS[node.status] || Clock;
              const color = STATUS_COLORS[node.status] || "text-muted-foreground";
              const isExpanded = expandedNodes.has(node.id);
              const duration = node.startedAt && node.finishedAt
                ? Math.round((new Date(node.finishedAt).getTime() - new Date(node.startedAt).getTime()) / 1000)
                : null;

              return (
                <div key={node.id} className="rounded-lg border border-border">
                  <button
                    onClick={() => toggleNode(node.id)}
                    className="flex w-full items-center gap-3 p-3 text-left hover:bg-accent/50"
                  >
                    {isExpanded ? (
                      <ChevronDown className="h-3.5 w-3.5 text-muted-foreground" />
                    ) : (
                      <ChevronRight className="h-3.5 w-3.5 text-muted-foreground" />
                    )}
                    <Icon className={`h-4 w-4 ${color} ${node.status === "running" ? "animate-spin" : ""}`} />
                    <div className="flex-1">
                      <div className="flex items-center gap-2">
                        <span className="text-sm font-medium">{node.nodeId}</span>
                        <Badge variant="default" className="text-[10px] uppercase">{node.nodeType}</Badge>
                        {node.branch && (
                          <Badge variant="warning" className="text-[10px] font-mono">→ {node.branch}</Badge>
                        )}
                        {node.attempt > 1 && (
                          <span className="text-xs text-muted-foreground">attempt {node.attempt}</span>
                        )}
                      </div>
                      {node.errorCode && (
                        <span className="text-xs text-destructive font-mono">{node.errorCode}</span>
                      )}
                    </div>
                    {duration !== null && (
                      <span className="text-xs text-muted-foreground">{duration}s</span>
                    )}
                  </button>

                  {isExpanded && (
                    <div className="border-t border-border p-3 space-y-2">
                      {node.input && (
                        <DetailBlock label="Input" data={node.input} />
                      )}
                      {node.output && (
                        <DetailBlock label="Output" data={node.output} />
                      )}
                      {node.error && (
                        <DetailBlock label="Error" data={node.error} error />
                      )}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        )}

        {runData.output && (
          <div className="mt-4 rounded-lg border border-green-500/30 bg-green-500/5 p-3">
            <h3 className="mb-2 text-xs font-semibold text-green-500">Final Output</h3>
            <pre className="overflow-x-auto whitespace-pre-wrap font-mono text-xs">
              {typeof runData.output === "string" ? runData.output : JSON.stringify(runData.output, null, 2)}
            </pre>
          </div>
        )}
      </div>
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const variant =
    status === "succeeded" ? "success" :
    status === "failed" || status === "timed_out" ? "destructive" :
    status === "canceled" ? "warning" :
    "default";
  return <Badge variant={variant as any} className="text-xs">{status}</Badge>;
}

function DetailBlock({ label, data, error }: { label: string; data: unknown; error?: boolean }) {
  return (
    <div>
      <div className={`mb-1 text-xs font-medium ${error ? "text-destructive" : "text-muted-foreground"}`}>{label}</div>
      <pre className={`overflow-x-auto rounded bg-muted p-2 text-xs font-mono ${error ? "text-destructive" : ""}`}>
        {typeof data === "string" ? data : JSON.stringify(data, null, 2)}
      </pre>
    </div>
  );
}
