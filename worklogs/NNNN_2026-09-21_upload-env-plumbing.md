# Worklog: design 0060 PR 2.5 — the upload-staging env plumbing

**Date:** 2026-09-21
**Session:** feat/upload-env-plumbing — §9 PR 2.5 (routed from worker 2's #1516 review): values.yaml knobs → controller flag → agentd env on both containers
**Status:** Complete (this PR; REBASED onto post-#1518 main — the PR-2 supervisor content rides main via the squash, this is the plumbing-only delta)

---

## Objective

Nothing flows UPLOAD_STAGING_BUDGET / CREDENTIAL_FLOOR / MAX_CONCURRENT / TTL_MS / APPLY_TIMEOUT_MS from helm into the pods — PRs 1/2 ship parsing + defaults that stand alone. This PR completes the deploy surface: helm values → the controller's `--upload-staging` k=v flag → agentd env on BOTH agentd-bearing containers (sidecar: admission/staging/apply-client; workspace container: upload_apply engine + destination scrub — one knob set, one behavior).

---

## Work Completed

- `controller/internal/workspace/upload_staging_env.go` (new): `UploadStagingConfig` + `ParseUploadStagingFlag` (k=v flag; **unknown keys rejected loudly at boot** — a typo'd helm knob never silently defaults) + `EnvVars()` (only SET fields emit; zero config emits nothing — agentd defaults stand).
- `controller/main.go`: `--upload-staging` flag (registration + parse extracted into helpers — main was at funlen), exit-on-invalid.
- Plumb: SetupControllers signature → Reconciler field → **both containers** (pod_builder.go's workspace-container env; agentd_sidecar.go's sidecar env).
- `helm/values.yaml`: `controller.agentdSidecar.uploadStaging.{budgetBytes, credentialFloorBytes, maxConcurrent, ttlMilliseconds, applyTimeoutMilliseconds}` (all 0 = unset).
- `helm/templates/controller-deployment.yaml`: the k=v flag render (non-zero fields only; all-zero renders nothing). Verified both directions with real `helm template` (set → `--upload-staging=budget=52428800,ttlMs=900000`; default → absent).

## Key decisions

1. ONE flag carrying k=v (not six typed flags) — the pod env is the typed surface (agentd's parsers); the flag is transport. Unknown-key rejection at boot is the typo guard.
2. Env lands on BOTH containers unconditionally-when-set: the two halves of the leg (staging vs apply/scrub) must see the same knobs or the budget and the TTL clocks diverge.

---

## Tests Run

- `go test -run 'TestUploadStaging' ./controller/internal/workspace/` — 7 green: flag round trip, empty=default, unknown-key rejection (3 malformed shapes), env exact-name emission (set/unset), the literal name-contract pin, and the pod wiring BOTH directions (set → both containers carry it; zero → NO container carries any UPLOAD_* env, sidecar AND single-container modes).
- Full `./controller/internal/workspace/` — ok (80s). Full `go build ./...` — ok. golangci-lint 0 issues (funlen resolved by extracting the flag helpers, not by nolint).

---

## Files Modified

- `controller/internal/workspace/upload_staging_env.go` — new
- `controller/internal/workspace/upload_staging_env_test.go` — new (7 tests)
- `controller/main.go` — flag + helpers (funlen: agentd-hash flag pair extracted too)
- `controller/internal/controller/controller.go` — signature + reconciler field
- `controller/internal/workspace/reconciler.go` — the field
- `controller/internal/workspace/pod_builder.go` — workspace-container env
- `controller/internal/workspace/agentd_sidecar.go` — sidecar env
- `helm/values.yaml` + `helm/templates/controller-deployment.yaml` — the knobs + render
- `cmd/workspace-agentd/sidecar_mode.go` — the tmpfs-guarded staging boot block + the sweeper inside it (r2's post-approval CI fix + r3)
- `cmd/workspace-agentd/upload_staging_test.go` — the agentd-side env pins + the guard pin family (r1/r4/r5)
- `cmd/workspace-agentd/upload_staging.go` — the sweeper-placement seam + guard (the PR-2-adjacent agentd changes that rode the branch)
- `cmd/workspace-agentd/upload_staging_test.go` — the guard pin family (wiring-level, mutation-verified)
- `cmd/workspace-agentd/upload_apply.go` / `upload_apply_test.go` — the copy cap + sentinel arms (the env-plumbing robustness finds)
- `worklogs/NNNN_2026-09-21_upload-env-plumbing.md` — this worklog


## Review round 1 (floor=0, cross-validation, duplicate keys, the copy cap, the two missing pin legs)

- **floor=0 parse/emission inconsistency**: an explicit floor=0 (disable the reserve — agentd honors n≥0) was silently conflated with unset. Fixed: a credentialFloorSet marker tracks explicitness; parse, FlagString, and EnvVars agree — floor=0 parses, flag-strings, and emits env value 0. Pinned.
- **The env-name contract's agentd half was missing** (a one-sided rename shipped green): three t.Setenv test funcs now pin the agentd-side names literally (stagingConfigFromEnv AND uploadApplyEngineFromEnv — both in-pod consumers; the divergent-clocks class), with the invalid→default arm.
- **The helm render verification was manual-only**: helm/upload_staging_chart_test.go pins both directions (set → the exact flag; default → absent) + the typo'd-values-key silent limitation made observable. 
- **ttl/applyTimeout cross-validation**: ParseUploadStagingFlag rejects ttlMs ≤ applyTimeoutMs+5000 (the sweeper must never reclaim an in-flight apply temp). Pinned both arms.
- **Duplicate flag keys**: rejected (no silent last-win). Pinned.
- **The copy loop was uncapped at the declared size** (a lying-small declaration streamed the whole staged object past the gate): the read window truncates at size-written, and at exactly-written a 1-byte probe distinguishes EOF from more-data. Pinned by the lying-small test (4096 staged / 100 declared → size_mismatch at the boundary).


## Review round 2 (the TTL guard hole, floor chart-reachability, the values comment, the cap pin)

- **The TTL guard raced single-knob configs** (the reviewer reproduced the live sweeper-reclaim): the guard now validates the EFFECTIVE bound on both sides — unset applyTimeoutMs uses agentd's compiled 60s; unset ttlMs uses the compiled 15m (the symmetric hole). Pinned both single-knob arms + the pass arm.
- **floor=0 was chart-unreachable (dead code through the deploy surface)**: credentialFloorBytes=-1 is the explicit-zero sentinel (values comment documents it); the chart renders floor=0. Pinned end-to-end by the chart test.
- **The values-layer fail-loud comment was provably false by our own committed test**: the comment now states the real boundary (the guard starts at the flag string; a values-key typo renders silently — verify rendering with helm template | grep). 
- **The copy-cap pin passed against the uncapped code** (both shapes end in size_mismatch): the open seam (a counting reader) makes it behavioral — the capped read stops at declared+probe (≤101 for declared 100); mutation-verified (cap reverted → 4096 read → red).
- **A chart render-time fail guard** (the house convention) rejects the violating ttl/apply pair at helm template time with the same arithmetic as the flag parser — pinned.


## Review round 3 (the guard commit's own gaps) — [CORRECTIONED r5: this round's "mutation-verified via the IsDir drop" claim was about the HELPER-LEVEL pin, which the r5 review empirically falsified at the wiring level (the if-false mutation shipped green) — the wiring pin landed in r4; this section's claim is true only of the helper arms]

- The r2-followup guard commit (f2c6d88a) shipped untested and with the sweeper outside the guard: the pin (TestBuildSidecarDeps_NoErrorLogsOrGaugesWithoutTmpfs — both arms, mutation-verified via the IsDir drop) landed with the guard on the e2e branch; the sweeper moved INSIDE the guard so the 'no shared-metrics writes without a tmpfs' claim is true of the ticker too (each tick pushes gauges).


## Review round 4 (the pin exercised the helper, not the wiring; the env pins were silently deleted)

- The r3 file-sync from the e2e branch REPLACED upload_staging_test.go wholesale — silently deleting the three agentd-side env-name contract pins (the r1 hard gate). Restored from 02189bda (the claims-without-landing class again, via a file-level checkout this time — the lesson: never checkout -- a whole test file across stacked branches; sync the delta).
- The guard pin tested stagingBootShouldRun/ensureStagingDir directly (removing the guard from buildSidecarDeps shipped green — reviewer mutation-proved it): now TWO pins — TestBuildSidecarDeps_NoErrorLogsWithoutTmpfs drives the REAL buildSidecarDeps with a captured zap logger (guard removed → the error-log fires → red, mutation-verified); TestStagingBootGuard_BothArms keeps the helper arms.


## Review round 5 (the guard's happy arm + the gauges half + the sentinels)

- The guard's happy arm through the REAL wiring: TestBuildSidecarDeps_BootBlockRunsWhenTmpfsParentExists — an existing parent + a seeded stale temp → buildSidecarDeps establishes the dir, boot-scrubs the temp (the §4.1.1 boot-reclaim contract), and the boot gauges run. Mutation-verified: `if false &&` → the pin fails (the boot block is no longer deletable green).
- The gauges half: TestBuildSidecarDeps_NoGaugesWithoutTmpfs — testutil.CollectAndCount on the shared singleton before/after buildSidecarDeps without a tmpfs: no staging series may appear.
- The sentinel-parse arms: TestStagingEnvSentinels_ZeroIsValid — literal "0" honored for UPLOAD_STAGING_CREDENTIAL_FLOOR and UPLOAD_DEST_MARGIN (the agentd half of the controller's floor sentinel).
- The commit-name cosmetic fixed here in the record: the r4 test is TestBuildSidecarDeps_NoErrorLogsWithoutTmpfs.


## Review round 8 (the rebase round — the dead seam + the worklog staleness)

- The dead onSweepTick seam (zero assignors, disprovable comment — the r8 review on the pre-rebase head flagged it; the rebase carried it) — deleted; sweepStarted is the placement pin's only seam.
- The worklog staleness corrected: the header/Files-Modified now state the rebase truth (post-#1518 main; the plumbing-only delta).
- The sweeper-placement delta record: the placement pin (TestBuildSidecarDeps_SweeperPlacementGuarded — sweepStarted closed BEFORE the goroutine launches; mutation B red) was the r5-r9 chain's load-bearing find; it rides this branch's agentd files.
