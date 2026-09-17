# Worklog: #761 — full pod termination grace + session-aware drain

**Date:** 2026-09-16
**Session:** Implement issue #761 (P1, controller): stop killing in-flight LLM turns on controller-initiated pod deletion — raise terminationGracePeriodSeconds to cover agentd's full shutdown budget, and add a session-aware drain before deferrable deletions.
**Status:** Complete

---

## Objective

Issue #761: two halves.

1. `terminationGracePeriodSeconds=5` (pod_builder.go) short-circuited agentd's outer ~25s+ shutdown budget, destroying in-flight HTTP/SSE connections on every controller-initiated deletion.
2. `deletePodByName` was a plain `r.Delete` on ~7 paths with no session check — active turns were killed mid-stream on suspend, restart-generation bump, arch drift, password-secret heal, and health restart.

---

## Work Completed

### Grace value: 5s → 40s (pod_builder.go)

Validated agentd's shutdown budget from code (assumption A1/A2 below): `runShutdown`
(main.go:488) is SERIAL in the worst case — 25s HTTP drain context + 5s
background-goroutine wait + 5s child SIGTERM→SIGKILL (`managed_process.go`
stop()) = 35s. The old comment treated the 25s context as the whole budget and
argued 5s matched "opencode's inner window" — but the child only receives
SIGTERM AFTER the HTTP drain completes, so the inner windows stack, they don't
overlap. 40s = 35s + 5s margin. Common case is unchanged (~2s — kubelet reaps
once containers exit); the grace only bounds the worst case it exists to
protect. Comment block rewritten with the derivation.

### Session-aware drain (session_drain.go, new)

`drainBeforePodDeletion(ctx, ws, reason)` gates the deferrable delete paths:

- Consults agentd `/v1/statusz` (admin mux, #887 bearer candidates — the
  fetch is shared with `enrichAgentStatus` via `fetchAgentStatusz`;
  `agentStatuszBearers` extracted from the old inline code).
- Idle → proceed. Busy → defer (requeue `drainPollInterval`=10s) while the
  sessions make progress.
- **Progress** = any statusz-snapshot delta between polls: busy-set
  membership, per-session status, per-session ContextUsed. This is the
  statusz-observable proxy for #1342's part-level `lastEventAt` — which
  statusz does NOT expose (verified: `BusyAges` is wall-clock;
  `busyPartitions` is agentd-internal).
- **Force** when no observable progress for `drainStallBound`=60m
  (progress-keyed, not wall-clock-from-start: a delta resets the clock). 60m
  deliberately exceeds documented legitimate turn lengths (30-40 min builds
  — the 2026-09-11 incident class; the owner retired fixed 15-min bounds
  agentd-side for exactly this reason). A forced delete still lands as a
  graceful SIGTERM under the 40s grace, and #1374's transcript repair folds
  orphaned running parts on the next read.
- **Fail open** on unreachable/unhealthy statusz — a dead agentd never
  blocks deletion (this is what makes the password-secret-missing path
  safe: the Secret is gone, the scrape 401s, the recycle proceeds).
- Events: `SessionDrainDeferred` (Normal, once per window),
  `SessionDrainForced` (Warning), `SessionDrainFailedOpen` (Normal).
- Metrics: `llmsafespaces_workspace_drain_deferred_total{reason}`,
  `..._drain_forced_total{reason}`, `..._drain_failed_open_total{reason}`.
- In-memory window (`drainStates`, mirroring `lastDeepStatus`): lost on
  controller restart → window restarts. Documented.

### Path integration

| Path | Drains? | Rationale |
|---|---|---|
| handleSuspending (user/org/idle/timeout suspend) | yes | the issue's primary path |
| handleActive restart-gen bump | yes | "Refresh compute" is deferrable |
| handleActive arch drift | yes | same class |
| handleActive password-secret missing | yes | fails open by construction (no Secret → 401) |
| handleTerminating (terminate) | no | explicit user destroy — intent trumps turns |
| restartAgentPod (health) | no | agent already failed the threshold; drain would at best fail open, at worst delay recovery |
| suspendFromPreActive | no | pre-Active pods proxy 503 — no user turns in flight |

Deferral semantics per path: suspend stays in `Suspending` (established SSE
streams keep flowing — only NEW requests 503); restart-gen/arch/pw-missing
stay Active with `ObservedRestartGeneration` NOT advanced (the bump is
re-consumed after the drain).

### Pre-existing flake fixed (Rule 5)

`TestOutboxDeliver_V2UnhappyPaths/admission_transport_cut` failed ~1-in-3
full-suite runs (reproduced 2/7, diagnosed, fixed, then 8/8 green). Root
cause: `shrinkOutboxTimers` sets `DeliveryTimeout=40ms`; under suite load the
admission POST starved past the deadline BEFORE being sent → pre-send
`context deadline exceeded` → correctly classified ambiguous/verifying, but
`admits==0` flaked the "exactly one admission attempt" assertion. Fix: the
test now grants an honest 5s delivery budget (every backend in it answers or
cuts in microseconds; nothing semantic depends on the shrunk value).

