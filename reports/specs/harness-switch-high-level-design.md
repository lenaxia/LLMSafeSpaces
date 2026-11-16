# Harness switch — high-level design (supervised recreation, preserved PVC)

**Status:** Design (2026-11-15) — the WHAT/WHY for the recommended option (b) of
`harness-switch-options-writeup.md`; the implementation contract is the companion
`harness-switch-low-level-design.md`. Patched charters of `harness-switch-api`
(`tsk-visioned-hurdle-leaves`) and `harness-switch-ui` (`tsk-vibrant-geyser-masks`) carry these
decisions and are binding.
**Seam verification:** anchors verified against `origin/main` `71cb54f80`; design 0073 read at
`eca3368ca` (`origin/pi-drift-s6`). The implementation stories branch off main AFTER the S6/S7
stack's main PR lands (their seams — `spec.harness`, router, webhook 7a, harness delivery — are
already on main; 0073 is cited for rulings, not code).

---

## 1. The switch, in half a page

A harness switch is a **user-consented, supervised pod recreation that preserves everything the
workspace owns except the running processes**. The owner requests `harness: pi` through a dedicated
API surface; the API sweeps the workspace's board stories out of live claims (clean requeue with
salvage pointers — no attempt burn), writes `spec.harness` behind a webhook gate, and answers
`202 Accepted`; the controller observes the harness drift, politely drains in-flight user turns,
deletes the pod, and rebuilds it from the new harness's delivery map against the retained PVC;
`status.harness` flips when the new pod is built, and from that moment the router, the session
sidebar, and the UI serve pi truth. Old-harness sessions stay on disk, hidden and reaper-shielded;
switching back restores them symmetrically.

The design changes exactly one ruling — "harness is immutable" becomes "**harness changes only
through the switch surface**" — and reuses every drilled mechanism for the consequences:
restart/architecture-drift recreation (`phase_active.go:79-114,202-231`), #761 session drain
(`session_drain.go:169`), the suspend/freeze/depose claim-hygiene family (`pkg/board/suspend.go`,
`depose.go`), the reconcile-aware session index (`proxy_session_index.go:285-299`), and the
existing long-op (202 + spec-write, controller-as-single-status-writer) API convention.

## 2. The lifecycle (one pass, end to end)

```
  owner: PUT /api/v1/workspaces/{id}/harness {harness:"pi"}
      │
      ├─ guards: phase ∈ {Active, Failed, Suspended}; harness ∈ {opencode,pi};
      │        same value → 204 no-op (nothing happens)
      ├─ BOARD SWEEP pass 1 (API, before any spec write):
      │     in_progress claims (incl. pending_input-flagged) → clean requeue to backlog:
      │       claim cleared, NO failure overlay, NO cooldown, salvage pointer attached
      │       ("harness switch — inherit, don't restart"), stop directive to the old worker
      │     in_review persona claims → claim cleared in place, NO #1813 accounting;
      │       the rc1 re-arm pass re-mints a reviewer after the switch
      ├─ SPEC WRITE: spec.harness=pi + marker annotation llmsafespaces.dev/harness-switch=pi
      │        (webhook 7a′: a harness delta without the matching marker is DENIED —
      │         raw kubectl flips stay refused, exactly like today, with a pointer to the API)
      │        ← the write arms the quiesce gate: on the gate's OBSERVED view no new
      │          claim enters (bounded informer-lag residual per LL §1a step 5)
      ├─ BOARD SWEEP pass 2 (under the armed gate — catches the pass-1/spec-write
      │     racer; idempotent, usually empty)
      └─ 202 {harness:"pi", phase, switched:true}
      │
  controller (handleActive, new harness-drift arm — the arch-drift shape):
      spec.harness != status.harness?
         ├─ status.harness backfill first ("" + live pod + NO marker → write status=spec —
         │  ANY harness; webhook 7a means every markerless existing pod was built for its spec
         │  value; no recreation — the legacy fleet, opencode AND kubectl-created pi, must not
         │  mass-recreate. Marker present ⇒ NOT backfill: a pre-backfill first switch — the
         │  drift arm recreates, never stamping status onto the old pod)
        ├─ drainBeforePodDeletion(reason="harness_switch")  ← user turns finish politely;
        │    board workers are already swept; force after 60m no-progress (#761 unchanged)
        ├─ deletePodByName → phase=Creating → clear the marker
        └─ buildPod re-reads spec.harness (overlay, supervisor subcommand, AGENTD_HARNESS —
           already harness-aware) → status.harness=pi written at build → Creating→Active
      │
  switch window (spec write → status flip):
      router serves status.harness (old) — mis-routed calls fail LOUD (wrong-harness
      bridge, 404-class; resolution failure pre-PodIP — LL §3),
      never silent-wrong; claim gate: on the gate's observed view, no new
      claims/spawns/review-mints into the workspace (the suspend-gate shape,
      keyed on spec≠status instead of phase; the multi-replica informer-lag
      residual is documented in LL §1a step 5 — one bounded grant, rearm exit)
      │
  sidebar/UI: rows WHERE harness=status.harness (old rows hidden, preserved);
  on switch-back the same predicate restores them. Done.
```

