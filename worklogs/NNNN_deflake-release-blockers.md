# Lane-deflake — the three release-blocking race families (PR #TBD)

Date: 2026-10-26 · Lane: lane-deflake (orchestrator novel-nutmeg-verifies) · Branch: fix/deflake-release-blockers · Base: origin/main e27c2e6d5 (post-v0.44.1 sectioning).

## Objective

Two releases blocked today (2026-10-25) by 'Test (full suite, race detector)' reds: the v0.44.1 pipeline aborted on its first attempt, and PR #1958's race job failed twice on DIFFERENT tests with a sibling run green on the same head — the flake triad, each with live evidence:

1. `TestUsageBridgeStreamResynced_NonBlockingAndCoalesced` (api/internal/handlers) — 40228899957 attempt 1, 21:12Z class. #1927's family: a recurrence/escapee of the merged #1933 fix.
2. `TestWorkflowExecute_ScriptForwardTimeoutMaps504` (cmd/workspace-agentd) — v0.44.1 release run attempt 1 (job 121292573940), 21:27Z.
3. `TestUploadApply_ConcurrentWallTime` (cmd/workspace-agentd) — 40228899957 attempt 2 (job 121285005209), 20:53Z.

Mandate: validate-first (pull the failing logs, reproduce locally under -race stress), then de-flake per the #1927/#1910 doctrine — establish preconditions, deterministic discriminators, no sleep-and-pray, no weakened assertions.

## Evidence pulled (exact signatures)

| Family | Run / job | Signature |
|---|---|---|
| NonBlockingAndCoalesced | 40228899957 attempt 1 / job 121276239383 | `proxy_usage_resync_test.go:146: Not equal: expected: 1, actual: 2 — a re-cue that raced the marker release must not have spawned a second fetch` (0.11 s) |
| ScriptForwardTimeoutMaps504 | Release 40233982936 attempt 1 / job 121292573940 | `workflow_exec_forward_test.go:172: timeout must map to 504, got 200: {"errorCode":"script_env_unavailable", … "read script_exec: read tcp 127.0.0.1:41746->127.0.0.1:34843: i/o timeout"}` (0.30 s); healthz readyz-refresh deadline warnings in the log tail immediately before |
| ConcurrentWallTime | 40228899957 attempt 2 / job 121285005209 | `upload_apply_test.go:652: concurrent 4 applies took 195.79773ms — near the serialized baseline 231.362939ms` (0.50 s) |

