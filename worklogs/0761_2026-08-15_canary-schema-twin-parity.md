# Worklog: Canary schema-version twin parity

**Date:** 2026-08-15
**Session:** PR #869 — Python/TS canary schema constant drift + parity guard
**Status:** Complete

---

## Objective

Close the schema-version drift class: #856 bumped `settings.SchemaVersion` to 11 and updated only the Go canary twin; the Python and TS twins kept 10. The Python section had never executed in CI (bootstrap bug fixed in #861), so the drift surfaced only when #647's post-merge canary run booted the section.

---

## Work Completed

1. `sdks/canary/python/scenarios/s_user_settings.py` — `EXPECTED_SCHEMA_VERSION` 10 → 11.
2. `sdks/canary/typescript/scenarios/s-user-settings.ts` — same, 10 → 11.
3. `sdks/canary/TESTPLAN.md:261` — stale "currently `1`" → "currently `11`, `settings.SchemaVersion`".
4. `pkg/repolint/canary_twin_parity_test.go` (new) — `TestCanary_SchemaVersion_TwinParity`: each of the three twins must equal the **authority constant** (`settings.SchemaVersion`, `pkg/settings/schema.go:45`), not merely each other. Anchoring to the authority closes the mode-B gap (a bump that updates none of the twins would pass twin-only parity); it runs in the blocking `Test (-short)` CI job (worklog 0596 precedent).

---

## Key Decisions

1. Authority-anchored guard over twin-only parity — the disease (twin-vs-authority divergence) rather than the symptom (twin divergence).
2. Lint home `pkg/repolint` — it already owns repo-wide consistency checks and runs in blocking CI.

---

## Tests Run

- `go test -run TwinParity ./pkg/repolint/` — pass; verified it fails when any twin disagrees with the authority (mutation-tested locally during development).
- `go test ./pkg/repolint/` — full package pass.
- `py_compile` / `tsc`-via-CI for the two twin edits.

---

## Blockers

None.

---

## Files Modified

- sdks/canary/python/scenarios/s_user_settings.py
- sdks/canary/typescript/scenarios/s-user-settings.ts
- sdks/canary/TESTPLAN.md
- pkg/repolint/canary_twin_parity_test.go (new)
- worklogs/0761_2026-08-15_canary-schema-twin-parity.md (this file)
