# Shape: the epic and plan — one atomic import with a dependency edge
<!-- pattern: slug=shape-epic-plan; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: The multi-story shape — board_import_plan materializes an epic plus its stories and their dependency edges from one file, all-or-nothing, with a dry-run approval moment before anything lands.

Reach for this shape when the work is a small campaign: several stories, one
outcome, real prerequisites between them. The plan IS the ceremony — one
document, validated and imported atomically, shown to the user before it
writes.

## The worked plan — a two-stage data deliverable

The outcome: a cleaned quarterly dataset feeding a churn report built ON the
cleaned data. The report story DEPENDS on the cleaning story — an edge, not
a hope. `plan-q3.json`:

```json kind=board-payload op=import_plan
{
  "epic": {
    "title": "Q3 churn picture — clean data, then report",
    "outcome": "A churn report the team can cite, built on a cleaned Q3 dataset whose row arithmetic reconciles."
  },
  "tasks": [
    {
      "key": "clean",
      "title": "Clean the Q3 signup + events extracts",
      "description": "## Input\n\n`data/raw/q3/` — the two extracts and their field notes.\n\n## Output\n\n`data/clean/q3.parquet` + `data/clean/q3-reconciliation.md` (typed fields documented; anomalies flagged, never dropped; rows in = rows out + accounted).\n\nThe downstream report story consumes this output — the reconciliation doc IS its data dictionary.",
      "tags": ["data"],
      "acceptanceCriteria": [
        {"text": "Every output field typed and documented in the reconciliation doc."},
        {"text": "Row arithmetic shown and re-addable: rows in = rows out + accounted (duplicates listed per cause)."},
        {"text": "Flag columns present with per-flag counts in the reconciliation table."}
      ]
    },
    {
      "key": "report",
      "title": "Q3 churn report on the cleaned dataset",
      "description": "## Depends on\n\nThe `clean` story's output (`data/clean/q3.parquet` + its reconciliation doc as the data dictionary) — read the reconciliation doc before touching the data.\n\n## Deliverable\n\n`reports/q3-churn.md`: churn definition stated, each hypothesis tested with its method named, findings ranked by effect size strongest-first, charts as standalone files in `reports/q3-churn/`.",
      "tags": ["analytics"],
      "acceptanceCriteria": [
        {"text": "Churn definition stated in the report and consistent with the cleaned dataset's flags."},
        {"text": "Each hypothesis: result + named method; contradicted hypotheses reported, not dropped."},
        {"text": "Findings ranked strongest-first as an explicit list; any two charts readable cold."}
      ],
      "dependsOn": ["clean"]
    }
  ]
}
```

## Import it — dry-run FIRST

```sh kind=shell
board_validate_file {op: "import_plan", file: "plan-q3.json"}   # shape checks
board_import_plan {projectId: "prj-…", planFile: "plan-q3.json", dryRun: true}
```

The dry run renders the graph (both stories, the `clean → report` edge),
warnings, and the initial ready set (`clean` — `report` is blocked). SHOW
the render to the user — it is the plan-approval moment; write only after
approval:

```sh kind=shell
board_import_plan {projectId: "prj-…", planFile: "plan-q3.json"}
```

All-or-nothing: both stories plus the edge land in one write (the planFile
is consumed). The edge means the engine will not feed `report` to a worker
until `clean` is done — the prerequisite is structural, not a charter
promise.

## Drive it

- `board_epic_activate` on the minted epic — an epic is draft until
  activated; only then do ready stories feed to workers.
- Each story then lives the happy path (`lifecycle-happy-path.md`)
  independently: the `clean` worker hands off to review; the reviewer's
  APPROVED verdict (not the worker's push) unblocks `report`.
- Edits after import: `board_update_task` per story, or `board_add_dependency`
  / `board_remove_dependency` for the edges — the plan file is the birth
  record, not a living document.

## Sizing the epic

Outcome-first: the `epic.outcome` names the result aimed at, the stories
name its checkable parts. Two-to-five stories is the sweet spot — an epic
needing ten is usually two epics, and a one-story epic is
`shape-single-story.md` wearing ceremony.
