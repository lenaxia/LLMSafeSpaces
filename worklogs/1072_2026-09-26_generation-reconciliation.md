# NNNN 2026-09-26 — PR2: #1576 layers 1+2 + #1573 asks 2/3 (generation reconciliation + declared timeouts)

Lane: fix/1576-generation-reconciliation (w2), off main@99f6cdde.
Split per the orchestrator: PR1 (#1578, MERGED) took the verify guards
+ busy-from-data; this PR takes the recovery half — the generation
signal, the D2 resets, tracker reconciliation, and the tools' declared
timeouts. PR3 (process liveness) and PR4 (D6 verify-then-act) follow.

## The terrain (what already exists)

- Layer 1's PROJECTION half exists: Reseed(GenerationChange) →
  captureOrphanedPartsLocked → the abort fold (#1342 S12, reason
  "harness restart"). The gap is the SIGNAL: in split mode the sidecar
  NEVER learns that the supervisor respawned opencode
  (supervise_opencode.go's onChildStarted = nil by design — no tracker
  in the supervisor), so the reseed + D2 reset fire on NEITHER
  boot-restore NOR respawn (#1573's live finding). Single-container
  mode wires onChildStarted (main.go:438).
- The signal path exists unused: the sidecar's status poller already
  fetches controlStatus.ChildPID/Restarts from the supervisor socket
  every 15s — a PID change IS the generation boundary (observable
  truth, the S6 doctrine; no new channel needed).
- Layer 2's raw material: ToolState carries StartedAt; the declared
  timeout lives in ToolPart.Input JSON (e.g. bash 700000ms). No ABI
  change needed — parse at PART_START fold, deadline map in the
  record, the reconcile pass terminates past-deadline RUNNING parts.
  Zero heuristics: no declared timeout → no layer-2 action.

## The deferral design steer (owner, effective branch-cut)

Restart deferral is on probation. This PR: (1) adds NO new deferral
machinery; (2) puts the busy consumers (restart decision, suspend
gating) behind a swappable seam — defer-until-idle is one
implementation; (3) prefers the turn-boundary shape for APPLYING
pending restarts (apply at turn-completion events, not busy-flag
polling) — policy choice left open; (4) surfaces the numbers for the
owner's keep/replace/remove decision: deferral firings, stall
durations, orphaned-flag clear rate.

## Round 2 (w3 resume — lane rotated from w2 after turn-death; assessment approved by the orchestrator)

Completed the WIP seam:
- **Gap 1 (the real one)**: the stall histogram's own contract promised applying/forcing/canceling — the FORCE leg never observed. Fixed: the force call site observes with deferredAt; TestDeferSeam_ForceLegObservesStall pins it (a source stuck busy-and-stalled defers, forces, and the datum lands — the deferred-then-forced restart is exactly the case the owner's keep/replace/remove decision needs data on). Test-detail: testutil.CollectAndCount counts METRICS not observations (a histogram is always "1") — the test reads the sample count via the default gatherer; the restart and the observation are deliberately separate events, so the test waits for the datum, not the restart.
- **Gap 2 (per orchestrator ruling)**: the two `source.(*sessionStatusTracker)` assertions kept, WHY documented at both sites (prune is tracker-specific C2a hygiene, not policy; folding it into the seam would widen it into new machinery, which the steer forbids).
- Gap 3 (the fake's advance-inside-Eventually timing coupling): agreed, left as-is.
- Branch rebased onto v0.34.9 main; all seam + restart-decision + rearm + generation-signal families green; vet/gofmt clean.

### Round 3 (#1584 r1 findings — all addressed)

1. **The D2 test failed on clean checkouts** (WORKSPACE_ID normalization): t.Setenv pins the label; verified green under a deliberately EMPTY env. The "green locally" claim in the PR body was masked by the dev-pod env — owned, and the full-suite claim this round is left to CI (targeted families listed with commands instead).
2. **The declared-timeout fold did not achieve its stated busy effect** — THREE stacked causes found and fixed: (a) the fold's terminal part stayed in the busy count → activePartCount counts only non-terminal parts (terminal TOOL parts render but stop counting); (b) rec.busy — the SSE STREAMING flag — kept busy latched with the harness dead → the fold clears busy and marks the session ERROR when it terminates the LAST active part (arming the #1578 terminal veto); (c) rederiveStatusLocked's un-gated up-direction re-latched busy from the dead harness's own stale store table → the fold-dead hold (rec.foldedDead) refuses store-BUSY re-derivation until any REAL harness event clears it (applyContractLocked). Pinned by the Reconcile-level row (store-backed authority, public pass, busy true→false with the ERROR part renderable) + FoldHoldClearsOnRealEvent (the recovery direction).
3. **"Fixes #1576" overclaimed closure** (4-layer issue, layers 3+4 unlanded): commit footers reworded via filter-branch (Refs #1576 layers 1+2), PR body updated; #1573 ask-2 attribution corrected (landed in #1578's busyTruthFrom, not here).
4. **Store-gate ordering**: enforceDeclaredTimeoutsLocked moved ABOVE the Store==nil return (its store-independence comments were false below it); the Reconcile-level row is the gate-order pin.
5. **No revert-proof wiring pin**: sidecar_generation_pin_test.go source-greps the production callback (D2 reset + retrying reseed) — deleting the wire now goes red (the poller test injects its own callback and never caught it).
6. **Robustness minors**: composite generation key (PID + Restarts epoch — PID reuse across container restarts pinned); toolDeadlines cleared with inFly (the per-session leak); per-part terminal stamps (the shared alias).
7. **Style**: the glued reconcileLocked/enforce docs split; DeclaredTimeouts surfaced (fold-site log + stats on both paths). While writing the row I found two further bugs — sweepAgainstEvidence's return REPLACED stats (wiping DeclaredTimeouts) and the stats assignment order — both fixed and folded into the Reconcile-level pin.
8. Fold-reason deviation documented in the PR body: the projection folds with the #1342-pinned "harness restart" reason, not the issue's literal orphaned_by_restart label.

## Round log

- (opening) Terrain mapped; tests first.
- (w3) Resume assessment delivered + approved; gaps 1-2 closed, gap-3 untouched; force-leg datum live.
- (w3 r1) Six+ findings closed: env-independence, the busy effect (three stacked causes), closure semantics, gate order, wiring pin, composite key, minors. Full sessionstate family green 31.4s; agentd targeted families green; full suites owned by CI.

## Round 1 code (two commits)

1. **The generation signal** (eac93e1f): the sidecar's status poller
   detects generation boundaries from the supervisor's ChildPID (first
   observation = boot; every change = respawn) and fires the D2 reset
   + the authority's S12 generation reseed at that edge — #1573's live
   finding (NEITHER fired in split mode) closed at the wiring level,
   revert-proof through the real socket. The orphaned-flag datum rides
   the EXISTING workspace_tracker_busy_resets_total counter (no
   duplicate metric).
2. **Declared timeouts** (layer 2): tool INPUT's declared timeout (ms)
   registers a per-part deadline at upsert (StartedAt + timeout); the
   reconcile pass folds RUNNING parts past their own deadline terminal
   (ERROR + "declared timeout exceeded" + completed stamp,
   store-independent, runs first, counted in
   ReconcileStats.DeclaredTimeouts). Zero heuristics: no declaration →
   no action (process liveness is PR3's). Four pins incl. the
   within-window hold and the completed-part untouched.
