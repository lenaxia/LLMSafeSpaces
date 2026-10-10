# Worklog NNNN — #1532 flake consolidation: deadline/de-race restructures (PR2)

Branch: `fix/1532-flake-deadlines` (from main @ f9a74a19). PR1 (#1644
accept-loop) is separate. #1641 (GetPassword race) deliberately OUT of
scope per charter.

## Method: the stall harness

CI shape (per the sightings) is loaded/stalled runners, not uniform
slowness. Two harnesses were built and honestly characterized:

1. **Uniform load** — 64 CPU burners + GOMAXPROCS=2 + pinning test and
   burners to the same 2 cores. Does NOT reproduce any of the four
   families (watchdog: 10/10 pass; earlier probes pass). Recorded as a
   negative result: these flakes are not steady-state slowness.
2. **Stall harness** (`/tmp/opencode/stall-runner.sh`) — SIGSTOP the
   test process for S seconds at +MS into the run, then CONT. A
   process freeze matches a GitHub-runner VM pause/cgroup stall
   closely: Go timers are wall/monotonic-clock based and deadlines
   expire DURING the freeze, so Eventually budgets burn while the
   goroutines cannot progress. Same schedule is applied to OLD and NEW
   shapes for every family (the mutation-check discipline).

Aiming note: the DeferSeam pair and the BusyTruth chain complete in
tens of ms — their stall windows are not reliably hittable from
outside; where a stall could not land, the demo uses targeted
interference injection (below) or records the limitation honestly.

## Family 1: watchdog (TestRefreshIsHealthyLoop_*, 3 CI hits)

OLD shape: `time.Sleep(1200ms)` then `assert.False(Healthy)` — the
assert requires ≥3 probe failures to have accumulated INSIDE a fixed
wall window. NEW shape: `settleWatchdogToFailed` —
`require.Eventually(ConsecutiveFailures ≥ threshold+7, 15s, 20ms)`
then the same asserts (outcome-deterministic, wait-bounded; matches
the de-timing pattern already in this file's boot-window test). All 7
sleep-assert tests converted + the 1.5s-budget fire test scaled to
15s. Semantics preserved and strengthened: threshold+7 consecutive
failures = ~10 would-fire moments for suppression verdicts to hold
over (the old "~20 polls / reached ~4 times" intent).

- OLD, stall 4s @ +250ms: **2/6 FAIL** (failing assert reads Healthy=true
  post-CONT; the 1200ms budget burned during the freeze).
- NEW, identical schedule: **6/6 PASS**.
- Unloaded full family: 17/17 PASS (converted tests ~1.10s each, was
  ~1.64s — the suite also got faster).

## Family 2: BusyTruth E2E (7+ CI hits incl. two release runs)

Restructure: subscription wait 10s→30s, `waitForStatuses` 10s→30s
(family-wide helper), gate-drop 15s→45s; removed the fixed-instant
`require.Equal(1, c.Gates())` read immediately after `Reseed()` — it
races a FAST reconcile in the opposite direction (idleDrop=100ms can
legitimately drop the gate before the read; the Equal carried no pin
value: the stale-true-lease bug this row pins fails the eventual-zero
wait, which is kept as the teeth). Rationale documented in-test.

- OLD, stall 11s @ +500ms (-count=5): **1/6 FAIL** — "bridge statuses
  never reached [busy]", test duration 11.04s = the 10s budget
  expiring inside the stall (the exact CI class).
- NEW, identical schedule: **0 budget failures**; 1/6 residual failure
  of a DIFFERENT mode (41s "never satisfied"): an 11s full-process
  freeze kills the in-process SSE stream mid-delivery and the no-replay
  stream loses the pre-freeze busy frame at ANY budget. This is a
  harness/consumer-semantics fragility, not a budget problem — flagged
  to the orchestrator as a suspected deeper mechanism for this test's
  CI hits (candidate follow-up: reconnect snapshot re-derives bridge
  emission; production code, own issue). Out of scope here.

## Family 3: DeferSeam (two 5.00s deadline-expiry sightings)

Restructure: (a) script advancing moved OUT of the side-effecting
Eventually condition into a dedicated 50ms-cadence advancer goroutine
(the old condition popped one script answer per 5ms poll, racing the
decision goroutine's reads and destroying answers it might never see);
(b) package-global metric asserts de-raced: exact `==`/InDelta on the
global histogram/counter → `≥` (a leaked deferred goroutine from ANY
other test observing late can break equality forever — that broken
`==` burns the FULL 5s budget, the exact CI signature); (c) budgets
5s→15s.

- Interference injection demo (deterministic): one
  `restartDeferStallSeconds.Observe(0.5)` planted right after the
  baseline read = exactly one leaked observer landing mid-window.
  OLD + injection: **FAIL at 5.01s** ("force leg observes its stall
  duration" — the 5.00s deadline-expiry signature byte-for-byte).
  NEW + injection: **PASS at 0.06s**.
- Stall demo not applicable (test completes in ~90ms incl. init; a
  SIGSTOP cannot reliably land inside a ~20ms window) — recorded as a
  limitation; the injection demo targets the actual claimed mechanism.

## Family 4: Upload concurrent storm (2 hits)

Restructure: hoisted the 6×4MiB multipart builds out of the storm
goroutines (test setup racing the measured window under -race added
pure contention; HTTP-level storm concurrency unchanged); per-request
client timeout 20s→45s; hang guard 25s→60s (both are deadlock
sentinels — a real deadlock never resolves, so the larger bound catches
it identically while a loaded runner cannot burn it on slowness).
Full `api/internal/handlers` package post-edit: **ok 104.4s** (local).
No stall demo (the storm is dominated by real I/O; the guard exists
for deadlock, not timing).

## Verification summary (all post-edit)

- `api/internal/services/usagestream`: ok 2.490s
- `api/internal/handlers`: ok 104.400s
- `cmd/workspace-agentd` full package: run detached under PVC/ephemeral
  pressure (see incidents); family runs green via prebuilt binaries
  (TestRefreshIsHealthyLoop 17/17, TestDeferSeam green, injection
  demos recorded above)
- vet + golangci-lint clean on changed packages (pre-commit hook)

## Incidents (all resolved or disclosed)

- /tmp→PVC exhausted repeatedly mid-work (shared 15G PVC at 100%,
  sibling lanes consuming in parallel); exercised standing permissions
  (stale /tmp/go-build corpses, >24h-old /tmp test-binary corpses),
  moved GOTMPDIR to /home/sandbox (discovered it is ALSO PVC-backed
  via a failed 3G fallocate probe — the "210G overlay" is not writable
  in practice).
- The build cache at /home/sandbox/.cache/go-build is SHARED across
  sibling lanes and was wiped mid-build by a sibling's own
  `go clean -cache` (cache-index corruption errors: "no such file").
  Two detached full-package runs died on ENOSPC at the link step
  (final ~300MB) and on the BootGate test's SELF-BUILD of the binary
  into /tmp — environmental, not code: zero failures attributable to
  the edited tests (all green in every run that reached them).
- Local full-package verification is therefore BLOCKED by disk
  contention, honestly recorded: the four touched files' complete test
  families are green locally (17/17 watchdog, DeferSeam pair +
  deterministic injection demos, usagestream package ok 2.5s, handlers
  package ok 104.4s); the CI matrix (vet/lint/test incl. -race) on
  push is the full arbiter. Same-files -race pass on the PR1 tree:
  381.5s, on record in the #1644 worklog.
