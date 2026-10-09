import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { workspacesApi } from "../../api/workspaces";
import type { DiskRecoveryReport } from "../../api/types";
import { formatBytes } from "../../lib/format";

// #1601 — the mechanical disk-recovery surface.
//
// SURFACED_AUTOMATIC_THRESHOLD mirrors pkg/agent/systemnotices'
// critical tier (0.95) — the single threshold source on the platform
// side. At or above it the strip renders as a prominent banner; below
// it a subtle on-demand action stays available (issue: "menu action"
// below the threshold).
const SURFACED_AUTOMATIC_PCT = 95;

// The strip is intentionally dumb about disk math: ChatPage feeds it
// the same status fields DiskUsageBar renders.
interface Props {
  workspaceId?: string;
  diskUsedBytes?: number;
  diskTotalBytes?: number;
}

type Phase =
  | { kind: "idle" }
  | { kind: "report"; report: DiskRecoveryReport; dryRun: boolean }
  | { kind: "busy"; dryRun: boolean };

const CLASS_LABELS: Record<string, string> = {
  "go-build-cache": "Go build cache",
  "go-module-cache": "Go module cache",
  "npm-cache": "npm cache",
  "pip-cache": "pip cache",
  "pnpm-store": "pnpm store",
  "cargo-download-cache": "Cargo downloads",
  "mise-download-cache": "mise downloads",
  "mise-xdg-cache": "mise cache",
  "tmp-build-residue": "Stale /tmp build dirs",
};

function classLabel(cls: string): string {
  return CLASS_LABELS[cls] ?? cls;
}

export function DiskRecoveryStrip({ workspaceId, diskUsedBytes, diskTotalBytes }: Props) {
  const queryClient = useQueryClient();
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const [error, setError] = useState<string | null>(null);

  const known = diskTotalBytes != null && diskTotalBytes > 0 && diskUsedBytes != null;
  const pct = known ? Math.min(100, Math.round((diskUsedBytes! / diskTotalBytes!) * 100)) : 0;
  const critical = known && pct >= SURFACED_AUTOMATIC_PCT;

  const mutation = useMutation({
    mutationFn: ({ dryRun }: { dryRun: boolean }) => workspacesApi.recoverDisk(workspaceId!, dryRun),
  });

  const run = (dryRun: boolean) => {
    if (!workspaceId) return;
    setPhase({ kind: "busy", dryRun });
    mutation.mutate(
      { dryRun },
      {
        onSuccess: (report) => {
          setError(null);
          setPhase({ kind: "report", report, dryRun });
          if (!dryRun) {
            // The sweep ran: refetch so DiskUsageBar reflects reality.
            void queryClient.invalidateQueries({ queryKey: ["workspace-status", workspaceId] });
          }
        },
        onError: (err: unknown) => {
          setPhase({ kind: "idle" });
          setError(err instanceof Error ? err.message : "Disk recovery failed");
        },
      }
    );
  };

  if (!workspaceId || !known) return null;

  // On-demand affordance below the auto-surface threshold: subtle, in
  // the metrics row's visual language, never a banner.
  if (!critical && phase.kind === "idle" && !error) {
    return (
      <div className="flex items-center justify-end px-4 py-0.5 text-[10px] text-muted-foreground/50">
        <button
          className="uppercase tracking-wider hover:text-muted-foreground"
          onClick={() => run(true)}
          data-testid="disk-recover-on-demand"
        >
          Free up disk space
        </button>
      </div>
    );
  }

  const report = phase.kind === "report" ? phase.report : null;
  // `?? []` is belt to the engine's braces (review r1 F2): the engine
  // initializes classes on every path, but a null on the wire (older
  // agentd, a regression) must degrade to an empty report — never a
  // TypeError that unmounts the whole chat header.
  const reportClasses = report?.classes ?? [];
  const reclaimable = reportClasses
    .filter((c) => c.status === "would_free" || c.status === "freed")
    .reduce((sum, c) => sum + c.bytes, 0) ?? 0;

  return (
    <div
      role="status"
      className="flex flex-col gap-1.5 border-b border-orange-500/30 bg-orange-500/10 px-4 py-2 text-xs text-orange-700 dark:text-orange-300"
      data-testid="disk-recovery-strip"
    >
      <div className="flex flex-wrap items-center gap-2">
        {critical && (
          <span className="font-medium">
            Disk at {pct}% — critically low. Free known-reproducible caches (build caches only — your files, source, and logs are never touched).
          </span>
        )}
        <span className="ml-auto flex items-center gap-2">
          {phase.kind === "idle" && (
            <button
              className="rounded-md border border-orange-500/40 bg-orange-500/20 px-2 py-0.5 font-medium hover:bg-orange-500/30"
              onClick={() => run(true)}
              data-testid="disk-recover-review"
            >
              Review what can be freed
            </button>
          )}
          {phase.kind === "busy" && (
            <span className="text-muted-foreground">
              {phase.dryRun ? "Measuring caches…" : "Freeing space…"}
            </span>
          )}
          {phase.kind === "report" && !phase.dryRun && (
            <button
              className="underline hover:no-underline"
              onClick={() => setPhase({ kind: "idle" })}
            >
              Done
            </button>
          )}
        </span>
      </div>

      {error && <div className="text-red-500">{error} <button className="underline hover:no-underline" onClick={() => setError(null)}>retry</button></div>}

      {report?.alreadyBelowTarget && (
        <div className="text-muted-foreground">
          Disk is already below the {Math.round(report.targetRatio * 100)}% target — nothing to free{report.dryRun ? "" : " (nothing was deleted)"}.
        </div>
      )}

      {report && !report.alreadyBelowTarget && reportClasses.length > 0 && (
        <div className="flex flex-col gap-1">
          <table className="w-full max-w-2xl text-left tabular-nums">
            <thead>
              <tr className="text-[10px] uppercase tracking-wider text-muted-foreground/70">
                <th className="pr-2 font-medium">Cache</th>
                <th className="pr-2 font-medium">{report.dryRun ? "Would free" : "Freed"}</th>
                <th className="font-medium">Status</th>
              </tr>
            </thead>
            <tbody>
              {reportClasses.map((c) => (
                <tr key={c.class} className="align-top">
                  <td className="pr-2">{classLabel(c.class)}</td>
                  <td className="pr-2">{formatBytes(c.bytes)}</td>
                  <td className="text-muted-foreground">
                    {c.status === "would_free" && "reclaimable"}
                    {c.status === "freed" && "freed"}
                    {c.status === "not_present" && "not present"}
                    {c.status === "skipped_target_met" && "skipped — target met"}
                    {c.status === "refused" && <span className="text-red-500">refused ({c.reason})</span>}
                    {c.status === "error" && <span className="text-red-500">error ({c.reason})</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {report.dryRun ? (
            <div className="flex items-center gap-2">
              <button
                className="rounded-md border border-orange-500/40 bg-orange-500/20 px-2 py-0.5 font-medium hover:bg-orange-500/30"
                onClick={() => run(false)}
                data-testid="disk-recover-free-now"
                disabled={reclaimable === 0}
              >
                Free {formatBytes(reclaimable)} now
              </button>
              <span className="text-muted-foreground">
                Stops as soon as the disk drops below {Math.round(report.targetRatio * 100)}%.
              </span>
            </div>
          ) : (
            <div className="text-muted-foreground">
              Freed {formatBytes(report.bytesFreed)} — disk now at {Math.round(report.afterRatio * 100)}%.
            </div>
          )}
        </div>
      )}
    </div>
  );
}
