import { useState, useEffect, useCallback } from "react";
import { Tooltip } from "../ui/Tooltip";
import { formatBytes } from "../../lib/format";

interface MetricProps {
  label: string;
  used: number;
  total: number;
  formatValue?: (v: number) => string;
  warningThreshold?: number;
}

function formatTokens(tokens: number): string {
  if (tokens < 1000) return `${tokens}`;
  if (tokens < 1_000_000) return `${(tokens / 1000).toFixed(0)}K`;
  return `${(tokens / 1_000_000).toFixed(1)}M`;
}

function MetricItem({ label, used, total, formatValue, warningThreshold = 85 }: MetricProps) {
  const pct = total > 0 ? Math.min(100, Math.round((used / total) * 100)) : 0;
  const isHigh = pct > warningThreshold;
  const fmt = formatValue ?? ((v: number) => `${v}`);

  return (
    <div className="flex items-center gap-1.5 min-w-0">
      <span className="shrink-0 text-[10px] font-medium uppercase tracking-wider text-muted-foreground/70">{label}</span>
      <div className="w-12 h-1 rounded-full bg-muted overflow-hidden shrink-0">
        <div
          className={`h-full rounded-full transition-all ${isHigh ? "bg-orange-500" : "bg-primary/40"}`}
          style={{ width: `${pct}%` }}
        />
      </div>
      <span className="text-[11px] text-muted-foreground whitespace-nowrap tabular-nums">
        {fmt(used)}<span className="text-muted-foreground/50"> / {fmt(total)}</span>
      </span>
      <span className={`text-[10px] font-medium tabular-nums ${isHigh ? "text-orange-500" : "text-muted-foreground/50"}`}>
        {pct}%
      </span>
    </div>
  );
}

// ContextUnknownItem renders when context limit is unknown (contextTotal=0).
// Shows used tokens with "Unknown" total and a position-aware tooltip.
function ContextUnknownItem({ used, formatValue }: { used: number; formatValue?: (v: number) => string }) {
  const fmt = formatValue ?? ((v: number) => `${v}`);

  return (
    <div className="flex items-center gap-1.5 min-w-0">
      <span className="shrink-0 text-[10px] font-medium uppercase tracking-wider text-muted-foreground/70">Context</span>
      {/* No progress bar — limit is unknown */}
      <span className="text-[11px] text-muted-foreground whitespace-nowrap tabular-nums">
        {fmt(used)}
        <span className="text-muted-foreground/50"> / </span>
      </span>
      <Tooltip
        side="top"
        align="start"
        maxWidth="sm"
        content={
          <>
            <p className="font-semibold mb-1">Context limit not reported by provider</p>
            <p className="text-muted-foreground">
              The model did not return a context window size. Auto-compaction is disabled —
              the session may stall when the context fills up without warning.
              Set <code className="font-mono">max_input_tokens</code> in the provider model
              config to fix this.
            </p>
          </>
        }
      >
        <button
          className="text-[10px] font-medium text-yellow-500 hover:text-yellow-400 underline decoration-dotted cursor-help"
          aria-label="Context limit unknown — hover for details"
        >
          Unknown
        </button>
      </Tooltip>
    </div>
  );
}

interface Props {
  diskUsedBytes?: number;
  diskTotalBytes?: number;
  memoryUsedBytes?: number;
  memoryTotalBytes?: number;
  contextUsed?: number;
  contextTotal?: number;
}

type MetricId = "context" | "context-unknown" | "disk" | "memory";

interface MetricDef {
  id: MetricId;
  label: string;
  used: number;
  total: number;
  formatValue?: (v: number) => string;
  warningThreshold?: number;
  unknown?: boolean;
}

