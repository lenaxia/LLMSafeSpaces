import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { workspacesApi } from "../../api/workspaces";
import type { DiskRecoveryReport } from "../../api/types";
import { formatBytes } from "../../lib/format";

// #1601 — the mechanical disk-recovery surface.
//
// SURFACED_AUTOMATIC_RATIO mirrors pkg/agent/systemnotices' critical tier
// (0.95) — the single threshold source on the platform side, compared on
// the RAW ratio (floored for display, exactly like the backend). At or
// above it the strip renders as a compact critical banner; below it the
// component renders NOTHING (#1686: the pre-#1686 permanent on-demand
// line trained users to ignore the safety banner, occupied a dedicated
// line under the metrics row, and broke the mobile Hide toggle's
// perceived behavior — the API stays available at any usage level, this
// is presentation-only).
const SURFACED_AUTOMATIC_RATIO = 0.95;

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
  // #1686: the critical banner is dismissible. A dismissal lasts for the
  // current critical EPISODE (the continuous stretch at >=95%); dropping
  // below the threshold ends the episode and re-arms the banner, so the
  // alert can be quieted per-spike but never permanently silenced while
  // the disk keeps crossing the line.
  const [dismissedEpisode, setDismissedEpisode] = useState(false);
  const [prevCritical, setPrevCritical] = useState(false);
  // All strip state is workspace-scoped (review r1): ChatPage's Outlet is
  // not keyed by workspace, so this instance survives workspace switches —
  // without the reset, a dismissal (or an in-flight report) from workspace
  // A would silence or mislead workspace B, whose numbers were never seen.
  const [prevWorkspaceId, setPrevWorkspaceId] = useState(workspaceId);

  const known = diskTotalBytes != null && diskTotalBytes > 0 && diskUsedBytes != null;
  const ratio = known ? (diskUsedBytes! / diskTotalBytes!) : 0;
  // The threshold compares the RAW ratio, and the display floors it —
  // mirroring pkg/agent/systemnotices' critical tier exactly (raw 0.9499
  // shows "94%" and does NOT surface; a rounded comparison would surface
  // 94.5-94.99% as "95%", over-alerting vs the mirrored source).
  const critical = known && ratio >= SURFACED_AUTOMATIC_RATIO;
  const pct = known ? Math.min(100, Math.floor(ratio * 100)) : 0;

  // Derived-state adjustment (the React-endorsed adjust-during-render
  // pattern): leaving the critical tier ends the dismissal episode, and a
  // workspace switch resets ALL interaction state (dismissal, report,
  // error) so nothing leaks across workspaces.
  if (prevWorkspaceId !== workspaceId) {
    setPrevWorkspaceId(workspaceId);
    setPhase({ kind: "idle" });
    setError(null);
    setDismissedEpisode(false);
    setPrevCritical(critical);
    return null;
  }
  if (prevCritical !== critical) {
    setPrevCritical(critical);
    if (!critical) setDismissedEpisode(false);
  }

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

  // Below the critical threshold the strip renders NOTHING (#1686) — not
  // even an on-demand affordance, and not a terminal error either (review
  // r1: a failed recovery's error line must not persist below the
  // threshold). Only a genuinely IN-FLIGHT interaction (busy/report) keeps
  // the strip mounted so results never vanish mid-interaction.
  if (!workspaceId || !known) return null;
  const inFlight = phase.kind === "busy" || phase.kind === "report";

  const report = phase.kind === "report" ? phase.report : null;
  // `?? []` is belt to the engine's braces (review r1 F2): the engine
  // initializes classes on every path, but a null on the wire (older
  // agentd, a regression) must degrade to an empty report — never a
  // TypeError that unmounts the whole chat header.
  const reportClasses = report?.classes ?? [];
  const reclaimable = reportClasses
    .filter((c) => c.status === "would_free" || c.status === "freed")
    .reduce((sum, c) => sum + c.bytes, 0) ?? 0;

  // The dismissible compact banner only shows while critical and not
  // dismissed; an in-flight phase (busy/report) keeps the strip mounted so
  // results never vanish mid-interaction even if usage dipped.
  const showBanner = critical && !dismissedEpisode;

  if (!showBanner && !inFlight) return null;

  return (
    <div
      role="status"
      className="flex flex-col border-b border-orange-500/30 bg-orange-500/10 px-4 py-1 text-[11px] text-orange-700 dark:text-orange-300"
      data-testid="disk-recovery-strip"
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
        {showBanner && (
          <span className="font-medium">
            Disk at {pct}% — critically low.
          </span>
        )}
        {showBanner && (
          <span className="text-muted-foreground">
            Free known-reproducible caches (build caches only — files, source, and logs are never touched).
          </span>
        )}
        <span className="ml-auto flex items-center gap-2">
          {phase.kind === "idle" && !error && (
            <>
              <button
                className="rounded-md border border-orange-500/40 bg-orange-500/20 px-2 py-0.5 font-medium hover:bg-orange-500/30"
                onClick={() => run(true)}
                data-testid="disk-recover-review"
              >
                Review what can be freed
              </button>
              <button
                className="underline hover:no-underline"
                onClick={() => setDismissedEpisode(true)}
                data-testid="disk-recovery-dismiss"
              >
                Dismiss
              </button>
            </>
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

      {error && <div className="text-red-500">{error} <button className="underline hover:no-underline" onClick={() => run(true)}>retry</button></div>}

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
                  {/* Freed rows show bytesFreed (the honest figure — a
                      partial sweep credits less than measured); every
                      other status shows the measured figure. */}
                  <td className="pr-2">{formatBytes(c.status === "freed" ? c.bytesFreed : c.bytes)}</td>
                  <td className="text-muted-foreground">
                    {c.status === "would_free" && "reclaimable"}
                    {c.status === "freed" && (c.bytesFreed < c.bytes ? `freed (${c.reason ?? "partial"})` : "freed")}
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
