# 1236 — "How the sprint board works": the board help drawer (owner ruling 2026-10-30, tsk-glassy-shovel-feels)

Branch: `board-help-drawer` (checkout `/workspace/LLMSafeSpaces`, off `origin/main` @ `acb8d3981`).

## 1. The owner ruling (verbatim intent, 2026-10-30)

"Add a help/info/learn more button which opens a drawer explaining to users how to use the board, and what the recommended workflow is. Include specific examples such as if writing code, doing research, writing a story, doing data analytics, data post-processing, etc. — a good list focused on how to use the board. Don't make assumptions about what kind of infra users are using."

## 2. What landed

Frontend-only. One new component carries ALL the drawer text:

- **`frontend/src/components/board/BoardHelpDrawer.tsx`** — the "How the sprint board works" right-side drawer. The app's existing Radix side-drawer pattern (`TaskDrawer`'s exact chrome: `fixed right-0 top-0 h-full w-[26rem] max-w-[90vw] border-l shadow-xl`, Overlay, `Dialog.Title` + `Dialog.Description` + Close button; Escape closes via the Radix dialog contract). Static prose in sections — no CMS, no API, no data dependency: the drawer opens identically on an empty board and a full one.
- **`frontend/src/pages/BoardPage.tsx`** — two entry points, one `helpOpen` state:
  1. the toolbar's HelpCircle icon button (icon-only, so the accessible name carries the full intent: `aria-label="Learn more: how the sprint board works"`, `data-testid="board-help-button"`, placed after *New project* in the toolbar's right-side group);
  2. the empty-board first-run state (`data-testid="board-empty"`, "No boards yet.") gains a ghost button `data-testid="board-empty-learn-more"` beside *Create your first project* — the first-run moment is when a user most needs the explainer, and the toolbar affordance is present there too.

## 3. The copy (written for someone who has never seen a kanban-with-agents)

Five sections, each with a pinned heading:

1. **What it is** (4 sentences) — stories as self-contained units of work with acceptance criteria defining done; the Backlog → In Progress → In Review → Done flow; workers (agent sessions) pick stories up on their own; nothing reaches Done until a review pass has checked every criterion.
2. **The recommended workflow** (6 numbered steps) — project for the effort + an epic describing the OUTCOME; one-sitting stories with CHECKable criteria (a file exists, a test passes, a number is computed, a document answers the question); descriptions written for a reader with NO memory of the conversation; let stories run (never assign/chase); read the audit timeline for what happened; Done = every criterion verified with evidence.
3. **Writing good acceptance criteria** — "improve performance" (uncheckable) vs "the p95 query time is under 200ms, shown in the benchmark output" (checkable); criteria as the contract between you, the worker, and the reviewer.
4. **Worked examples** (6 cards, each: use-case label + story title + 2–3 criteria + what the evidence looks like):
   - **Writing code** — "Add CSV export to the report page": a file a spreadsheet application opens cleanly; empty-table/unicode edge cases covered by tests; the suite passes. Evidence: the diff + the passing run of the suite.
   - **Research** — "Answer: which of our three markets grew fastest last quarter, and why": specific numbers cited; every claim names its source; one-paragraph act-on-able answer up top. Evidence: the report itself.
   - **Creative writing** — "Draft chapter 7 (target ~2,500 words)": hits the outline beats; the protagonist's voice matches chapters 1–6; ends on the planned hook. Evidence: the draft — the review pass is an editorial read.
   - **Data analytics** — "Quantify churn drivers among users who signed up in Q3": each hypothesis tested with the stated method; findings ranked by effect size; charts readable without the write-up. Evidence: the analysis document + its charts.
   - **Data post-processing** — "Clean and normalize the 2026 sensor archive": every field typed and documented; anomalies flagged, never silently dropped; row counts reconcile before/after. Evidence: the cleaned dataset + the reconciliation table.
   - **Batch content operations** (the sixth — the ruling's "etc." earned a concrete member) — "Write product descriptions for the 40 spring SKUs": all 40 present; each under 60 words; tone matches the brand sheet. Evidence: the completed sheet, spot-checked by the review pass.
5. **When not to use the board** — quick questions/one-off lookups are faster as a direct conversation in a workspace session; the board pays off when work has a definition of done and benefits from an independent review pass.

### 3.1 Infra-neutrality (binding rule, held + pinned)

Every example names ARTIFACTS and VERIFICATION OUTCOMES — "the suite passes", not "GitHub Actions passes"; "the review pass", not a named reviewer; "a spreadsheet application", never a vendor. The board reads usable whatever stack the workspace runs on. Pinned by a test scanning the whole rendered drawer against a vendor blocklist (github, gitlab, actions, jenkins, circleci, jira, slack, notion, excel, google, openai, copilot) — the list pins the obvious candidates; the rule itself is broader.

### 3.2 Vocabulary (the Sprint Board/story rename coordination)

The drawer speaks in *stories* / *the sprint board* — never "task board" — so it lands in either order with the rename lane. Pinned by a case-insensitive whole-drawer test (`/task board/i` absent, `/stories/` present). **Coordination note for the rename lane**: your sweep does not need to touch `BoardHelpDrawer.tsx` (already renamed); the surrounding BoardPage chrome ("New task", "Board" h1) remains yours.

## 4. Tests

`frontend/src/pages/BoardPage.helpdrawer.test.tsx` (7, all green through the real BoardPage mount):

1. the toolbar affordance renders with the exact accessible label;
2. the drawer opens from the toolbar and closes via Escape (board untouched underneath);
3. all five section headings + all six example cards present (headings pinned by TEXT, not just node);
4. the empty-board first-run entry point renders (same aria label), opens the same drawer, and the toolbar affordance is present on the empty board too;
5. the Close button path;
6. the "task board" vocabulary pin;
7. the infra-neutral vendor scan.

## 5. Gates

- `npx vitest run` — 225 files / 2583 tests green (was 2576 + my 7).
- `npm run typecheck` — clean. `npm run lint` — 0 errors (7 warnings pre-existing on main: 6 generated-abi disable directives + BoardPage's `rememberEpic` exhaustive-deps).
- `make fmt-check` — clean (frontend-only change).
- Radix `Missing Description` console warning addressed with a real `Dialog.Description` (subtitle line) — the drawer's aria wiring is complete, not suppressed.

## 6. Process

Conventional commits; this worklog; CHANGELOG `[Unreleased]` entry; COORDINATE.md active-claim row (files claimed: the new component + test + BoardPage wiring + bookkeeping). AI reviewer until APPROVED; orchestrator merges; never self-merge.