## 3. Design decisions

- **D1 — Dedicated endpoint, not PATCH.** `PUT /api/v1/workspaces/{id}/harness`. The house already
  ruled against a general PATCH ("the only mutable networkAccess field in v1 is devPreview" —
  dedicated `PUT /dev-preview` instead, `router.go:1638-1640`); harness is exactly the same shape:
  one mutable topology field, one dedicated verb. `PUT` (declarative set, idempotent): same value
  → `204` no-op; changed value → the long-op path. `POST`-as-action and `PATCH` were considered
  and rejected (not an action verb's semantics; PATCH contradicts the recorded policy).
- **D2 — Long-op convention: spec write + 202, controller is sole status writer.** Mirrors
  suspend (`202`, `router.go:1673-1684`) and refresh-compute (`202` + JSON body,
  `router.go:1710-1740`); progress via `GET /:id/status` + `workspace.phase` SSE. No new job
  object: the recreation *is* the observable (phase walk + `status.harness` flip), exactly like
  restart-generation — a jobs table would duplicate the CRD's own state machine.
- **D3 — Webhook 7a′: switch-gated, not immutable.** A harness delta is admitted only with the
  marker annotation `llmsafespaces.dev/harness-switch` equal to the new value (the
  `AnnotationForceRecycle` bookkeeping pattern, `force_recycle.go:38-64`); markerless flips keep
  today's denial, re-pointed at the API. The marker is consent bookkeeping, not an auth boundary
  (same trust posture as force-recycle); the controller clears it after first observation.
- **D4 — Controller: a harness-drift arm in `handleActive`, plus a marker-aware backfill.**
  Desired vs `status.harness`; polite drain with a new reason `harness_switch`; delete →
  `Creating` → `buildPod` (already harness-aware). `status.harness == ""` on legacy pods
  backfills to **spec.harness** (any harness — webhook 7a immutability means every MARKERLESS
  existing pod, including kubectl-created pi workspaces, was built for exactly its spec value)
  without recreation. A marker-present `status.harness == ""` object is the pre-backfill
  first-switch case — the backfill skips it and the drift arm recreates (stamping
  `status=desired` onto the old-harness pod would wedge the switch permanently: spec==status
  forever, no recreation, permanent mis-routing, hidden live sessions — LL §2a; pinned by
  `TestHarnessDrift_BackfillSkipsMarkerPresent`). Switch requested while Suspended needs no
  recreation at all — the next activation builds the new harness naturally.
- **D5 — Board: sweep, don't freeze.** The #2040 lesson (never burn attempts on infra moves)
  is satisfied by *requeueing* the work to the new harness, not by freezing dead claims: the
  old-harness sessions can never resume (their harness is unservable), so a frozen lease has no
  exit. The sweep is the depose *handoff* (salvage pointer, inherit-don't-restart,
  `depose.go:30-40`) without the depose *accounting* (failure overlay + backoff, `depose.go:169-186`
  — right for lapse, wrong for infra). In-review claims are cleared in place with no #1813
  accounting; the existing re-arm pass re-mints reviewers after the switch.
- **D6 — The switch window is claim-quiesced.** Between spec write and status flip, the claim
  choke point quiet-no-grants, `task_start` 409s, dispatch-drive skips, and review re-arm skips
  the workspace — the suspension gates' exact shape (`engine.go:359-367,175-181`,
  `reconciler.go:315-360,735-809`) keyed on `spec.harness != status.harness` instead of phase. A
  fresh worker must not spawn into a pod about to change harness (and the spawner's
  `EnsureActive` auto-resume hazard is the recorded reason the suspension gate exists).
