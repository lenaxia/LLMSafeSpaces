# Harness switch — low-level design (implementation contract)

**Status:** Design (2026-11-15) — the HOW companion to `harness-switch-high-level-design.md`
(decisions D1–D12 cited below by tag). Anchors verified against `origin/main` `71cb54f80`.
Migration numbering: next available at implementation time (`000055` today; if the wrap-up pair
(#2057) lands first, take the next free number — never share a file, the 000042→000049 precedent).
Where a story name appears in brackets, that story owns the change: **[api]** =
`harness-switch-api` (`tsk-visioned-hurdle-leaves`), **[ui]** = `harness-switch-ui`
(`tsk-vibrant-geyser-masks`).

---

## 1. API surface [api]

### 1a. Route + DTOs

`api/internal/server/router.go`, inside `registerWorkspaceRoutes` next to the dev-preview
precedent (`:1638-1640` — the recorded no-general-PATCH ruling; D1):

```go
// Harness switch: dedicated endpoint, not a general PATCH (the dev-preview
// ruling). PUT = declarative set: same value is a 204 no-op; a changed
// value rides the long-op convention (spec write + 202, controller is the
// sole status writer). The recreation preserves the PVC; board claims are
// swept BEFORE the spec write (no attempt burn — #2040).
idGroup.PUT("/harness", func(c *gin.Context) { ... wsSvc.SwitchHarness ... })
```

Wire shapes (`pkg/types/workspace.go`):

```go
type SwitchHarnessRequest struct {
    Harness string `json:"harness" binding:"required,oneof=opencode pi"`
}
type SwitchHarnessResponse struct {
    Harness  string `json:"harness"`            // the requested (desired) harness
    Phase    string `json:"phase"`              // phase at response time
    Switched bool   `json:"switched"`           // false ⇒ was a no-op
}
```

Service `SwitchHarness(ctx, userID, wsID, harness)` in
`api/internal/services/workspace/workspace_service.go` (next to `RefreshWorkspaceCompute` `:965` /
`SuspendWorkspace` `:766`; the phase-guard + `RetryOnConflict` shapes are lifted from
`ActivateWorkspace` `:1767-1876`):

1. **Guard** — terminal/Terminating reject (the refresh shape `:988`); phase ∈
   {`Active`,`Failed`,`Suspended`} else `409 {error, phase}` (D10 open question default); suspend
   semantics: a `Suspended` target performs NO recreation — spec write only; the next activation
   builds the new harness (controller arm handles it naturally, §2). A LEGACY workspace
   (`status.harness == ""`) in an allowed phase is ADMITTED, not 409'd: the first switch on an
   existing workspace is this epic's flagship use case, and the marker-aware backfill (§2a)
   converges it correctly (drift-recreate of the old-harness pod) — a
   retry-when-status-backfilled guard was considered and rejected for exactly that reason.
2. **Same-value short-circuit** — `204`, no sweep, no write, `switched:false` semantics.
3. **Board sweep, pass 1** (§4) — BEFORE any spec write; a sweep failure aborts the request
   (500), nothing else happened.
4. **Spec write** — under `RetryOnConflict`: `spec.harness = <harness>` AND
   `annotations[AnnotationHarnessSwitch] = <harness>` AND (set-once)
   `annotations[AnnotationHarnessSwitchedAt] = now()` in ONE update (the marker must land with the
   delta or webhook 7a′ denies the write itself; the persistent companion lands with it and is
   never cleared — §2b). This arms the quiesce gate (§4b): from here on,
   on the gate's observed view, no new claim enters the workspace (the informer-lag residual is
   step 5's, below).
5. **Board sweep, pass 2** (§4) — after the spec write. Pass 1 closes the live-claim surface
   before anything moves; pass 2 catches the only remaining race — a claim granted in the window
   between pass 1 and the spec write (a claimOnce firing in those milliseconds) — and a racer
   swept by pass 2 cannot re-claim (observed view; the residual's one lagging-replica grant is
   below). Usually empty; idempotent. **Pass 2
   waits for the gate's observed arm**: it polls `SwitchingWorkspaces()` (bounded — ≤30s, 2s
   interval; fail-open to sweeping on timeout, residue documented below) until THIS process's
   cache observes the workspace armed, because grants issued before the observed arm are swept
   and grants after it are refused — only then does pass 2 declare quiet. Residual (documented,
   the suspension freeze's accepted posture, `board_suspend_source.go:19-25`): an API replica
   whose informer cache lags the spec write can admit one grant after THIS replica's pass 2
   declared quiet — a bounded (informer-lag, seconds) propagation window, one row worst case,
   whose outcome is a single burned attempt with the rearm lane as the exit. The contract claims
   quiet relative to the gate's observed view, never an absolute. A persistent pass-2 failure
   responds 500 with the switch in flight (same residue class).
6. **Respond** `202` + `SwitchHarnessResponse`.

### 1b. Create-side selection [api] (D11)

`types.CreateWorkspaceRequest` (`pkg/types/workspace.go:35-47`) gains
`Harness string \`json:"harness,omitempty"\`` (`oneof=opencode pi` at the handler; empty = CRD
default opencode); `buildWorkspaceCRD` (`workspace_service.go:1296-1348`) copies it through. No
other create-path change; pi's delivery availability stays a controller-build concern
(`validateWorkspaceHarness` fails loud, §2).

### 1c. Status/read surface [api]

- `pkg/apis/llmsafespaces/v1/workspace_types.go` `WorkspaceStatus` (`:671-794`) gains:

```go
// Harness is the harness the CURRENT pod incarnation was built for
// (written at buildPod time). spec.harness is desired; this is actual.
// Router, session sidebar, and UI read THIS — routing flips when the
// new pod exists, not when the request lands (D8).
Harness HarnessType `json:"harness,omitempty"`
```

  Mirrored in `helm/crds/workspace.yaml`. Empty = pre-switch legacy object (treated as opencode
  by every reader; backfilled by the controller, §2).
- `GET /:id/status` (`router.go:1749-1761`) DTO gains `harness` (actual) and `pendingHarness`
  (spec.harness when it differs from status.harness, else absent) + computed `switchable: bool`
  (phase ∈ {Active, Failed, Suspended}) — computed at read, never stored (open-question default).
  The workspace list/detail DTO gains `harness` so the sidebar badge stops hardcoding
  (`frontend/src/components/layout/Sidebar.tsx:412-415`).

## 2. Controller [api]

### 2a. The drift arm + backfill — `controller/internal/workspace/phase_active.go`

New arm in `handleActive`, after the architecture-drift block (`:202-231`; identical shape, D4):

```go
// Harness drift (D4): spec.harness vs status.harness. Backfill FIRST —
// but only for MARKERLESS objects: status.harness=="" + live pod + no
// AnnotationHarnessSwitch ⇒ a pre-switch-era object of ANY harness
// (webhook 7a immutability means every markerless existing pod was built
// for exactly its spec value — including kubectl-created pi workspaces,
// per F1) — write status=desired, no recreation. The marker breaks the
// premise: it proves the spec was written by the switch surface AFTER
// the pod existed (a pre-backfill FIRST switch on a legacy workspace —
// r3 review: the phase-only guard in §1a step 1 admits it, correctly),
// and such an object is DRIFT, not backfill — stamping status=desired
// onto the old-harness pod would wedge it permanently (spec==status
// blocks the drift arm forever; status-first routing mis-serves the
// live old pod with no TTL bound; §5c hides its live sessions; the §5a
// sweep excludes the workspace — annotation present — so nothing ever
// heals). The fleet must not mass-recreate on upgrade;
// unconditional-when-empty-AND-markerless is the only shape that spares
// the legacy pi fleet from an unswept, ungated recreate (the F3/#2040
// chain) while converging the pre-backfill switch to the recreation it
// asked for.
desired := workspace.Spec.Harness; if desired == "" { desired = v1.HarnessOpencode }
if workspace.Status.Harness == "" && pod != nil && pod.Status.Phase == corev1.PodRunning &&
    workspace.Annotations[v1.AnnotationHarnessSwitch] == "" {
    workspace.Status.Harness = desired                 // plain status write, no recreation
} else if workspace.Status.Harness != desired {
    if r.drainBeforePodDeletion(ctx, workspace, drainReasonHarnessSwitch) == drainDefer {
        return ctrl.Result{RequeueAfter: drainPollInterval}, nil   // #761 polite drain
    }
    r.deletePodByName(ctx, name, workspace.Namespace)
    r.clearHarnessSwitchMarker(ctx, workspace)        // force-recycle shape (force_recycle.go:79)
    workspace.Status.Phase = v1.WorkspacePhaseCreating
    // status.Harness is written in handleCreating at buildPod time (below)
}
```

Pin: `TestHarnessDrift_BackfillPiNoRecreate` (controller/internal/workspace/phase_active_test.go) —
spec=pi / status="" / live pod → status=pi, no drain, no recreation; same for spec=opencode.
Pin: `TestHarnessDrift_BackfillSkipsMarkerPresent` — status="" / live pod / marker present →
backfill SKIPPED, drift arm runs (drain → recreate → status at build): a pre-backfill switch on
a running legacy pod never stamps `status=desired` onto the old pod. Known benign cost: an
ABORTED pre-backfill first switch (spec PUT back to the original, marker re-gated to it) leaves
status="" + marker on a pod that already matches spec — the arm reads it as drift and pays ONE
convergent recreation (rebuilt for the same harness); the controller cannot know a status=""
pod's true harness, and this reading is the only one that never wedges a real switch.
A status="" object with NO live pod has nothing to backfill from — it takes the ordinary
Creating path, which builds the pod and writes status at build (§2a second bullet).

- `drainReasonHarnessSwitch = "harness_switch"` joins the reasons in `session_drain.go:72-74`.
- `handleCreating` (`phase_creating.go:29`): at `buildPod` (`:127`) — which already resolves
  `spec.harness` into overlay + supervisor subcommand + `AGENTD_HARNESS` via
  `harness_delivery.go` (`:66,75,90,111`) — write `status.Harness = desired` alongside the
  Creating bookkeeping, so routing flips only when the new pod is being built (D8). A
  mid-rebuild spec change is convergent: `buildPod` reads current spec; the webhook re-gates the
  marker.
- **Suspended targets**: no `handleActive` pass runs while suspended; on activation
  `Resuming → Creating → buildPod` reads the new spec — the switch lands with the resume, no
  extra code (verify by pin, §8).

### 2b. Marker + webhook gate [api]

- `pkg/apis/llmsafespaces/v1/workspace_types.go` (next to `AnnotationForceRecycle` `:39`):

```go
// AnnotationHarnessSwitch authorizes ONE spec.harness delta: the API's
// switch surface writes it with the same value as the new spec.harness;
// webhook 7a′ admits a harness change only when present and equal. Consent
// bookkeeping, not an auth boundary (the force-recycle trust posture).
AnnotationHarnessSwitch = "llmsafespaces.dev/harness-switch"

// AnnotationHarnessSwitchedAt is the PERSISTENT companion, set (RFC 3339
// timestamp of the first accepted switch) in the same update as the
// transient marker and NEVER cleared — the controller clears the marker
// after first observation but leaves this one. It is the durable
// "this workspace has used the switch surface" signal: the §5a boot sweep
// stamps rows to spec.harness ONLY for workspaces without it (a
// never-switched workspace's spec IS its actual harness, webhook 7a; a
// switched one may own legitimately-foreign rows).
AnnotationHarnessSwitchedAt = "llmsafespaces.dev/harness-switched-at"
```

- Webhook section 7a (`workspace_webhook.go:646-672`) becomes 7a′ (D3): deny when
  `oldHarness != newHarness` **unless** `annotations[AnnotationHarnessSwitch] == newHarness`;
  the denial message re-points at the API (`PUT /api/v1/workspaces/{id}/harness`) instead of
  "recreate the Workspace". Stale markers are harmless (value must equal the new spec) and the
  controller clears the marker after first observation (§2a).
- **Backfill/annotation interplay (r3)**: the marker is not only the webhook's consent token —
  on a `status.harness==""` object it is the signal that separates backfill from drift (§2a).
  The two clear/observe duties stay in one place: the drift arm recreates, then clears the
  marker after first observation; the backfill arm never touches marker-present objects. The
  persistent `AnnotationHarnessSwitchedAt` (§1a step 4) is orthogonal — set-once at the API,
  never cleared, read only by the §5a sweep.

## 3. Routing [api]

`api/internal/handlers/resolver_host.go` `GetWorkspaceHarness` (`:102-120`) — resolution order
flips to **status-first** (D8):

```go
if ws.Status.Harness != "" {
    return agent.AgentType(ws.Status.Harness), nil   // what the pod actually runs
}
if ws.Spec.Harness == "" {
    return agent.AgentTypeOpenCode, nil              // pre-S5 object: CRD default at read seam
}
return agent.AgentType(ws.Spec.Harness), nil
```

The lookup-failure fail-open in `harnessrouter.go` (`:146,165-169`) tightens to the last-known
observed harness (cache's prior entry) rather than the static default where a prior entry exists.
The header comment (`:26-29`) is rewritten: the harness is switch-gated (7a′), the cache's
staleness window is bounded by the 60s TTL (`:70`), and mis-routing inside it fails LOUD — by
two mechanisms, both honest about the port topology (both harness incarnations hold the SAME
harness port 4096: opencode serve on opencode pods, the pi bridge on pi pods —
`cmd/workspace-agentd/pi_bridge.go:8-9`; both adapters dial `agentd.AgentPort` —
`pkg/agent/opencode/adapter.go:131`, `pkg/agent/pi/podtransport.go:78`; and the pod IP resolves
fresh per call from the CRD with no incarnation pinning, `resolver_host.go:126-138`):

- **Post-flip (new pod Active, cache still old ≤60s):** the stale route dials the NEW pod's LIVE
  wrong-harness bridge — the port is open, not closed. It fails loud anyway because the bridge is
  a dumb pipe with no cross-harness route shims ("No compatibility shims onto opencode's route
  shapes — a pi pod is honestly a pi pod", `pi_bridge.go:14-15`): foreign route shapes hit a
  `/v1/pi/*`-only mux and answer 404-class — never wrong-store data.
- **Pre-PodIP (old pod deleted, new pod not yet Running):** `status.PodIP` is cleared at
  deletion (`phase_active.go:104,136,179,223`), so the fresh resolution yields no IP and the
  call fails at resolution — the new harness cannot be reached before it exists.

**The pre-backfill first-switch window (status.harness == "" — the flagship legacy case).**
The two mechanisms above and the TTL bound describe the STATUS-SET switch. For a status=""
object the resolver's spec fallback returns the NEW harness from the moment the PUT lands:
for the first ≤60s the router's stale cache actually routes CORRECTLY (old harness = the live
old pod); after TTL expiry the API serves the NEW harness adapter against the OLD live pod
until `buildPod` writes `status.harness` — a window bounded by controller reconcile plus
drain, which a busy-session defer can stretch toward the 60m force (`session_drain.go:91`),
NOT by the TTL. New calls in that window fail loud 404-class against the old pod's shim-free
surface (never wrong-data); in-flight turns still finish — their streams predate the flip and
the drain is watching them, so the drain's politeness covers in-flight turns, not new calls,
for this class. This window also exists on the abort path's §2a benign recreation (same
status="" shape). Pin: `TestHarnessRouter_PreBackfillFallbackWindow`
(pkg/agent/harnessrouter_test.go — a status="" object resolves spec after TTL, and the
mis-route fails 404-class, never wrong-store).

Multi-replica drift also bounded by TTL — except the pre-backfill fallback window above,
which every replica enters at its own TTL expiry and which ends only when the controller
writes status.

## 4. Board: sweep + quiesce [api]

New `pkg/board/switch.go` (sibling seams, the suspend.go pattern). No new claim lifecycle.

### 4a. The sweep — `SweepForHarnessSwitch(ctx, actor, workspaceID, to Harness)`

Enumerate live claim surface of the workspace (projects are workspace-bound,
`projects.target_workspace_id`):

```sql
SELECT t.id, t.stage, t.short_id, COALESCE(t.assignee_session,''), t.pending_input
FROM tasks t JOIN projects p ON t.project_id = p.id
WHERE p.target_workspace_id = $1 AND t.stage IN ('in_progress','in_review')
```

Per row, in one `eventWriter` transaction each (action `harness_switch_requeue` /
`harness_switch_declaim`, payload `{to, from, short_id, was_pending_input}` — the depose audit
discipline):

- **in_progress (incl. pending_input-flagged)** — the CLEAN requeue (D5): the depose requeue
  statement (`depose.go:169-186`) **minus the failure accounting** — same claim-field clears
  (`claim_token, lease_expires_at, assignee_session, implementer_session, claim_persona,
  nudge_id, nudged_at, last_heartbeat_at`, `claim_incremented_attempts=false`), stage →
  `backlog`, `pending_input` cleared (the machine's backlog exit clears it; the awaiting-input
  state rides the pointer), **no** `last_attempt_failed`/`failure_count+1`/`cooldown_until`,
  attempts untouched (the increment belongs to the next claim), plus the salvage pointer:

```sql
UPDATE tasks SET stage='backlog', stage_entered_at=now(),
    claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL, implementer_session=NULL,
    claim_persona=NULL, nudge_id=NULL, nudged_at=NULL, last_heartbeat_at=NULL,
    claim_incremented_attempts=false, pending_input=false,
    salvage=$2
WHERE id=$1 AND stage='in_progress'
```

  `salvage` = `{state: "harness switch opencode→pi: requeued mid-attempt (infra move, no attempt burn); inherit, don't restart", note: "was pending_input: <bool>"}` (caps per `depose.go:32-36`;
  the next claim's charter embeds it automatically — inherit, don't restart). Post-commit, the
  DEPOSED-class stop directive (`depose.go:289-298` delivery shape, switch-flavored text: "the
  workspace is switching harness; your claim was requeued with a salvage pointer — stop working")
  goes address-direct to each former assignee, best-effort.
- **in_review** — declaim in place (D5): claim fields cleared, stage stays `in_review`, **no**
  #1813 accounting, no cooldown, no failure_count:

```sql
UPDATE tasks SET claim_token=NULL, lease_expires_at=NULL, assignee_session=NULL,
    implementer_session=NULL, claim_persona=NULL, last_heartbeat_at=NULL
WHERE id=$1 AND stage='in_review'
```

  The existing re-arm pass (`reconciler.go:735-809` (b)) re-mints a reviewer claim + spawn once
  the workspace is switchable again — but only after the quiesce gate (§4b) opens, so no
  reviewer spawns into the window.

Ordering guarantee (the two-pass sweep, §1a): pass 1 empties the live-claim surface before
anything moves; the spec write arms the quiesce gate (§4b); pass 2 — under the gate — catches the
single possible racer (a claim granted between pass 1 and the spec write); on the gate's
observed view it cannot re-claim after being swept (the informer-lag residual — one lagging-
replica grant, §1a step 5 — is the documented exception, rearm as its exit). Swept workers
learn via the stop directive post-commit per row; a worker
mid-turn finishes its thought against a still-Active pod — its next token-carrying board write
409s `claim_token` (the depose discipline).

### 4b. The quiesce gate — switch window = claim-quiet [api] (D6)

- New seam `pkg/board/switch.go`:

```go
// SwitchSource reports workspaces mid harness-switch (spec.harness !=
// status.harness). Nil = inert = today's semantics (the SuspendSource
// discipline, suspend.go:68-70). POSITIVE evidence only.
type SwitchSource interface{ SwitchingWorkspaces(ctx context.Context) map[string]bool }
```

- Production adapter: `api/internal/app/board_suspend_source.go` grows the method over the same
  watcher-fed cache — the cache holds phases today (`GetAllKnownPhases`); it gains a
  harness-delta view (the watcher already sees full Workspace objects; expose
  `SwitchingSet()` alongside phases). Honest default: unseen ⇒ not switching. Because grants key
  on this same observed view, **sweep pass 2 waits until it observes the workspace armed before
  declaring quiet** (§1a step 5 — grants before the observed arm are swept, grants after are
  refused); the multi-replica propagation residual is documented there (the suspension freeze's
  accepted posture — one bounded-grant worst case, rearm as the exit).
- Gates (each mirrors its suspension twin verbatim): `claimOnce` quiet no-grant
  (`engine.go:359-367`); `task_start` loud 409 `GuardBackpressure`
  (`engine.go:175-181`); dispatch-drive filter (`reconciler.go:1081`); persona lapse/re-arm skip
  (`reconciler.go:735-809` — renewal arms skip too: a switching workspace neither reaps nor
  mints). Deliberately NOT a freeze (D5): frozen leases have no exit — the old-harness sessions
  can never resume.

## 5. Session space [api]

### 5a. Migration (next free number; `000055` if #2057 hasn't landed)

```sql
-- session_index rows carry the owning harness: HIDDEN-PRESERVED visibility
-- (D7). The SQL DEFAULT 'opencode' is the ORPHAN/rolling-window fallback only
-- — the true backfill is the repeatable boot sweep below: F1+F5 mean kubectl
-- pi workspaces exist with live pi sessions registered under the hardcoded
-- "opencode" (proxy_session_index.go:538,696,882) — the SQL default alone
-- would mis-stamp that operator-known fleet, and §2a's status=pi backfill
-- would then HIDE their live sessions (D7) until the sweep heals them.
ALTER TABLE session_index ADD COLUMN IF NOT EXISTS harness text NOT NULL DEFAULT 'opencode';
```

(Mirrored under `helm/migrations/` per house practice.)

**Boot sweep to spec (code-side — SQL cannot see the CRD; repeatable, not one-time).** At every
api boot after this story deploys (api/internal/app startup, beside the board seeding blocks),
stamp session_index rows to their owning workspace's `spec.harness` — but ONLY for workspaces
WITHOUT `AnnotationHarnessSwitchedAt` (§2b):

```sql
UPDATE session_index s SET harness = w.spec_harness
FROM <workspace-view> w
WHERE s.workspace_id = w.id AND s.harness <> w.spec_harness
  AND w.spec_harness <> '' AND NOT w.switched_at_present
```

(over the k8s-cache-resolved workspace list; empty spec normalizes to `opencode`, today's
behavior). **The safety argument is the predicate, not run-once-ness**: a workspace that has
never switched has `spec.harness == actual harness` for every moment of its life (webhook 7a
immutability), so spec-stamping its rows is a truth operation, safe to repeat at every boot
forever; a workspace that HAS switched owns legitimately-foreign HIDDEN-PRESERVED rows, and the
persistent annotation excludes it from the sweep entirely — no completion marker is needed and
none is used (a one-time marker would freeze the stamp at first boot, which is exactly the
rolling-update hole below).

**Rolling-update window, closed by repetition.** The sweep runs at FIRST new-replica boot while
old API replicas (hardcoded `"opencode"` registrations, `proxy_session_index.go:538,696,882`;
§5b's replacement not yet live on them) are still serving — a session they register after that
boot lands `harness='opencode'` from the SQL DEFAULT. On a kubectl-pi workspace (the F1 fleet)
such a row is a live pi session mis-stamped and hidden (D7). Because the sweep is idempotent
and repeats at EVERY boot, the first boot after rollout completion (every remaining boot is
new-code) heals every window row. Residual, stated honestly: until that heal, the window row is
HIDDEN — a visibility gap bounded by the rollout, self-healing at the next boot, never data
loss. Narrower accepted residual (the gate's informer-lag posture, §1a step 5): a switch
executed MID-rollout sets the persistent annotation, so a subsequent old-replica registration
on that same workspace mis-stamps a row the sweep will never touch — that class requires a
user switch inside the rollout window AND a stale-replica write on that workspace afterward;
it is bounded by rollout completion and accepted (the row stays on disk, HIDDEN-PRESERVED,
direct reads answer the gone-class).

### 5b. Writes

`EnsureSessionRegistered` (`api/internal/services/database/short_session_ids.go:88`) persists the
`harnessType` it already receives; the three hardcoded `"opencode"` registrations in
`proxy_session_index.go` (`:538,696,882`) pass the workspace's CURRENT `status.harness`
(resolved once per request through the existing resolver).

### 5c. Reads

- **List** (`router.go:1437-1511` → `ListWorkspaceSessions`): consider only rows
  `harness = <status.harness>`; foreign-harness rows are invisible (D7). The archived/`q` filters
  apply within that set. `BackfillSessionParents` scoped the same way.
- **Busy merge + counts**: `ServeHarnessMessageCounts` and the busy overlay naturally key to the
  current harness adapter; foreign rows are excluded by the list predicate (no separate rule).
- **Reconcile shield** (`proxy_session_index.go:285-299`): foreign-harness rows never enter the
  diff — no miss counting, no reap candidacy, no `kept_for_operator` storm. With the boot sweep
  (§5a) every pre-switch-era row carries its true harness (window rows after the next boot's
  heal), so F5's false-positive class is unreachable for never-switched workspaces — the claim
  is scoped to truthful rows (a mis-stamped row would falsify it, which is why the sweep and its
  rollout-window pins are in §8). Guard pin:
  switching must not decrement any row's miss-counter universe.

### 5d. Direct reads of foreign-harness sessions

Transcript fetch by id for a `harness != status.harness` row answers the existing typed
gone-class (the #1340 read path) — never a wrong-harness error, never disk access.

## 6. Frontend [ui]

- **Wire** (`frontend/src/api/types.ts`, `workspaces.ts:65-135`): workspace + status DTOs gain
  `harness?`, `pendingHarness?`, `switchable?`; `switchHarness(wsId, harness)` beside
  `refreshCompute`.
- **Control** — `WorkspaceSettingsDrawer.tsx` gains a Harness section (radio/segmented
  opencode|pi; the drawer is the settings surface — Custom Instructions / Secrets / Dev Preview
  live here, `:118-210`). Disabled with reason while `!switchable` or `pendingHarness` set;
  label shows `Switching to pi…` + inline `Spinner` (`Spinner.tsx:10-23`) while pending.
- **Confirm** — `ConfirmDialog` (`ConfirmDialog.tsx:4-35`, `destructive`) with the
  refresh-compute copy shape (`Sidebar.tsx:537-548`) plus the two switch-specific lines:
  session-space ("current harness sessions step aside — they return if you switch back") and
  board ("in-flight board stories are re-queued; workers inherit a salvage pointer").
- **Status during recreation**: nothing new — phase `Creating` already drives `isTransitioning`
  (`ChatPage.tsx:1116`) → composer disabled + transition UI; the workspace.phase SSE event
  carries the walk.
- **Errors**: API errors render through the drawer's existing error lane; transcript-level
  tool errors unaffected.
- **Badge truth** — `Sidebar.tsx:412-415`: `opencode v${agentVersion}` hardcode becomes
  `${harness === "pi" ? "pi" : "opencode"}${agentVersion ? ` v${agentVersion}` : ""}` shaped off
  `status.harness`.

## 7. Abort / rollback (D10)

No dedicated cancel verb. Pre-drain, on a STATUS-SET switch (`status.harness` written — the
post-backfill fleet, or any workspace that has already completed one switch): `PUT` the old
value back (marker re-gated; desired==status ⇒ the drift arm never fires, no recreation).
Pre-drain on a PRE-BACKFILL first switch (`status.harness == ""`): the abort leaves
status="" + marker on a pod that already matches spec — §2a's known benign cost applies (the
arm reads it as drift; exactly one convergent same-harness recreation). In both cases the
sweep's cost — re-claims with salvage pointers — is the only residue, and it is honest work,
not damage. Post-deletion: committed; switch back is the
rollback (symmetric; D7's predicate restores old sessions). Failed new-harness builds land in
`Failed` with the verify condition (`PiVerified` 85/86 class, `phase_active.go:271`) and
switch-back remains allowed (phase guard includes Failed).

## 8. Test map (born-red per README-LLM rule 0)

| Pin | Red because |
|---|---|
| Webhook 7a′: markerless delta denied / matching marker admitted / mismatched marker denied | today all deltas denied |
| `SwitchHarness`: no-op 204; guard 409s (Suspending/Creating/Terminating); two-pass sweep ordering (pass-1 failure ⇒ 500 with nothing moved; the racer-between-pass-1-and-spec-write lands swept by pass 2 and cannot re-claim — pin with a claim injected mid-call; `TestSweep_Pass2WaitsForGateArmed`, pkg/board/switch_test.go — pass 2 does not declare quiet until the gate reports armed); 202 shape | endpoint doesn't exist |
| Controller: drift arm recreates preserving PVC name; backfill no-recreate for BOTH harnesses (`TestHarnessDrift_BackfillPiNoRecreate` — spec=pi/status=""/live pod → status=pi, no drain, no recreation); `TestHarnessDrift_BackfillSkipsMarkerPresent` — status=""/live pod/marker present → backfill SKIPPED, drift runs (a pre-backfill switch on a running legacy pod never stamps status=desired onto the old pod); drain defer/force via `harness_switch` reason; marker cleared; Suspended-switch lands at next activation | arm doesn't exist |
| Router: status-first resolution; stale-TTL post-flip mis-route reaches the LIVE wrong-harness bridge and fails loud 404-class — `TestHarnessRouter_StaleTTLPostFlip404` (pkg/agent/harnessrouter_test.go: opencode route shapes against the pi bridge's `/v1/pi/*`-only mux), pre-PodIP sub-window fails at resolution (`status.PodIP` cleared); the pre-backfill fallback window (status="" ⇒ spec served against the old pod after TTL, reconcile+drain-bounded — §3) — `TestHarnessRouter_PreBackfillFallbackWindow`; never wrong-store data | resolver is spec-first |
| Board sweep: in_progress clean requeue (NO failure_count/`last_attempt_failed`/cooldown, salvage embedded, next charter inherits); pending_input cleared + noted; in_review declaim no-accounting; stop directive post-commit | sweep doesn't exist |
| Quiesce: claimOnce quiet no-grant; task_start 409; dispatch skip; persona re-arm skip — all while spec≠status, none after flip | gates key on phase today |
| Session index: migration + the repeatable boot sweep (pins: legacy kubectl-pi rows stamped to spec-harness — live pi sessions stay visible; `TestSessionIndex_BootStampRolloutWindow` — a row the SQL DEFAULT stamped during the rollout window on a spec=pi never-switched workspace IS stamped spec-harness by the next boot's sweep; a workspace carrying `AnnotationHarnessSwitchedAt` is NEVER re-stamped — post-switch HIDDEN-PRESERVED rows and switch-back foreign rows intact; switch-back on a legacy-pi workspace neither reaps nor mis-lists foreign transcripts); foreign rows invisible in list; reconcile never miss-counts foreign rows (switch mid-test, 3 reconcile passes, rows intact); direct read gone-class | column doesn't exist |
| UI: control disabled states; confirm copy carries both warnings; badge truthful for pi | control doesn't exist |

## 9. Story split + build order

1. **[api]** `harness-switch-api` — everything in §1–§5, §7, §8 rows 1–7. Branch off main
   **after** the S6/S7 pi stack's main PR lands (design 0073 is cited context; the code seams
   are already on main). `dependsOn`: prelim (existing).
2. **[ui]** `harness-switch-ui` — §6 + §8 row 8; adds the `dependsOn` edge on
   `harness-switch-api` (the wire fields must exist). The prelim patches both charters (§below)
   and adds that edge.
