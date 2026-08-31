# Worklog: US-70.0 — delivery test harness (fault injection, corruption fixtures, token rows, gVisor pool, chaos)

**Date:** 2026-08-31
**Session:** Epic 70 US-70.0 (#1182) — build the delivery test harness that unblocks the epic's cluster-bound ACs: the API fault-injection seam, the fault/chaos e2e suite, key-row corruption fixtures, SA-token rows, runsc provisioning, and the delivery-pool workflow.
**Status:** Complete — code + unit/pin tests green; cluster execution belongs to the pool workflow (cannot run in this environment).

---

## Objective

Issue #1182's deliverables: deterministic API 500 injection (AC-8), partition mechanism usable on kind (W5), `user_keys` corruption fixtures asserting the #1114 loud degrade (AC-9 degrade half), SA-token time-travel rows (AC-14 equivalence), gVisor provisioning + the ≥1h #1087 gate via a scheduled pool (AC-13 runsc legs / AC-2), and formalized chaos kills — all pinned by Go tests, with the nightly happy-path suite's pass behavior unchanged.

---

## Work Completed

### API fault-injection seam (TDD)

- `api/internal/middleware/fault_injection.go`: `FaultInjectionRule{Count,Method,PathPrefix}`, `ParseFaultInjectionRules` (whitespace-trimmed, comma-separated `COUNT:METHOD:PATH_PREFIX`; empty/unset → no rules → feature absent; malformed rule → error naming it), `FaultInjectionMiddleware` (atomic per-rule counters; the first Count matches get a 500 with the marker body `{"error":"fault injection: METHOD PREFIX"}` per the #862 string error contract; one warn log per injected failure — method/path/remaining only, never secrets).
- Wiring: `RouterConfig.FaultInjectionRules` (`api/internal/server/router.go`), engine-level `Use` only when ≥1 rule (placed after preview-origin interception, before Security/RateLimit/Logging — failures are deterministic and consume no rate budget; covers the root-registered `POST /internal/v1/pod-bootstrap`). Env read once at wire-up in `api/internal/app/app.go` (same spot as `RELAY_ROUTER_SVC_URL`); parse failure fails `New` → process exits (loud-or-absent).
- Helm: `api.e2eFaultInjection` value (default `""`, harness-only comment) + conditional env render in `api-deployment.yaml` + `TestAPIE2EFaultInjectionEnv`.
- Tests: `fault_injection_test.go` (parse matrix incl. zero-count/unknown-method/non-`/`-prefix rejection, byte-identical passthrough when unset, exhaustion, method/prefix mismatch, 64-goroutine `-race` run → exactly Count 500s), `router_fault_injection_test.go` (real `NewRouter` + real `PodBootstrapHandler`: K×500-marker then 200; chain unchanged when disarmed).

### Harness libraries

- `local/lib/us70-common.sh`: the us-70 suite's helpers extracted verbatim (logging, `kc`, port-forward lifecycle, `wait_phase`, `secrets_converged`, `agent_environ`, `seed_user`/`seed_workspace`/`bind_env`, `reconnect_api`, `detect_runtime_class`); env contract preserved exactly. `local/us-70-secret-delivery-e2e.sh` refactored to source it — 6 insertions / 194 deletions, zero row-logic changes (verified by diff).
- `local/lib/gvisor.sh`: runsc provisioning extracted from the S5.6 recipe (sha512-verified direct binaries + containerd shim, handler registration, containerd restart, RuntimeClass gvisor/runsc). `s5-overlay-validation.sh` deliberately untouched (weekly-validated; keeps its inline copy).

### Fault/chaos suite + pool workflow

- `local/us-70-faults-e2e.sh` rows: **F1** armed-seam probe → boot under 500s reaches Active (never-block-boot) → degrade-window sample (env absent post-Active, race-guarded) → autopush heal converges (loud-skip when the deploy is seam-inert); **F2** suspend → API scale-to-zero → `kubectl patch` resume → Active with env ABSENT (W5 coupling) + child alive → API restore + `reconnect_api` → burn residual fault budget → converge; **F3** `wrapped_dek` capture/corrupt (`decode('00','hex')`) → sessionless resume + `secret_audit_log` row `action='pod_bootstrap_dek_failed'` (exact-match; faults WS names ≤36 chars — the writer rune-truncates at 36) → exact-byte restore (verified) → converge; **F4** `kubectl create token workspace-<name> --audience=llmsafespace-api` rows — tampered → exactly 401, control untampered → NOT 401, expired (sleep to the JWT's own `.exp`+10s, loud-skip if validity >3700s) → exactly 401, fresh mint → NOT 401; **F5** sidecar container kill via node-level `crictl stop` (pod-scoped `crictl pods --name "^<pod>$"` → `crictl ps --pod … --name agentd`) → restartCount increment + converge; pod-delete mid-bind → converge.
- `.github/workflows/us-70-delivery-pool.yml`: weekly Sun 07:00 UTC + dispatch, `timeout-minutes: 300`; mirrors e2e-nightly's bootstrap; runsc provisioning via the lib; helm install **seam-inert**; delivery suite first (`SUSPEND_SECONDS: 3600` ≥1h #1087 gate, `RESUME_SCALE: 100`, `P95_BUDGET_MS: 45000` — W7 gVisor tax documented); fault seam armed between suites via `kubectl set env deployment/llmsafespaces-api LLMSAFESPACES_FAULT_INJECTION="${FAULT_COUNT}:POST:/internal/v1/pod-bootstrap"` + rollout wait (fresh process = fresh budget; the delivery suite never sees a fault); faults suite second.
- `local/us70_harness_script_test.go` pins: `bash -n` ×4, the sha512 guard extracted from `lib/gvisor.sh` executed against valid/invalid checksums, fault-seam lockstep (env name in script AND `set env` step in workflow; workflow `FAULT_COUNT` literal == script default), workflow pins (no `api.e2eFaultInjection` at install, arming step present, `timeout-minutes: 300`, gvisor lib used, `SUSPEND_SECONDS: 3600`), 401 + corruption-UPDATE/restore presence.

### Docs

- README-LLM §Testing Requirements gains "Delivery fault harness (Epic 70, US-70.0)"; `local/lib/README.md` added.

---

## Assumptions (Rule 7 — stated and validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | Engine-level `router.Use` covers the root-registered pod-bootstrap route | Gin copies engine handlers at group creation (all groups created after the Use); proven behaviorally by `TestRouter_FaultInjectionCoversPodBootstrap` (real router, K×500 then 200) |
| A-2 | `kubectl set env` + rollout gives a fresh fault budget | Env is read once per process at wire-up (app.go:1333); rollout = new process |
| A-3 | `kubectl create token` mints real TokenReview-validatable tokens for `workspace-<name>` SAs | SA name from `controller/internal/workspace/constants.go:95-97`; audience `llmsafespace-api` from `pod_bootstrap.go:28`; SA exists because the pod runs under it (`pod_builder.go:392`); no pod exec needed (the agentd image is `FROM scratch` — no `cat`) |
| A-4 | Token expiry is observable from the JWT's own `.exp` | jq `split(".")[1] + "===" \| @base64d \| fromjson \| .exp` validated locally against synthetic + realistic base64url payloads (over-padding tolerant) |
| A-5 | Sidecar kill needs a node-level mechanism | No `kill`/`cat` in the scratch image (`cmd/workspace-agentd/Dockerfile`); SIGKILL to PID-1 from inside the pid-namespace is dropped (pid_namespaces(7)) → host-side `crictl stop --timeout 0` (S5.5's docker-exec precedent) |
| A-6 | Fault-suite WS names must stay ≤36 chars | `secret_audit_log.workspace_id varchar(36)`; writer rune-truncates (`pkg/secrets/secret_service.go:489-496`) → `WS_BASE=e2esdf0-0000-0000-000000000` (30 with suffix) |
| A-7 | gVisor workspaces need the admin override annotation | `controller/internal/webhooks/workspace_webhook.go:69-85` rejects `spec.runtimeClass` without `llmsafespaces.dev/allow-runtime-class-override: "true"`; added to `seed_workspace`'s rc branch |
| A-8 | Default `kubectl create token` validity fits the harness budget | Unknown/cluster-dependent → F4b derives the wait from the token itself and loud-skips when >3700s — never assumes |

---

## Key Decisions

1. **Deterministic counted faults over probabilistic/toggled injection** — one env at wire-up, exact failure counts, auto-recovery by exhaustion; no runtime toggle surface, no flaky percentages (Rule: reliable, repeatable).
2. **Partition = API deployment scale-to-zero** — kindnet does not enforce NetworkPolicy (`local/kind-cluster.yaml:24-27`); a real API outage is both simpler and more honest than inert NetPol applies.
3. **Token rows via kubectl-minted tokens, not pod-volume reads** — the agentd image is `FROM scratch`; minting exercises the same TokenReview + audience + SA-binding path the pod's projected token takes, works in both pod modes, and the JWT's own `exp` makes expiry deterministic (found by adversarial review: the original `cat`-based row could never execute).
4. **Seam armed between suites, not at install** — a pool-wide armed seam would have the ~105-bootstraps delivery suite eat the 8-fault budget and boot AC-1 against 500s (validator B1); scoping via `kubectl set env` keeps every suite's assumptions true.
5. **S5 script untouched; lib extracted alongside** — the weekly-validated S5.6 recipe keeps its inline copy; duplication is the price of not destabilizing a just-debugged CI gate (documented in `local/lib/README.md`).

## Adversarial review (Rule 11)

One validator round over the full change. Real findings fixed: pool-wide seam arming (B1, redesign per Decision 4), `cat`-based token read impossible in a scratch image (B2, redesign per Decision 3), `kill -9 1` impossible + PID-1 semantics (B3, node-level crictl stop), audit-column truncation vs 38-char names (B4, shorter WS_BASE), missing webhook annotation breaking the gVisor batch (B5), missing README-LLM/local-lib docs (M6), malformed worklog (M7 — this file), F1 under-assertion (M8 — degrade-window sample added), thin 240m budget (M9 — 300m), fault-count coupling + burn-loop truthiness (M10 — single `FAULT_COUNT` + die-on-still-500), weak lockstep pin (N11 — both names + count equality). False alarms investigated and disproven (engine-level route coverage, helm render, exec-quoting, psql hex round-trip, patch field validity, gvisor drift) — evidence recorded in the review artifacts of this session.

---

## Blockers

None in-repo. The cluster-bound rows (F1–F5, runsc legs, ≥1h gate) execute only in the pool workflow — first scheduled run Sun 2026-09-06 07:00 UTC (or manual dispatch); expect CI-only validation until then.

---

## Tests Run

- `go test -timeout 300s -race ./api/internal/middleware/ ./api/internal/server/` — ok
- `go test -timeout 300s ./api/internal/app/` — ok
- `go test ./helm/... -timeout 300s` (incl. `TestAPIE2EFaultInjectionEnv`) — ok (helm v3.16.4 on PATH; skips where helm absent)
- `go test -timeout 60s ./local/` — ok (8 pin tests)
- `go build ./...`, `go vet` (touched pkgs), `gofmt`/`goimports` — clean
- `bash -n` on all four scripts — clean
- Cluster e2e NOT run here (no docker/kind in this environment) — pool workflow is the execution vehicle

---

## Next Steps

1. Dispatch `.github/workflows/us-70-delivery-pool.yml` manually once merged; triage the first run (runsc resume p95 under the 45s budget is the W7 open question).
2. US-70.2 (#1183): one builder + two-tier revisions + conditional pull — branch `feat/us-70-2-one-builder-revisions` from main; note the concurrent `chore/w8-deepcopy-followups` branch touches `pkg/secrets/secret_service.go` (size validation) — expect a small merge interaction.
3. Nightly keeps the bounded happy-path variant unchanged; revisit `P95_BUDGET_MS=45000` after real runsc numbers exist.

---

## Files Modified

- `api/internal/middleware/fault_injection.go` (new), `fault_injection_test.go` (new)
- `api/internal/server/router.go`, `router_fault_injection_test.go` (new)
- `api/internal/app/app.go`
- `helm/values.yaml`, `helm/templates/api-deployment.yaml`, `helm/extra_env_test.go`
- `local/lib/us70-common.sh` (new), `local/lib/gvisor.sh` (new), `local/lib/README.md` (new)
- `local/us-70-secret-delivery-e2e.sh` (mechanical refactor to lib)
- `local/us-70-faults-e2e.sh` (new), `local/us70_harness_script_test.go` (new)
- `.github/workflows/us-70-delivery-pool.yml` (new)
- `README-LLM.md` (§Testing Requirements — delivery fault harness)
- `design/stories/epic-70-secret-delivery-v2/README.md` (story-table issue links — with the US-70.2 PR)
- Issues filed: #1182 (US-70.0), #1183 (US-70.2)
