# Harness switching — options writeup and recommendation (prelim deliverable, 2026-11-15)

**Status:** Prelim-story deliverable for board epic "User-facing harness switching (opencode <-> pi)"
(`b6eff9d9`, project `prj-orient-mint-stencils`). Enumerates the option space, recommends one with
code evidence, and settles the session-space semantics. The recommended option is elaborated in the
companion pair `harness-switch-high-level-design.md` / `harness-switch-low-level-design.md`; the two
implementation stories (`harness-switch-api` = `tsk-visioned-hurdle-leaves`, `harness-switch-ui` =
`tsk-vibrant-geyser-masks`) are patched from those designs — the patched charters are binding.
**Seam verification:** every anchor below was verified against `origin/main` `71cb54f80` (2026-11-15);
design 0073's rulings were additionally read at `eca3368ca` (`origin/pi-drift-s6`, PR #2039 — the
S6/S7 stack awaiting its main PR; the switch stories branch off main AFTER that lands).

---

## 1. The question

A workspace runs exactly one coding-agent harness — `opencode` today, `pi` as the second harness
(S1–S7 landed/landing). `spec.harness` (enum `opencode|pi`, default `opencode`,
`pkg/apis/llmsafespaces/v1/workspace_types.go:200-208`) selects it at pod-build time:
the controller wires the per-harness overlay, the agentd supervisor subcommand
(`controller/internal/workspace/harness_delivery.go:66,75,90`) and the `AGENTD_HARNESS` env
(`agentd_sidecar.go:215-216`), and the API server routes per-workspace agent traffic through
`HarnessRouter` (`pkg/agent/harnessrouter.go:78`, per-call lookup at `:131`, 60s TTL cache at `:70`).

The harness is **immutable after create** — enforced by the validating webhook
(`controller/internal/webhooks/workspace_webhook.go:646-672`, section 7a) with the recorded reason:
*"sessions, agent config, and artifacts are harness-specific … an in-place switch would strand the
old harness's state on the shared PVC. Switching harness = create a new Workspace."*
The epic exists because that answer is clunky for owners who want pi on an established workspace.
This writeup asks which relaxation is sound.

## 2. What the code actually says (the facts every option must survive)

- **F1 — the REST API never sets `spec.harness` at all.** `types.CreateWorkspaceRequest`
  (`pkg/types/workspace.go:35-47`) has no harness field and `buildWorkspaceCRD`
  (`api/internal/services/workspace/workspace_service.go:1296-1348`) never writes one, so every
  REST-created workspace is opencode by CRD default. Harness selection today is kubectl-only. Any
  "user-facing" answer — including (a) — has a create-side gap to close.
- **F2 — the PVC survives every existing recreation path.** The PVC is created in Pending
  (`phase_pending.go:41-81`) and deleted only at Terminating (`phase_terminating.go:49-54`);
  suspend, resume, restart-generation bump, architecture drift, and refresh-compute all recreate
  the bare Pod (no Deployment) around it. "Recreation-class change with preserved storage" is
  therefore an existing, drilled machinery, not a new invention (`phase_active.go:79-114,202-231`).
- **F3 — board claims live in Postgres, not on the pod.** Claims/leases are `tasks` rows
  (`pkg/board/engine.go:424-441`); worker sessions die with the pod but the rows survive. The
  suspension-aware freeze (`pkg/board/suspend.go`, `reconciler.go:315-360`) renews claims of
  Suspending/Suspended workspaces in place — no attempt burn — but it keys off the *phase*
  (`api/internal/app/board_suspend_source.go:40-67`), and a harness switch walks
  `Active → Creating → Active`, phases the freeze never matches. Unmitigated, a switch would kill
  workers mid-turn, their leases would lapse → nudge into a dead session → wedge verdict →
  `failed` with attempts burned: exactly the #2040 incident class the freeze was built to stop.
