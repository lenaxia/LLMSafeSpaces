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
   builds the new harness (controller arm handles it naturally, §2).
2. **Same-value short-circuit** — `204`, no sweep, no write, `switched:false` semantics.
3. **Board sweep, pass 1** (§4) — BEFORE any spec write; a sweep failure aborts the request
   (500), nothing else happened.
4. **Spec write** — under `RetryOnConflict`: `spec.harness = <harness>` AND
   `annotations[AnnotationHarnessSwitch] = <harness>` in ONE update (the marker must land with the
   delta or webhook 7a′ denies the write itself). This arms the quiesce gate (§4b): from here on,
   no new claim can enter the workspace.
5. **Board sweep, pass 2** (§4) — after the spec write, now under the armed gate. Pass 1 closes
   the live-claim surface before anything moves; pass 2 catches the only remaining race — a claim
   granted in the window between pass 1 and the spec write (a claimOnce firing in those
   milliseconds) — and because the gate is armed, a racer swept by pass 2 cannot re-claim. Usually
   empty; idempotent. A persistent pass-2 failure responds 500 with the switch in flight: the
   bounded residue is that single racer's attempt (documented; the rearm lane exists) — the gate
   guarantees there is never a second victim.
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
// Harness drift (D4): spec.harness vs status.harness. Backfill FIRST:
// status.harness=="" + live pod + spec==opencode ⇒ pre-S5 fleet — write
// opencode, no recreation (the fleet must not mass-recreate on upgrade).
desired := workspace.Spec.Harness; if desired == "" { desired = v1.HarnessOpencode }
if workspace.Status.Harness == "" && desired == v1.HarnessOpencode {
    workspace.Status.Harness = v1.HarnessOpencode      // plain status write, no recreation
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
```

- Webhook section 7a (`workspace_webhook.go:646-672`) becomes 7a′ (D3): deny when
  `oldHarness != newHarness` **unless** `annotations[AnnotationHarnessSwitch] == newHarness`;
  the denial message re-points at the API (`PUT /api/v1/workspaces/{id}/harness`) instead of
  "recreate the Workspace". Stale markers are harmless (value must equal the new spec) and the
  controller clears the marker after first observation (§2a).

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
staleness window is bounded by the 60s TTL (`:70`) and mis-routing inside it fails loud — the
harness port (4096) serves exactly one harness per pod incarnation, so a stale route hits a
closed port, never a wrong-harness server. Multi-replica drift also bounded by TTL.

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
single possible racer (a claim granted between pass 1 and the spec write) and it cannot re-claim
after being swept. Swept workers learn via the stop directive post-commit per row; a worker
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
  `SwitchingSet()` alongside phases). Honest default: unseen ⇒ not switching.
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
-- (D7). Existing rows are opencode's (F1: REST-created workspaces were
-- opencode-only; kubectl pi workspaces post-S5 are operator-known).
ALTER TABLE session_index ADD COLUMN IF NOT EXISTS harness text NOT NULL DEFAULT 'opencode';
```

(Mirrored under `helm/migrations/` per house practice.)

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
  diff — no miss counting, no reap candidacy, no `kept_for_operator` storm (F5's false-positive
  class is unreachable by construction). Guard pin: switching must not decrement any row's
  miss-counter universe.

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

No dedicated cancel verb. Pre-drain: `PUT` the old value back (marker re-gated; desired==actual
⇒ no recreation; the drift arm never fires). Post-deletion: committed; switch back is the
rollback (symmetric; D7's predicate restores old sessions). Failed new-harness builds land in
`Failed` with the verify condition (`PiVerified` 85/86 class, `phase_active.go:271`) and
switch-back remains allowed (phase guard includes Failed).

## 8. Test map (born-red per README-LLM rule 0)

| Pin | Red because |
|---|---|
| Webhook 7a′: markerless delta denied / matching marker admitted / mismatched marker denied | today all deltas denied |
| `SwitchHarness`: no-op 204; guard 409s (Suspending/Creating/Terminating); two-pass sweep ordering (pass-1 failure ⇒ 500 with nothing moved; the racer-between-pass-1-and-spec-write lands swept by pass 2 and cannot re-claim — pin with a claim injected mid-call); 202 shape | endpoint doesn't exist |
| Controller: drift arm recreates preserving PVC name; backfill no-recreate; drain defer/force via `harness_switch` reason; marker cleared; Suspended-switch lands at next activation | arm doesn't exist |
| Router: status-first resolution; stale-TTL mis-route fails loud (connection refused), never wrong-store data | resolver is spec-first |
| Board sweep: in_progress clean requeue (NO failure_count/`last_attempt_failed`/cooldown, salvage embedded, next charter inherits); pending_input cleared + noted; in_review declaim no-accounting; stop directive post-commit | sweep doesn't exist |
| Quiesce: claimOnce quiet no-grant; task_start 409; dispatch skip; persona re-arm skip — all while spec≠status, none after flip | gates key on phase today |
| Session index: migration backfill; foreign rows invisible in list; reconcile never miss-counts foreign rows (switch mid-test, 3 reconcile passes, rows intact); direct read gone-class | column doesn't exist |
| UI: control disabled states; confirm copy carries both warnings; badge truthful for pi | control doesn't exist |

## 9. Story split + build order

1. **[api]** `harness-switch-api` — everything in §1–§5, §7, §8 rows 1–7. Branch off main
   **after** the S6/S7 pi stack's main PR lands (design 0073 is cited context; the code seams
   are already on main). `dependsOn`: prelim (existing).
2. **[ui]** `harness-switch-ui` — §6 + §8 row 8; adds the `dependsOn` edge on
   `harness-switch-api` (the wire fields must exist). The prelim patches both charters (§below)
   and adds that edge.
