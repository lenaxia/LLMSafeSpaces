# Worklog: orphan zombie reaper for agentd as PID 1 (#904)

**Date:** 2026-08-16
**Session:** Reap adopted orphaned tool processes; fixes #904.
**Status:** Complete

---

## Objective

agentd is PID 1 in the workspace container. Descendants orphaned mid-execution (opencode bash-tool children whose intermediate parent died, children of prior opencode generations) reparent to agentd, and the Go runtime reaps only children its own `os/exec` waiters block on — so adopted orphans that exit accumulate as permanent zombies. 2026-08-16 evidence: two prod pods each carried `[bash]`/`[go]` defunct entries (PPID 1) for hours with `restartCount 0`.

## Work Completed

- `orphan_reaper.go`: SIGCHLD + ticker-driven reaper. Each pass scans `/proc` for children of this process in `Z` state, skips pids in the owned registry, and reaps the rest with pid-specific `Wait4(WNOHANG)` only after they have been zombie longer than `orphanGrace` (5s). New metric `workspace_orphans_reaped_total{workspace_id}`.
- Owned registry: `managedProcess.supervise` registers the opencode pid between `Start()` and `Wait()`; `trackedOutput` (new helper, `Output()` semantics — stdout returned, stderr captured into `ExitError.Stderr`; corrected in the review round, was initially stderr-inherited) replaces the direct `exec.Command(...).Output()` in `secrets.go buildEnvFrom`. These are the only two direct-exec sites in the package (validated by grep).
- `main()` calls `prctl(PR_SET_CHILD_SUBREAPER)` in `--supervise` mode (no-op in effect when agentd is already PID 1; keeps the fix effective under another init). The loop runs in `startBackgroundLoops` on `bgCtx`/`bgWg`.
- `go.mod`: `golang.org/x/sys` promoted indirect → direct (prctl constant).
- Tests (`orphan_reaper_test.go`): bug baseline (zombie persists without reaper), reaps adopted orphan (+ metric), tracked zombie never reaped even far past grace (late `Wait()` still sees exit 3), 40 concurrent untracked Start+Wait never stolen, `trackedOutput` semantics, Wait4 syscall plumbing. Red-without-fix verified by neutralizing `pass()` (`ReapsAdoptedOrphan` fails on metric 0); full package suite green under `-race` (381s); vet + golangci-lint clean.

## Key Decisions

- **/proc scan + pid-specific Wait4, not `waitid(P_ALL)` or `Wait4(-1)`.** `x/sys/unix.Siginfo` on linux-amd64 exposes no `si_pid` (union is unexported bytes), so waitid cannot identify *which* child to skip; `Wait4(-1, WNOHANG)` reaps indiscriminately and would steal exit statuses. A pid-specific wait can only ever touch the zombie it named.
- **Grace 5s.** A child with an active blocking `Wait()` is reaped by its waiter in-kernel at exit (microseconds of zombie visibility); a zombie visible past grace has no waiter by construction. 5s is orders of magnitude above that window and far below the hours the bug allowed — sized to also tolerate CFS-throttle scheduling gaps (the 2026-08-15 starvation incident observed multi-second gaps).
- **Owned registry is belt, grace is suspenders.** With both direct-exec sites registered, the reaper by construction never touches a waited child; grace alone would also protect an unregistered `Output()`-style caller. The `TrackedZombieNeverReaped` test pins the belt (owned zombie survives 4× grace), the `ConcurrentUntrackedWaitsNotStolen` test pins the suspenders.
- **Supervisor starvation window covered.** If the supervise goroutine is starved while its child exits (owned zombie persisting > grace), the registry still prevents theft — the reason ownership is keyed on "has a waiter", not "zombie is young".

## Assumptions (validated)

