# Worklog: board leases suspension-aware — the freeze (tsk-heavy-textile-plumbs)

## Context

The 2026-11-13 and 2026-11-14 incidents (owner: "recurring, structural,
not flake"): a rate-limit suspension tears the workspace pod down; while
suspended, every board worker claim inside it lapses; the RC1 reclaim
machinery burned the attempts to exhaustion (3/3); stories escalated to
`failed`; the ready set drained and the board froze. Both occurrences
required a manual rearm sweep to recover. Owner-approved fix direction
(2026-11-14): "a suspended workspace must not burn its workers' claims."

## The seam survey

- The lease-expiry evaluation seam is the board reconciler's RC1 pass
  (`pkg/board/reconciler.go` — `rc1Reclaims` for in_progress worker
  claims, `rc1PersonaClaims` for in_review persona claims; the S8
  seamless-lease ladder lives inside rc1). The sweep runs in the API
  server (app.go wires `board.NewReconciler` with the same spawnOpts as
  the Service).
- Suspension truth is the Workspace CRD's phase (`Suspending`/
  `Suspended`; manual, idle auto-suspend, org/rate-limit suspensions all
  land there via `Spec.Suspend=true`). The DB `workspaces` table carries
  NO phase column — the API's workspace watcher
  (`api/internal/services/workspace/watcher.go`) holds the in-process
  phase cache (`GetAllKnownPhases`, nil-safe through the proxy handler),
  already consumed app-side (`SetActivePhasesChecker`).
- Hidden hazard found during the survey: the board spawner's
  `EnsureActive` (api/internal/workflows/engine.go) PATCHES
  `spec.suspend=false` on a Suspended workspace — a dispatch into a
  suspended workspace would have AUTO-RESUMED it. The freeze's claim
  gate closes this too.

## Shape decision — (a) freeze, by renewal

Shape (a) (freeze) over (b) (auto-pause): the lease clock already has an
evidence-driven renewal arm (the seamless ladder's `ladderRenew`), so a
suspension — POSITIVE evidence the assignee cannot answer — is one more
renewal source, not a new claim-lifecycle mechanism. Auto-pause would
churn pause flags the human never asked for and race the
pause-must-pause directive machinery for workers that are already gone.

One deliberate refinement: the freeze RENEWS (re-arms the lease by one
full window) rather than merely skipping evaluation. A mere skip leaves
`lease_expires_at` in the past, so the first post-resume sweep would
evaluate an expired lease and race pod startup through the nudge
delivery path — a delivery failure there burns an attempt on the bounce
seam. Renewal leaves a LIVE claim with a full window on resume; the row
exits the victim set (~2 writes/day/row while suspended); rapid
suspend/resume cycles converge by the victim predicate (idempotent).

## What shipped

- `pkg/board/suspend.go` (NEW): the `SuspendSource` seam
  (`SuspendedWorkspaces(ctx) map[string]bool` — one POOR question per
  sweep, the `AssigneeLivenessSource` house pattern; nil = inert =
  today's semantics, pinned), `suspendSnapshot` (sweep form),
  `workspaceSuspended` (claim choke-point form), `freezeLeaseSQL`
  (renewal write: one window, nudge slot cleared, busy_renews reset,
  last_progress_at untouched — suspension is not progress).
- `pkg/board/reconciler.go`: the freeze runs FIRST in `rc1Reclaims`
  (victims of suspended workspaces renewed in place, removed from the
  ladder — no observation consult, no sentinel, no verdict; lease event
  `renewed`/`workspace_suspended` projected post-commit with the
  ladder's `leaseOut`); the persona lapse pass renews instead of
  accounting; the re-arm pass skips suspended workspaces (fresh-mint AND
  escalation arms — the whole pass is inert for the workspace); the
  dispatch drive filters them out of the workspace scan.
- `pkg/board/engine.go`: the claim choke point (`claimOnce`) returns the
  quiet no-grant ("suspended", the pressure gate's shape) — covers
  dispatch drive, event-driven drive, worker self-claims; `task_start`
  refuses loud (409-class, `GuardBackpressure`). Both feed after resume
  with no cleanup.
- `pkg/board/service.go`: `WithSuspendSource` option + `suspended` field.
- `pkg/board/lease_events.go`: `RenewSourceSuspended`.
- `api/internal/app/board_suspend_source.go` (NEW) + app.go wiring: the
  production adapter over `proxyHandler.GetAllKnownPhases` —
  Suspending/Suspended ⇒ suspended; unseen ⇒ not suspended (honest;
  the liveness seam's absence-of-evidence discipline).

## Tests

`pkg/board/suspend_integration_test.go` (NEW, BoardSuite): Pin A
suspension mid-claim → 3 sweeps → zero attempt increments, no reclaim/
escalate events, token+stage held, lease future-armed; resume sweep is a
no-op on the same claim (no manual rearm). Pin B genuine lapse while
RUNNING (seam installed, ws not in set) → reclaims with `failure_count`
accounting exactly as today — the regression pin. Pin C rapid
suspend/resume cycles ×4 → attempts constant, same claim, one
in_progress row (no double burn/requeue). Pin D choke point →
`ErrNoCandidate` + `task_start` 409 + zero dispatch spawns while
suspended; claim feeds after resume. Pin E persona → in_review freeze
(no accounting), re-arm mint skip, escalation skip, convergence to
`failed` after resume (genuine pre-suspension failures). Pin N nil seam
inert. `api/internal/app/board_suspend_source_test.go` (NEW): phase
mapping + nil-safety. Full `pkg/board` integration suite green
`-race`; `make fmt-check` clean.

## Notes

- RC2's PR-merge drift (external truth) can still mint an in_progress
  re-entry claim into a suspended workspace (edge: a PR merges while the
  workspace is suspended); the claim sits frozen and, if the worker
  never materializes after resume, lapses ONCE through the ordinary
  ladder — one genuine attempt, never the suspension-drain class.
  Accepted; recorded here for the reviewer.
- The `res.Reclaims` sweep counter counts everything that landed
  (renewals included, the r1-C5 `applied` contract) — the freeze's
  renewals ride it; the pins assert on events + row state.
- Local PG for the suite: extracted postgresql-15 server debs under
  /tmp/opencode/pgserver (client-only image), cluster on 127.0.0.1:5433,
  migrations from api/migrations applied in order (the CI recipe).
