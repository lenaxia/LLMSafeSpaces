# Data post-processing — a story whose cleaning reconciles
<!-- pattern: slug=data-post-processing-story; doc=board; use-case=data-post-processing; nodes=none; wiring=none -->
Description: How to write a data post-processing story where every field is typed and documented, anomalies are flagged rather than silently dropped, and row counts reconcile before and after.

A cleaning pass is done when it ACCOUNTS for every row: fields typed and
documented, anomalies flagged for a human look, and the reconciliation table
proving nothing vanished. Silent dropping is the failure mode the criteria
exist to prevent.

## The worked story — "Clean and normalize the 2026 sensor archive"

`story-sensors.json`:

```json kind=board-payload op=create_task
{
  "title": "Clean and normalize the 2026 sensor archive (12 monthly files)",
  "description": "## Input\n\n`data/raw/sensors-2026/sensors-2026-01.csv` … `-12.csv` — one row per sensor reading. Known dirt (from a first pass): timestamps in two formats (ISO and epoch-seconds mixed); temperatures in both °C and °F with no unit column; occasional blank sensor_id; duplicate rows from a re-transmit burst in March.\n\n## Output\n\n`data/clean/sensors-2026.parquet` plus `data/clean/sensors-2026-reconciliation.md`.\n\n## Cleaning rules\n\n- Timestamps: parse both formats, emit one ISO-8601 UTC column.\n- Temperature: normalize to °C in a column named `temp_c`; where the unit is ambiguous for a whole FILE (all values plausibly one unit), decide by file using the sensor's stated range in `data/raw/sensors-2026/README` and record the decision in the reconciliation notes.\n- Blank sensor_id rows: FLAG (column `flag_blank_id`), never drop.\n- Duplicates: drop exact duplicates (all columns equal), count them per month in the reconciliation table.\n- Anomalous readings (temp outside the sensor's stated range): keep the row, set `flag_out_of_range`.\n\n## Documentation\n\nThe output's field list and every flag's meaning go in the reconciliation doc — a reader who has never seen the raw files must understand the clean ones from it alone.",
  "tags": ["data", "cleaning"],
  "acceptanceCriteria": [
    {"text": "Every output field is typed and documented in the reconciliation doc — field name, type, unit where applicable, and each flag column's meaning."},
    {"text": "No row silently disappears: input rows = output rows + explicitly-accounted rows (duplicates dropped, listed per month) — the reconciliation table shows the arithmetic."},
    {"text": "Ambiguous and anomalous rows are flagged, not dropped: the parquet carries flag columns, and per-flag counts appear in the reconciliation table."}
  ]
}
```

Validate with `board_validate_file` (op `create_task`), then submit with
`board_create_task` `{epicId, bodyFile}`.

## Why each criterion holds

- Criterion 1 checks the documentation deliverable by enumeration — the
  reviewer walks field list against doc.
- Criterion 2 is the arithmetic of trust: rows in, rows out, and every
  difference named. The reviewer re-adds the table's columns.
- Criterion 3 pins the no-silent-drop rule to artifacts: flag columns exist
  and their counts are in the table.

## Evidence shape

A data post-processing story's evidence is **the cleaned dataset plus the
reconciliation table** — the parquet's path and the reconciliation doc's
path/URL together. The worker attaches the reconciliation doc's path/URL as
`evidenceUrl` when moving the story to `in_review`; the reviewer checks the
arithmetic, spot-opens the dataset's schema against the doc, and records the
verdict.
