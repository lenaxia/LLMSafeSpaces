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
  loud and never wrong-data, but NOT via a closed port: both harness incarnations hold the SAME
  port 4096 (the pi bridge on pi pods, `pi_bridge.go:8-9`) and the pod IP resolves fresh per
  call, so a stale post-flip route reaches the LIVE wrong-harness bridge and fails loud
  404-class against its shim-free route surface; pre-PodIP the resolution itself fails
  (`status.PodIP` cleared at deletion). (r2 review correction — the r0 "connection refused"
  mechanism was false.)
- Board claims live in Postgres, not the pod — the switch can sweep BEFORE anything dies;
  the only option with that luxury.

## §5 Story patches (binding)

Both implementation stories patched via the manager route (patchFile under /exchange, per the
#0070 file-only cutover) with change summaries; `dependsOn` edge added:
harness-switch-ui → harness-switch-api (the wire fields must exist before the UI control).
Recommendation (b) keeps the epic's plan — no pending_input raised.

Sequencing honesty (r3): the ui charter landed in the r2 working session; the api charter's
patch was lost when that attempt's lease lapsed mid-work (the board row still carried its stub
— caught by re-reading both rows at r3 start). The api charter was (re)applied at r3 from the
final post-r3 design text. Both rows now carry the binding preamble. Criteria rewrites are
platform-barred for spawned workers (ruling 2026-10-27, enforced at the patch call) — both
charters embed REFINED ACCEPTANCE CRITERIA as binding prose for the orchestrator to formalize.

## §6 Review trail (PR #2068, docs lane)

- **r0** (`0b05901ee`): CHANGES_REQUESTED — (1) §2a backfill spared only spec=opencode legacy
  objects (the whole kubectl-pi fleet took drain+recreate at upgrade, the doc's own F3
  attempt-burn chain, fleet-wide); (2) "never a second victim" overstated the watcher-fed
  gate's observed view. Fixed: backfill unconditional on live pod (any harness);
  pass 2 waits (≤30s) for the gate's observed arm; residuals documented; both test pins added.
- **r1** (`68b054186`): both r0 findings FIXED; two new — (1) the disclaimed absolute survived
  at five quotable sites; (2) the session-space migration's SQL DEFAULT mis-stamped the
  kubectl-pi fleet's live rows (orphaning them under HIDDEN-PRESERVED). Fixed: five sites
  qualified to the observed view; one-time boot stamp to spec.harness introduced.
- **r2** (`838cdf07b`): both r1 findings FIXED; two new — (1) the routing fail-loud mechanism
  was FALSE as stated ("closed port / connection refused" — both incarnations serve 4096 and
  IP resolves fresh, so the stale route reaches a live wrong-harness bridge: 404-class); the
  false claim lived at four sites + a test pin that could never pass; (2) the one-time boot
  stamp had an undocumented rolling-update window (old replicas still registering hardcoded
  'opencode' after the marker lands — mis-stamped forever). r3 fixes, both verified against
  main before editing (`pi_bridge.go:8-9,14-15`, `pkg/agent/opencode/adapter.go:131`,
  `pkg/agent/pi/podtransport.go:78`, `resolver_host.go:126-138`,
  `phase_active.go:104,136,179,223`, `proxy_session_index.go:538,696,882`):
  - routing rewritten to the honest two-window contract at all four sites + the test map
    re-pinned (`TestHarnessRouter_StaleTTLPostFlip404`, 404-class; pre-PodIP = resolution
    failure);
  - the one-time stamp replaced by a REPEATABLE boot sweep scoped to never-switched workspaces
    (new persistent `AnnotationHarnessSwitchedAt`, set-once at first accepted switch, never
    cleared — the sweep's predicate; safety from the predicate, not run-once-ness), which
    heals rolling-window rows at the next boot by construction; residuals documented
    (bounded visibility gap; the narrower mid-rollout-switch class accepted) and pinned
    (`TestSessionIndex_BootStampRolloutWindow`).
- **r3** (`fb3c1d31d`): both r2 findings FIXED and verified by the reviewer; one new —
  `pre-backfill-switch-wedge`: the §2a backfill premise ("webhook 7a ⇒ every status="" pod was
  built for its spec") is falsified by the contract's OWN switch surface in the pre-backfill
  window — a legacy Active workspace's first switch passes the phase-only guard, writes
  spec+marker, and if the controller's backfill arm reaches the object first it stamps
  status=desired onto the OLD-harness pod: spec==status forever, drift never fires, permanent
  mis-routing + permanently hidden live sessions with no self-heal (the sweep excludes the
  workspace — annotation present). r4 fix: MARKER-AWARE backfill — `status=="" && live pod &&
  AnnotationHarnessSwitch present ⇒ NOT backfill; drift` (the marker proves the spec postdates
  the pod; recreation is what the switch asked for). The reviewer's alternative (SwitchHarness
  409s while status.harness=="") was considered and REJECTED: the first switch on an existing
  workspace is the epic's flagship use case — recorded in §1a step 1. Known benign cost
  documented: an aborted pre-backfill first switch costs one convergent same-harness
  recreation (the controller cannot know a status="" pod's true harness; the marker-present
  reading is the only one that never wedges a real switch). `TestHarnessDrift_BackfillSkipsMarkerPresent`
  pinned (§2a + test-map row 3); §2b gains the backfill/annotation interplay sentence; HL D4,
  lifecycle, and the failure table updated (legacy-fleet row scoped markerless; new
  first-switch row).
- **r4** (`fb7d1a5ba`): the r3 marker-aware backfill FIXED and verified; two new findings, both
  unpropagated consequences of the newly-introduced pre-backfill class:
  - `pre-backfill-abort-recreation` — §7/HL D10 still promised "pre-drain abort ⇒ no
    recreation, drift never fires", contradicting §2a's benign cost for the status="" class.
    Fixed: §7 and D10 split the abort contract (status-set: no recreation; pre-backfill:
    exactly one convergent same-harness recreation — the sweep residue is the only other
    cost); pinned in the Controller test-map row.
  - `pre-backfill-routing-window` — D8's "≤60s stale window" and the lifecycle's "router
    serves status.harness (old)" are false for status="" objects: the spec fallback serves
    the NEW harness against the OLD live pod from router-TTL expiry (the first ≤60s the
    stale cache routes correctly!) until buildPod writes status — reconcile+drain-bounded
    (defer can stretch toward the 60m force), not TTL-bounded; new calls fail loud 404-class
    while in-flight turns still finish (streams predate the flip; the drain watches them).
    Fixed at HL D8, HL lifecycle, LL §3 (new paragraph + `TestHarnessRouter_PreBackfillFallbackWindow`
    pin), Router test-map row, and OW (d2) proactively (same claim class).
