# Shape: the one-sitting single story
<!-- pattern: slug=shape-single-story; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: The smallest useful board shape — one story, sized to finish in one sitting, created flat and driven to done without a plan or dependencies.

Reach for this shape when the work is one deliverable, one sitting, no
structural prerequisites — the vast majority of stories. If you are typing
"and then also", you want `shape-epic-plan.md` instead.

## Size the story first

One sitting means: a single worker session with a fresh context can take it
from claim to handoff. The test is the charter — if the description needs
more than one `## Deliverable`, or the criteria span more than one artifact,
split. A story that is really three stories starves review: the reviewer can
verify one artifact cleanly, and muddles verifying three.

## The flat create (no plan needed)

A single story rides `board_create_task`'s flat bodyFile — no
`board_import_plan` ceremony. `story-single.json`:

```json kind=board-payload op=create_task
{
  "title": "Add a --dry-run flag to the import script",
  "description": "## Background\n\n`scripts/import.py` writes rows on first pass; a typo in the column map means re-running the cleanup afterwards. A dry-run mode that parses and validates without writing would have caught yesterday's bad map before any row landed.\n\n## Scope\n\n- `--dry-run` parses the input, validates the column map, prints the row count it WOULD write per table, writes nothing.\n- Exit code 0 with the summary on stdout; a validation problem exits non-zero with the offending column named.\n\n## Deliverable\n\nThe flag in `scripts/import.py`, plus tests covering the writes-nothing path and the bad-map path.",
  "tags": ["tooling"],
  "acceptanceCriteria": [
    {"text": "Running with --dry-run writes zero rows (the test asserts the target tables are unchanged)."},
    {"text": "A bad column map under --dry-run exits non-zero and names the offending column in stderr."},
    {"text": "The suite passes with the two new tests included."}
  ]
}
```

`board_validate_file` (op `create_task`), then `board_create_task`
`{epicId, bodyFile}` — done. One story, one artifact, one review.

## Drive it

The lifecycle is `lifecycle-happy-path.md` verbatim: claim → work → push →
`board_move_stage {toStage: "in_review", evidenceUrl: the pull request URL}`
→ verdict. Process rules: `process-discipline.md`.

## When this shape is wrong

- The deliverable needs a prerequisite first (data must exist before the
  report) — that is a dependency edge, i.e. a plan
  (`shape-epic-plan.md`).
- The work is genuinely N parallel similar units (40 descriptions) — still
  ONE story (`batch-content-ops-story.md`): the batch is the artifact.
- The work is exploratory with no checkable done — not a story at all; ask
  it in a session first.
