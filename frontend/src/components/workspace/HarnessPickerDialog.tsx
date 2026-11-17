import * as Dialog from "@radix-ui/react-dialog";
import { useEffect, useRef, useState } from "react";
import { Button } from "../ui/Button";
import { DEFAULT_HARNESS, FALLBACK_HARNESS_OPTIONS, harnessMeta } from "./harnessMeta";
import { cn } from "../../lib/utils";

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  workspaceName: string;
  /** Enabled harnesses for the workspace (harnessOptions on the
   * workspace listing) — the offer list, exactly as the server
   * renders it. */
  options?: string[];
  submitting: boolean;
  error?: string | null;
  /** Fires with the selected harness: the caller sends it as the
   * sessions/new body — binding and session creation are atomic
   * server-side (the bind-once API story). */
  onSubmit: (harness: string) => void;
}

/**
 * HarnessPickerDialog — the FIRST-SESSION PICKER, the frontend's only
 * harness interaction (owner ruling 2026-11-16: one harness per
 * workspace, bound once, immutable; there is no switch anywhere, ever).
 *
 * Shown exactly when the user starts a session on an UNBOUND workspace
 * and more than one harness is enabled — the workspace's one permanent
 * harness decision. Exactly one enabled harness never opens this
 * modal (silent auto-bind); a bound workspace never opens it either.
 *
 * a11y per the #2021 discipline: Radix Dialog (focus trap, Escape,
 * aria-modal), the options are a roving-tabindex radiogroup
 * (arrows/Home/End move selection, focus-visible ring), and a polite
 * live region announces the selection.
 */
export function HarnessPickerDialog({
  open,
  onOpenChange,
  workspaceName,
  options,
  submitting,
  error,
  onSubmit,
}: Props) {
  const offer = options && options.length > 0 ? options : FALLBACK_HARNESS_OPTIONS;
  const preselected = offer.includes(DEFAULT_HARNESS) ? DEFAULT_HARNESS : (offer[0] ?? DEFAULT_HARNESS);
  const [selected, setSelected] = useState<string>(preselected);
  const optionRefs = useRef<(HTMLButtonElement | null)[]>([]);

  useEffect(() => {
    if (open) setSelected(preselected);
  }, [open, preselected]);

  const moveSelection = (index: number) => {
    const wrapped = ((index % offer.length) + offer.length) % offer.length;
    const next = offer[wrapped];
    if (!next) return;
    setSelected(next);
    optionRefs.current[wrapped]?.focus();
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    const idx = offer.indexOf(selected);
    switch (e.key) {
      case "ArrowDown":
      case "ArrowRight":
        e.preventDefault();
        moveSelection(idx + 1);
        break;
      case "ArrowUp":
      case "ArrowLeft":
        e.preventDefault();
        moveSelection(idx - 1);
        break;
      case "Home":
        e.preventDefault();
        moveSelection(0);
        break;
      case "End":
        e.preventDefault();
        moveSelection(offer.length - 1);
        break;
    }
  };

  return (
    <Dialog.Root open={open} onOpenChange={submitting ? () => undefined : onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50 data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=closed]:animate-out data-[state=closed]:fade-out-0" />
        <Dialog.Content className="fixed left-1/2 top-1/2 z-50 w-[24rem] max-w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2 rounded-md border border-border bg-background p-5 shadow-xl data-[state=open]:animate-in data-[state=open]:fade-in-0 data-[state=closed]:animate-out data-[state=closed]:fade-out-0">
          <Dialog.Title className="text-sm font-semibold">Choose a harness</Dialog.Title>
          <Dialog.Description className="mt-1 text-xs text-muted-foreground">
            Pick the coding agent for “{workspaceName}”. A workspace keeps its harness for its
            entire lifetime — this choice cannot be changed later.
          </Dialog.Description>

          <div
            role="radiogroup"
            aria-label="Harness"
            className="mt-4 flex flex-col gap-2"
            onKeyDown={handleKeyDown}
          >
            {offer.map((harness, i) => {
              const meta = harnessMeta(harness);
              const Icon = meta.icon;
              const checked = harness === selected;
              return (
                <button
                  key={harness}
                  type="button"
                  role="radio"
                  aria-checked={checked}
                  tabIndex={checked ? 0 : -1}
                  ref={(el) => {
                    optionRefs.current[i] = el;
                  }}
                  onClick={() => setSelected(harness)}
                  onFocus={() => setSelected(harness)}
                  className={cn(
                    "flex items-start gap-3 rounded-md border p-3 text-left transition-colors",
                    "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-1",
                    checked ? "border-primary bg-accent" : "border-border hover:bg-accent/50",
                  )}
                >
                  <Icon className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
                  <span className="min-w-0">
                    <span className="block text-sm font-medium">{meta.name}</span>
                    <span className="block text-xs text-muted-foreground">{meta.blurb}</span>
                  </span>
                </button>
              );
            })}
          </div>

          <p aria-live="polite" className="sr-only" data-testid="harness-picker-announcement">
            Selected harness: {selected}
          </p>

          {error && <p className="mt-2 text-xs text-destructive">{error}</p>}

          <div className="mt-4 flex justify-end gap-2">
            <Button variant="ghost" size="sm" disabled={submitting} onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button
              size="sm"
              disabled={submitting || !selected}
              onClick={() => selected && onSubmit(selected)}
              data-testid="harness-picker-submit"
            >
              {submitting ? "Creating…" : "Start session"}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
