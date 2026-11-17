import { Bot, Terminal } from "lucide-react";
import type { LucideIcon } from "lucide-react";

/** The platform default harness (union-enforced server-side: it is
 * always present in harnessOptions and preselected in the picker). */
export const DEFAULT_HARNESS = "opencode";

export interface HarnessMeta {
  name: string;
  /** One-line blurb shown in the picker. */
  blurb: string;
  icon: LucideIcon;
}

/** Presentation metadata for the known harnesses. Unknown ids (a
 * harness enabled server-side that this frontend build predates)
 * degrade to a generic entry rather than breaking the picker. */
export const HARNESS_META: Record<string, HarnessMeta> = {
  opencode: {
    name: "opencode",
    blurb: "The platform default — a terminal-native coding agent.",
    icon: Terminal,
  },
  pi: {
    name: "pi",
    blurb: "A minimal, hackable agent runtime — lightweight and fast.",
    icon: Bot,
  },
};

export function harnessMeta(harness: string): HarnessMeta {
  return HARNESS_META[harness] ?? { name: harness, blurb: "Coding-agent harness.", icon: Bot };
}

/** The offer list when the workspace listing predates harnessOptions
 * (or a degraded read): opencode alone, matching the server's
 * settings-failure degrade (D8 of the bind-once story). */
export const FALLBACK_HARNESS_OPTIONS: string[] = [DEFAULT_HARNESS];