- **D7 — Session-space: HIDDEN-PRESERVED (the writeup's recommendation).** `session_index` rows
  carry the owning harness; lists/overlays consider only `harness = status.harness`;
  foreign-harness rows are invisible to users, invisible to the reconcile reaper (no miss
  counting — the "harness store reset" false-positive class, F5), and restored on switch-back by
  the same predicate. Direct reads answer the typed gone-class. Disk is never touched.
- **D8 — Routing pins to the pod: `status.harness`.** `GetWorkspaceHarness` resolves
  `status.harness` first (spec fallback for pre-switch objects); the router's 60s TTL stays.
  On a STATUS-SET switch the stale window is ≤60s of loud errors, never wrong data. Because both
  harnesses hold the
  same port (4096 — the pi bridge on pi pods, `pi_bridge.go:8-9`) and the pod IP resolves fresh
  per call, a stale post-flip route DOES reach the new pod's live wrong-harness bridge — but
  the bridge is a dumb pipe with no cross-harness route shims (`pi_bridge.go:14-15`), so
  foreign route shapes fail loud 404-class; before the new pod reports its IP (`status.PodIP`
  cleared at deletion) resolution itself fails — the new harness cannot be reached before it
  exists. PRE-BACKFILL first switch (status.harness == "", the flagship legacy case): the spec
  fallback serves the NEW harness from router-TTL expiry (the first ≤60s the stale cache
  actually routes correctly — old harness, old pod) until `buildPod` writes status — a window
  bounded by controller reconcile plus drain, which a busy-session defer can stretch toward the
  60m force (`session_drain.go`), not by the TTL. In that window new calls against the old live
  pod fail loud 404-class (never wrong-data) and in-flight turns still finish (their streams
  predate the flip; the drain watches them) — the drain's politeness covers in-flight turns,
  not new calls, for this class. The lookup-failure fail-open
  tightens from the static opencode default to the observed harness.
- **D9 — UI: a settings-drawer control with the destructive-confirm convention.** The harness
  selector lives in `WorkspaceSettingsDrawer` (the settings surface; lifecycle kebab actions stay
  in the sidebar); confirm dialog per `ConfirmDialog` conventions with the refresh-compute copy
  shape ("pod is rebuilt… your files and workspace data are preserved") plus the session-space
  consequence and the board-sweep consequence stated plainly; during the switch the existing
  `isTransitioning` machinery (phase `Creating`) disables the composer and the control renders
  "Switching to pi…"; errors render in the existing lanes; the control is enabled iff the API
  reports a switchable state (phase ∈ {Active, Failed, Suspended}). The hardcoded
  `opencode v…` badge (`Sidebar.tsx:412-415`) becomes harness-truthful.
- **D10 — Abort and rollback are the same move: switch back.** Before pod deletion the drift arm
  hasn't fired; PUTting the old value back converges with no recreation on a STATUS-SET switch
  (status.harness written — the post-backfill fleet and every once-switched workspace). A
  PRE-BACKFILL first switch (status.harness == "") aborted pre-drain pays §2a's known benign
  cost — exactly one convergent same-harness recreation (status=""+marker is indistinguishable
  from drift; the controller cannot know a status="" pod's true harness). In both cases the
  sweep's cost —
  re-claims with salvage pointers — is the only residue, and it is honest work, not damage).
  After deletion the switch is committed; there is no in-flight cancel (mid-recreation
  cancellation would add a third pod state for no product value). A failed new-harness build
  (e.g. pi overlay verify) lands the workspace in Failed with the verify condition — from which
  switching back is allowed. Switch-back restores old sessions by D7's predicate, symmetrically.
- **D11 — Create-side selection ships with the API story.** `CreateWorkspaceRequest.Harness`
  (validated enum, default opencode) → `buildWorkspaceCRD` (F1: selection is kubectl-only today).
  Same validation surface as the switch; without it, "user-facing harness selection" remains
  half-true.
- **D12 — What does NOT happen.** No config migration between harnesses (each harness's writer
  renders its own tree at boot); no session conversion or export; no dual-runtime anything
  (option (c), rejected); no cross-workspace session import; no new board lifecycle (the sweep is
  two SQL siblings of existing statements); the board itself is already harness-agnostic (the
  seamless-lease ruling) — its PVC registry and DB rows survive the switch untouched.

## 4. Failure modes and answers

| Failure | Answer |
|---|---|
| Spec write lands, sweep already ran, user aborts pre-drain | Switch back (D10); residue = honest re-claims with salvage pointers |
| Drain wedges (busy session, no progress) | #761 unchanged: force after 60m (`session_drain.go:91,220-229`) |
| New-harness pod fails build/verify | Phase Failed + verify condition (85/86 for pi); switch back allowed (D10) |
| Claim lands in the window | Quiesced on the gate's observed view (D6): no-grant / 409 / dispatch skip / re-arm skip; bounded informer-lag residual — one admitted grant on a lagging replica, rearm exit (LL §1a step 5) |
| API replica's router cache stale ≤60s post-flip | Loud errors only (D8); self-heals at TTL |
| Session-index reconcile sees foreign-harness rows | Shielded by harness predicate (D7); no reap, no metric storm |
| Raw kubectl harness flip | Webhook 7a′ denies markerless deltas (D3) |
| Legacy fleet (status.harness empty, marker absent) | Marker-aware backfill to spec, no recreation — any harness (D4) |
| First switch on a legacy workspace, before its backfill reconcile | Marker present + status="" ⇒ backfill SKIPPED, drift arm recreates (the switch converges to what it asked for); an aborted pre-backfill switch costs one benign convergent recreation of a same-harness pod (LL §2a) |
| Legacy kubectl-pi session rows (hardcoded "opencode" registrations, F5) | Repeatable boot sweep to the owning workspace's spec.harness, scoped to never-switched workspaces (LL §5a) — live pi sessions stay visible; the SQL default covers orphans only |
| Session row mis-stamped in the rolling-update window (old replica registers after the sweep ran) | Next boot's sweep heals it (visibility gap bounded by the rollout, never data loss); mid-rollout switch + stale-replica write is the accepted narrow residual (LL §5a) |
| Switch while board stories pending_input | Swept with the flag cleared, pointer notes the awaiting-input state (D5) |

## 5. Non-goals

Per-harness config translation; session format conversion; dual-runtime pods (option (c));
cross-workspace anything; a switch job/status object beyond the CRD's own phase + harness fields;
auto-rollback heuristics (the owner decides; switch-back is one PUT).

## 6. Open questions (pinned defaults, owner-overridable)

- **Switch while Suspending/Resuming/Creating** — default: `409` with the phase named (retry when
  settled). Suspended *is* allowed (no pod to lose).
- **Concurrent switches from two tabs** — last write wins (the marker matches the newest spec;
  the webhook re-validates); the UI disables the control while a switch is pending.
- **`switchable` on the wire** — default: derived (phase ∈ {Active, Failed, Suspended}); exposed
  as a computed field on the status DTO rather than new CRD state.