- **F4 — depose/reclaim requeues are failure-shaped.** The depose lever's in_progress requeue
  stamps `last_attempt_failed=true, failure_count+1`, and a backoff cooldown
  (`pkg/board/depose.go:169-186`) — right for a lapsed claim, wrong for an infra-caused move. A
  switch needs the depose *handoff* (salvage pointer, inherit-don't-restart) without the failure
  accounting.
- **F5 — the session index is reconcile-reaped against the current harness's list.** The sidebar
  list is the Postgres `session_index` overlaid with harness truth
  (`api/internal/server/router.go:1437-1511`), and `ReconcileSessionIndex`
  (`api/internal/handlers/proxy_session_index.go:285-299`) deletes count-0 rows the agent reports
  absent for 2 consecutive checks, keeping history-bearing rows with a WARN — a guard built for
  "a vanished session WITH history may indicate a harness store reset" (`:447-452`). A naive
  switch mass-triggers precisely that class: every old-harness row looks like a store reset.
  Related: the registration seam takes a `harnessType` but discards it
  (`api/internal/services/database/short_session_ids.go:88`), and callers hardcode `"opencode"`
  (`proxy_session_index.go:538,696,882`).
- **F6 — routing resolves per-call from the CRD with a fail-open default.** `HarnessRouter`
  resolves `spec.harness` per request (`harnessrouter.go:131-152`), caches 60s, and fails open to
  opencode on lookup error (`:146,165-169`). Both harnesses serve the same harness port (4096):
  opencode's HTTP server or pi's bridge. So a spec flip ahead of pod recreation routes pi traffic
  at an opencode server (loud 404-class errors, not silent corruption), and post-recreation the
  old harness's port is simply gone (connection refused). The window is loud, bounded, and
  self-healing — but `status` carries no harness field (`WorkspaceStatus`,
  `workspace_types.go:671-794`), so nothing pins routing to what the *pod* actually runs, and the
  frontend hardcodes the opencode badge (`frontend/src/components/layout/Sidebar.tsx:412-415`).
- **F7 — the drain machinery is reusable as-is.** `drainBeforePodDeletion`
  (`controller/internal/workspace/session_drain.go:169`) politely defers pod deletion while
  statusz reports busy-with-progress (10s requeue), forcing after 60m no-progress; the
  architecture-drift arm (`phase_active.go:202-231`) is the exact shape a harness-drift arm needs.

## 3. The options

### (a) CREATION-TIME ONLY — status quo formalized

Harness chosen at create; switching means a new workspace. The webhook message already says this.

Assessed honestly: it is the cheapest answer and its core insight — a harness change is
recreation-class, not a field flip — is *correct* and absorbed into (b). But as the epic's answer
it fails on three counts. First, F1: "chosen at creation" is not even true today — the REST create
path cannot choose; formalizing (a) still requires new API surface (create field + validation +
docs), so the "do nothing" appeal is illusory. Second, the cost it imposes on owners is the
epic's stated problem: a new workspace abandons the session index (per-workspace rows), board
projects (bound to the workspace), uploads, drafts, dev-preview setup, and the PVC itself
(workspace-bound name `workspace-<name>`, deleted at Terminating — there is no carry-over path).
Third, it converts a reversible product question ("try pi on this workspace") into an irreversible
logistics exercise (two workspaces, manual copying, a dead one to clean up).

**Verdict: rejected as the epic's answer.** Its honest core survives as (b)'s framing: a switch IS
a supervised recreation, never an in-place flip. The create-side field (F1) is folded into the
recommended design's API story as a secondary criterion.

### (b) SWITCH-WITH-RECREATE — recommended

A user-facing switch surface that writes `spec.harness` through a gated path; the controller treats
the change as a topology drift and performs the same supervised pod recreation the
architecture-drift and restart paths already perform (F2, F7), preserving the PVC. Board claims are
swept to a clean requeue before the pod dies (no attempt burn, salvage pointers attached — the #2040
lesson, F3/F4); the session index becomes harness-aware so old-harness sessions are hidden,
shielded from the reaper, and restored on switch-back (F5); routing pins to a new
`status.harness` so the API serves only what the pod actually runs (F6). Full designs in the
companion pair.

**Verdict: recommended.** It is the only option that answers the epic's question ("pi on *this*
workspace") while reusing drilled machinery for every hard part: recreation-with-PVC (restart /
arch drift), drain (#761), claim hygiene (the suspend/freeze/depose family), and the
reconcile-aware session index. The one genuinely new mechanism — a clean (non-failure) requeue and
a switch-window claim gate — is a small sibling of existing board seams, not a new claim lifecycle.

### (c) DUAL-RUNTIME POD — rejected

Both harnesses in one workspace, per-session choice. Rejected on five anchored grounds:

1. **It contradicts the shipped topology.** The pod runs ONE agentd supervisor selected at build
   time — `supervise-opencode` vs `supervise-pi` is the container command
   (`agentd_sidecar.go:423-430`, `harness_delivery.go:90`); `AGENTD_HARNESS` branches agentd's
   whole in-pod driver (`cmd/workspace-agentd/harness_driver.go`, `sidecar_pi.go`). Dual-runtime
   means two supervisors (or one dual-mode one — a rewrite), two overlays, and a new per-session
   dispatch dimension the S5 design deliberately placed at the *workspace* boundary ("the harness
   is a structural dimension of the workspace", `workspace_types.go:96-99`).
2. **It fights design 0073 RULING A** (process-per-session is THE pi policy,
   `design/0073_2026-11-14_pi-validation-rulings.md` §2.1): pi's cost model is ~79 MiB per
   attached session on top of the pod. Carrying opencode's multi-session server *and* the pi
   child fleet doubles the resident surface and the wedge/liveness observation plane for zero
   product win — the epic asks for switching, not simultaneity.
3. **The harness port is singular.** Both harnesses serve agentd's harness port (4096); per-session
   choice would need port partitioning plus router-side per-session routing state — new topology,
   contradicting the S5/S7 split the immutability ruling documents.
4. **Config stays two-headed anyway.** The two `AgentConfigWriter`s target disjoint trees
   (`/workspace/.local/share/opencode...` vs `/workspace/.local/pi` + `/workspace/pi-sessions`);
   dual-runtime must keep both rendered and both verified, forever, on every workspace — cost paid
   by everyone for a feature the epic does not request.
5. **It dissolves none of the hard problems.** Session-space semantics, board claims at the
   boundary, and UI honesty all still need answers — with *more* reachable states (two harnesses
   live at once) to keep honest.

**Verdict: rejected.** Recorded so the next scoping pass does not re-litigate it blind.

### (d) WHAT THE CODE SUGGESTS — five findings, all folded into (b)

- **(d1) The create-side gap (F1):** "user-facing harness selection" is incomplete without
  `harness` on `CreateWorkspaceRequest` → `buildWorkspaceCRD`. Added to the API story as a
  secondary criterion (same validation surface as the switch).
- **(d2) The routing window (F6):** spec-resolved routing is wrong-by-design for a mutable
  harness; the designs pin resolution to `status.harness` (the pod's actual harness) with spec
  fallback, and shrink the stale window to the existing 60s TTL — during which mis-routing fails
  loud (port absent), never silent-wrong-data. The fail-open-to-opencode default is tightened for
  the lookup-failure case to the *observed* harness, not the static default.
- **(d3) No observable harness (F6):** `status.harness` is new — the controller writes it at pod
  build; it drives the router, the wire DTOs, and the UI badge (replacing the hardcoded opencode
  string).
- **(d4) The reaper hazard (F5):** the session index needs a `harness` column (the seam already
  takes the parameter and drops it), a backfill, list filtering, and reconcile shielding for
  foreign-harness rows — without which a switch auto-reaps count-0 old-harness sessions after 2
  list misses and metric-storms the kept-history guard.
- **(d5) Claims are DB-side (F3):** the switch can sweep claims *before* the pod dies — the
  only option with that luxury; the sweep (clean requeue + salvage pointer + stop directive +
  in-review declaim) is specified in the low-level design.

## 4. Session-space semantics — the first-class question

Old-harness sessions remain on disk after a switch (both stores are PVC subtrees — opencode under
`/workspace/.local` via `XDG_DATA_HOME` (`cmd/workspace-agentd/managed_process.go:593`), pi at
`/workspace/pi-sessions` (`pi_supervisor.go:70-77`, design 0073 RULING C)) and are not servable
while switched away (the pod runs one harness; F6). Three visibility semantics were considered:

1. **SHOWN-INERT** — list them greyed out. Rejected: every row is a dead-click target (open →
   wrong-harness error), the busy/message-count overlays have no truthful data for them, and the
   reconcile reaper still needs a foreign-row policy — showing them solves nothing the harness
   column doesn't, while making the sidebar lie about what's addressable.
2. **READED (deleted from the index)** — Rejected: destructive, unnecessary (the data is intact on
   the PVC), and it destroys switch-back by severing the short-id → session mapping
   (`session_index.short_id` is write-once, migrations/000044).
3. **HIDDEN-PRESERVED** — RECOMMENDED. Index rows carry the owning harness; the sidebar list and
   busy overlays consider only rows whose harness equals `status.harness`; foreign-harness rows are
   invisible, never miss-counted by the reconciler (no reap, no kept_for_operator storm), and
   reappear automatically on switch-back (same predicate, zero extra state). Direct reads of a
   foreign-harness session answer the existing typed gone-class. Disk state is never touched by the
   switch.

**Recommendation: HIDDEN-PRESERVED with switch-back restore** — the predicate is one clause, it
makes every overlay truthful by construction, and reversibility falls out for free.

## 5. Recommendation

**(b) SWITCH-WITH-RECREATE**, with (d1)–(d5) folded in, session-space semantics
HIDDEN-PRESERVED, and (a)'s core honesty kept as the framing: *a switch is a supervised recreation
that happens to preserve the workspace's identity, storage, board, and history — never an in-place
flip.* The two implementation stories are patched to this shape; the high-level and low-level
designs carry the contract.
