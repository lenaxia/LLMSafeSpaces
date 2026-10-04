# Worklog: #1587 — cluster-scoped reconcile reads through the direct reader

**Date:** 2026-10-04
**Session:** Structural close of #1551 defect 1 (#1587): the unbounded cached Get on the reconcile path. Two turn-death interruptions (2026-10-03 18:19Z mid-`go test ./...`; resumed from an uncommitted-but-coherent worktree, then committed per step).
**Status:** Complete (pending review)

---

## Objective

Make the reconcile path structurally immune to never-syncing cluster-scoped informers: the reads that caused the 2026-09-23 incident (4/4 workers wedged 75 minutes, Ready probes + leader lease held, zero signal) must never hang silently — a stuck read must surface as a loud, logged, retryable error.

---

## Work Completed

### Diagnosis (verified in tree, not assumed)

- `resolveRuntimeImage` was called with `r.Client` (`pod_builder.go:24`) — the manager's **cached** client. RuntimeEnvironment is cluster-scoped and never `For`/`Owns`/`Watches`-registered, so a cached Get **lazily starts an informer** that blocks unconditionally when RBAC withholds list/watch (`#1551`).
- Second instance of the same class found: `pvcUsesWaitForFirstConsumer` (`pvc.go:44`) reads cluster-scoped **StorageClass** via the cached `r.Get` on the Pending path (called from `phase_pending.go:85`). Only reached when `spec.storage.storageClassName` is set (empty → early return), but structurally identical: unwatched, cluster-scoped, forever-block.
- All other cached reads in the reconcile path are namespaced types (Workspace/Pod/Secret/SA/PVC watched; ConfigMap/NetworkPolicy namespaced in the controller's own namespace, RBAC-covered) — left cached deliberately.
- Verified both chart grants already carry `get` (`helm/templates/rbac.yaml` storageclass-reader `:229-231`, runtime-env-reader `:644-653`) — a direct GET needs **no RBAC change** beyond #1586.

### Remedy: direct-reader bypass (design 0058 §4.3 precedent)

Chosen over a per-read deadline: the defect class is the never-syncing informer; a direct read is a single HTTP request where an RBAC gap 403s **immediately** (loud error → existing `enterRecovery` backoff/requeue/event machinery), while a bounded cache read still never succeeds and every reconcile pays the full budget — slow degradation, not a fix. A black-holed apiserver stalls every reconcile-path HTTP equally (status writes included) — that class is #1588's watchdog, not this lane.

- `WorkspaceReconciler.APIReader client.Reader` (new field, `reconciler.go`).
- `direct_reader.go`: `directReader()` returns `APIReader`, or an `unwiredReader` whose Get/List fail with `errClusterReaderUnwired` (names the wiring fix). Refusal is lazy by construction — explicit image references (`/` in `spec.runtime`) never read, so unwired direct constructions still build explicit-image pods.
- `buildPod`: `resolveRuntimeImage(ctx, r.directReader(), ...)` — **no cached fallback** (a fallback reintroduces the wedge the moment the informer is the broken thing).
- `pvcUsesWaitForFirstConsumer`: same swap; **fail-open semantics preserved** (any read error → not-WFFC; pinned explicitly).
- `SetupControllers` wires `APIReader: mgr.GetAPIReader()`; `SetupWithManager` refuses nil (**boot-loud** — production cannot run unwired).
- `reconcilerFor` (shared test helper) models the direct path (`APIReader: fakeClient`).

### Evidence

Red-first: all pins written before implementation; first run failed compile (`unknown field APIReader`) — the Go-red form; load-bearing behavior proven by mutation (below).

Unit pins (`controller/internal/workspace/runtime_reader_test.go`):
1. `TestBuildPod_NeverSyncingRuntimeEnvCacheDoesNotWedge` — a `neverSyncingCacheClient` (Get/List of the cluster-scoped type block forever, everything else delegates) as the embedded Client; buildPod with runtime `python:3.11` (exercises Get + colon-munged Get + List legs) must complete inside a 5s window via the direct reader; asserts RTE image + `llmsafespaces.dev/runtime-env` annotation.
2. `TestBuildPod_NilAPIReaderNamedRuntimeFailsLoud` — cached client CAN serve the object; buildPod still fails naming `mgr.GetAPIReader()` (no silent cached fallback).
3. `TestBuildPod_NilAPIReaderExplicitImageStillBuilds` — lazy refusal.
4. `TestBuildPod_DirectReaderForbiddenPropagatesLoud` — 403 wraps (not swallowed into not-found), `apierrors.IsForbidden` survives the chain (requeue/classification consumers).
5. `TestPVCWFFC_NeverSyncingStorageClassCacheDoesNotWedge` — SC leg of the wedge pin.
6. `TestPVCWFFC_DirectReaderFailureFailsOpen` / `TestPVCWFFC_UnwiredReaderFailsOpen` — fail-open preserved (error path returns false — pinned per orchestrator watch-item, not just the happy path).
7. `TestSetupWithManager_RefusesNilAPIReader` — boot-loud guard.

Envtest wiring pin (`controller/internal/controller/setup_wiring_envtest_test.go`, `-tags envtest`): real apiserver + real manager through `SetupControllers` (the only production wiring path); named runtime `python` + WFFC StorageClass drive Pending→Creating without a provisioner; asserts the pod appears with the RTE image — the pod's existence proves BOTH cluster-scoped direct reads completed through the wired reader. Envtest workflow path trigger extended to `controller/internal/controller/**`.

Mutation verification (scratch worktrees at the commit — never `git checkout` of live edits; all reverts restored by deleting the worktrees):

| Mutation | Pins gone red | Evidence |
|---|---|---|
| M1: `buildPod`+`pvcUsesWaitForFirstConsumer` revert to cached client (the #1551 defect itself) | both never-syncing wedge pins | FAIL at exactly the 5.00s window, wedge message |
| M2: `APIReader: mgr.GetAPIReader()` wiring removed from `SetupControllers` | envtest wiring pin | `SetupControllers` returns the refusal error at the require |
| M3: `directReader()` silently falls back to `r.Client` when nil | no-silent-fallback pin | `TestBuildPod_NilAPIReaderNamedRuntimeFailsLoud` FAIL (build succeeded via cache) |

Test runs (final tree, `count=1`): `go test ./internal/workspace/` ok (68s); `go test ./internal/controller/` ok; envtest `TestEnvtestSetupWiring` PASS (11.2s); `go build ./...`, `go vet ./...`, gofmt/goimports clean. Local `-race` on the full controller tree skipped deliberately: the race compile exceeds the shared-pod disk/time budget (post-OOM standing directive — targeted runs while sibling lanes are active; `-race` rides CI).

---

## Key Decisions

1. **Direct reader over deadline-bounded cache read** — rationale above; orchestrator-endorsed at first read.
2. **Two-read scope** (RTE + StorageClass), namespaced reads and the feature-gated InferenceRelay reconciler (boot-synced; a never-syncing informer there wedges at boot, visible) left as-is.
3. **Lazy nil-refusal** — keeps ~90 existing explicit-image buildPod tests untouched; production nil is impossible anyway (SetupWithManager refuses).
4. **No RBAC change** — `get` is a strict subset of the informer grants (#1586 + F1.3.7).
5. **WFFC fail-open preserved exactly** — reader swap only; changing failure semantics would be unvalidated scope creep.

### Assumptions stated and validated (Rule 7)

| Assumption | Validation |
|---|---|
| RuntimeEnvironment/StorageClass are the only unwatched cluster-scoped cached reads on the reconcile path | grepped every `r.Get`/`r.List`/`r.Client.*` in `internal/workspace` non-test files; enumerated namespaced vs cluster-scoped |
| `mgr.GetAPIReader()` never participates in the cache (direct HTTP) | controller-runtime v0.20.3 `GetAPIReader` documented semantic; the envtest test exercises it through the real manager |
| Direct GET permitted by both chart grants | `rbac.yaml:229-231` and `:644-653` — `get,list,watch` |
| buildPod errors requeue loudly | `phase_creating.go:127-131` → `enterRecovery` (log + backoff + status + RecoveryExhausted warning event) |
| The webhook performs no cached RTE read | webhook decodes the admission request directly (no client Get) — verified in tree |

---

## Blockers

None. (Historical note for this lane: the 2026-09-27 `/fix` bot run claimed this fix on `feat/issue-1587-runtime-env-direct-reader`, but that branch never reached origin and no PR exists — delivery flap, orchestrator ledgered. This lane redid the work on the same chosen shape.)

---

## Tests Run

- `go test ./internal/workspace/ ./internal/controller/ -timeout 300s -count=1` — ok (68s)
- `KUBEBUILDER_ASSETS=… go test ./internal/controller/ -tags envtest -run TestEnvtestSetupWiring -timeout 300s -count=1 -v` — PASS (11.2s)
- Mutation runs M1/M2/M3 in scratch worktrees — all red, then removed
- `go build ./...`, `go vet ./...` (incl. `-tags envtest`), `gofmt -l`, `make imports` — clean

---

## Next Steps

- PR review rounds to APPROVED; orchestrator adjudication (packet sent separately).
- Out-of-scope adjacents tracked elsewhere: #1588 (worker watchdog — the detection layer for any future wedge class incl. black-holed apiserver), #1589 (resume-path status-write race).

---

## Files Modified

- `controller/internal/workspace/direct_reader.go` (new)
- `controller/internal/workspace/runtime_reader_test.go` (new)
- `controller/internal/workspace/reconciler.go` (APIReader field + SetupWithManager refusal)
- `controller/internal/workspace/pod_builder.go` (resolveRuntimeImage via directReader)
- `controller/internal/workspace/pvc.go` (StorageClass Get via directReader)
- `controller/internal/workspace/controller_test.go` (reconcilerFor wires APIReader)
- `controller/internal/controller/controller.go` (SetupControllers wiring)
- `controller/internal/controller/setup_wiring_envtest_test.go` (new)
- `.github/workflows/envtest.yml` (path trigger)
