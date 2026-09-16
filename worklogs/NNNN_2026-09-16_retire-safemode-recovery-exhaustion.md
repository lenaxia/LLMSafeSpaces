# Worklog: #760 — retire SafeMode; recovery-exhaustion escalation

**Date:** 2026-09-16
**Session:** Implement the reframed issue #760 (P2, controller): delete the write-only SafeMode machinery and replace it with a stateless, derived recovery-exhaustion escalation — per-class thresholds (Infrastructure non-zero: the Longhorn silent-loop fix), a `RecoveryExhausted` condition, a warning K8s Event on the crossing, and one alertable counter.
**Status:** Complete

---

## Objective

Issue #760 as reframed by the owner on 2026-09-16: SafeMode is a write-only
signal — nothing consumes it (no alerts when it mattered, no API/DTO/SDK
surface, no behavioral gate; retry pacing is `NextRetryAt` backoff and the
#699 `Spec.Suspend` hatch). Worse, the one class that most needs the signal
(`FailureClassInfrastructure`) had `SafeModeAfter=0` and could never trip it —
the Longhorn silent infinite-retry loop.

Two halves:

1. **Remove** the SafeMode machinery (policy field, both entry paths, the
   condition, the Active/Entries/Exits metrics funnel, the exit paths, the
   CRD `status.safeMode` field).
2. **Replace** with derived escalation: `RecoveryExhausted` condition derived
   from `ConsecutiveFailures` crossing a per-class threshold, one warning
   Event per episode on the crossing, one counter increment per episode.

---

## Work Completed

### Removal inventory

- `recovery_policy.go` — `SafeModeAfter` field → `ExhaustionAfter`;
  `shouldEnterSafeMode` → `recoveryExhausted`; the `enterRecovery` SafeMode
  block → `markRecoveryExhausted` (crossing-only). New `clearRecoveryState`
  helper centralizes every ConsecutiveFailures reset (previously 4 open-coded
  copies) and drops the derived condition with the counters.
- `health.go` — `maybeEnterSafeModeFromRestarts` +
  `controllerRestartSafeModeThreshold` (5) deleted; `restartAgentPod` no
  longer evaluates the persistent-unreachability trigger.
  `removeCondition` converted from method to free function (its only
  remaining callers are package functions).
- `metrics.go` / `metrics_wiring.go` — `WorkspaceSafeModeActive`,
  `WorkspaceSafeModeEntriesTotal`, `WorkspaceSafeModeExitsTotal` deleted.
  `WorkspacesFailedTotal` deleted too: its only writer was SafeMode entry
  (worklog 0377 H7 — it never measured terminal Failed phase); its alert and
  dashboard panel are repointed at the honest replacement (see below).
- `phase_creating.go` / `phase_terminating.go` — SafeMode exit blocks
  (restartGeneration bump, termination) deleted.
- `main.go` — gauge seeder no longer seeds `WorkspaceSafeModeActive`.
- `pkg/apis/.../workspace_types.go` — `status.safeMode` field removed;
  `WorkspaceConditionRecoveryExhausted` + `ReasonRecoveryExhausted` added.
  `make deepcopy` re-run: no generated diff (bool field).
- `helm/crds/workspace.yaml` — `status.safeMode` property removed
  (hand-trimmed CRD, not controller-gen output; schema-drift test
  `TestCRDSchemaMatchesGoTypes` enforces the match).
- SafeMode-era tests deleted with the machinery (controller_restart_test,
  gauge_drift_test, metrics_wiring_test rewrites).

### Replacement semantics (stateless, derived)

- **Threshold table** (one place, beside the backoff policy table):
  Infrastructure=10, Resource=6, Process=6, Configuration=3.
  Resource/Process/Configuration reuse the old `SafeModeAfter` defaults.
  Infrastructure=10: infra backoff is 5s→2m (2x), so 10 failures span ~12
  minutes of retries — long enough to ride out transient kubelet/PVC/CSI
  hiccups (which typically resolve in 1–2 retries) while still escalating
  permanent loops. The original #760 fix branch proposed 30m/10 attempts;
  10 consecutive failures is the attempt-count equivalent.
- **Crossing** (in `enterRecovery`, post-increment): set the
  `RecoveryExhausted` condition (True, reason `RecoveryExhausted`, message
  names the class + count + states retries continue + points at
  `spec.suspend=true` #699), emit one warning Event via the recorder
  (#1392 `ReasonPVCCleanupDelegated` precedent), Inc
  `WorkspaceRecoveryExhaustedTotal{failure_class}` (#1392/#1395 pattern:
  low-cardinality counter, one label).
- **No double-fire**: the Event/counter fire only on the episode's
  not-exhausted→exhausted transition (`markRecoveryExhausted` returns the
  crossing). A mid-episode class switch does not re-fire — the counters are
  class-agnostic, so the crossed condition stands until state resets.
- **Clearing**: everywhere `ConsecutiveFailures` resets — 2-minute stability
  window (`maybeResetConsecutiveFailures`), restartGeneration bump, suspend,
  Failed-phase recovery paths — via `clearRecoveryState`. The condition never
  outlives the counters it is derived from.
- **Not a gate**: retries continue under backoff (no behavior change — same
  as SafeMode, which never gated anything either).

### Observability follow-through

- `helm/templates/prometheus-rules.yaml` — `LLMSafeSpacesSafeModeActive`
  (queried the deleted gauge) removed; `LLMSafeSpacesWorkspaceFailures`
  repointed at the new counter with corrected semantics (its old query hit
  the SafeMode-entry counter; the alert text claimed "entering Failed
  state" — wrong since 2026-06, per worklog 0377 H7).
- `helm/dashboards/operational.json` — SafeMode stat panel →
  "Recovery-Exhausted Episodes (1h)"; the failed_total timeseries →
  exhausted_total by failure_class.
- `helm/chart_test.go` — alert list updated accordingly.
- `docs/architecture/lifecycle.md` — mermaid + class table now carry the
  exhaustion thresholds (the old table omitted Infrastructure entirely);
  SafeMode section rewritten as Recovery exhaustion (#760); counter-reset
  section mentions the condition clear.
- `docs/reference/crds.md` — `safeMode` row removed.

### Storage note (CRD)

`status.safeMode` was an optional bool with no consumers. Objects written by
older controllers may carry it in etcd; the CRD no longer declares it, so
the API server prunes it on the next status write (any writer). Pinned by
`TestEnvtestRecoveryExhausted_SafeModePrunedOnWrite` against a real API
server + the shipped CRD schema.

---

## Key Decisions

1. **Delete `WorkspacesFailedTotal` rather than rewire it.** Its only writer
   was the SafeMode entry; keeping a "failed" counter that measures
   exhaustion perpetuates the H7 mislabel. The alert slot it fed is
   repointed at the new counter under corrected text. (Validated: no other
   writer exists — `rg WorkspacesFailedTotal` hits only worklogs.)
2. **Infrastructure=10, not 6.** Infra backoff (5s base, 2m cap) retries
   ~2.5x faster than Process/Resource (10s base); equal wall-clock patience
   requires more attempts. 10 attempts ≈ 12 minutes, past the window where
   transient kubelet/PVC events resolve.
3. **Class switch mid-episode does not re-fire.** `ConsecutiveFailures` is
   class-agnostic; re-firing on reclassification would double-count one
   episode and double-page the operator. The condition message names the
   class of the crossing failure.
4. **Health-check restart path (persistent unreachability) deliberately
   produces no exhaustion signal.** That path is not a classified failure
   episode and already has `WorkspaceControllerRestartsTotal` +
   `HighConsecutiveFailures`-adjacent coverage; escalating from restarts
   alone reintroduces a second, drifting counter. Pinned by
   `TestControllerRestart_5Consecutive_NoEscalationSignal`.
5. **Suspend clears the condition** (old code preserved SafeMode across
   suspend to feed a TTL carve-out in `handleSuspended` that was never
   built). The derived condition must not outlive its counters.

### Assumptions

- A1: no external consumer reads `status.safeMode` or the
  `WorkspaceSafeMode*` metrics. Verified: zero hits in frontend/, api/,
  SDKs, dashboards, alert rules after this change; issue reframe comment
  confirms ("no alerts/dashboards reference WorkspaceSafeMode*").
- A2: `status.safeMode` is safe to drop from the CRD (optional bool, API
  server prunes on write). Verified via envtest against the shipped CRD.
- A3: the #699 suspend hatch is the operator remedy — the condition and
  Event message say so explicitly.

---

## Review Round 1 (AI reviewer, CHANGES_REQUESTED → addressed)

All three findings validated REAL (independently re-verified before fixing):

1. **Must-stay-gone pins** — the repo's chart_test convention (retired
   alerts `require.False`d) wasn't applied to `LLMSafeSpacesSafeModeActive`.
   Added: absence pins for that alert and for the five retired metric names
   in the rendered PrometheusRule + dashboard ConfigMap data.
2. **Writerless alert** — `LLMSafeSpacesHighConsecutiveFailures` queried
   `llmsafespaces_workspace_consecutive_failures_max`, which has no producer
   anywhere in the tree (dead since introduction; the "Next Steps" note in
   the first commit deferred it — the reviewer correctly refused the deferral
   for a file this PR edits). Deleted the alert with a supersession comment;
   its intent is served by the exhaustion counter. Chart test pins its
   absence.
3. **Stale story contracts** — epic-24 story docs still prescribed SafeMode
   entry as live contract. Added #760 supersession/amendment notes to the
   epic README, US-24.7 (AC 3), US-24.13, and TESTPLAN.md.

Missing test cases from the review, added:

- `TestEnterRecovery_ReExhaustionAfterClear_FiresAgain` — episode semantics:
  after a full clear, a second crossing re-fires condition + Event + counter.
- `TestEnterRecovery_ClassSwitch_ConditionKeepsCrossingClass` — the persisted
  condition message names the class that crossed, not the latest failure's.

---

## Blockers

None.

---

## Tests Run

- Red-first: the new `recovery_exhaustion_test.go` was written against the
  absent `ExhaustionAfter`/`markRecoveryExhausted` surface, then driven
  green. Matrix: per-class threshold boundaries (incl. below-threshold),
  the Infrastructure Longhorn regression pin (unit + integration),
  condition set/clear (stability window, restartGen bump, suspend,
  Failed-path), Event emitted (reason + remedy text), counter advanced +
  no-double-fire (same class and mid-episode class switch), health-restart
  path sets nothing.
- `go build ./...` — pass.
- `go test ./...` (full repo incl. helm chart tests) — pass.
- `golangci-lint run` — 0 issues.
- `go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestRecoveryExhausted`
  (KUBEBUILDER_ASSETS=v1.31.0) — 2/2 pass (CRD round-trip + safeMode pruning).
- `make install-hooks` — installed.
- `make deepcopy` — no generated diff.

---

## Next Steps

- AI review loop on the PR; address validated findings with regression tests.
- Successor issue candidates noted during review: FailedMount container-reason
  classification (still falls to Process — descoped by the owner reframe);
  these are recorded in the PR thread, not blocking #760.
