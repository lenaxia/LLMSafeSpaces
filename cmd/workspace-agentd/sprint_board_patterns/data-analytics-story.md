# Data analytics — a story whose findings are ranked and reproducible
<!-- pattern: slug=data-analytics-story; doc=board; use-case=data-analytics; nodes=none; wiring=none -->
Description: How to write a data-analytics story where each hypothesis is tested by a stated method, findings rank by effect size, and every chart stands alone.

An analysis is done when a reviewer can re-walk the path from data to
conclusion: each hypothesis paired with the method that tests it, findings
ordered by how much they matter, charts that survive being read alone.

## The worked story — "Quantify churn drivers among users who signed up in Q3"

`story-churn.json`:

```json kind=board-payload op=create_task
{
  "title": "Quantify churn drivers among Q3-signup users",
  "description": "## Data\n\n`data/signups-q3.parquet` (one row per signup, fields documented in `data/signups-q3-dictionary.md`) plus `data/events-q3.parquet` (activity events). Both are in the workspace; do not fetch anything external.\n\n## Hypotheses to test (add others the data suggests)\n\nH1: users who complete onboarding in the first session churn less.\nH2: users with zero activity in their second week churn more.\nH3: signup source (organic / referral / campaign) changes churn rate.\n\n## Method rules\n\n- Churn = no activity in the 30 days before quarter end (state this in the report; if you must redefine it, state the definition AND why).\n- Each hypothesis gets the method stated alongside its result (cohort comparison, rate ratio — whatever you use, name it).\n- Rank findings by effect size, strongest first — not by hypothesis order.\n\n## Deliverable\n\n`reports/churn-drivers-q3.md` plus `reports/churn-drivers-q3/` holding each chart as its own file (PNG). Every chart carries a title, axis labels, and units — readable without the write-up.",
  "tags": ["analytics"],
  "acceptanceCriteria": [
    {"text": "Each hypothesis (H1–H3) has a result with its testing method named alongside it, and any hypothesis the data contradicted is reported as contradicted, not dropped."},
    {"text": "Findings are ranked by effect size strongest-first, and the ranking is explicit in the report (a ranked list, not prose)."},
    {"text": "Every chart file is self-contained — title, labeled axes, units — verified by opening any two charts without the report text."}
  ]
}
```

Validate with `board_validate_file` (op `create_task`), then submit with
`board_create_task` `{epicId, bodyFile}`.

## Why each criterion holds

- Criterion 1 forces method-hypothesis pairing and forbids silent drops —
  the reviewer reads each hypothesis block and checks both halves exist,
  including the contradicted ones.
- Criterion 2 makes the ranking a structural feature the reviewer can see —
  a list whose order is the effect sizes, not narrative convenience.
- Criterion 3 is a spot-check with a clear pass condition: open two charts,
  read them cold.

## Evidence shape

A data-analytics story's evidence is **the analysis document plus its
charts** — the report path/URL, with the chart directory beside it. The
worker attaches the report's path/URL as `evidenceUrl` when moving the story
to `in_review`; the reviewer walks the criteria against the artifacts
(reading hypothesis blocks, the ranked list, and at least two charts cold)
and records the verdict.
