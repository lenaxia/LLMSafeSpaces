# Worklog: 1289 — the .15 refresh-compute dead click: CRD-skew starves the legacy-backfill arm and the whole phase machine with it

**Session:** Owner report fix — "refreshing compute via the UX no longer works — workspaces are not refreshing" on v0.44.15 (2026-11-16), task tsk-wayward-tuxedo-hovers.

**Date:** 2026-11-17

## Reproduction and root cause

What the owner's "refresh compute via the UX" maps to (verified in the tree):

- **The affordance**: sidebar workspace kebab → "Refresh compute" → ConfirmDialog → `refreshComputeMutation` (`Sidebar.tsx:109-115`) → `POST /workspaces/:id/refresh-compute` → `onSuccess` invalidates `["workspaces"]` + `["workspace-status"]`.
- **The live path**: the controller recycles the pod (Active → Creating → Active), the API watcher's `onPhaseChange` publishes `workspace.phase` on the user stream, `useUserEventStream` invalidates the same queries.

A DevTools-style trace of the frontend found every link intact — and byte-identical to .14 (the invalidation sites are an exact set-diff match; the provider's only .15 delta is the per-session-drafts cleanup). The frontend provider suites (`SessionActivityProvider.*`), the SSE-invalidation pins (`useUserEventStream.test.tsx`), and the Sidebar affordance pins were all green on main. The break is upstream of the UI: **the phase transitions never happened**.

The root cause sits in the .15 harness bind-once lane's controller arm. `reconcileHarnessBinding` (`harness_binding.go`) backfills every legacy workspace (marker-less `spec.harness == ""` — the entire pre-.15 fleet) to explicit `opencode`, and — by design — returns `handled=true` ("stop this pass; requeue re-reads the bound object") whenever it attempts the stamp. The assumption: the UPDATE's success means the stamp landed.

That assumption is false on a **normally-upgraded cluster**: helm never upgrades `crds/` on `helm upgrade` (helm/README.md "CRD upgrades" — manual `kubectl apply -f helm/crds/` is the documented step), so the new controller runs against the pre-.15 Workspace CRD, which has no `spec.harness`. A structural apiserver **prunes unknown fields and returns success**: the backfill write is accepted, the field never persists, the next reconcile reads `""` again, backfills again — and returns `handled=true` again. **Forever.** The phase machine never dispatches again for any legacy workspace:

- `handleActive` never observes the refresh-compute `restartGeneration` bump → no pod recycle → the POST returns 202 and nothing happens ("refreshing compute no longer works");
- no phase transitions → `onPhaseChange` never fires → no `workspace.phase` SSE events → both the live updates and the mutation's own invalidation refetch the SAME stale phase forever ("workspaces are not refreshing");
- side damage: STARTING workspaces never complete (owner-confirmed 2026-11-17 — `handleCreating` is starved the same way, no pod is ever built); no suspend/resume handling, no health checks, no auto-suspend — every legacy workspace is a zombie; the backfill also becomes a hot write loop (10 writes in 10 reconciles in the fixture), racing every API-tier CRD write.

Why nothing caught it: the controller test seam `SetDefaults_Workspace` still stamps `Harness: opencode` into every fixture (`defaults.go`, the M5-b mirror of the OLD schema default the .15 lane removed from the CRD), so the entire controller suite exercises BOUND workspaces — the legacy arm had no coverage of its failure shape. (Production never calls `SetDefaults_Workspace`; only tests do.)

Born-red evidence (captured verbatim on unfixed main, `controller/internal/workspace/harness_binding_skew_test.go`): `expected phase "Creating", actual "Active"`, generation 7 never observed, `RequeueAfter 0s` (no drain — `handleActive` never ran), and `"10" is not less than or equal to "1"` harness writes in 10 reconciles.

## The fix (root cause, no polling band-aid)

`controller/internal/workspace/harness_binding.go`:

- `backfillLegacyHarness` now **verifies persistence** (read-back after the write) and reports `landed` — an accepted-but-pruned write is distinguished from a landed stamp.
- The legacy arm reserves `handled=true` for a write that PERSISTED. An unlandable stamp logs a loud once-per-window operator error (naming `kubectl apply -f helm/crds/workspace.yaml`) and **falls through to phase dispatch in the same pass** — the phase machine is never starved. The legacy workspace is effectively opencode with or without the stamp (`workspaceHarness` — the controller-side seam, harness_delivery.go: "" defaults to opencode), so proceeding is semantically exact.
- Re-attempts ride `harnessBackfillRetryInterval` (10m, var for tests): one write per window per workspace (no hot loop, crash-safe attempt stamping), converging within minutes of the operator applying the CRD. Bookkeeping: `harnessBackfillAttemptAt` map + mutex on the reconciler (concurrent reconciles).

API-tier companions (`api/internal/services/workspace/`):

- `RefreshWorkspaceCompute`'s GET-mutate-UPDATE is now conflict-retried (the `bindUnboundWorkspace` shape) — a lost resourceVersion race retries from a fresh read instead of 500ing the user's refresh; each attempt re-derives generation+force-marker together; a terminal phase observed mid-retry keeps its 409 shape.
- `bindUnboundWorkspace` verifies the bind persisted (read-back); an accepted-but-pruned bind is a named internal error `harness_bind_not_persisted` carrying the operator remediation — pre-fix it answered 200 while the born-unbound workspace parked in Creating forever (no pod, no error, no event).

## Pins

- `controller/internal/workspace/harness_binding_skew_test.go` (NEW, born-red on main):
  - SKEW-1 the repro — user-forced refresh (generation bump + #1505 marker) on a legacy Active workspace under a pruning apiserver MUST recycle: pod deleted, phase Creating (the event source the UI rides), generation observed.
  - SKEW-1b — plain bump + busy still takes the polite #761 drain (the phase machine dispatched at all).
  - SKEW-2 — backfill write cadence bounded: ≤1 write per window in 10 reconciles.
  - SKEW-3 — convergence: once the interceptor stops pruning (operator applied the CRD), the next window's attempt lands the explicit stamp.
  - SKEW-4 — healthy cluster unchanged: stamp-then-proceed contract kept.
  - SKEW-5 (owner confirmation 2026-11-17, the WIDENED symptom): a legacy workspace still STARTING (provisioning, phase Creating, no pod) completes start under the same skew — `handleCreating` builds the pod (leg 1) and the Creating→Active transition fires once the pod runs (leg 2). Born-red on unfixed main: "handleCreating must build the pod — start does not stall on the skew."
  - Fixture: `pruningCRDInterceptor` — fake-client interceptor zeroing `Spec.Harness` on workspace Update, the byte-exact semantics of structural pruning.
- `api/internal/services/workspace/harness_binding_test.go`: `TestBindWorkspaceHarness_NotPersistedFailsLoudly` (born-red: pre-fix returns silent success).
- `api/internal/services/workspace/workspace_refresh_test.go`: `TestRefreshWorkspaceCompute_ConflictRetries` (born-red: pre-fix 500s on the first conflict).
- `frontend/src/components/layout/Sidebar.test.tsx`: the affordance pin (d) extended — confirmed refresh invalidates `["workspaces"]` and renders the fresh phase ("Creating") without reload.
- Live-updates pin (c) rides the existing green `useUserEventStream.test.tsx` (workspace.phase → invalidation) + `useWorkspaces.test.tsx` (invalidation → refetch → render); the backend leg is SKEW-1's phase-transition assertion (the event the watcher feeds on).

## Interaction risk (adjacent lanes, checked)

- **Workers-tree/SSE staleness (#1966/#1646/#1687 lanes)**: frontend untouched (byte-identical invalidation map); all `SessionActivityProvider.*` suites (192 tests) and `useUserEventStream`/`useWorkspaces` suites green on the fix head — the #1687/#1848 resync/recheck pins are unaffected.
- **#2030a served-transcript hygiene**: construction-seam only (`NewAdapterForType` hygiene assertion); no overlap with the CRD write path. Not in the blast radius.
- **#2047 silent-worker arm** (`onPhaseChange` additions): audited — lock-local, failure-isolated, non-blocking; not causal (and the phase machine starving means fewer `onPhaseChange` calls, not more).

## Evidence

- Born-red: all four controller arms + both API pins fail on unfixed main (stash-verified twice; verbatim outputs above).
- Green on the fix head: `go test ./controller/...` (incl. the 68s workspace suite), `go test ./api/...`, frontend providers (192), Sidebar (35), useUserEventStream+useWorkspaces (17).
- `make fmt-check` clean; `go build ./...` clean.

## Follow-ups (out of scope)

- The CRD-upgrade footgun itself: the chart could gain a pre-upgrade job or the README step could become a chart gate (the pi/opencode mandatory-value `fail` gates already catch the values half — the CRD half is still manual).
- `SetDefaults_Workspace` still mirroring the pre-ruling harness default is a fixture-only hazard; a follow-up could drop that default line now that the CRD has none (left alone here — it would churn every controller fixture for zero behavior gain).

## Post-merge addendum (2026-11-17)

PR #2088 was MERGED by the orchestrator at `ed484ea22` (2026-11-17T05:43Z) while the review-round-2 remediation commits were still in flight — the merge carries the root-cause fix, all six skew pins, and the widened docs, but NOT the three review findings' fixes, and main went red on Frontend/Build (the TS2554 matcher args, runtime-invisible locally). The remediation delta (bumpedGeneration response contract, the verify's stale-read gate, workspaceHarness symbol refs, the TS fix) rides the SAME branch as follow-up PR #2089, reviewed on its own head. GitHub's push events were also not firing for one push window (the #1844 class — zero runs for 75838cd9e); the empty tree-identical `ci: retrigger` commit (46189ad16) restored flow.
