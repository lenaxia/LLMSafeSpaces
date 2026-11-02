// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import * as Dialog from "@radix-ui/react-dialog";
import { Button } from "../ui/Button";
import { X } from "lucide-react";

interface BoardHelpDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

/** One worked-example card (§4): a use-case label, the story title as
 * it would appear on the board, the criteria a reviewer checks, and
 * what the evidence looks like. Infra-neutral by construction — every
 * entry names ARTIFACTS and VERIFICATION OUTCOMES ("the suite passes",
 * "the report", "the cleaned dataset"), never tooling or vendors, so
 * the help reads usable whatever stack the workspace runs on. */
interface HelpExample {
  key: string;
  label: string;
  story: string;
  criteria: string[];
  evidence: string;
}

const WORKFLOW_STEPS: string[] = [
  "Create a project for the effort, then an epic inside it that describes the OUTCOME you want — the result you are aiming at, not the steps you imagine.",
  "Break the work into stories small enough to finish in one sitting. Give each one acceptance criteria a reviewer can CHECK: a file exists, a test passes, a number is computed, a document answers the question.",
  "Write each story's description as if the reader has NO memory of your conversation — because they don't. Paste in the background, name the files, quote the numbers.",
  "Let the stories run. Workers claim stories, do the work, and hand off to review on their own — you never assign a worker or chase a status.",
  "When you want to know what happened — not just what the result was — open the story's audit timeline. Every claim, stage move, and review verdict is recorded.",
  "Done means every criterion was verified with evidence: a diff, a report, a dataset, a link. If nobody can point at the evidence, it isn't done.",
];

const EXAMPLES: HelpExample[] = [
  {
    key: "code",
    label: "Writing code",
    story: "Add CSV export to the report page",
    criteria: [
      "The export button produces a file that opens cleanly in a spreadsheet application.",
      "Edge cases — an empty table, unicode text in cells — are covered by tests.",
      "The test suite passes.",
    ],
    evidence: "the diff, plus the passing run of the suite.",
  },
  {
    key: "research",
    label: "Research",
    story: "Answer: which of our three markets grew fastest last quarter, and why",
    criteria: [
      "A short report cites the specific numbers behind the ranking.",
      "Every claim names its source, so a reader can trace it.",
      "A one-paragraph answer up top that a busy reader can act on.",
    ],
    evidence: "the report itself.",
  },
  {
    key: "creative",
    label: "Creative writing",
    story: "Draft chapter 7 (target ~2,500 words)",
    criteria: [
      "The draft hits every beat in the outline.",
      "The protagonist's voice is consistent with chapters 1–6.",
      "The chapter ends on the planned hook.",
    ],
    evidence: "the draft — the review pass is an editorial read.",
  },
  {
    key: "analytics",
    label: "Data analytics",
    story: "Quantify churn drivers among users who signed up in Q3",
    criteria: [
      "Each hypothesis is tested using the method stated alongside it.",
      "Findings are ranked by effect size, strongest first.",
      "Every chart is readable on its own, without the write-up.",
    ],
    evidence: "the analysis document plus its charts.",
  },
  {
    key: "post-processing",
    label: "Data post-processing",
    story: "Clean and normalize the 2026 sensor archive",
    criteria: [
      "Every field is typed and documented.",
      "Anomalous rows are flagged for a human look — never silently dropped.",
      "Row counts reconcile before and after the cleaning.",
    ],
    evidence: "the cleaned dataset plus the reconciliation table.",
  },
  {
    key: "batch",
    label: "Batch content operations",
    story: "Write product descriptions for the 40 spring SKUs",
    criteria: [
      "All 40 descriptions are present — none blank, none duplicated.",
      "Each description is under 60 words.",
      "The tone matches the brand sheet.",
    ],
    evidence: "the completed sheet, spot-checked by the review pass.",
  },
];

function SectionTitle({ id, children }: { id: string; children: React.ReactNode }) {
  return (
    <h2
      id={id}
      data-testid={id}
      className="text-xs font-semibold uppercase tracking-wide text-muted-foreground"
    >
      {children}
    </h2>
  );
}

/**
 * BoardHelpDrawer — "How the sprint board works" (owner ruling
 * 2026-10-30): a right-side static-prose drawer explaining the board
 * and its recommended workflow to someone who has never used a
 * kanban-with-agents. Two entry points open it: the toolbar's
 * HelpCircle affordance and the empty-board first-run state. The copy
 * deliberately uses the story/Sprint-Board vocabulary (the
 * task→story rename lane — it must never say "task board") and names
 * artifacts, never tooling (infra-neutral).
 */
