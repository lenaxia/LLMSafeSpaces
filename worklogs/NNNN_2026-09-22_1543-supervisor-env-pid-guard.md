# Worklog: #1543 — the watchdog-flake diagnosis + the supervisor-subprocess pid-identity guard

**Date:** 2026-09-22
**Session:** Standby assignment: diagnose the twice-observed CI flake filed as #1543 ("watchdog_vitals_test.go:323 hang"). Scope guard honored: #1537's file set re-verified disjoint (no watchdog_vitals_test.go / healthz_cache.go / supervisor_subprocess_test.go overlap) before touching anything.
**Status:** Complete

---

## Objective

Determine whether the recurring CI failure is (a) a test-side deadline/cleanup gap, (b) a genuine liveness-loop starvation shape in US-72.4-adjacent production code (healthz_cache watchdog/defer loop), or (c) something else the issue's signature misattributes — then fix what is test-side with evidence.

## Work Completed

### The diagnosis (both occurrences decoded from the raw job logs)

**The `watchdog_vitals_test.go:323` anchor is cosmetic.** Every "traceback" line pointing at :323 is a zap STACKTRACE field attached to ordinary WARN logs (`readyz refresh failed`, `health-watchdog deferring restart`) from the watchdog tests' OWN healthy loop goroutines — the `created by` frame of the goroutine spawned by `runWatchdogLoop`. The failing-test output carries other tests' buffered zap output (shared stdout + per-test log attachment), which is why the anchor looks like a hang site. **No goroutine was ever wedged and no watchdog defect exists in either occurrence's evidence.**

**Occurrence 1** (run 35678423373, `-short` + coverage, 02:09Z): a plain package-budget exhaustion — `panic: test timed out after 5m0s` with `running tests: TestWatchdogRespawnBootWindow_NeverKills_RealSubprocess (3s)`: the alarm fired while the CURRENT test was 3 seconds old, i.e. the package as a whole consumed the 300s budget with legitimate work. Log-timeline proof: the last emit-time-stamped log line predates the alarm by ~45s (tests were progressing, just slowly). **Already root-caused and fixed the same morning, 2.5h after this flake, by d82d80f9 ("fix(ci): the -short timeout budget 300s → 600s (the agentd package outgrew it)")** — occurrence 1's head predates that fix. Corroboration: on this pod the full `-short` package run takes 226.7s — near the old 300s budget on slower/loaded runners.

**Occurrence 2** (run 35752704051, full-race leg, 600s budget): NOT a hang and NOT the watchdog — `TestSupervisorSubprocess_LifecycleAndContract` failed the platform-env assertion at supervisor_subprocess_test.go:184 ("platform env present on the pre-push child", Should be true, test duration 1.12s). The deferred-restart logs and :313/:323 frames in that output are again cosmetic (buffered zap + stacktraces). The env-composition chain (`buildEnvFrom` → `scrubAdminEnv` → `prependPathEnv` → `opencodeChildEnv`) is purely ADDITIVE w.r.t. the supervisor's env — `GO_TEST_SUPERVISOR=1` cannot be dropped for a real child. Therefore the read observed a pid that was NOT the supervisor's stub child: the assertion read `/proc/<ChildPID>/environ` through an unguarded identity window — if the stub child died (e.g. runner OOM/kill churn) between `Status` and the read, the supervisor's crash recovery has not necessarily moved `ChildPID` yet while the race leg's parallel helper processes recycle pids fast enough for the SAME number to be reallocated — a stranger's environ: readable, alive, missing the var. Exactly the observed failure shape. Single occurrence in the sampled CI history (12 recent failed runs checked: the rest are other lanes).

**Disposition:** (a) occurrence 1 already fixed; (b) occurrence 2 is test-side — the production watchdog/liveness code shows NO starvation shape in the evidence (the defer loop logs are per-test designed behavior at shrunk timings); (c) the issue's signature conflated two different failure modes sharing the cosmetic anchor. No production code changed.

### The fix (test-side, TDD)

`supervisor_subprocess_test.go` gains a pid-identity-guarded observation:

