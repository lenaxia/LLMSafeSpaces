# Worklog: Task Board Config UI section (#1832 item 6, part 2/2)

## Scope
Settings → Task Board tab: user-level board defaults editor
(reviewColumn off|manual|auto, autoAssignReviewer, defaultWorkerSlots
int>=1) + per-project override editors (reviewColumn +
autoAssignReviewer ONLY — defaultWorkerSlots is a user-level key the
project tier rejects) + effective view with per-key provenance
(override vs inherited). Client validation mirrors the server and
blocks the send before any request (typed bodies; enum/integer gates).

## Surface
- frontend/src/api/board.ts: BoardConfig/BoardConfigOverride/
  ProjectBoardConfig types + 4 client methods (GET/PUT
  /me/board/config, GET/PUT /me/projects/:id/config, requestId on
  mutations).
- components/settings/BoardConfigTab.tsx (+ SettingsPage tab, router
  path settings/board-config).
- Tests: BoardConfigTab.test.tsx (7 rows, RED-first), e2e
  board-config.spec.ts (3 rows: render+provenance, full-config PUT,
  REPLACE-semantics override PUT).

## Semantics pinned
- Override PUT = REPLACE with absent = inherit: the editor sends the
  override SET (preserving other existing overrides), {} when
  everything inherits; never defaultWorkerSlots at the project tier.
- Provenance comes from the stored overrides (not an effective/global
  diff).

## Gates
Unit sweep 2386/2386 incl. the 7 new; tsc clean; eslint 0 errors;
repolint green (post-heal base); e2e board-config 1/1 (plus the full
board harness unaffected).