1. Go 1.26 runtime does not auto-reap adopted orphans — validated: no `waitid/WNOWAIT` reaping in `/usr/local/go/src/runtime`; prod zombies are the empirical confirmation.
2. Only two direct-exec sites exist in the package (`managed_process.go:467`, `secrets.go:1117`) — validated by grep for `exec.Command`/`cmd.Run/Output/CombinedOutput`; `oom_detection.go` imports `os/exec` for types only.
3. A blocking `Wait()` waiter reaps at child exit in-kernel; zombie visibility without a waiter persists — validated in-process by `TestOrphanReaper_ZombiePersistsWithoutReaper` and `TestOrphanReaper_Wait4Echo`.
4. `signal.Notify(SIGCHLD)` coexists with `os/exec` waiting (the runtime needs no signal delivery to complete `wait4`) — validated by the full existing suite green under `-race`, including the real-subprocess managed-process/watchdog harnesses.
5. Zombie pids cannot recycle mid-scan (a zombie pins its pid until reaped), so the scan→`Wait4` window cannot hit a reused pid.

## Accepted residuals

- A future direct `exec.Command(...).Output()` caller in agentd would rely on grace + kernel handoff alone. `trackedOutput` exists for this; not mechanically enforced (a repolint grep rule would be the follow-up if it recurs).
- Zombies appearing between `bgCtx` cancel and process exit (shutdown window) are not reaped — bounded by the shutdown path's own lifetime.
- Orphans reparented to *opencode* (not agentd) are opencode's children; only when their chain parent dies do they reach agentd. Out of scope here.
- The #904 "related observation" (phantom-busy clear took ~45 min with no reconcile path) is **not** addressed here: the API SSE-watch-blindness half was fixed by #903 (merged, deployed 0.15.9 — watch re-arming, not busy-reconciliation per se); the agentd-side SSE-gap busy reconciliation remains tracked under #892 — it needs a queryable busyness source opencode 1.18.10 does not expose.

## Blockers

None.

## Review round 2 (PR #908 REQUEST CHANGES → addressed)

Findings and fixes:
1. Wiring points mutation-deletable undetected → added `TestOrphanReaper_SupervisorRegistration_Wiring` (real `managedProcess` via the TestHelperProcess factory; asserts owns(pid) while alive, released after stop) and `TestOrphanReaper_StartupWiring_ReapsAndDoesNotSteal` (real `startBackgroundLoops`: adopted orphan reaped + metric counted; supervised child owned and released). Both re-verified mutation-red: deleting the track/untrack calls fails the former; no-op'ing the reaper goroutine fails the latter.
2. `readProcStat` untested against hostile comm → split string parsing into `parseProcStat`; table-driven `TestReadProcStat` (spaces, parens, missing fields).
3. `ReapsAdoptedOrphan` flake exposure → sawZombie no longer judged in a tight early window; single reaped-or-timeout assertion over 10s.
4. Record accuracy: worklog line 16 "stderr inherited" corrected (capture into `ExitError.Stderr`); "#903 API-side half" softened to "API SSE-watch-blindness half (#903 is watch re-arming for #902, not busy-reconciliation)".

Debugging note for posterity: the startup-wiring test initially failed because `RecordOrphanReap` normalizes the empty workspace ID to `"unknown"` while the test polled label `""` — the reap was working all along; the label was wrong.

## Review round 3 (APPROVE + two nits and one robustness note — all addressed)

Round 3 verdict was APPROVE with non-blocking observations; addressed in one pass:
1. `ReapsAdoptedOrphan` residual flake (a VM stall spanning exit→reap could hide the zombie from every poll) → metric-only assertion: a counted reap proves a zombie existed.
2. `StartupWiring` part (b) comment overclaimed ("supervisor still observes the true status") → rewritten to state exactly what is asserted (registry transitions) and where the status-preservation property is pinned.
3. Robustness note: `scanZombieChildren` returned nil on /proc walk failure, indistinguishable from "no zombies" — a transient failure wiped pending grace clocks. Now returns `([]int, error)`; `pass()` aborts on scan error before any state mutation. Pinned by `TestOrphanReaper_ScanErrorPreservesPendingClocks` (clock preserved, no prune, recovery reaps + metric delta — delta-based, not cumulative, so it stays vacuity-proof).