Note: the sibling-green-on-same-head pattern (PR #1958 attempt 1 red on family 1, attempt 2 red on family 3, sibling runs green) is itself the flake fingerprint — all three are timing-state bugs, not pushed-code regressions.

## Root causes

### 1. NonBlockingAndCoalesced — the #1927 recurrence: shared-key TOCTOU on the flight-coalescing marker

`busyFlightsInFlight` (proxy_busy_snapshot.go) is a package-global `sync.Map` keyed by workspace id; `triggerBusySnapshotFlight` LoadOrStores the key in the cue and the flight goroutine Deletes it in a defer that runs AFTER the flight's final publish — which can strictly follow the test's return (every flight test returns on a complete that precedes its marker Delete; #1927 documented this leak). #1933's fix cleared the stale marker at HARNESS CONSTRUCTION — but a leaked flight's Delete can land any time AFTER construction: it deletes the KEY, and with every harness sharing ONE id (`busySnapWS`), the next test's cue1 had already LoadOrStore'd ITS marker under that key. The leaked Delete removes the LIVE marker; the test's rapid re-cues then find no marker and spawn a second authority flight → `fetches == 2`. The failure's position (final assert at 0.11 s, second `Eventually(==1)` passed) fits exactly: the unlocked re-cue's fetch reaches the stub milliseconds after the second Eventually's first poll, inside the 100 ms settle window. Product verdict: `LoadOrStore` is an atomic CAS (no product race — #1910 proved the same for the POST path); the defect is missing inter-test isolation of shared state, the #1927 class.

### 2. ScriptForwardTimeoutMaps504 — two clocks tied, classification raced

`controlClient.ScriptExec` derives its conn arm from the caller's ctx deadline (`timeout = time.Until(dl)`), so the parked read's `os.ErrDeadlineExceeded` and the ctx timer fire at the SAME absolute instant. `callDeadline` already returns a `deadlineFired` bool — built (per its own doc) as "the deterministic signal UploadApply's §3.3 timeout classification rests on: no clock race between the conn arm and the ctx timer, because the two are the same arm when derived" — but ScriptExec DISCARDED it (`_`) and classified by `ctx.Err() != nil`: a check that races the two clocks. Under runner load (the healthz readyz-refresh deadline noise in the log tail — the box was missing timers), the read error surfaced while the ctx timer callback had not run: ctx.Err()==nil → transport → `script_env_unavailable` HTTP 200 instead of the 504 `script_timeout`. `UploadApply` (control_client.go:344-360) has had the deterministic `ctxDerived && deadlineFired` classification since design-0060 §3.3 — ScriptExec simply never adopted it.

### 3. ConcurrentWallTime — wall-ratio discriminator, load-biased in opposite directions

The test proved parallelism via `concurrentWall < 3/4 × serializedWall`, both measured in-run. But the serialized phase (one apply at a time) and the concurrent phase (4 applies × 1 MiB copies under -race instrumentation) suffer load in OPPOSITE directions: contention slows only the concurrent phase's copies. The CI red (195.8 ms vs 231.4 ms bound 173.5 ms) false-failed while the applies were fully parallel — each concurrent apply ran ~3.4× slower than in the serialized phase, pure CPU contention. The property the pin exists to prove is the ABSENCE of the #1539 applyMu serialization, whose observable is that applies cannot overlap inside `Apply` — the rename seam sits inside the (formerly) locked region.

## Fixes

1. **F1 (test-side, `api/internal/handlers`)**: `newBusySnapHarness` mints a per-harness UNIQUE workspace id (`busySnapWS-<seq>` via `busySnapWSSeq`), carried as `hs.ws` and used by every cue/expectation. A leaked flight's deferred Delete can now only ever remove its own harness's key — the shared-key channel is structurally closed, no timing assumptions. The #1927 construction-time marker Delete stays (no-op guard) with an updated rationale; `TestBusySnapHarness_MarkerKeysPerHarness` pins the uniqueness. `TestFetchStatuszSessions_UnhappyPaths` keeps the raw const (it builds handlers directly and never touches the marker).
2. **F2 (production, `cmd/workspace-agentd/script_exec.go`)**: `ScriptExec` adopts UploadApply's §3.3 discipline — on `err != nil`, `ctxDerived && deadlineFired → script_timeout` by construction (deterministic regardless of which clock wakes first); the `ctx.Err()` leg remains for timer-first wakeups and cancellation; the closed-enum and transport legs unchanged. New pin `TestScriptExec_ClassifiesDerivedDeadlineAsTimeout`: accept-but-never-answer supervisor + 300 ms budget must classify `script_timeout`.
3. **F3 (test-side, `cmd/workspace-agentd/upload_apply_test.go`)**: the discriminator is now the rename seam's concurrency high-water mark — each apply holds the seam 50 ms (pure timer parking, no CPU), `seamMax` tracks the max simultaneously-held seams. Serialized world (the #1539 lock): exactly 1, deterministically. Parallel world: ≥2 — four goroutines reach the seam within single-digit-ms (the 1 MiB copy) and overlap their 50 ms holds; a miss would require leaving a runnable goroutine unscheduled 50 ms+ while the others sleep on idle CPUs. Baseline apply kept (sanity through the real copy+rename path). No assertion weakened: seam-overlap IS the parallelism property, asserted directly instead of via a load-biased ratio.

## Validation (red-first) + evidence

- **F1 red**: pre-fix origin/main + planted exact-leak simulation (a `busyFlightsInFlight.Delete(busySnapWS)` landing between cue1's LoadOrStore and the re-cues — the byte-for-byte operation a leaked predecessor flight's defer executes): RED deterministically, `expected: 1, actual: 2` at 0.11 s — IDENTICAL to the CI signature (numbers and duration). Post-fix: the same plant is a no-op (key mismatch) — GREEN ×5 `-race -count=5`.
- **F2 red**: the CI attempt-1 log is the red (the exact interleaving needs timer-wheel latency an idle box won't produce; the signature `read i/o timeout` observed with ctx.Err()==nil is only reachable through the raced check). Structural parity: UploadApply's comment names this exact race and its fix. Post-fix pin green under stress (below).
- **F3 red**: the CI attempt-2 log is the red (needs 4× copy-time skew between phases). Mutation verification below proves the new discriminator catches the exact regression it exists for.

## Verification runs

(post-fix, isolated worktree /tmp/opencode/lsq-deflake at e27c2e6d5 + fixes; memory-guarded, one heavy command at a time)

- see PR body for the executed matrix; gates: `go test ./api/internal/handlers/ -race -count=3`, `go test ./cmd/workspace-agentd/ -race -count=3`, full-repo `go test ./... -race -count=1` (104 pkgs) with a private TEST_DATABASE_URL on a private PG port, plus family stress (-count≥10, GOMAXPROCS 1/2 arms) and the F3 mutation (serialization lock re-added → deterministic RED `max concurrent seams: 1`).

## Operational notes

- **Shared-worktree collision (2026-10-26)**: the /workspace/LLMSafeSpaces worktree's HEAD was moved to lane-1959 mid-lane by a sibling session; their uncommitted pkg/board + api/internal/app work was swept into stash@{0} ("On lane-deflake: deflake-wip") when this lane stashed its edits. The stash entry is KEPT (do not drop — it carries the sibling's swept edits); this lane moved to the isolated worktree /tmp/opencode/lsq-deflake (git worktree, branch fix/deflake-release-blockers) and recovered its five files from the stash read-only. Sibling lane: coordinate via COORDINATE.md.

## House

Worklog `worklogs/NNNN_deflake-release-blockers.md` (sentinel — bot renumbers) · CHANGELOG [Unreleased] append · COORDINATE.md active-claim row. Never self-merge — orchestrator gates and merges.
