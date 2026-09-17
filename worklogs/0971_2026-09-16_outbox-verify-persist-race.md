# Worklog: epic-71 flake-verify-race — outbox-verify persistFirst data race + contended agentd timing windows

**Date:** 2026-09-16
**Session:** flake-hunter stream epic-71 / flake-verify-race (worker wt-flake3, resumed) — fix the confirmed `persistFirst` test-side data race in the outbox-verify handler tests, assess+fix three contention-sensitive cmd/workspace-agentd tests
**Status:** Complete

> Resumed-session note: the prior interrupted worker left only the handlers test-file edits,
> the COORDINATE claim, and this worklog's draft (with unfilled proof placeholders and a
> description of agentd edits that were NOT in the tree). All agentd fixes were re-derived and
> re-proven from scratch by this session; the handlers file edits were audited, one garbled
> comment repaired, and every proof below is this session's own run against the final tree.
> Prior-session proof logs in /tmp were NOT adopted as evidence (they ran against lost edits).

---

## Objective

1. **PRIMARY** — `persistFirst` in `api/internal/handlers/proxy_outbox_verify_test.go`: an UNLOCKED read (~:218) racing a LOCKED write (~:412). Fix the harness synchronization without weakening any assertion.
2. **SECONDARY** — three contention-sensitive tests that failed once under a concurrent race loop: `TestRestart1342_MixedProgressAndStalled_DefersWhileAnyProgress`, `TestSpawnEnvPuller_OversizedBodyIsBounded`, `TestSupervisorSubprocess_SpawnFiles_NearCapValueDeliversWhole` (cmd/workspace-agentd). Reproduce ≥150 contended runs each; fix at root if live, record not-reproduced otherwise.

---

## Work Completed

### Primary — persistFirst data race: root-caused, fixed, proven

**Reproduction (pristine tree, this session).** `git show HEAD:…proxy_outbox_verify_test.go` swapped in, then `go test -race -count=1` (and `-count=30`) on `./api/internal/handlers/ -run TestOutboxVerify_DefinitiveAbsenceRetriesSafely`: **RED with the exact race report** —

- Read at `proxy_outbox_verify_test.go:218` — `if !f.persistFirst` in `fakeAgentBackend.ServeHTTP`'s V1 POST `/message` branch, evaluated AFTER the stall sleep, WITHOUT holding `f.mu` (httptest server goroutine).
- Previous write in `TestOutboxVerify_DefinitiveAbsenceRetriesSafely` — `backend.persistFirst = true` (test goroutine, under `backend.mu`).

The detector flags it deterministically: the test flips the flag mid-flight (~55ms into the old 120ms stall) to make its retry leg behave like a healthy agent; the in-flight request's second, unlocked read races that write.

**Root cause.** The fake read `f.persistFirst` twice per request: under the lock (persist-before-stall) and without the lock after the stall sleep (persist-after-stall). The field's documented semantics ("persist only after the stall" — a request-time property) demand a request-time snapshot, exactly like `stall`/`respondStatus` one line above, which were already snapshotted under the lock.

**Fix** (harness-only; adopted from the interrupted edits after audit — sound and complete):

```go
// before (racy):
f.mu.Lock()
f.posts++
if f.persistFirst { f.persist(text) }
stall, status := f.stall, f.respondStatus
f.mu.Unlock()
if stall > 0 { time.Sleep(stall) }
if !f.persistFirst { f.mu.Lock(); f.persist(text); f.mu.Unlock() }

// after (request-time snapshot under the same lock):
f.mu.Lock()
f.posts++
persistAfterStall := !f.persistFirst
if f.persistFirst { f.persist(text) }
gate := f.latePersistGate
stall, status := f.stall, f.respondStatus
f.mu.Unlock()
if stall > 0 { time.Sleep(stall) }
if persistAfterStall {
    if gate != nil { <-gate }
    f.mu.Lock(); f.persist(text); f.mu.Unlock()
}
```