- `childEnvironObserver` — an injectable seam (`status`/`environ`/`statPPID`/`sleep`) whose `observeVerified` accepts an environ reading ONLY when BOTH identity checks hold at read time: the pid's `/proc/<pid>/stat` ppid names the SUPERVISOR (a recycled pid has a different parent), and a re-Status still reports the SAME pid (crash recovery has not moved on). Retries through churn; fails loudly after a bounded deadline naming the mechanism (and #1543) so a genuine env-composition bug is never misread as flake.
- 4 red-first unit tests over the seam: stable child (exactly 2 status calls — no churn cost), ppid-mismatch retry (a stranger's environ can never pass through), currency-failure retry (a moved-on pid's stale reading is discarded), persistent-churn bounded failure. The red phase caught a real bug in my own first implementation (the injected world-advance sleep was never called — retries could not progress; the deadline was select-based with real `time.After` only).
- The lifecycle test's assertion now reads through `sp.verifiedChildEnviron(t, cc)` (the production seam: real `cc.Status`, real `/proc` reads, 10s deadline, 50ms retry interval). `procPPID` parses field 4 of `/proc/<pid>/stat` from the LAST `)` so a process name containing spaces/parens can never misalign the field split.

## Key Decisions

1. **Diagnosis before code:** both raw job logs pulled and decoded (inner zap emit timestamps vs the outer flush timestamps — the log stream's wall-clock prefixes are flush-time, not emit-time; sorting by `"ts"` fields is what exposed the 45s-progress gap in occurrence 1).
2. **The guard is identity + currency, not content:** the helper does not know what the env SHOULD contain (the assertion keeps that job) — it guarantees the reading belongs to the right process at the right time.
3. **No production changes:** per the assignment's rule, the US-72.4-adjacent watchdog/liveness code is untouched — and the evidence says it is not implicated anyway.
4. **The deadline error names the mechanism AND the alternative hypothesis** ("if this repeats with a stable pid, the env composition itself is broken") — so a future recurrence is self-triaging.

## Blockers

None. Frequency note: occurrence 2 is the only observed instance of this assertion failing in the sampled history; the guard makes the window unexploitable rather than proving the underlying churn.

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestChildEnvironObserver' -count=1 -v` — 4/4 (grown red-first: 2 failed against the first implementation).
- `go test ./cmd/workspace-agentd/ -run 'TestSupervisorSubprocess_LifecycleAndContract|TestChildEnvironObserver' -race -count=3` — green (also green under an artificial 4-CPU stress load, 25 iterations, though the flake never reproduced locally — expected for a churn-window race).
- `go test ./cmd/workspace-agentd/ -short -count=1` — full package green, 226.7s (the occurrence-1 corroboration data point).
- `gofmt`/`go vet`/`golangci-lint run ./cmd/workspace-agentd/...` (0 issues)/`misspell` — clean; `make repolint` — all checks passed.

## Next Steps

1. Merge → watch the race leg for recurrence (the guard converts the flake into either a clean pass or a loud, self-triaging failure).
2. #1543 closes on the diagnosis: occurrence 1 = d82d80f9 (already on main); occurrence 2 = this guard; the "watchdog hang" signature retired as cosmetic log attachment.
3. If the guard's deadline error EVER fires with a stable pid, that is an env-composition bug — production territory (managed_process.go's env chain), owner lane.

## Files Modified

`cmd/workspace-agentd/supervisor_subprocess_test.go` (the observer seam + tests + the guarded assertion), this worklog.

## r1 — coverage gaps, the parser split, and my sentinel violation

- **The parser is now split + table-tested** (bot finding 1a): `procPPID` is the I/O wrapper; `parseStatPPID` is pure and carries the 8-case edge table (plain comm; comm with spaces; comm with parens — the last-`)` rule; comm that is just parens; no terminator; empty tail; whitespace-only tail; non-numeric ppid) plus one live-wrapper leg against this process. Writing the table caught a real off-by-one in my first guard (`at+2 > len` misclassified an empty tail as "no comm terminator" — the slice is `stat[at+1:]` and Fields eats the leading space; malformed is reserved for a missing terminator).
- **The ENOENT retry path is pinned** (finding 1b): `RetriesOnEnvironReadError` — a transient environ-read failure (the child died between identity check and read) retries exactly once and captures the good reading.
- **The status-error asymmetry is now explicit + pinned** (finding 1c): `StatusErrorAbortsImmediately` — a control-socket failure aborts with the wrapped error and ZERO retry cycles; the retry budget exists for identity churn, not for a broken socket (surfacing that at once beats 10s of dead retries).
- **My sentinel violation (finding 2), stated plainly:** I ran `make pre-commit-fix` BEFORE committing; its fix-worklogs pass renamed the NNNN sentinel to 1050 locally and I committed the number — main has since consumed 1050-1052, exactly the hand-pick race the sentinel rule exists to prevent. Corrected: the worklog carries the NNNN_ sentinel and the merge-time hook assigns.
- Rebased onto current main at the rename (no overlap); gofmt clean.
