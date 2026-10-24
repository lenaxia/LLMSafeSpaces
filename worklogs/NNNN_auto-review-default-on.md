# Lane — auto-review default-on (owner ruling 2026-10-24) (PR #TBD)

Date: 2026-10-24 · Lane: lane-autoreview (orchestrator novel-nutmeg-verifies) · Branch: fix/auto-review-default-on · Base: origin/main 922d725fb.

## Objective — the owner ruling (verbatim)

> "Please flip defaults for review to on."

Context (sub-orchestrator board test, epic "Fix npm publish leg", 2026-10-24): board AUTO-REVIEW (#1832 items 5+6, landed in #1840/#1854/#1857) is landed-but-dormant — effective `reviewColumn=auto` AND `autoAssignReviewer=true` is required to dispatch a reviewer, both code defaults are OFF (`manual`/`false`), and no manager configuration surface pre-configures them, so a freshly-created board project never auto-reviews. Mandate: validate-first on current origin/main, flip the defaults, keep explicit stored opt-outs authoritative (back-compat), minimal diff, no redesign (the claim-then-spawn reviewer design rec is out of scope).

## Validation on origin/main (922d725fb) — the exact dormancy chain

Searched issues/PRs (`autoAssignReviewer`, `reviewColumn`, `auto-review`): landing PRs #1840 (config storage+API, user-scope ruling, merge tiers code → user → project), #1857 (gated reviewer dispatch + verdict chain), #1854 (Settings UI). Every default-off site that keeps auto-review inactive for a freshly-created project:

| # | Site | Role |
|---|---|---|
| 1 | `pkg/board/config.go` `DefaultBoardConfig()` — `ReviewColumn: ReviewColumnManual` ("manual"), `AutoAssignReviewer: false` | THE code-default floor; every effective-config resolution starts here (`GetUserBoardConfig`, `GetProjectBoardConfig`, and `effectiveConfigForProject` → the gate) |
| 2 | `pkg/board/review_assign.go:73` gate — `eff.ReviewColumn != "auto" \|\| !eff.AutoAssignReviewer → return` | the AND-gate: requires BOTH keys; manual/false never fires |
| 3 | `frontend/src/components/settings/BoardConfigTab.tsx:82,92` — display fallbacks `g?.reviewColumn ?? "manual"`, `g?.autoAssignReviewer ? "on" : "off"` | pre-data render fallbacks (defensive; the loading early-return makes them near-unreachable) — display only, nothing stored |
| 4 | CRD / env / helm | NONE FOUND — grep across `controller/`, `helm/`, `cmd/`, `values*`: no `reviewColumn`/`autoAssignReviewer` surface; the feature is user-scoped config storage only |

Fresh project = no `board_config_defaults` row for the owner + no `board_project_config_overrides` row for the project → read-time merge == code defaults → manual/false → dormant. Confirmed on main by the RED run below (assertions on the effective row AND the dispatch both failed pre-flip).

Semantics check vs charter: charter said `reviewColumn=auto` AND `autoAssignReviewer=true` — code on main agrees exactly (the gate at review_assign.go:73). No divergence; followed the code.

## Change (sites flipped)

1. `pkg/board/config.go` `DefaultBoardConfig()`: `ReviewColumnManual → ReviewColumnAuto`, `AutoAssignReviewer: false → true` (+ the two comment blocks updated to record the ruling).
2. `sdks/openapi.yaml` `BoardConfig`: added `default: auto` / `default: true` with the opt-out-wins note (contract documents the flip).
3. `frontend/src/components/settings/BoardConfigTab.tsx`: the two display fallbacks flipped (`?? "auto"`, `(g?.autoAssignReviewer ?? true)`) so the pre-data render matches the new floor.
4. NOT flipped (out of scope / not dormancy): the gate itself, the merge semantics, the 5-combo matrix test (all five combos write explicit values — flip-invariant), the `defaultWorkerSlots` default (2 — untouched by the ruling), CRD/env (none exist).

## Back-compat: explicit opt-out survives (the critical rule)

The read-time merge already guarantees set-key-wins; the flip changes only the inheritance FLOOR. Pinned by the new `TestReview_ExplicitOptOutSurvivesDefaultFlip` (PG): explicit `reviewColumn=manual` (project tier), explicit `autoAssignReviewer=false` (project tier), explicit `reviewColumn=off` (user tier) each keep the reviewer OFF after the flip; clearing the map (REPLACE `{}`) re-enables the new default (the opt-out lives in the stored map, never a sticky flag). The concurrency pin's `mapA` now writes `autoAssignReviewer=false` explicitly so the two racing maps stay disjoint on all three keys (the union detector keeps its dimension).

## Red-first protocol

Tests committed FIRST at a6ffa0f07 (RED on main's defaults):

- `TestBoardConfigDefaults` (unit) FAIL — config_test.go:36
- `TestReview_AutoReviewDefaultOn` (PG) FAIL — review_assign_integration_test.go:194,196 (effective row) + 205,206 (no dispatch)
- `TestReview_ExplicitOptOutSurvivesDefaultFlip` (PG) FAIL — :251,:269 (the cleared/re-enable arms; the opt-out arms pass pre-flip trivially and are the back-compat pins)

Implementation + stale-pin updates in the follow-up commit (see PR).

## Test evidence (final head, exact commands + outcomes)

```
go build ./...                                                                                      → ok
go vet ./... ; go vet -tags integration ./pkg/board/ ./api/internal/handlers/ ./api/internal/server/ → ok
go test -count=1 -run 'TestBoardConfig|TestMergeBoardConfig|TestValidateBoardConfig|TestDecodeBoardConfig' ./pkg/board/ → ok
TEST_DATABASE_URL=… go test -timeout 600s -tags=integration -count=1 -p 1 ./pkg/board/...          → ok 20.028s (310 subtests; fresh DB, single run — incl. both new tests)
TEST_DATABASE_URL=… go test -timeout 300s -race -tags=integration -count=1 -p 1 -run "TestIntegrationBoardWorkerSemantics|TestIntegrationBoardConfig" ./api/internal/handlers/ → ok
TEST_DATABASE_URL=… go test -timeout 300s -race -tags=integration -count=1 -run "TestIntegrationRouterBoardConfig" ./api/internal/server/ → ok
go test -timeout 480s -count=1 ./api/internal/handlers/ ./api/internal/server/                     → ok 113.963s / 0.591s
go test -count=1 -run "TestOpenAPIRouterContract" ./api/internal/server/                           → ok
frontend: npx vitest run (full)                                                                     → 2489/2489 passed (incl. BoardConfigTab 7/7)
make openapi-validate                                                                               → valid
make repolint (pre-push)                                                                            → all checks passed
```

Postgres 16.4 (port 5439, private instance, migrations 1–48 via golang-migrate). Known pre-existing wart (NOT this lane's, not fixed here): `TestMintUniqueOwnerWideLadder` seeds a fixed workspace id and never cleans it, so a SECOND full-suite run against the same DB fails on `workspaces_pkey`; CI's fresh-DB-per-run never sees it.

## Validated assumptions

- The ruling targets the code defaults (the inheritance floor) — not a forced-on override: explicit stored opt-outs remain authoritative (pinned).
- "No manager configuration surface exposes them" (charter context) held: the only surfaces are the user/project config maps + the Settings UI; no CRD/env/helm site exists.
- `defaultWorkerSlots` stays 2 — the ruling is about review, and slots feed create-fallback (out of the flip).
