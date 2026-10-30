# Research — a story whose answer is traceable
<!-- pattern: slug=research-story; doc=board; use-case=research; nodes=none; wiring=none -->
Description: How to write a research story whose deliverable is a report a reviewer can audit — numbers cited, sources named, an actable answer up top.

Research done is not "I read around"; it is a report that answers the
question and survives a trace check. The criteria below force traceability:
each claim names its source, and the ranking rests on numbers a reviewer can
re-find.

## The worked story — "Which of our three markets grew fastest last quarter, and why"

`story-research.json`:

```json kind=board-payload op=create_task
{
  "title": "Answer: which of our three markets grew fastest last quarter, and why",
  "description": "## The question\n\nRank our three markets (north, south, coastal — the labels used in the quarterly workbook in the workspace root) by revenue growth last quarter, and explain the top market's lead in two or three drivers.\n\n## Method\n\nUse the quarterly workbook's per-market sheets as the primary numbers; where a figure is missing, say so explicitly rather than estimating silently. Growth is quarter-over-quarter revenue change in percent.\n\n## Deliverable\n\n`reports/market-growth-last-quarter.md` in the workspace, structured as:\n\n1. A one-paragraph answer up top (the ranking plus the why, readable in thirty seconds).\n2. The numbers table (per-market revenue both quarters, growth percent).\n3. The drivers section — each driver tied to the specific numbers that evidence it.\n\n## Ground rules\n\nEvery claim cites its source (file and sheet, or the exact conversation/notes it came from). If the data cannot support a driver, say that — an honest 'the data does not show why' beats an invented driver.",
  "tags": ["research"],
  "acceptanceCriteria": [
    {"text": "The report's one-paragraph answer names the fastest-growing market and at least two drivers, with the growth percentages inline."},
    {"text": "Every claim in the drivers section cites its source (a file-and-sheet reference or a named note) — a reviewer can spot-check three at random and land on the cited numbers."},
    {"text": "The numbers table shows per-market revenue for both quarters and the computed growth percent, and the ranking matches the table's order."}
  ]
}
```

Validate with `board_validate_file` (op `create_task`), then submit with
`board_create_task` `{epicId, bodyFile}`.

## Why each criterion holds

- Criterion 1 checks the deliverable does its job: a busy reader gets the
  answer without excavating it — the reviewer reads one paragraph.
- Criterion 2 is the audit: trace-checking three random claims is a concrete
  move a reviewer can perform, and it either lands on the cited numbers or
  it does not.
- Criterion 3 checks internal consistency — the ranking cannot contradict
  the table without the reviewer seeing it.

## Evidence shape

A research story's evidence is **the report document itself** — a workspace
file or link the reviewer opens. No diff, no suite: the review pass is a
read-and-trace audit. The worker attaches the report's path/URL as
`evidenceUrl` when moving the story to `in_review`; the reviewer opens it,
runs the trace check, records the verdict.
