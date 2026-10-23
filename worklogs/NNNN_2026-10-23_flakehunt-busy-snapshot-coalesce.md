# Flake hunt — TestRequestSessionStatusSnapshot_Coalesces de-flake (issue #1907) + #1842 post-fix verification

Date: 2026-10-23 · Lane: flakehunt (orchestrator leisure-wasabi-covers) · Branch: fix/flakehunt-upload-stream · Issues: #1907 (filed this lane, fixes) · #1842 (verification-only)

## Objective

De-flake the two known CI flake classes on main: fix the busy-snapshot coalesce flake (test-side, red-first) and verify the already-landed #1842 upload-stream fix — no production code changes.

## Charter scope

Two known CI flake classes on main:

1. **#1842 TestUpload_Streams_WithoutFullBuffering** — found ALREADY FIXED on main (commit 2a300c9d, 2026-10-23T08:00Z; issue CLOSED; worklog 1200). Handled as **verification-only, no diff**: `-race -count=50` green (100 streaming runs, 6.9 s) and the lane's mutation-verify recipe re-confirmed on current main — `streamSignalThreshold` flipped to `cap1MiB` → BOTH `TestUpload_Streams_*` red deterministically at 10.02 s in one `-count=1` → restored green. No post-fix reds on main CI since (runs through 40014117897 checked: zero failed jobs).
2. **TestRequestSessionStatusSnapshot_Coalesces** — live flake, last main-red evidence run 39914818351. No issue existed → **filed #1907** with the reproduction evidence. Fixed test-side in this PR.

## Root cause of the coalesce flake (proven, not presumed)

CI red (run 39914818351, job 120297406989 "Test (-short, with coverage)", head ba3bc7bf):

```
--- FAIL: TestRequestSessionStatusSnapshot_Coalesces (2.01s)
        Error Trace: proxy_busy_snapshot_test.go:567
        Messages:    concurrent triggers must coalesce to one authority fetch (got 2)
```

**Test-design flaw: the coalescing precondition ("while a flight is running") is never established. Not a product race** — the guard `busyFlightsInFlight.LoadOrStore` (proxy_busy_snapshot.go:262-270) is an atomic CAS with no check-then-act window.

The old body fired two naked `http.Post`s "nearly simultaneously" and asserted `fetches == 1`. Against a statusz stub that answers instantly, the first flight's entire lifetime (LoadOrStore → emit: begin → fetch → rows → complete → Delete) is sub-millisecond. Nothing proved the second POST's guard ran inside that window. Under CI load (-short + coverage instrumentation + 25 parallel packages) the second POST's dial+handler latency exceeded the first flight's duration → no flight running at guard time → the second trigger **legitimately** started its own flight (the first had completed and published its markers) → `fetches = 2`. The counter is monotonic, so `assert.Eventually(==1)` could never recover — the 2.01 s signature.

Organic local rate on an unloaded box: 0 in 50 iterations (0.02 s each) — consistent with a load-dependent window miss, mirroring the #1842 lane's local/CI rate asymmetry.

## Fix (test-only)

`TestRequestSessionStatusSnapshot_Coalesces` rewritten to ESTABLISH the precondition deterministically instead of hoping for it:

- The statusz stub **parks the first authority fetch** (`firstFetch` channel + `release` channel).
- Trigger 1 fires; the test waits until its flight's fetch is provably parked. Trigger 2 fires only then — **strictly inside the open coalescing window**: the parked flight's map entry cannot be gone (Delete runs only after the fetch returns), so trigger 2's guard (proven run by its 202) MUST coalesce.
- Four nets (overlapping, not fully independent — r1 review finding): fetch-count assert at the parked phase; stub-side `t.Errorf` on any second fetch while the stub lives; pre-release `snapshot_complete` drain (impossible while the only flight is parked); post-settle `t.Cleanup` verdict. The verdict does NOT rely on server-drain ordering (an earlier draft's comment wrongly claimed the deferred router-server Close drained stub calls — the statusz stub is a different server, closed by an earlier-registered, hence LIFO-later, cleanup): the counter increments at stub-handler entry, so every fetch that has begun is already counted, and in the happy path no second fetch can exist past the observed completion (both triggers coalesced, none remain). The one escape window — a fetch that has not yet begun by cleanup time — is disclosed; both causal mutations land well inside it (author 3/3, reviewer-independent 1/1 each).
- No production code changed. No sleeps/retries/skips added.

## Red-first / mutation-verify

Pin commit **613238f3** (the rewritten test) reddens deterministically under both causal mutations — runs cited at that exact tree:

- **Mutation A — neutered guard** (`triggerBusySnapshotFlight` always spawns): RED `-race -count=3` 3/3 (and 1/1 re-run at the committed hash), `authority fetch #2` stub flag + `final verdict: exactly one authority fetch must serve both triggers (got 2)`.
- **Mutation B — early Delete** (map entry released before the flight runs — the steady-state analogue of what CI load did transiently, window collapsed to zero): RED `-race -count=3` 3/3 (and 1/1 at the committed hash), same double signature.

Mutation B is the important one: an earlier concurrent-fire draft of the pin caught it only 0/3 (the second POST's guard consistently beat the first flight's goroutine to the map). The parked-first-fetch construction removes that race from the TEST itself — trigger 2's guard runs strictly after the park, so an early-deleted key is deterministically observed absent.

Orchestrator gate recipe (b), as registered: delete the `LoadOrStore` guard in `triggerBusySnapshotFlight` on the merged tree → the test must go red within one `-race -count=1` (deterministic; no loop needed).

## Evidence

- New pin green: `-race -count=20` (20/20); stability arms on the pin: `-short -covermode=atomic -coverpkg=./... -count=100` green; `-race -count=100` green; `GOMAXPROCS=1 -count=50` green; `GOMAXPROCS=2 -race -count=50` green — 300+ consecutive iterations, zero flake.
- #1842 verification: `-race -count=50` green; recipe (a) re-confirmed (threshold flip → deterministic red both streaming tests → restored).
- Full `api/internal/handlers` package: `-race -count=1` ok (256.8 s) and CI-shape `-short -covermode=atomic -count=1` ok (53.4 s, 75.4 % coverage) — both green at 91d6bb3b.

## Assumptions / residuals

- Production coalescing is correct and unchanged; `got 2` was the product doing exactly what its contract says for non-overlapping triggers.
- `busyFlightsInFlight` is a package-level global shared across tests in the file; a leaked entry from an earlier test would surface as the parked-phase count assert failing (fetches==0 path via the firstFetch timeout) — an exposure the old test shared, now with a louder message.
- #1842 needed no new work: fix verified on current main; recipe (a) re-validated for the orchestrator's gate.
