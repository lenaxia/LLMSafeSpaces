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

## Round log

- (opening) Terrain mapped; tests first.

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
