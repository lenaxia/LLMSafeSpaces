# Worklog: 1105 — #1632 wedge self-heal (detection + reaping) & #1507 verification

**Date:** 2026-10-09
**Session:** w14 — issues #1632 (wedged opencode never self-heals) + #1507 (suspend blocks forever on hung busy sessions)
**Status:** In progress — implementation plan below; PR1 (#1632 detection+reaping) starting

---

## Recon findings (read both issues in full, read the code)

### #1507 — ALREADY FIXED IN MAIN (verified, not predicted)

- Commit `ee787444` "feat(controller): #1507 — suspend drops the busy gate; the pod's
  termination grace IS the bounded graceful opencode termination (#1510)" IS an ancestor
  of HEAD (git merge-base --is-ancestor verified).
- `controller/internal/workspace/phase_suspend.go:80` `handleSuspending`: no busy gate,
  deletes pod on first reconcile, comment cites #1507 + the d8bed486 incident; the
  "deferring pod deletion behind busy sessions" log string exists ONLY on the recycle
  paths (restart-generation / architecture-drift / password-secret, phase_active.go),
  which is the intended post-#1507 residual (documented in the #1510 review notes).
- `force_recycle.go:32` cross-references: "The suspend path needs no force since
  #1510/#1507: handleSuspending never drains."
- Remaining #1507 work for me: verify each acceptance criterion against main (flag
  `--workspace-termination-grace-period-seconds`, helm value, the four test classes),
  then report to orchestrator with a close-the-issue recommendation. Any gap found →
  small PR. Cluster observation (AC5) is production verification — flag for the
  release train, not something I can locally fabricate.

### #1632 — the four blind layers, current state in main @ 4615c2b3

Topology (sidecar mode, the incident deployment): workspace container runs
`workspace-agentd supervise-opencode` as PID 1 (own PID ns); agentd sidecar
(uid 2000, separate container, shared netns) serves admin mux :4098 and runs
the health-watchdog; control socket 127.0.0.1:4099 (JSON-lines) bridges them.

1. **Watchdog UNKNOWN-forever (the core defect).** Sidecar's vitals =
   `socketVitalsGatherer` (socket_vitals.go): TCP probe + supervisor status over
   the control socket; CPU evidence honestly unavailable cross-container →
   `cpuKnown=false` → classify() → verdictUnknown → suppress. 36+ suppressions
   in the incident. The ONLY kill verdict is `verdictHung` = tcpRefused —
   unreachable for a listening wedge (kernel accept backlog answers handshakes).
2. **K8s liveness blind.** Sidecar-mode workspace-container liveness is
   `tcpSocket:4096` (agentd_sidecar.go applyAgentdSidecar — deliberately swapped
   away from the sidecar-served /v1/healthz so a sidecar wedge can't restart
   opencode; the comment calls that "precisely backwards"). Same kernel-accept
   blindness. Sidecar's own /v1/healthz hardcodes Healthy:true (US-22.1,
   process-only) — never reflects agent health.
