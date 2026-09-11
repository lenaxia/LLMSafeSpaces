# Worklog: Epic 71 / 0c (part 1) — canary groundwork: loop-liveness gauge + probe design

**Date:** 2026-09-11
**Session:** Stream 0c kickoff (#1312 canary skeleton, metrics-only). Landed the dead-loop-detection piece (the epic's explicit Wave-0 addition) and recorded the probe design in the reserved comment.
**Status:** In Progress

---

## Objective

Claim and begin #1312's canary skeleton (epic-71 / 0c): metrics-only canary + "every periodic loop exports a last-run gauge — dead loops must be detectable."

---

## Work Completed

### Loop-liveness gauge (landed slice)
- `llmsafespaces_loop_last_run_timestamp_seconds{loop}` GaugeVec on the agentd `:4098` scrape surface; `loop="reconcile_watchdog"` stamped at END of each pass (review r1: initially placed mid-pass, contradicting the "last COMPLETED pass" Help contract — moved). The stale-stamp signal covers HANGS/wedges (a pass that wedges anywhere freezes the stamp at the previous pass); a PANIC kills the process and the whole scrape surface — detection of that is the scrape's own absence, not this gauge (review r1 worklog correction).
- The gauge's Help text is the CONTRACT for the other loops: 0b's parked-error sweeper and 2a's pending-lease loop export the same family with their own `loop` label (the epic's "every periodic loop" rule, made mechanical).
- Pinned: scrape-completeness row (the metric must appear on the real promhttp surface) + watchdog e2e assertion (the gauge is set by the LOOP, not a direct call).

### Probe design (recorded in the 0c reserved comment; summary here)
- **Driver lives API-side** (a background `Run(ctx)` service, jwtSessionJanitor lifecycle pattern): the canary is cross-workspace by charter; the API owns workspace resolution, the §D1 transport, and the prometheus registry.
- **Metrics-only skeleton probe, zero model spend:** per configured workspace class — (1) `GetSnapshot` round-trip (snapshot-path liveness + the I12/p99<250ms budget), (2) `Act(answer_question, synthetic input id)` → exercises 1a's resolve-by-absence end-to-end (S6) without running a turn (the 404 path short-circuits before any model call), (3) outcome counters + duration histograms with #1312's failure classification (which invariant, which leg). Real-ask L1 probing and Deliver-path probing (token spend) stay gated behind their waves.
- **Transport seam:** `abiclient` over the §D1 Basic transport (`newUsageStreamClient` twin) + an injected `WorkspaceTargetResolver` (workspaceID → baseURL/password) so tests fake it and production adapts the pod-IP resolver.
- **Enable knob:** off by default (nil-service gating in app.go, the established pattern); prod baseline flips it via config.

---

## Key Decisions

1. Liveness gauge FIRST (before the probe): it is the piece every other loop needs to conform to, it is independently landable, and the epic text calls for it explicitly.
2. The skeleton probe is free (snapshot + absence-resolve): a canary that spends tokens per tick needs a budget decision that is not mine to make — surfaced for the owner in the reserved comment.
3. "Workspace class" = configured selector list (v1: runtime environment name per class); no class concept exists in the codebase today, and inventing a taxonomy would be speculative.

---

## Blockers

None.

---

## Tests Run

- `go test -race ./cmd/workspace-agentd/` full package — ok (263s), including the two new pins.

---

## Next Steps

1. API-side `canary` service: Config{interval, classes[], enable}, the resolver seam, probe runner with classification; TDD against fakes (abiclient stub + resolver stub), wiring in `app.go` behind the knob, config + helm values plumbing.
2. 0b (landed meanwhile?) and 2a wire their loops into the `loop_last_run` family on landing.
3. After L3/L4 green (Wave 2): alert wiring on gauge staleness + violation counters — explicitly out of skeleton scope.

---

## Files Modified

- `cmd/workspace-agentd/sessionstate_metrics.go`
- `cmd/workspace-agentd/sessionstate_metrics_test.go`
