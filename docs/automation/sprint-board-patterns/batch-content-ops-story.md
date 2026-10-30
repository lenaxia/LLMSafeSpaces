# Batch content operations — a story whose count closes
<!-- pattern: slug=batch-content-ops-story; doc=board; use-case=batch-content-ops; nodes=none; wiring=none -->
Description: How to write a batch content story where the deliverable is a complete sheet — every row filled, per-row constraints checked mechanically, and the tone verified against the brand sheet.

A batch is done when the count closes: every row present, none blank, none
duplicated, per-row constraints verified by a check a reviewer can re-run,
and the voice matched to the stated reference.

## The worked story — "Write product descriptions for the 40 spring SKUs"

`story-skus.json`:

```json kind=board-payload op=create_task
{
  "title": "Write product descriptions for the 40 spring SKUs",
  "description": "## Input\n\n`content/spring-2027-skus.csv` — 40 rows, one per SKU, columns: sku, name, category, features (semicolon-separated), photo_note.\n\n## Reference\n\n`content/brand-sheet.md` — voice, reading level, banned phrases, the two-sentence shape (first sentence what it is, second sentence who it is for).\n\n## Deliverable\n\n`content/spring-2027-descriptions.csv` — the same 40 rows plus a `description` column. Write every row; do not defer 'hard' SKUs to later — if a SKU genuinely lacks material, write the best two sentences its features column supports.\n\n## Checks before handoff\n\nRe-run your own constraints on the finished sheet: 40 rows, none blank, none duplicated (exact-match dupes AND near-identical dupes), every description ≤ 60 words, none containing a banned phrase from the brand sheet. Fix what the checks catch, then hand off.",
  "tags": ["content"],
  "acceptanceCriteria": [
    {"text": "The output sheet has exactly 40 description rows, one per input SKU — none blank, none duplicated (the reviewer re-runs the dup check on the column)."},
    {"text": "Every description is ≤ 60 words (the reviewer spot-counts five rows including the longest)."},
    {"text": "The tone check passes: each description follows the two-sentence shape and contains no banned phrase from the brand sheet (the reviewer reads ten rows against it)."}
  ]
}
```

Validate with `board_validate_file` (op `create_task`), then submit with
`board_create_task` `{epicId, bodyFile}`.

## Why each criterion holds

- Criterion 1 closes the count — a mechanical check (row count, blank
  scan, duplicate scan) the reviewer re-runs rather than trusts.
- Criterion 2 is a number with a sampling procedure (five rows including
  the longest — the reviewer is told HOW to check, not just what).
- Criterion 3 samples the voice against the named reference — ten rows is
  a concrete, bounded editorial check.

## Evidence shape

A batch content story's evidence is **the completed sheet** — its path/URL.
The worker attaches the sheet's path/URL as `evidenceUrl` when moving the
story to `in_review`; the review pass is the re-run of the mechanical checks
plus the bounded spot-read, then the verdict.