3. **Zombie reaping missing in supervise-opencode mode.** server.go:713 runs
   `pkgOrphanReaper.run(bgCtx)` (single-container/sidecar agentd only).
   `runSuperviseOpencodeCommand` (supervise_opencode.go:64) calls
   `becomeSubreaper()` but NEVER starts the reaper loop → orphaned tool children
   reparent to PID 1 and stay `<defunct>` forever. Matches the incident's
   `[python] <defunct>` (3:30 CPU) + 5× `[esbuild] <defunct>` and the issue's
   "PID 1 … doesn't reap" finding. The reaper machinery already exists
   (orphan_reaper.go, #904/#908) — the supervisor mode just never runs it.
4. **D6 notify-only.** API-side escalation cycles hung→recovered every ~60-100s
   (incident log 02:44–02:48). Product surface (API/frontend) — see scoping
   decision below.

### Incident-class subtlety that sizes fix #2

The 2026-10-08 wedge CYCLED: stall ~10min → brief recover → stall. A
"continuous unhealthiness" bound would never fire. The #892 starved-healthy
incident (the reason killing is banned on weak evidence) had SHORT bursts
(~3 failures) with genuine recovery between. Design conclusion: the liveness
sustain bound must measure EPISODE time (since the last *sustained* recovery =
N consecutive healthy polls), not continuous failure time.

## Implementation plan

**PR1 — `#1632` detection + reaping (branch fix/1632-wedge-selfheal):**

- **A. Reaper wiring:** start `pkgOrphanReaper` in `runSuperviseOpencodeCommand`
  (one goroutine, rootCtx). Red-first pin: real-subprocess test that double-forks
  an orphan under the running supervisor and asserts it is reaped within
  grace+scan bound (follow supervisor_subprocess_test.go harness patterns).
- **B. CPU evidence over the control socket (issue fix #1, "minimal vitals
  helper" option):** control-protocol v1 `vitals` method — supervisor reads
  `/proc/<child>/stat` utime+stime in ITS pidns (kernel-level read; works even
  when the child's event loop is blocked) and returns {pid, cpu_ticks}.
  `socketVitalsGatherer` samples twice across its existing window →
  `cpuKnown=true` → verdicts become honest FLAT/STARVED. Mixed-fleet compat:
  `method_unknown` (older supervisor) degrades to today's UNKNOWN path.
- **C. Bounded UNKNOWN-episode soft kill (issue fix #2):** in
  refreshIsHealthyLoop's suppression branch — an UNKNOWN episode older than
  `watchdogUnknownEpisodeBound` (default 15m, var for tests) escalates to a
  soft restart: marker written (new reason), existing maybeFire rate limit
  (3/10min) applies, episode clock resets with the latch on recovery.
  Defense-in-depth with B: UNKNOWN persists only when the control socket
  itself is degraded.

**PR2 — `#1632` liveness backstop (issue fix #3), branch off PR1:**

- Sidecar serves `/v1/agentz` (bearer-gated like readyz): 200 unless the agent
  has been in an unhealthy EPISODE ≥ `agentUnhealthyEpisodeSustain` (default
  10m). Episode = since the last 3-consecutive-healthy polls (brief single-poll
  recoveries — the incident's cycle shape — do NOT reset). !Initialized → 200.
- Workspace-container liveness (sidecar mode) becomes HTTPGet /v1/agentz on the
  admin port (+bearer) — replaces tcpSocket:4096. Sidecar-wedge asymmetry
  preserved: sidecar's own liveness (80s) heals it long before a 10m episode
  bound could 503; a sidecar restart resets the cache (uninitialized → 200) so
  it cannot cascade into an opencode kill. #892 duty-cycle safe: bursts with
  real recovery reset the episode.
- Watch wiring: the watchdog restart (in-place, via socket) remains layer 1;
  kubelet container restart is the backstop when soft restarts don't converge.

**Scope decision (disclosed):** issue fix #5 (D6 beyond notify-only) is API+
frontend product surface; the orchestrator ruling scopes my lane to the agentd
supervisor/termination primitives. With B+C in place the "watchdog suppressing
UNKNOWN while D6 sees hung" trigger condition largely disappears, and D is the
residual backstop. I recommend #5 as a follow-up issue unless the orchestrator
rules otherwise.

**#1507:** AC-by-AC verification against main (no new code expected); report +
close recommendation to orchestrator.

## Tooling baseline (all verified this session)

go1.26.6, helm v3.21.4, golangci-lint 2.13.2 — all via PATH=/tmp/opencode/bin:$PATH.
Disk 44% used. OPENCODE_BINARY=/opencode/usr/local/bin/opencode exists.

---

## Progress log

- [x] Both issues read in full; recon of all six key files; #1507-already-in-main
      verified via merge-base.
- [x] PR1-A reaper wiring + red-first pin (sabotage: loop not started → red,
      "orphan zombie was never reaped")
- [x] PR1-B control-socket vitals + sidecar CPU evidence (sabotage: cpuKnown
      forced false → FLAT/STARVED pins red)
- [x] PR1-C bounded UNKNOWN episode (sabotage: bound ×1000000 → escalation pin
      red; disclosure: two earlier sabotage attempts were INVALID — one
      sed-injected a comment that swallowed the brace (build failure), one
      accidentally made the condition always-true (wrong direction); both
      redone properly before counting)
- [x] PR1 pushed (4bc11b51) → PR #1637 opened
- [x] PR2 agentz episode clock + handler + probe swap (sabotage: sustain
      unreachable → 503 pin red; episode pins; controller probe pin inverted
      from tcpSocket — red on main by construction) → b2b6dbb8 → PR #1639
      (stacked on #1637's branch)
- [x] #1507 AC verification (see below)

## #1507 acceptance-criteria verification (against main @ 4615c2b3)

- AC1 no busy gate: `handleSuspending` (phase_suspend.go:80) deletes the pod on
  first reconcile; the "deferring pod deletion behind busy sessions" string
  exists only on recycle paths (restart-gen/arch-drift/password-secret) —
  intended post-#1510 residual, documented in its review notes. ✓
- AC2 bounded graceful termination: pod terminationGracePeriodSeconds default
  40s (agentd's 35s serial drain budget + 5s margin), configurable via
  `--workspace-termination-grace-seconds` (controller/main.go, startup
  validation rejects < 36) and helm `controller.workspaceTerminationGraceSeconds`
  (values.yaml + controller-deployment.yaml). ✓
- AC3 wedged workspace reaches Suspended bounded: suspend is one reconcile —
  pod delete + status flip; bounded by the 40s grace + reconcile latency. ✓
  (TestSuspendBounded_FlappingBusySessionsSuspendImmediately pins the incident
  shape.)
- AC4 tests: phase_suspend_1507_test.go — flap-suspend-immediate,
  live-agent-never-consulted, dead-agent-immediate, grace-rides-the-pod,
  restart-gen-drain-unchanged; PVC retention asserted (fetched after suspend).
  All green locally this session. ✓
- AC5 cluster observation: production verification — flagged for the release
  train; not locally verifiable.

Recommendation: close #1507 as completed by #1510.

## Incidents & disclosures (this session)

1. Premature `git checkout` during a sabotage check reverted my own
   UNCOMMITTED socket_vitals.go implementation (PR1-B) — re-applied identical
   content, then adopted commit-before-mutation-check discipline. The same
   mistake recurred once on server.go during PR2 (re-applied, committed
   immediately). Net-zero hidden.
2. Shared-pod disk hit 94% (ENOSPC broke a test build). Cleaned 3GB of stale
   /tmp/go-build corpses under the standing 120s-untouched rule; sibling
   wt-1601's active build dir left untouched.
3. Full-package local `go test ./cmd/workspace-agentd/` was starved by sibling
   disk contention (D-state processes, 11+ min stalls); my own background runs
   were killed twice by session interrupts. Verified via targeted chunks
   instead: 78+ tests green across watchdog/socket/supervisor/restart-reason/
   managed-process/agentz/episode families; full suite rides CI (disclosed in
   PR body). CI's agentd suites subsequently ran green on #1637.
4. govulncheck red on #1637 (and fleet-wide: #1634, others) — GO-2026-6617 in
   x/net@v0.58.0 + stdlib advisories; PR #1636 is the fleet unblock. Commented
   on #1637; no dep changes in my PRs by design.
5. The first `review / review` run on #1637 crashed after 31m56s WITHOUT
   delivering a verdict (steps after "Run OpenCode" skipped) — re-triggered
   via `gh run rerun --failed`.
6. Testing-inside-a-workspace-pod gotcha (found via a red test that should
   have been green): refreshOnce-based tests that don't point the package
   agent-addr at their mock will poll THIS pod's real opencode on :4096 and
   get healthy answers. The new helper sets/restores the addr; documented in
   the PR body for future test authors.

## Remaining

- [ ] #1637 review loop → APPROVED → orchestrator mutation-check → merge
- [ ] #1639 review loop → APPROVED (retarget to main after #1637) → merge
- [ ] Release-train production verification (AC5 of #1507 + incident-class
      wedge observation)