Also fixed under Rule 5 (lint, all pre-existing, surfaced after the first
`make lint` cache warmed): gosec G202 on a package-constant SQL concatenation
in `database.go` (resolved by extracting the query so the sql call takes a
plain identifier + a `#nosec` annotation stating why), and 7 redundant
`// +build` lines in integration test files (kept `//go:build` only).

### Adversarial self-review (Rule 11) — findings fixed

1. **Drain state keyed by bare workspace name** collided across namespaces
   (two tenants each with workspace "demo"). Fixed: `drainKey` =
   `namespace/name`.
2. **Stale window across pod recreation**: opencode session IDs survive pod
   recreation (session DB on the PVC), so a new pod's busy snapshot can match
   the dead pod's byte-for-byte and inherit its progress age → spurious
   force. Fixed: the window is pinned to the pod IP and resets on identity
   change (`TestDrain_PodRecreationResetsWindow`).

False alarms (validated, not fixed): requeue-only deferral losing ticks on
controller crash (same trust model as every other RequeueAfter path —
controller restart re-reconciles from the initial list); health checks
skipped during an Active-phase deferral (the drain's own statusz poll every
10s is the liveness signal; a dead agentd flips the drain to fail-open
immediately); drain-state entries outliving a Failed workspace (bounded map,
mirrors the `lastDeepStatus` precedent, cleared on terminate).

### Refactor

`enrichAgentStatus`'s bearer assembly + statusz GET extracted into
`agentStatuszBearers` + `fetchAgentStatusz` (shared with the drain;
deep-status keeps its 30s client, the drain uses 10s).

---

## Assumptions (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | agentd outer shutdown is serial: 25s HTTP ctx + 5s bg wait + 5s child window = 35s worst case | Read `cmd/workspace-agentd/main.go:488-524` (runShutdown) + `managed_process.go:426-456` (stop) |
| A2 | Grace 40s covers A1 + 5s margin; common case ~2s unaffected (kubelet reaps on container exit) | A1 + k8s termination semantics; pinned by `TestPodBuilder_TerminationGracePeriod_CoversAgentdShutdownBudget` (35 ≤ grace ≤ 60) |
| A3 | statusz does NOT expose #1342's progress signal; BusyAges is wall-clock | Read `pkg/agentd/types.go:237-276`, `server.go:154-166`, `session_tracker.go:103-129` — hence step-level snapshot deltas as the controller's progress proxy, bound 60m vs agentd's 30s part-level `LeaseConvergenceBound` |
| A4 | Password-secret path composes with fail-open (missing Secret → no bearers → 401 → proceed) | Read `statuszWithBearers` (health.go) empty-candidates branch; pinned by `TestDrain_PasswordSecretMissingFailsOpen` |
| A5 | Pre-Active pods cannot have user turns in flight | `api/internal/handlers/proxy.go` not-Active 503 path (cited in issue #761) |
| A6 | Prior design resurrected from issue #761's implementation comment (4 deferrable paths, statusz consult, fail-open, metrics). The task's `gh pr diff 791` pointer leads to an UNRELATED agentd session-tracker PR (verified: #791 touches only api/ + cmd/workspace-agentd/) — its `sessions: [{id,status}]` statusz shape was still useful confirmation | `gh pr view 791`; issue #761 comment 5260612204 |
| A7 | A drain bounded below legitimate turn lengths recreates the force-kill (why 60m, progress-reset) | #1342 commit 9d983a1c: owner retired 15-min wall-clock maxDefer after 30+ min turns were force-killed |

---

## Testing (TDD — tests written first, red confirmed, then green)

`session_drain_test.go` (new, 16 tests): decision matrix (idle→proceed,
busy→defer/requeue with event+metric once per window, busy→idle flip,
progress-extends-window, stalled-beyond-bound→force with event+metric,
unreachable→fail-open, unhealthy statusz→fail-open, empty PodIP skips consult,
busy-set churn counts as progress, pod-recreation resets the window) +
per-path integration (suspend flow defers then completes, restart-gen defers
and does not observe the generation, arch-drift defers, password-missing
fails open, terminate does not consult, health restart does not consult nor
create state, terminating clears state). Grace pinned by the rewritten
pod_builder test. All verified red-first (compile failure on the missing
drain symbols), then green.

- `go build ./...` green
- `go test ./...` green (full repo; one unrelated agentd subprocess test
  failed once in the window where the build disk was 96% full — not
  reproducible in 4 subsequent runs, package untouched by this diff)
- `make lint` green (0 issues)

---

## Not implemented (out of scope / by design)

- SSE phase-change events to ALL users with workspace access (issue item 3) — API-server concern, was already deferred in the prior design; not part of this controller PR.
- agentd-side changes (statusz progress exposure) — #1374's territory; the controller composes with what statusz exposes today.
