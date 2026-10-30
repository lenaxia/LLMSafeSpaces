# Shape: reviewer-checkable — criteria written for the verifier
<!-- pattern: slug=shape-reviewer-checkable; doc=board; use-case=mechanics; nodes=none; wiring=none -->
Description: How to write criteria a reviewer can verify with evidence — the uncheckable rewritten checkable, the verification move named, and the evidence shape decided before work starts.

Criteria are the contract between the writer, the worker, and the reviewer
(the help drawer's §3 lesson, agent-shaped). The reviewer's whole job at
verdict time is: for each criterion, perform the check, read the outcome. A
criterion that names no check cannot be approved — only believed.

## The three rules

1. **Name the observable, not the adjective.** "The export is robust" →
   "The export produces a file a spreadsheet application opens cleanly,
   verified on the empty-table and unicode fixtures."
2. **Name the verification move.** The reviewer should not invent how to
   check — the criterion says: run this, open that, count these. "…shown in
   the suite run", "…the diff's file list shows exactly two files",
   "…spot-count five rows including the longest".
3. **Decide the evidence shape at write time.** If you cannot say what
   artifact proves the criterion, the story is not ready — neither worker
   nor reviewer can win. Evidence shapes per work type live in
   `process-discipline.md`.

## Before and after — the same intent, rewritten

Uncheckable charter fragments, each rewritten checkable:

| Uncheckable (a vibe) | Checkable (a contract) |
| --- | --- |
| "Make renewal secure" | "An expired refresh token returns 401 with error code `refresh_expired`, shown in the new test's output" |
| "The report should be useful" | "A one-paragraph answer up top names the ranking and two drivers with percentages inline" |
| "Chapter reads like the series" | "Close-third present tense on Mara throughout; the lockkeeper's dialogue keeps the chapter-3 ledgers-and-counts register" |
| "Clean the data properly" | "Rows in = rows out + accounted, shown in the reconciliation table; anomalies flagged, never dropped" |
| "Descriptions sound on-brand" | "Each description ≤ 60 words, two-sentence shape, no banned phrase from `content/brand-sheet.md` — ten-row spot-read" |

## The rewrite walk

Take "the description generator handles edge cases" through the rules:

1. Which edge cases? → empty features column; semicolons inside quoted
   feature text; a 120-character name.
2. What is the observable? → the generator returns a description for each
   (never an empty string), and the quoted-semicolon row parses with its
   features intact.
3. What is the verification move? → the three fixture rows exist in the
   test file; the suite run shows the three named tests passing.

Written: "The three fixture rows (empty features, quoted semicolons,
120-char name) each yield a non-empty description, with the quoted-semicolon
row's features parsed intact — the three named tests pass in the suite run."

## Worked checkable charter

The same export example, as the payload you would actually submit (each
criterion: observable + verification move + where the outcome is read):

```json kind=board-payload op=create_task
{
  "title": "Add CSV export to the report page",
  "description": "## Background\n\nThe report page renders in the browser, but the weekly review reads it from a file — today someone copies the table out by hand.\n\n## Scope\n\n- An export affordance on the report page writes the current table as a CSV file.\n- The file opens cleanly in a spreadsheet application.\n\n## Deliverable\n\nThe export on the report page plus its tests.",
  "tags": ["frontend"],
  "acceptanceCriteria": [
    {"text": "The export produces a file that opens cleanly in a spreadsheet application — verified on the empty-table and unicode-cell fixtures included in the test file."},
    {"text": "The suite run on the branch shows the two new export tests passing alongside the full suite."},
    {"text": "The diff touches only the export module and its test file — the pull request's file list shows exactly two files."}
  ]
}
```

Each criterion: observable + verification move + where the outcome is read.
The reviewer's verdict writes itself — that is the test of the shape.

## Smell test

Before submitting any create: read each criterion and ask "what TOOL CALL
or READ does the reviewer perform?" If the answer is "believe the worker",
rewrite. Two-to-three criteria per story is the sweet spot — ten criteria
means either five stories or a checklist nobody re-reads.