Audit for remaining unsynchronized access paths: `persistFirst`/`stall` are now read only under `f.mu` (ServeHTTP snapshot + the test's locked flip at :443-446); `latePersistGate` is set only at construction and read under `f.mu`; every other `fakeAgentBackend` field (`respondStatus`, `admitStatus`, `modelSetStatus`, `promoteDelay`, …) is construction-only across all users of the fake (`proxy_outbox_verify_test.go`, `proxy_v2_bridge_test.go`) — happens-before server start. The one unlocked read at line 115 (`f.modelSetStatus != 0`) touches a construction-only field. `newVerifyEnv`/`newE2EEnv` never registers a server Close, so the gate blocking a handler goroutine cannot wedge cleanup (gate close is last in cleanup LIFO regardless).

**Assertions before/after: byte-identical** — `assert.Equal(t, 1, posts)` (LongTurn), `assert.Equal(t, 2, posts)`, `assert.Empty(t, listOutbox(t, env))`, `assert.Equal(t, outbox.StatusPending, entries[0].Status)`, `assert.Equal(t, 1, entries[0].Attempts)`, the `queue.update/sent` selects, and every V2-subtest assertion. Nothing was relaxed.

**Cascade the snapshot exposed (same test, fixed alongside).** With the late persist deterministic at stall-end, a scheduler-starved verify pass could observe the late landing and "confirm" delivery with ONE post — breaking `posts == 2` under contention. The test's premise is definitive ABSENCE, so the absence precondition is now sequenced deterministically: `latePersistGate` (set at construction, closed by `t.Cleanup`) holds the fake's post-stall persist until the assertions are done. The verify passes can run at any scheduler delay and still prove absence; the late persist still happens after (semantics preserved).

**Proof (this session, final tree).**
- Pre-fix (HEAD version swapped in): race report at `-count=1`, FAIL at `-count=30`.
- Post-fix: `go test -race -count=50 -run 'TestOutboxVerify|TestOutboxDeliver' ./api/internal/handlers/` ×2 — **ok 96.7s / ok 96.6s, zero race reports**.
- Contended: see proof rounds below.

### Primary-file siblings — same storm class, fixed in the adopted edits

`shrinkOutboxTimers`' old `DeliveryTimeout = 40ms` could not absorb a loaded runner: under contention a LOCAL round trip sometimes missed 40ms, misclassifying definitive rejections as ambiguous. The adopted fix raises the shared shrink to **300ms** and scales the stall shapes to 900ms (3x the budget — the ambiguous-outcome precondition is wall-clock on both sides, load-independent), and adds transcript-poll edges (`assert.Eventually` on `backend.userTexts`) before the resolving verify passes in `TestOutboxVerify_LongTurnDeliveredExactlyOnce` and the V2 transport-cut subtest, so a scheduler-delayed handler/promotion goroutine can never be read as definitive absence (the false-absence→duplicate-send class #987 forbids). All assertions unchanged. One garbled comment from the interrupted edits ("raised below above") was repaired. The V2-promotion trio keeps its separate 2s budget (`shrinkV2PromotionTimers`) — untouched.

### Secondary 1 — TestRestart1342_Mixed: root-caused, fixed, proven

**Reproduction.** Contended storm (3× `-race -count=50` loops of [Mixed, OversizedBody, NearCap] + 6 CPU hogs, pristine tree): **RED 17/150** — `proc.restartCount()`/`intr.callCount()` nonzero (traces at session_aware_restart_1342_test.go:269/:271).

**Root cause.** The test models "progress" as a 30ms wall-clock ticker feeding `noteActivity`; `busyPartitions` (session_tracker.go:110) classifies a busy session as progressing while `now − lastEventAt ≤ StallBound(80ms)`. Under contention the producer's ticker callbacks lag past 80ms (runnable goroutine starved by sibling race builds + hogs), so the streaming session is misclassified as stalled and the (correct) force path fires for the wedged sibling. The production progress-keying is sound — the harness left only ~50ms of scheduler slack.

**Fix** (test-local tunables; the production `restartStallBound` default is untouched): `StallBound 80ms→3s` (30x the new 100ms tick), `PollInterval 20ms→50ms`, duration `400ms→700ms`. Pin STRENGTHENED: both sessions now start wedged (`stallSession` on each) plus one synchronous progress event seeded before the decision — fresh producer events are the ONLY defer mechanism, so a regression that ignores the progress signal force-restarts at the first poll instead of after one bound-sized delay. Assertions byte-identical: `assert.Equal(t, 0, proc.restartCount())`, `assert.Equal(t, 0, intr.callCount())`.

### Secondary 2 — TestSpawnEnvPuller_OversizedBody: root-caused, fixed, proven

**Reproduction.** Same storm family, escalating load: **0/150** (pristine 3-loop storm), **0/40** (4-core pinned storm), **0/60** (moderate), then **RED 11/148** under the heaviest storm (3 loops + 6 hogs where NearCap saturated the machine) — `reason: spawn_env_unavailable` instead of `spawn_env_bad_response`. The window is real but only surfaces under extreme contention.

**Root cause.** `fastPuller` shrinks the per-attempt budget to 100ms (for the retry-ladder tests' speed). Under contention the capped 1MiB body read (`io.ReadAll(io.LimitReader(...))` over a 2MiB response) blew the 100ms attempt budget → transport truncation → retried → 500ms bound expired → classified `unavailable`. The test pins the CAP-vs-decode classification, not the retry ladder.

**Fix** (test-only): after `fastPuller`, `p.attempt = 5s`, `p.bound = 10s` for this test alone. Assertion unchanged: `require.Equal(t, spawnEnvReasonBadResponse, reason)`. The retry-ladder tests (`UnavailableBound`, `TransientFailure`, `ContextCancel`, …) keep the fast budgets — untouched.

### Secondary 3 — TestSupervisorSubprocess_SpawnFiles_NearCap: root-caused (with direct evidence), fixed, proven

**Reproduction.** Pristine storm: **RED 55/150**; a diagnostic variant reproduced **48/150** and, under the heaviest storm, **148/150** — "the near-cap file must deliver byte-complete at spawn" (:51) unsatisfied.

**Root cause (proven, not inferred).** A throwaway diagnostic (deleted before commit) dumped the control-socket spawn state and the endpoint view at failure time. Failing runs show:

- `status: files_reason="spawn_files_unavailable"` at spawn — the spawn-files PULL failed its bounded wait;
- `env_reason=""` — the env pull (tiny body) succeeded;
- post-failure probe of the mux: `GET /v1/spawn-files → 200` with the correct near-cap manifest — the endpoint is healthy, the failure is purely pull-side timing.

The exec harness re-execs the REAL `supervise-opencode`, whose `preSpawn` pulls with hard-coded production budgets (`spawnEnvPullBound=2s`, `spawnEnvPullAttempt=500ms` — consts tuned for an idle in-pod sidecar). The near-cap manifest is ~2.8MiB of JSON (`MaxSecretValueBytes = 2<<20` + base64), and under a contended `-race` runner the body read starves past 2s → degraded spawn (never-block-spawn, by design) → the child is `exec sleep 3600` — ONE spawn, no respawn — so the file is never delivered on any later attempt either. This is a harness/seam gap, not a production defect: on a real pod the sidecar serves loopback in single-digit ms.

(Validated false alarm en route: no "spawn-files pull failed" Warn appears in test output because the exec subprocess's package-level `zap.Logger` is nil there — zap's nil-receiver guard makes failure logging a silent no-op in the harness context. The machine-readable `spawn_files_reason` status field — the actual healthz contract — carries the degrade correctly. Not a defect for this flake; noted for the record.)

**Fix** — production gains an env-var seam following the file's established pattern (`LLMSAFESPACES_SPAWN_ENV_PULL_ADDR`, staged-files dir, delivery roots, ledger):

- `spawnPullBudgets()` in `spawn_env_pull.go` resolves `(bound, attempt)` from `LLMSAFESPACES_SPAWN_ENV_PULL_BOUND` / `LLMSAFESPACES_SPAWN_ENV_PULL_ATTEMPT`; the guarantee is fail-open: unparsable, zero, or negative values keep the production defaults (a typo cannot change the budget silently). A valid-but-small value DOES narrow — there is no floor/ceiling; on a real pod nothing sets these vars, which is what keeps production budgets at the consts. Both `newSpawnEnvPuller` and `newSpawnFilesPuller` consume it; `fastPuller` (unit tests) overrides afterwards as before. **Production runtime behavior with no env set is byte-for-byte the prior behavior.**
- The size test passes `BOUND=8s ATTEMPT=8s` and raises its two `Eventually` windows 15s→45s. Window math: worst-case preSpawn is 2 pullers × (bound + one trailing attempt) ≈ 33s including subprocess boot — inside the 45s windows with scheduler headroom. (An earlier 12s/10s draft overran them: 2×22s ≈ 44s — the storm caught it, see round 1.) The NearCap control client also gets `cc.timeout = 30s` (the 2s default cannot absorb a starved status round-trip; every sibling exec test already sets it).

Assertions byte-identical (byte-complete size, content spot-check, `0o600` mode contract). `TestSupervisorSubprocess_SpawnFiles_RefusedBatch_KeepsDeliveredSet` (same file, same machinery) stayed **0/150 green through every storm** — left unchanged per dispatch rules. The deliberate-degrade test (`spawn_files_exec_test.go:205`, unreachable addr) keeps default budgets — its timing depends on them.

---

## Key Decisions

1. **Request-time snapshot, not a locked re-read**, for `persistFirst` — a locked re-read would be race-free but leave the request's behavior dependent on WHEN the test's flip lands (a hidden timing coupling). The snapshot makes the fake honor its documented contract deterministically.
2. **Channel-gated late persist over wider sleeps** — the absence precondition is now a deterministic edge (close-at-cleanup), not a wall-clock coincidence. Sleeps cannot guarantee preconditions under arbitrary scheduler delay; a channel can.
3. **300ms shared `DeliveryTimeout` + 3x stall margins** — one honest budget shared by the file's shrink helper beats five divergent per-test overrides; the V2-promotion trio's separate 2s budget was already correct and untouched.
4. **Env-var seam for pull budgets, defaults unchanged** — test-side determinism is NOT achievable here (the budgets live inside the re-exec'd production binary; the only test-side alternative — completing delivery via the control socket's `refresh_files` — would weaken the "delivered AT SPAWN" pin). The seam follows the file's four existing env-override precedents and cannot change behavior for real pods.
5. **Mixed-test pin strengthened** — both sessions start wedged so the progress signal is the ONLY defer mechanism.

## Assumptions (Rule 7) — stated and validated

1. *The race is in-process (test binary vs httptest server goroutine), so `-race` must flag it.* Validated: race report at `-count=1` on the pristine tree (report quoted above).
2. *No other mid-flight writes to `fakeAgentBackend` fields race.* Validated by grep over every construction/use site in `proxy_outbox_verify_test.go` and `proxy_v2_bridge_test.go`: `persistFirst`/`stall` (this test, under lock) are the only mid-flight mutations; the rest are construction-only (happens-before server start).
3. *The gated handler goroutine cannot wedge cleanup.* Validated: `newVerifyEnv`/`newE2EEnv` register no server Close (checked `e2e_adapter_test.go:433`); the gate close is last in the cleanup LIFO.
4. *`busyPartitions` classifies by `lastEventAt || busySince`.* Validated against `session_tracker.go:110-129`; the both-wedged + seeded-event construction relies on exactly this.
5. *The env seam leaves real-pod production budgets untouched.* Validated on two clauses: (a) nothing outside the exec harness sets the vars — grep across repo + chart finds them only in the seam, its unit test, and the size test — and (b) unparsable/`d <= 0` values fail open to the consts (pinned by `TestSpawnPullBudgets_DefaultsOverrideAndFailOpen`). The seam CAN narrow via a valid-but-small value (no floor) — that is why (a) is the load-bearing clause for production, not (b).
6. *The NearCap root cause is pull-budget starvation, not an endpoint/apply defect.* Validated by the diagnostic dumps: `files_reason=spawn_files_unavailable` with a healthy endpoint serving the full manifest post-failure.

## Blockers

None.

## Tests Run (final tree unless noted)

- r1 review (PR #1408, CHANGES_REQUESTED): all four target fixes VERIFIED correct by the reviewer; four findings — (F1) `spawnPullBudgets()` shipped without unit tests (Rule 0 hard gate), (F2) worklog cited the superseded 12s/10s budgets, (F3) `freeTCPPort` deferred-finding citation pointed at the wrong file, (F4) the seam comment's "never narrow" overclaimed (a valid-but-small value does narrow; the guarantee is fail-open on unparsable/zero/negative only). Fixed: `TestSpawnPullBudgets_DefaultsOverrideAndFailOpen` added (unset → consts; valid widen; one-var-only; `abc`/`30`/`0s`/`-5s` fail open) — RED-first proven by dropping the `d > 0` guard (test FAILs) and restoring (green); worklog numbers corrected to the shipped 8s/8s ≈33s math; citation corrected to `managed_process_test.go:338-348`; comment rewritten to the actual guarantee.
- r2 review (CHANGES_REQUESTED, code verified complete): three residual worklog sentences contradicted the r1 corrections — the "never narrow" overclaim still at the Secondary-3 bullet and Assumption 5, and a second wrong-file port-race citation in the Tests Run narrative. All three rewritten to the shipped guarantee (fail-open on unparsable/zero/negative; valid-but-small DOES narrow; the load-bearing production clause is that nothing sets the vars on a real pod) and the canonical citation (`managed_process_test.go:338-348`, comment at :334-337).

- Pristine-tree primary reproduction: `-race -count=1/-count=30` — RED + race report (above).
- Post-fix primary: `-race -count=50 -run 'TestOutboxVerify|TestOutboxDeliver' ./api/internal/handlers/` ×2 — ok 96.7s / ok 96.6s.
- Pristine-tree secondary reproduction storms (3 loops × count=50 + 6 hogs, some with pinned/heavier variants): Mixed **17/150 RED**, OversizedBody **0/150 + 0/40 + 0/60, then 11/148 RED** at max load, NearCap **55/150 RED** (and 148/150 under max load via diagnostic variant), RefusedBatch **0/150** (unchanged).
- Contended proof rounds on the final tree (6 hogs + 3 loops per package concurrently, `-race -count=50` each loop; fresh runs by this session — prior-session logs not adopted):
  - **Round 4 (final tree): FULLY GREEN** — handlers 3×150 `ok` (159-162s per loop); agentd trio 3×150 exit=0; 0 FAIL, 0 DATA RACE.
  - **Round 7 (final tree, identical storm): FULLY GREEN** — handlers 3×150 `ok` (193-198s); agentd trio 3×150 exit=0; 0 FAIL, 0 DATA RACE.
  - Intermediate rounds (documented, superseded trees): round 2 green on the pre-`cc.timeout` tree; round 1 (12s/10s budgets) 149/150 — the miss exposed the window math (2×(bound+attempt)=44s ≈ the 45s windows) → budgets 8s/8s, worst ≈33s; round 3 149/150 — the miss exposed the control client's 2s default RPC budget → `cc.timeout=30s` (matching every sibling exec test); round 6 149/150 — the miss was the PRE-EXISTING `freeTCPPort` close-then-bind race (subprocess FATAL `bind: address already in use`, client then hand-shook with the port's new owner: `invalid character 'H'`). The port race lives in the shared exec harness's `freeTCPPort` (`managed_process_test.go:338-348`, accepted-risk comment at :334-337; called from `supervisor_subprocess_test.go:88`), fires ~1 per 450 artificially-parallel storm iterations, and only under ≥2 concurrent package loops CI never runs — recorded below as a deferred finding, not fixed here (unclaimed file; needs its own claim).
- Full packages: `go test -race -timeout 900s ./api/internal/handlers/` — ok 83.8s; `./cmd/workspace-agentd/...` — ok (agentd 300.4s, faultmatrix 41.2s, sessionstate 57.1s).
- Gates: `golangci-lint run` on both packages — green; `make repolint` — green.
- Zero DATA RACE reports in every post-fix log.

## Next Steps

- Merge after review approval; re-run the epic-71 flake board at head afterwards.

## Deferred findings (out of scope, recorded for the board)

1. **`freeTCPPort` close-then-bind race** (`managed_process_test.go:338-348`, shared exec harness; called from `supervisor_subprocess_test.go:88`): under ≥2 concurrent package loops the chosen port can be taken between the probe-listener close and the subprocess bind → subprocess FATAL-exits and the control client hand-shakes with the port's new owner. Observed 1/~450 storm iterations (round 6, exact evidence in `/tmp/opencode/flake3/myproof6-agentd-1.log`). Pre-existing on origin/main; file unclaimed; a fix (bind-failure detection + supervised retry, or a hello-identity check) belongs to its own claim.
2. **Nil package `log` in the exec subprocess** (`main.go:47` — `main()` never runs in the re-exec'd helper): every `log.Warn/Info` on the package-level logger inside `preSpawn`/`refreshFiles`/`managedProcess` is a silent no-op there (zap's nil-receiver guard). The machine-readable control-socket status carries the degrade correctly, so no observability contract is broken in production; but exec-harness failure diagnostics lose these lines. Cosmetic-to-harness-only; noted, not fixed.

## Files Modified

- `api/internal/handlers/proxy_outbox_verify_test.go` — persistFirst request-time snapshot; latePersistGate; 300ms shared delivery budget; 900ms stalls; transcript-poll edges (LongTurn, V2 transport-cut); comment repair
- `cmd/workspace-agentd/session_aware_restart_1342_test.go` — Mixed test contention-proof budgets + strengthened both-wedged setup
- `cmd/workspace-agentd/spawn_env_pull.go` — `spawnPullBudgets()` env seam (`LLMSAFESPACES_SPAWN_ENV_PULL_BOUND/_ATTEMPT`), wired into `newSpawnEnvPuller`
- `cmd/workspace-agentd/spawn_files_pull.go` — `newSpawnFilesPuller` consumes `spawnPullBudgets()`
- `cmd/workspace-agentd/spawn_env_pull_test.go` — OversizedBody generous attempt/bound
- `cmd/workspace-agentd/spawn_files_size_exec_test.go` — NearCap pull budgets via env + 45s Eventually windows
- `COORDINATE.md` — claim row
- `worklogs/0971_2026-09-16_outbox-verify-persist-race.md` — this entry
