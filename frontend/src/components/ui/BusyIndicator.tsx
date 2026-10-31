import { Circle, Loader2 } from "lucide-react";
import { cn } from "../../lib/utils";

interface BusyIndicatorProps {
  className?: string;
  size?: "sm" | "md";
  /** RC-C S2 (badge honesty): the spinner reflects LIVENESS, never
   * claim state. true (default — back-compat for the session-activity
   * surfaces) renders the spinning loader; false renders the DISTINCT
   * quiet marker (claimed but quiet: idle, unknown, or lapsed — never
   * a spin). */
  busy?: boolean;
}

const sizeMap = { sm: "h-3 w-3", md: "h-3.5 w-3.5" };

export function BusyIndicator({ className, size = "md", busy = true }: BusyIndicatorProps) {
  if (!busy) {
    // Claimed-but-quiet: a static muted dot — the claim stays readable
    // (someone holds the row) without claiming work is happening.
    return (
      <Circle
        className={cn("fill-muted-foreground/40 text-muted-foreground/40 flex-shrink-0", sizeMap[size], className)}
        aria-label="Agent claimed — quiet"
        data-busy="false"
      />
    );
  }
  return (
    <Loader2
      className={cn("animate-spin text-blue-500 flex-shrink-0", sizeMap[size], className)}
      aria-label="Agent working"
      data-busy
    />
  );
}
