# Process discipline — the standing rules every charter references
<!-- pattern: slug=process-discipline; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: The process rules every story charter hand-copies today, written once — review, merge, push, evidence, and format discipline — so charters can reference this file and stay short.

Vocabulary, once: the board speaks in **stories** — a story (the wire/API term is 'task') is one
self-contained unit of work on the Sprint Board; the tool names
(`board_create_task`, `board_move_stage`, …) keep the wire term.
Everything below applies to every story regardless of use case.

## Review and merge

- **AI reviewer until APPROVED.** A story is not done when the worker
  finishes; it is done when the review persona records an APPROVED verdict
  with every criterion met. Expect bounce loops; iterate on findings.
- **Never self-merge.** The worker never merges its own pull request — the
  ORCHESTRATOR merges after APPROVED. Pushing and handing off is the end of
  the worker's merge involvement.

## Push and turn discipline

- **`make fmt-check` before every push.** A red format check never leaves
  your machine.
- **Push, verify the run started, END the turn.** After pushing: confirm the
  verification run actually started (the check run / suite run appears on
  the branch), then END your turn with a report. **No sleep-polling** — do
  not sit in the turn waiting on the run or the review; the board wakes the
  right session when there is something to do.
- One report per turn: what was done, the artifact URL, what is next (if
  anything). The report is the audit humans read.

## Evidence URLs, per work type

Every `board_move_stage {toStage: "in_review"}` carries an `evidenceUrl` —
the artifact a reviewer verifies against. The shape by work type:

| Work type | evidenceUrl carries | Reviewer verifies by |
| --- | --- | --- |
| Writing code | the pull request URL | the diff + the suite's passing run on the branch |
| Research | the report document's path/URL | reading + trace-checking cited sources |
| Creative writing | the draft document's path/URL | editorial read against the craft criteria |
| Data analytics | the analysis document's path/URL (charts beside it) | re-walking method→finding; reading charts cold |
| Data post-processing | the reconciliation doc's path/URL (dataset beside it) | the row arithmetic + schema-vs-doc |
| Batch content operations | the completed sheet's path/URL | re-running the mechanical checks + bounded spot-read |

Infra-neutral by construction: evidence names ARTIFACTS and VERIFICATION
OUTCOMES — "the suite passes", "the diff", "the report" — never tooling or
vendors. Whatever stack the workspace runs on, these shapes hold.

## Claim discipline (workers)

- The claim token in your charter envelope authorizes YOUR in-flight writes
  on THIS story only.
- A BOARD LEASE NUDGE gets ONE `board_heartbeat` immediately, then continue
  working. No periodic heartbeat — liveness is platform-observed.
- A 409 `claim_token` anywhere means the claim is gone: STOP, `board_read`
  the story, follow what you find (see `lifecycle-salvage.md` if a prior
  attempt's work exists).
- Throttled by the provider mid-work? `board_report_throttle` once, then
  stop — the story re-queues without attempt penalty.

## For charter writers

Reference this file instead of copying its rules: a charter needs only the
work-specific parts (background, scope, where, deliverable) plus criteria.
Each use case — writing code, research, creative writing, data analytics,
data post-processing, batch content operations — has a worked charter in
this directory; grep '^Description:' for the catalog.

The canonical example payloads live in the sibling files; the shape
patterns (single story, epic+plan, reviewer-checkable) are worked in
`shape-single-story.md`, `shape-epic-plan.md`, and
`shape-reviewer-checkable.md`.
