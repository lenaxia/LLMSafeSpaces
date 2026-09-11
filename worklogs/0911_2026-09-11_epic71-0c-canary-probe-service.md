# Worklog: Epic 71 / 0c (part 2) — the API-side canary probe service

**Date:** 2026-09-11
**Session:** Stream 0c continuation (epic-71 / 0c, #1312 canary skeleton — metrics-only). Agent: opencode-kestrel [glm-5.3] (handoff from opencode-agent-b recorded in the 0c thread, comment 5630422163).
**Status:** In Progress

---

## Objective

Land the probe-service unit designed in part 1: an API-side background canary that probes every configured workspace class on an interval — a GetSnapshot round-trip (snapshot-path liveness) and an Act(answer_question) resolve-by-absence leg (S6) — with #1312's failure classification as metrics. Zero model spend; alerts stay gated on L3/L4 per the wave plan.

---

## Work Completed

### pkg/obs — the loop-liveness family gets one registration owner
- Created `pkg/obs/loopliveness.go`: the family constants (identical names/values to open PR #1322's constants-only version — trivial merge either direction) PLUS the single `promauto` registration, `StampLoopLastRun(loop)`, `LoopLastRun()` test accessor, and loop-name constants (`reconcile_watchdog`, `outbox_parked_sweeper`, `canary_probe`).
- Why centralized: two per-package registrations of the same metric name in one binary panic at init. #1322 (0b follow-up, open) registers the family in the outbox package; the canary joining API-side makes a second consumer a certainty. Coordination note posted on the epic (comment 5630754727): #1322 should drop its local registration and call `obs.StampLoopLastRun`; either merge order converges to exactly one registration.
- agentd's part-1 gauge (my file) switched to `obs.StampLoopLastRun(obs.LoopReconcileWatchdog)`; local registration deleted (also upgrades the stamp from whole-second to ns precision — the per-pass-advance pin still holds, verified).

### api/internal/services/canary — the probe runner (new package)
- `canary.go`: Config{Interval(60s), Timeout(2s), Classes, Pick, Resolve, NewClient, Logger}; `New` validates seams when classes are configured (empty classes = valid idle service — knob-off shape); `Run` (janitor lifecycle) ticks `runOnce` and stamps `obs.LoopCanaryProbe` at END of pass only (the loop-owned-stamp discipline #1322 established); `probeClass` → both legs with targeting-failure classification (`no_target`/`pick_error`/`unresolved_target` recorded on both legs — a probe that never reached a pod says nothing about the pod).
- Classification (connect.CodeOf + context.DeadlineExceeded): snapshot leg `ok|not_found|invalid_argument|unauthenticated|not_supported|timeout|unavailable|error|...`; resolve leg `resolved` (S6 satisfied, post-1a) vs `not_found_error` (S6 VIOLATION — the pre-1a baseline this canary exists to expose) etc.
- Synthetic identifiers: `canary-probe-session-<class>` / `canary-probe-input-<class>` — prefix discipline: real harness ids are generated tokens, so the resolve leg can never resolve a real pending input (S10 by construction). CustomText non-empty (agentd's validateAction requires an answer payload — pinned by test).
- `metrics.go`: `llmsafespaces_canary_probe_outcomes_total{class,leg,outcome}` + `llmsafespaces_canary_probe_duration_seconds{class,leg}` (promhttp default registry — the API /metrics surface, verified router.go:823).
- `client.go`: `NewAbiClient` — the §D1 Basic-auth transport twin (newUsageStreamClient's shape) over the reference abiclient.
- Tests (TDD — suite written first, red, then implemented): classification table, S6 violation/resolved legs, timeout wedge, wrong-result-shape drift, wrong-session drift (Rule 11 finding, fixed with regression test), no-target/pick-error/unresolved-target, request-shape pins, Run-loop e2e (stamps family; runOnce alone does NOT stamp), empty-classes-still-stamps, ctx-cancel stop, scrape completeness, and a wire-level round-trip (real abiconnect handler over abitest behind a Basic-auth gate — the #1308 pattern) incl. the unauthenticated path surfacing typed CodeUnauthenticated with no credential leakage.

### Config + app wiring + Helm
- `api/internal/config`: `Canary{Enabled,Interval,Timeout,Classes}` block; `applyCanaryEnv` (LLMSAFESPACES_CANARY_{ENABLED,INTERVAL,TIMEOUT,CLASSES}; malformed durations degrade to defaults — IntervalFromEnv convention); `validateCanary` fails boot on enabled+empty classes (Turnstile fail-closed discipline: a canary probing nothing is false coverage). 4 config tests.
- `api/internal/app/canary_adapters.go`: `k8sCanaryTargetPicker` (CRD list via the workspaceCRDListerClient seam, paginated, in-memory filter Phase==Active && spec.Runtime==class, deterministic oldest/then-name pick) + `k8sCanaryPodResolver` (agentdEndpoint twin: CRD PodIP + workspace-pw Secret + agentd.AgentdPort; no state-store cache — 60s cadence doesn't need it) + `newCanaryService` (nil unless enabled). 6 adapter tests (fakes; pagination, determinism, tie-break, RBAC/empty-secret failure modes).
- `app.go`: `canarySvc` field, constructed behind the knob, started in Run() after the janitor block (janitor lifecycle — root-ctx cancel in Shutdown). Every replica may run it (idempotent probes; per-replica series expose replica-local network paths).
- `helm/values.yaml` + `api-deployment.yaml`: `api.canary.{enabled,interval,timeout,classes}` → env rendered ONLY when enabled (inert default verified by render); enabled render emits all four vars (interval/timeout conditional). Render verified with synthetic delivery-image digests (the chart's mandatory-pin gate) at kube-version 1.35.

---

## Key Decisions

1. **Centralized loop-gauge registration in pkg/obs** (vs #1322's per-package constants+registration): the second API-side consumer makes per-package registration an init-panic; one owner per binary is the only stable shape. Coordinated cross-stream rather than touching outbox files (their PR is open).
2. **`resolve/not_found_error` is a first-class outcome**: on pre-1a main the resolve leg reads S6-violated by design — that red baseline is the canary's reason to exist ("eyes while the fixes are built"); it flips to `resolved` when 1a lands.
3. **Snapshot leg accepts typed not_found as liveness-ok**: the synthetic session never exists; the pod ANSWERING within budget is the signal. A 2xx with the wrong session id classifies error (drift, not health).
4. **Deterministic oldest-Active pick**: stable target → stable signal; oldest biases to longest-lived pod (least resume churn). Random spread rejected for v1 (signal flapping).
5. **Sequential per-class probing**: worst-case pass = classes × 2 × Timeout (2-3 classes ≈ ≤12s « 60s interval); a slow pass shows honestly as a late loop stamp. Parallelism deferred until class count demands it.
6. **Rule 7 assumptions** (all validated, evidence in the PR description): A1 agentd ABI = PodIP:4097 + Basic "opencode" + workspace-pw Secret (proxy_actions.go:146, proxy_connections.go:94); A2 GetSnapshot unknown session → typed NotFound (sessionstate/service.go:99); A3 answer_question absent input → typed NotFound pre-1a (sessionstate_wiring.go:569-592: question-404 → permission-404 → typed error); A4 answer_question unconditionally declared (sessionstate_wiring.go:466-470); A5 synthetic-id prefix cannot collide (harness ids are generated msg_/ses_ tokens); A6 multi-replica probing safe (read-only + absence-resolve on synthetic ids through the sanctioned Act seam); A7 spec.Runtime is the class selector field (workspace_types.go:116).

---

## Blockers

None. Standing (owner decisions, surfaced in the 0c reserved comment): budget for token-spending probes (real-ask L1, Deliver-path); prod-baseline enable of the knob at deploy time (the chart ships it off).

---

## Tests Run

- `go test -race ./api/internal/services/canary/... ./pkg/obs/...` — ok
- `go test -race -run "TestConfig_Canary" ./api/internal/config/` — ok
- `go test -race -run "TestCanary" ./api/internal/app/` — ok
- `go test -race ./cmd/workspace-agentd/` full package — ok (262s; the obs switch + per-pass stamp re-verified end-to-end)
- `go build ./...`, `golangci-lint run` — clean
- `helm template` enabled/default renders + `helm lint` — correct (inert by default; all four envs when enabled)

---

## Next Steps

1. PR review-iterate to APPROVE (branch `feat/epic71-0c-canary-probe-service`); merge.
2. If #1322 merges after this: their outbox registration must switch to `obs.StampLoopLastRun` (3-line rebase) — else the API binary double-registers. If it merged BEFORE this, this branch's rebase performs that switch on main.
3. 2a/2b consume the classification surface for L3/L4 harness assertions; alerts (staleness + violation counters) wire after L3/L4 green.

---

## Files Modified

- pkg/obs/loopliveness.go (new) + loopliveness_test.go (new)
- api/internal/services/canary/canary.go, metrics.go, client.go, canary_test.go (new)
- api/internal/config/config.go, config_test.go
- api/internal/app/canary_adapters.go (new) + canary_adapters_test.go (new)
- api/internal/app/app.go
- cmd/workspace-agentd/sessionstate_metrics.go, sessionstate_metrics_test.go
- helm/values.yaml, helm/templates/api-deployment.yaml