export function DiskUsageBar(props: Props) {
  const { diskUsedBytes, diskTotalBytes, memoryUsedBytes, memoryTotalBytes, contextUsed, contextTotal } = props;

  // Build ordered metric list (context always first)
  const allMetrics: MetricDef[] = [];

  {
    const used = contextUsed ?? 0;
    const total = contextTotal ?? 0;
    if (total > 0) {
      allMetrics.push({ id: "context", label: "Context", used, total, formatValue: formatTokens, warningThreshold: 80 });
    } else {
      allMetrics.push({ id: "context-unknown", label: "Context", used, total: 0, formatValue: formatTokens, unknown: true });
    }
  }

  if (diskUsedBytes != null && diskTotalBytes != null && diskTotalBytes > 0) {
    allMetrics.push({ id: "disk", label: "Disk", used: diskUsedBytes, total: diskTotalBytes, formatValue: formatBytes, warningThreshold: 85 });
  }
  if (memoryUsedBytes != null && memoryTotalBytes != null && memoryTotalBytes > 0) {
    allMetrics.push({ id: "memory", label: "Memory", used: memoryUsedBytes, total: memoryTotalBytes, formatValue: formatBytes, warningThreshold: 80 });
  }

  // ---- Mobile drawer state (sticky: stays open until explicit close) ----
  // These hooks MUST be declared before the early return below — hook calls
  // must be unconditional (Rules of Hooks). See ChatPage.tsx for the same
  // pattern and the explanation of React error #310.
  const [mobileOpen, setMobileOpen] = useState(false);
  const [mobileAutoOpened, setMobileAutoOpened] = useState(false);

  // Track which critical metrics are above threshold (unknown metrics never count as critical)
  const criticalMetrics = allMetrics.filter((m) => {
    if (m.unknown || m.total === 0) return false;
    const pct = Math.round((m.used / m.total) * 100);
    return pct > (m.warningThreshold ?? 85);
  });
  const hasCritical = criticalMetrics.length > 0;

  // Auto-open drawer when a new critical metric appears (if not explicitly closed)
  useEffect(() => {
    if (hasCritical && !mobileOpen && !mobileAutoOpened) {
      setMobileOpen(true);
      setMobileAutoOpened(true);
    }
  }, [hasCritical, mobileOpen, mobileAutoOpened]);

  const toggleDrawer = useCallback(() => {
    setMobileOpen((prev) => !prev);
    setMobileAutoOpened(false);
  }, []);

  if (allMetrics.length === 0) return null;

  const metricsInDrawer = allMetrics.slice(1); // context always shown in compact row

  const renderMetric = (m: MetricDef) => {
    if (m.unknown) {
      return <ContextUnknownItem key={m.id} used={m.used} formatValue={m.formatValue} />;
    }
    return <MetricItem key={m.id} {...m} />;
  };

  return (
    <>
      {/* Desktop: show all metrics inline */}
      <div className="hidden sm:flex items-center gap-3 px-4 py-1 text-xs text-muted-foreground flex-wrap">
        {allMetrics.map(renderMetric)}
      </div>

      {/* Mobile: context always visible in compact row + sticky drawer */}
      <div className="sm:hidden">
        <div className="flex items-center justify-between px-4 py-1">
          <div className="flex items-center gap-2 min-w-0 overflow-hidden">
            {/* Always show context */}
            {allMetrics[0] && renderMetric(allMetrics[0])}
            {/* Show critical metrics inline when drawer is closed */}
            {!mobileOpen && criticalMetrics.length > 0 && (
              <span className="text-[10px] font-medium text-orange-500 shrink-0">
                {criticalMetrics.length} critical
              </span>
            )}
          </div>
          {metricsInDrawer.length > 0 && (
            <button
              onClick={toggleDrawer}
              className="text-[10px] uppercase tracking-wider text-muted-foreground/50 hover:text-muted-foreground shrink-0 ml-2"
            >
              {mobileOpen ? "Hide" : "Metrics"}
            </button>
          )}
        </div>

        {/* Sticky drawer: stays open until explicitly closed */}
        <div
          className={`overflow-hidden transition-all duration-200 ease-in-out ${
            mobileOpen ? "max-h-96 opacity-100" : "max-h-0 opacity-0"
          }`}
        >
          <div className="flex flex-col gap-1.5 px-4 pb-1.5 pt-0.5">
            {metricsInDrawer.map(renderMetric)}
          </div>
        </div>
      </div>
    </>
  );
}
