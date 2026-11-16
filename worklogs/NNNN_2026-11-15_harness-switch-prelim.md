# Harness-switch prelim: options writeup + high/low-level designs

2026-11-15 · branch `docs/harness-switch-prelim-designs` · task tsk-secluded-stovepipe-remembers

## §1 The task

Prelim story for board epic "User-facing harness switching (opencode <-> pi)" (`b6eff9d9`,
project `prj-orient-mint-stencils`): enumerate the option space, recommend one with code
evidence, produce high/low-level designs, then patch the epic's implementation stories
(`harness-switch-api` = tsk-visioned-hurdle-leaves, `harness-switch-ui` =
tsk-vibrant-geyser-masks) from the findings.

## §2 Deliverables

- `reports/specs/harness-switch-options-writeup.md` — options (a) creation-time-only,
  (b) switch-with-recreate, (c) dual-runtime pod, (d) five code-revealed findings; each
  grounded in verified anchors (F1–F7 fact base). RECOMMENDED: (b) with (d1)–(d5) folded in.
  (c) rejected on five anchored grounds (S5 topology, design 0073 RULING A, singular harness
  port, two-headed config, solves nothing). (a) rejected as the epic's answer — its core
  honesty (a switch IS recreation) is absorbed into (b)'s framing. Session-space semantics:
  HIDDEN-PRESERVED with switch-back restore (shown-inert and reap both rejected with reasons).
- `reports/specs/harness-switch-high-level-design.md` — lifecycle, decisions D1–D12,
  failure-mode table, non-goals, open questions with pinned defaults.
- `reports/specs/harness-switch-low-level-design.md` — the implementation contract:
  `PUT /workspaces/{id}/harness` (dedicated endpoint, the dev-preview no-PATCH ruling),
  two-pass board sweep (clean requeue — depose's handoff without its failure accounting;
  in_review declaim; quiesce gate = the suspension-gate shape keyed on spec≠status),
  webhook 7a′ marker gate (the force-recycle annotation pattern), controller harness-drift
  arm + legacy backfill (the arch-drift shape), `status.harness` + status-first router
  resolution, `session_index.harness` migration + reconcile shielding, frontend control +
  confirm + badge-truth, abort/rollback = switch back, born-red test map, story split.

## §3 Seam verification (against origin/main 71cb54f80)

Two explore passes mapped the code; decisive anchors re-verified by hand: webhook 7a
(`workspace_webhook.go:646-672`), HarnessRouter TTL + fail-open (`harnessrouter.go:26-29,131-169`),
`GetWorkspaceHarness` spec-resolution (`resolver_host.go:102-120`), no-PATCH ruling + long-op
202 conventions (`router.go:1638-1740`), arch-drift recreation + drain (`phase_active.go:202-231`,
`session_drain.go`), harness-aware buildPod (`harness_delivery.go:66-111`), suspend freeze +
phase source (`pkg/board/suspend.go`, `board_suspend_source.go:40-67`), depose requeue failure
overlay (`depose.go:169-186`), persona lapse/re-arm accounting (`reconciler.go:735-809`),
session-index reconcile + reaper guard (`proxy_session_index.go:285-299,447-452`), hardcoded
registrations (`:538,696,882`; `short_session_ids.go:88`), create-path harness gap
(`types/workspace.go:35-47`, `workspace_service.go:1296-1348`), frontend surfaces
(`WorkspaceSettingsDrawer.tsx`, `ConfirmDialog.tsx`, `Sidebar.tsx:412-415,537-548`,
`ChatPage.tsx:1116`). Design 0073 read at `eca3368ca` (`origin/pi-drift-s6`, PR #2039 merged
into the stack branch — S6/S7's main PR pending; noted as a precondition in the api charter).

## §4 Design pivots worth recording

- Sweep, don't freeze: the #2040 lesson (never burn attempts on infra moves) is satisfied by
  requeueing work to the new harness, NOT by freezing dead claims — an old-harness session can
  never resume, so a frozen lease has no exit. The needed requeue is depose's handoff minus
  depose's failure accounting (new SQL sibling, specified verbatim).
- Two-pass sweep: closes the only claim-race (a claim granted between pass 1 and the spec
  write) — pass 2 runs under the armed quiesce gate so the racer cannot re-claim.
- Routing pins to `status.harness` (the pod's truth), not spec — the mis-route window fails
  loud (single harness port per pod incarnation), never wrong-data.
- Board claims live in Postgres, not the pod — the switch can sweep BEFORE anything dies;
  the only option with that luxury.

## §5 Story patches (binding)

Both implementation stories patched via the manager route (patchFile under /exchange, per the
#0070 file-only cutover) with change summaries; `dependsOn` edge added:
harness-switch-ui → harness-switch-api (the wire fields must exist before the UI control).
Recommendation (b) keeps the epic's plan — no pending_input raised.
