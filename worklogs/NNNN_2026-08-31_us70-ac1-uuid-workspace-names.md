# Worklog: US-70.0 AC-1 pool failure — harness defect (UUID workspace names + workspaces metadata seeding)

**Date:** 2026-08-31
**Session:** Root-cause + fix for the US-70.0 pool run's AC-1 failure (`bind_env` died at the first `PUT /workspaces/:id/env`). Operator root-caused the resolution path read-only; this session validated it end-to-end and fixed the harness. Product NOT implicated.
**Status:** Complete — pin tests green; pool re-dispatch after merge.

---

## Objective

Make `local/us-70-secret-delivery-e2e.sh` (and the faults suite, which shares the lib) actually able to bind/suspend/activate workspaces through the API.

---

## Work Completed

- `local/lib/us70-common.sh`:
  - `WS_BASE` default is now a **valid UUID** and `ws_id()` stamps a 4-digit suffix onto the base's first 32 chars (`e2e5d000-0000-4000-8000-000000000001`). Production mints `workspaceID = uuid.New().String()` and the **CR name IS that UUID** (`workspace_service.go:409` → `ObjectMeta.Name: workspaceID`); every API workspace op resolves `WHERE workspaces.id = $1` against a **uuid column** (`database.go:482`), so a non-UUID CR name can never resolve — 22P02 (500), not merely a missing row.
  - `seed_workspace` now also seeds the `workspaces` metadata row (psql `INSERT ... ON CONFLICT (id) DO UPDATE`, test.sh precedent): the API's workspace routes resolve through PostgreSQL and ownership is established at API create — nothing back-fills a kubectl-applied CR (correctly so). The row is required in addition to the UUID name shape.
- `local/us-70-faults-e2e.sh`: `WS_BASE` default → UUID-shaped (`e2e5f000-…`); doc updated (names are 36-char UUIDs == `workspaces.id`, exactly fitting `secret_audit_log.workspace_id varchar(36)`).
- `local/us70_harness_script_test.go`: `TestUS70WorkspaceNamesAreUUIDs` — extracts `ws_id()` from the lib and executes it (s5 guard-extraction pattern; no environment deps), pinning UUID output shape for indexes 1/2/101/9999, plus presence pins for the metadata INSERT and the single-line `ws_id` definition.

## Key Decisions

1. **UUID names + metadata row, not API-created workspaces** — the gVisor legs need kubectl-applied CRs (`spec.runtimeClass` + the admin override annotation the webhook requires), which the API create path does not carry. Seeding both halves in the harness keeps the CR control and satisfies the DB resolution (test.sh precedent).
2. **Name-shape fix is primary** — seeding the row alone would NOT have fixed it: a non-UUID name fails the `id uuid` comparison regardless of rows. Both defects had to go.

## Assumptions (Rule 7 — stated and validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | The pool AC-1 failure is the resolution 404/500, not the product | Pool run 33433298163 log: `✗ bind_env SD_FIRST` 0.15s after the row header — the first API bind; middleware path quoted by the operator verified in code |
| A-2 | Production CR names are UUIDs == `workspaces.id` | `workspace_service.go:409` (`uuid.New().String()`) + `:1248` (`ObjectMeta.Name: workspaceID`) |
| A-3 | The nightly's red runs share this cause | e2e-nightly failing since ≥08-29; the us-70 rows as-wired could never bind — the exec-level Go suites carried the real #1164 validation |
| A-4 | `ws_id` suffix domain suffices | 4 digits → 9999 slots; the largest batch is AC-13's ~105 |

## Adversarial review (Rule 11)

- Could psql-seeding the row alone fix it? **No** (A-2) — name shape fails the uuid comparison first.
- Does the metadata row need more columns (org_id, agent state)? `GetWorkspace` LEFT JOINs `workspace_agent_state` (NULL fine); `org_id` nullable.
- Stale rows after `kc delete` reuse: `ON CONFLICT ... deleted_at = NULL` resets soft-delete state.
- Pin test robustness: extraction pattern means no kubectl/jq dependency (runs everywhere).

## Blockers

None. Pool re-dispatch after merge (then W8 #1184, then #1194 — operator Rev-5 priority).

---

## Tests Run

- `go test -timeout 60s ./local/` — ok (incl. the new UUID pin)
- `bash -n` ×3 scripts — clean; gofmt/goimports/vet — clean
- Cluster validation = the pool re-dispatch

---

## Next Steps

1. Merge → re-dispatch `us-70-delivery-pool.yml`; expect AC-1/F-rows to exercise the product path for real.
2. W8 #1184 (CHANGES_REQUESTED) → merge.
3. #1194 red suites (incl. the arm64 frontend build failure) → iterate to green.

---

## Files Modified

- `local/lib/us70-common.sh` (WS_BASE UUID default, ws_id, seed_workspace_metadata wiring)
- `local/us-70-faults-e2e.sh` (WS_BASE default + doc)
- `local/us70_harness_script_test.go` (TestUS70WorkspaceNamesAreUUIDs)

## Addendum (review round): executable resolution-contract pins

`api/internal/services/database/workspace_resolution_integration_test.go`
(integration tag, testharness PG): UUID name + seeded row → resolves with
ownership; non-UUID CR name → cannot resolve (the 22P02 failure mode,
documented); valid UUID without a row → nil/not-found (the 404 failure
mode). Both AC-1 failure modes are now pinned as executable truth, not
harness convention.
