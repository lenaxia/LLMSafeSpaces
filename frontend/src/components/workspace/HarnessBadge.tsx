import { harnessMeta } from "./harnessMeta";
import { cn } from "../../lib/utils";

interface Props {
  harness: string;
  className?: string;
}

/**
 * HarnessBadge — the read-only bound-harness badge (owner ruling
 * 2026-11-16: one harness per workspace, immutable forever). This is
 * the workspace's ONLY post-binding harness surface besides the
 * settings drawer row: informational, not interactive — a span, never
 * a control. There is deliberately no switch affordance anywhere.
 */
export function HarnessBadge({ harness, className }: Props) {
  const meta = harnessMeta(harness);
  const Icon = meta.icon;
  return (
    <span
      data-testid="harness-badge"
      title={`Harness: ${meta.name} — permanent for this workspace`}
      aria-label={`Harness ${meta.name}, permanent for this workspace`}
      className={cn(
        "inline-flex shrink-0 items-center gap-1 rounded-full border border-border bg-muted/50 px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground",
        className,
      )}
    >
      <Icon className="h-3 w-3" aria-hidden="true" />
      <span>{meta.name}</span>
    </span>
  );
}
