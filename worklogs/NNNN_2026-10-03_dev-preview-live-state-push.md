# Worklog: Dev-preview state live push — #1617

**Date:** 2026-10-03
**Session:** Fix #1617: the dev-preview toggle left the in-pod tools (`feature_status`, `dev_preview_url`) reporting the boot-time env snapshot until a pod recreate. Feature flags must not require pod reboots.
**Status:** Complete — local tests green (agentd, agentpush, workspace service, local pins); PR pending

---

## Objective

Make `spec.networkAccess.devPreview` toggles reach the RUNNING pod. The controller projects `WORKSPACE_DEV_PREVIEW_ENABLED` into the pod exactly once (`pod_builder.go:118`, `agentd_sidecar.go:176`); env is immutable, so `SetDevPreview`'s CRD update (live for the API tunnel gate) diverged from the in-pod tool surface until a pod recreate. Production evidence 2026-10-03: workspace `dda717cb-…-230ff` CRD `devPreview:true`, pod created 07:52Z with env `false` (enabled afterwards) — settings said enabled, the session's tools said disabled.

## Assumptions (stated, then validated)

1. **The user-mux push channel fits this state.** Validated: `user_timezone.go` is the exact precedent (API push → atomic → tool reads live); `agentpush.PushUserTimezone` is the API-side precedent. The push carries an ABSOLUTE value, so it self-heals — no ordering concerns.
2. **Push failures must not fail the toggle.** Validated against the design comment in `agentpush.go` ("notify failure costs latency, never correctness") and the suspended-workspace case (`ErrNoRunningPod` is a sentinel, not a hard failure). The CRD remains the source of truth; the next pod build re-projects the env from it.
3. **The pod-rebuild convergence claim.** Validated: `pod_builder.go` reads `workspace.Spec.NetworkAccess.DevPreview` at every pod build; suspend/resume and upgrade churn both pass through it.
4. **`kubectl patch` of the CRD bypasses the API push.** Validated by reading the route (router.go `PUT /dev-preview` is the only SetDevPreview caller); documented as a latency-only gap in `docs/user/dev-preview.md` (converges on next pod build).

## Work Completed

### agentd (`cmd/workspace-agentd/`)
- **`dev_preview_state.go`** (new): `devPreviewPushedAtomic` (absolute `"true"`/`"false"`, `""` = never pushed), `devPreviewState() (active, reported, live)` — push first, boot env second, skew last; `devPreviewStateHandler` on `POST /v1/dev-preview-state` (§D1 pair auth, POST-only, `enabled` must be an explicit boolean — `*bool`, never guessed).
- **`server.go`**: route registered next to `/v1/user-timezone` (the API-driven lineage).
- **`feature_status.go`**: the `dev_preview` entry reads `devPreviewState()`; `source_detail` names the live push when it is the basis, keeps the boot-env text otherwise, keeps the UNREPORTED skew note only for the no-push-no-env case.
- **`mcp_server.go`**: `dev_preview_url` refuses when `reported && !active` (was: env literally `"false"`); the skew note fires only when unreported. A live enable now mints even under a stale `false` boot env — the exact production bug.

### API
- **`agentpush.PushDevPreviewState`**: the `PushUserTimezone` pattern verbatim (pod-IP resolve → password resolve → authed JSON POST, 5s-bounded client, `ErrNoRunningPod` sentinel for suspended workspaces).
- **`workspace.Service`**: `DevPreviewStatePusher` seam + `SetDevPreviewPusher` (optional setter injection, the `SetPolicyChecker` lineage); `SetDevPreview` pushes after a successful CRD update, log-only on failure; the stale "no pod restart is needed" comment rewritten to name both surfaces.
- **`app.go`**: wiring `wsSvc.SetDevPreviewPusher(agentPusher)` beside the other shared-notifier consumers; compile-time interface pin in `devpreview_push_pin_test.go` (the timezone pin's pattern).

### e2e + docs
- **`local/dev-preview-tunnel-e2e.sh`**: two #1617 arms before the existing #1580 disabled arm — API PUT disable → tool refuses on the SAME pod (`pod_uid` pinned); API PUT re-enable → tool mints on the SAME pod. The #1580 arm keeps its env-projection-convergence role (kubectl patch + recreate).
- **`local/dev_preview_script_test.go`**: three assertion-row pins for the new arms.
- **`docs/user/dev-preview.md`**: toggles are live through Settings/API; the `kubectl patch` path converges on next pod build.

## Key Decisions

- **Push over in-place env tricks.** ConfigMap-mounted files / pod spec mutation were rejected: the former adds a per-workspace API object for one boolean; the latter fights k8s immutability. The push channel already exists and carries absolute state.
- **`reported` vs `active` split.** The skew semantics (#1580) survive unchanged: unreported is only "no push AND no env" — a pushed state is reported even when it says false (pinned by `TestFeatureStatus_DevPreviewUnreportedWithPush`).
- **Synchronous push inside the toggle.** Bounded by the 5s agentpush client; keeps the tests deterministic and the worst case (hung pod) is a slow settings save, not a lost update. The suspended case short-circuits on the pod-IP resolve.
- **No controller change.** The controller's env projection stays the boot-time floor; #1617 is a read-surface staleness bug, not a projection bug.

## Adversarial Self-Review (validated findings)

- **Live push true + stale env false, then agentd container restart (not pod recreate):** env re-reads as false but the pushed atomic is process state — container restart wipes it → tools regress to stale-false until next toggle/resume. REAL but bounded: agentd container restarts are the fault path (crash), the CRD is truth, and the pre-fix behavior was strictly worse (always stale). Not fixed here; noted on #1617 as a known convergence window (the secrets-resync notify path is the future convergence channel if it matters in practice).
- **Multi-replica API races (two toggles in flight):** last push wins; both pushes carry absolute values from their own CRD writes; the CRD serializes via resourceVersion conflicts. No ordering hazard.
- **`dev_preview_url` refusal text unchanged?** Deliberately — the frontend pins on fragments of it (`frontend/tests/e2e/dev-preview-button.spec.ts`); no shape change shipped.
- **Existing #1580 tests:** re-run — `TestMCPHandler_DevPreview_*` and `TestMCPHandler_FeatureStatus_*` pass unmodified (env-only path is the fallback and the atomic defaults to `""`).

## Verification

- `go build ./...` + `go vet` on every touched package — clean
- `go test ./cmd/workspace-agentd/` FULL suite — green (505s, `-count=1`); the new tests plus every pre-existing #1580/#0062 arm
- `go test ./api/internal/services/agentpush/ ./api/internal/services/workspace/ ./api/internal/app/` — green (push contract + toggle wiring, incl. push-failure and no-push-on-CRD-failure)
- `go test ./local/ -run TestDevPreviewScript` + `bash -n` — green (script syntax + new assertion rows)
- kind e2e (nightly) covers the new arms; not run locally this session (the shared cluster is production; the nightly owns it)