export function BoardHelpDrawer({ open, onOpenChange }: BoardHelpDrawerProps) {
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 bg-black/40 z-50" />
        <Dialog.Content
          className="fixed right-0 top-0 z-50 flex h-full w-[26rem] max-w-[90vw] flex-col overflow-y-auto border-l border-border bg-background shadow-xl"
          aria-label="How the sprint board works"
          data-testid="board-help-drawer"
        >
          <div className="flex items-start justify-between gap-2 border-b border-border px-4 py-3">
            <div className="flex flex-col gap-0.5">
              <Dialog.Title className="text-sm font-semibold" data-testid="board-help-title">
                How the sprint board works
              </Dialog.Title>
              <Dialog.Description className="text-[10px] text-muted-foreground">
                Stories, the recommended workflow, and worked examples — code, research,
                writing, analytics, and more.
              </Dialog.Description>
            </div>
            <Dialog.Close asChild>
              <Button variant="ghost" size="sm" aria-label="Close" data-testid="board-help-close">
                <X className="h-4 w-4" />
              </Button>
            </Dialog.Close>
          </div>

          <div className="flex flex-1 flex-col gap-5 px-4 py-4 text-xs leading-relaxed" data-testid="board-help-body">
            {/* §1 — What it is */}
            <section className="flex flex-col gap-1.5" aria-labelledby="board-help-heading-what">
              <SectionTitle id="board-help-heading-what">What it is</SectionTitle>
              <p>
                The sprint board organizes work into <strong>stories</strong> — self-contained
                units of work, each with a description and acceptance criteria that define what
                done means. Every story flows through the same four columns:{" "}
                <strong>Backlog → In Progress → In Review → Done</strong>. Workers — agent
                sessions — pick up ready stories on their own, do the work, and hand the result
                to review. Nothing reaches Done until a review pass has checked every
                acceptance criterion.
              </p>
            </section>

            {/* §2 — The recommended workflow */}
            <section className="flex flex-col gap-1.5" aria-labelledby="board-help-heading-workflow">
              <SectionTitle id="board-help-heading-workflow">The recommended workflow</SectionTitle>
              <ol className="flex list-decimal flex-col gap-1.5 pl-4">
                {WORKFLOW_STEPS.map((step, i) => (
                  <li key={i} data-testid={`board-help-step-${i + 1}`}>
                    {step}
                  </li>
                ))}
              </ol>
            </section>

            {/* §3 — Writing good acceptance criteria */}
            <section className="flex flex-col gap-1.5" aria-labelledby="board-help-heading-criteria">
              <SectionTitle id="board-help-heading-criteria">
                Writing good acceptance criteria
              </SectionTitle>
              <p>
                “Improve performance” cannot be checked — no reviewer can verify a vibe. “The
                p95 query time is under 200ms, shown in the benchmark output” can be: run it,
                read the number, tick the box. Criteria are the contract between you, the
                worker, and the reviewer — the worker knows what to aim at, and the reviewer
                knows what to check before signing off.
              </p>
            </section>

            {/* §4 — Worked examples */}
            <section className="flex flex-col gap-2" aria-labelledby="board-help-heading-examples">
              <SectionTitle id="board-help-heading-examples">Worked examples</SectionTitle>
              <p>
                The same shape fits very different kinds of work. Each card below is one story:
                a title, the criteria a reviewer checks, and the evidence that proves it.
              </p>
              {EXAMPLES.map((ex) => (
                <div
                  key={ex.key}
                  className="flex flex-col gap-1 rounded-md border border-border bg-muted/30 px-2.5 py-2"
                  data-testid={`board-help-example-${ex.key}`}
                >
                  <span className="text-[11px] font-semibold">{ex.label}</span>
                  <span className="rounded bg-background px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
                    Story: “{ex.story}”
                  </span>
                  <ul className="ml-3 list-disc">
                    {ex.criteria.map((c, i) => (
                      <li key={i}>{c}</li>
                    ))}
                  </ul>
                  <span className="text-[10px] text-muted-foreground">
                    Evidence: {ex.evidence}
                  </span>
                </div>
              ))}
            </section>

            {/* §5 — When not to use the board */}
            <section className="flex flex-col gap-1.5" aria-labelledby="board-help-heading-when-not">
              <SectionTitle id="board-help-heading-when-not">
                When not to use the board
              </SectionTitle>
              <p>
                Not everything belongs here. A single quick question, a one-off lookup, or a
                live back-and-forth is faster as a direct conversation in a workspace session.
                The board pays off when a piece of work has a real definition of done —
                criteria someone else can check — and benefits from an independent review pass
                before you rely on the result. If a story can't say what done looks like, it
                isn't a story yet; ask it in chat first.
              </p>
            </section>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
