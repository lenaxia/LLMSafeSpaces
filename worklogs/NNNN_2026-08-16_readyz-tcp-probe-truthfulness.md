# Worklog: probe truthfulness — readyz TCP semantics + starvation-proof probe budgets (#892 D4)

**Date:** 2026-08-16
**Session:** Implement design 0050 D4 — kubelet probes must detect death, never slowness. Branch `fix/892-d4-probe-truthfulness`, stacked on PR #894, tracking #892.
**Status:** Complete

---

## Objective

The 2026-08-15/16 incident included kubelet as a second, blunter watchdog: `/v1/readyz` performed synchronous opencode HTTP (`cachedState` under `cache.mu`, refreshing on TTL expiry), timed out under CPU starvation, and the startup probe built on it (120×1s budget) expired mid-boot under quota contention — kubelet killed containers (events: `Startup probe failed: context deadline exceeded` → `Killing`), feeding the 6-restart churn.

---

## Work Completed

### agentd (cmd/workspace-agentd)

- `server.go` `buildReadyzHandler(deps, readyChecker)`: Ready = cache initialized AND `readyChecker()` — production wires `opencodeTCPReady()`, a kernel-level TCP connect to opencode's port (2s timeout). The kernel completes handshakes for a listening socket from the accept backlog regardless of event-loop health: refused = booting/dead (hold), accepted = can take traffic. No opencode HTTP, no agentd event-loop work beyond the handler itself — starvation-immune by construction. nil checker preserves legacy semantics (tests, partial wiring).
- Provider info in the response now comes from `providerCache.lastKnown()` (mutex read, never fetches) instead of `cachedState` — readyz can no longer block for seconds on opencode. `RelayInjected`/gate-recording behavior unchanged.
- G5 verification: `/v1/healthz` confirmed process-only and lock-free (US-22.1 contract intact; no change needed there).

### controller (controller/internal/workspace)

- `pod_builder.go` probes: readiness 5s/5s/×12 (60s budget); startup 5s/×36 → 180s budget with 3s timeout (covers boot-to-listen under a saturated 2-CPU quota, which the old 120s budget did not); liveness 10s period/10s timeout/×8 (~80s grace) on the lock-free healthz.
- `pod_builder_test.go`: tests updated to the new budgets, with the D4 rationale in doc comments; startup test now asserts `threshold×period ≥ 180s` rather than bare threshold.

### Tests (agentd)

- `readyz_d4_test.go`: TCP-accepting-but-never-accepting listener ⇒ Ready (the starved shape — HTTP would time out); refused port ⇒ 503 (boot window held); uninitialized cache ⇒ 503; nil checker ⇒ legacy semantics; hanging-opencode proof that readyz answers with zero opencode HTTP round-trips; `lastKnown` no-fetch semantics.

---

## Key Decisions

- **TCP listener as the readiness signal** rather than "process exists": keeps the boot window closed (refused until opencode binds) while being immune to event-loop starvation. A "process exists" check would expose a booting opencode to traffic.
- **lastKnown providers over cachedState:** reporting slightly stale provider info on a health endpoint is free; blocking the probe path on a synchronous fetch is the incident.
- **180s startup budget:** the incident exceeded 120s booting under quota saturation; 3 minutes covers observed worst cases without meaningfully delaying detection of a genuinely dead boot (crash recovery restarts the child far faster).

---

## Assumptions (stated + validated)

1. Kernel completes TCP handshakes for a listening socket with a full backlog (drop ≠ refuse). **Validated:** kernel-documented behavior; `TestReadyz_TCPListenerReady_EvenWhenHTTPWouldStarve` demonstrates the never-accepting listener answering Ready.
2. No consumer depends on readyz meaning opencode responsiveness. **Validated:** consumers are kubelet probes (this change), the API relay-state checker (reads `RelayInjected` body field only), and the post-restart diagnostic probe (any-200) — `grep -rn readyz` across api/, pkg/, cmd/ (non-test).
3. Liveness timeout 10s is safe because healthz is lock-free. **Validated:** handler body — no mutexes, no opencode calls (US-22.1); the only file I/O is a bounded tmpfs read.

---

## Blockers

None. G7 stress harness will exercise the probe budgets under a real CPU storm before release.

---

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestReadyz|TestProviderCache_LastKnown' -count=1 -race` — green
- `go test ./controller/internal/workspace/ -run TestPodBuilder -count=1` — green
- `go build ./... && go vet ./... && gofmt -l` — clean

---

## Next Steps

- D5 elapsed-time badges on running tools (frontend)
- D3 durable prompts (after G4 drain audit); G7 stress harness as the merge gate

---

## Files Modified

- cmd/workspace-agentd/server.go
- cmd/workspace-agentd/session_tracker.go
- cmd/workspace-agentd/readyz_d4_test.go (new)
- controller/internal/workspace/pod_builder.go
- controller/internal/workspace/pod_builder_test.go

---

## Round 2 corrections (review on #895)

- **Ship-blocker fixed:** `opencodeTCPReady()` dialed `getAgentAddr()` —
  a URL (`http://localhost:4096`) — as a raw TCP address; `net.Dial` on
  that form fails unconditionally ("too many colons"), so readyz would
  have 503'd forever and the 180s startup probe would kill every
  workspace container in a loop. The checker is now parametrized on a
  host:port addr; production wires `127.0.0.1:<AgentPort>` (mirroring
  watchdog_vitals.go). Regression: production-form dial against a live
  listener returns true while the global agent addr holds the URL form
  (and the URL-form dial is asserted to fail as a sanity on the bug
  class).
- **Admin-server wiring test added:** requireBearerToken + readyz +
  opencodeTCPReady composed as production registers them — 401 on
  missing/bad token, 200 + Ready against a never-accepting (starved)
  listener.
- **Design 0050 amended:** startup budget ×36/180 s (shipped) vs the
  draft's ×30/150 s — deviation recorded in the doc per review.
- gofmt clean.


---

## Round 3 corrections (review rounds 3–4 on #895)

- **Round-2 worklog falsely claimed gofmt clean twice** — the branch
  carried a `make fmt-check` violation (pod_builder_test.go string-concat
  spacing) through four rounds. Corrected; `make fmt-check` now passes.
- **"Starvation-immune by construction" was overclaimed:** `lastKnown()`
  originally took `providerCache.mu` — the same mutex `cachedState`
  holds across three synchronous opencode HTTP calls on TTL expiry (up
  to ~15s under the exact starvation this PR targets, with the
  controller's deep-status poll hitting statusz every ~60s). readyz could
  block past the 5s readiness / 3s startup timeouts behind a concurrent
  statusz fetch. Fix: `providerReadySnapshot` behind an
  `atomic.Pointer` (mirroring healthzCache), written under mu on every
  cache update, read lock-free by lastKnown. Regression:
  `TestProviderCache_LastKnownNeverBlocksOnCacheMu` holds mu in another
  goroutine and asserts lastKnown answers (deadlocks pre-fix).
- **Healthy-decoupling pinned:** `TestReadyz_HealthyDecoupledFromChecker`
  asserts 200 with Initialized=true, Healthy=false, checker=true — a
  regression to `Initialized && Healthy && checker()` passed every prior
  test and would reintroduce starvation flap through the back door.
- **Probe timeouts pinned exactly** (startup 3s, liveness 10s + period
  10s + threshold 8): the incident's literal failure mode was a probe
  timeout; floors alone were satisfied by the old values.
- Stale managed_process.go healthCheckURL comment updated to D4
  semantics; the no-fetch test's mechanism comment corrected (raw
  http.Get has no client timeout — the go-test binary deadline is the
  backstop).
