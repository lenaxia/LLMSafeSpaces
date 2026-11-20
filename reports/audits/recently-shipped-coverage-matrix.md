# Recently-shipped surface: integ/e2e coverage gap matrix

Owner direction 2026-11-17: "make sure we have integration and e2e tests
around the recently-shipped features — and the bug classes they just
exposed — so they work consistently." The .15 bugs prove per-story pins
existed but missed real user paths: the board rail shipped with no
orientation/viewport-change test, and the workspace refresh/start
starvation (PR #2088: CRD-skew starving the legacy-harness backfill arm
and the phase machine) passed every unit pin.

This is the inventory (what runs in CI today / what only runs locally /
what doesn't exist) for the recently-shipped surface, and the record of
what this PR closed. Scope: board UI (1x4 rail + scroll-snap + focus
column + kebab actions + done cap/drawer), workspace lifecycle
(create/start/provisioning/suspend/resume/refresh, phase machine),
harness bind-once, wrap-up machinery (engine/watermark/verdicts where
surface-visible), turn-end lane.

Verdict key: **CI** = a GitHub workflow executes it (name in brackets);
**nightly/weekly** = scheduled-only; **local** = only runnable by hand;
**absent** = doesn't exist. "CI" for frontend means the whole Playwright
suite runs on frontend-touching PRs (`ci.yml` frontend-test job +
`e2e-pr.yml`).

## 1. Board UI — the 1x4 rail (v0.44.15/.16, PRs #2087 + the .16 follow-ups)

| Coverage | Where | Verdict before this PR |
| --- | --- | --- |
| Rail renders, 95/5 split, title wrapping, buttons | `board.spec.ts` "mobile: 1x4 rail" | CI |
| Scroll-snap lands per column (programmatic/keyboard) | `board.spec.ts` "scroll-snap" + "keyboard parity" | CI |
| Real-touch swipe pans, done reachable by touch | `board-rail-touch.spec.ts` pins 1–3 (.16, born-red record in changelog) | CI |
| Focus-column: enter/transit/exit/re-center | `board.spec.ts` focus describe + touch spec pin 4 | CI |
| Focus mode reset on viewport class change (md crossing) | `board.spec.ts` "md crossing force-exits" + touch spec pins 6–7 | CI |
| Orientation round-trips restore exact layout | `board-rail-touch.spec.ts` pins 6–7 (.16, shipped with the orientation fix) | CI |
| Landscape 4-in-a-row (doubles as desktop rule) | `board.spec.ts` "landscape/desktop" + touch spec pin 5 | CI |
| **Empty-column skeletons at every drain state** | `board.spec.ts` "empty columns keep their width" covered ONLY the all-done shape; `BoardPage.rail.test.tsx` the all-done unit | **partial — per-column drain states absent** |
| **Done cap + show-all drawer at PHONE viewport** | `board.spec.ts` done-cap test ran at desktop viewport only | **absent at the phone class** |
| **Task-card kebab actions at phone viewport** | unit (`BoardPage.kebab.test.tsx`) + desktop-viewport e2e only | **absent at the phone class** |
| **Settings surfaces at phone viewport** | `settings.spec.ts` desktop viewport only | **absent at the phone class** |

Closed by this PR: `board-rail-drain.spec.ts` (the per-column drain sweep
— each of the four stages drained alone, plus the all-empty board; the
permanent wall for "the layout cannot jump as stages drain") and
`board-mobile-smoke.spec.ts` (thin, broad: task kebab fires its
move-stage/pause write, done drawer opens/closes from the done column,
project kebab fires, settings surfaces render at 375px).

## 2. Workspace lifecycle — the phase machine (v0.44.16, PR #2088 + #2093)

| Coverage | Where | Verdict before this PR |
| --- | --- | --- |
| Phase dispatch unit pins (fake client) | `controller/internal/workspace` suite → `ci.yml` test job | CI |
| CRD-skew shape, fake-client interceptor (SKEW-1..6) | `harness_binding_skew_test.go` (.16, born-red record) → `ci.yml` test job | CI |
| **CRD-skew shape against a REAL apiserver** | nothing — envtest served only the current CRD | **absent** |
| **Start/provisioning completing through the phase machine, real apiserver** | nothing at PR tier | **absent** (nightly kind only) |
| **Refresh/recycle observed, real apiserver** | nothing at PR tier | **absent** (nightly kind only) |
| **Suspend/resume preserves state, real apiserver** | nothing at PR tier | **absent** (nightly kind only) |
| Full-stack lifecycle (create→Active, refresh recycle, suspend/resume, history continuity) | `local/test.sh` → `e2e-nightly.yml` | nightly, healthy CRDs only |
| **Kind-tier lifecycle on a SKEWED (pre-.15) CRD** | nothing | **absent** |
| API-tier refresh conflict-retry / bind-not-persisted | `workspace_refresh_test.go`, `harness_binding_test.go` → `ci.yml` | CI |

Closed by this PR (PR tier, real apiserver): `lifecycle_envtest_test.go`
(LC-1 pruning-is-real, LC-2 start completes, LC-3 refresh recycles,
LC-4 suspend/resume preserves, LC-5 CRD-apply convergence) against the
pinned pre-.15 CRD fixture (`testdata/legacycrd/`, from tag v0.44.14),
wired into `envtest.yml` as an explicit step (the #820 drift class: a
suite no workflow executes is not a suite). Born-red verified: on
pre-#2088 main (`dc5632044^`) LC-2/3/4 starve — no pod is ever built,
no phase transition fires — and on current main all five pass.

Closed by this PR (kind tier): `scripts/workspace-lifecycle-skew-e2e.sh`
+ `workspace-lifecycle-integration.yml` — the us2-kind pattern
(controller-lean chart, `--skip-crds`, pre-.15 CRD pre-applied) driving
the owner-visible user path: start → Active, refresh → recycle → Active,
suspend → Suspended (PVC retained) → resume → Active, then the operator
CRD apply converging the harness stamp. Dispatch + weekly + PR-path-gated
on the phase-machine surface.

## 3. Harness bind-once (v0.44.15, owner ruling 2026-11-16)

| Coverage | Where | Verdict |
| --- | --- | --- |
| Webhook bind-once matrix (11 cases, marker discriminator) | `workspace_webhook_test.go` → `ci.yml` test job | CI |
| Controller backfill/park/gate/discriminator | `harness_binding_test.go` → `ci.yml` | CI |
| API bind matrix, enabled-set, first-session binding | `api .../workspace/harness_binding_test.go` → `ci.yml` | CI |
| No-switch-or-recreate absence pin | `harness_no_switch_pin_test.go` → `ci.yml` | CI |
| Real-apiserver bind round-trip + convergence | this PR's LC-5 | CI (envtest.yml) |
| SDK mirrors (Go/TS/Py/Java) | sdk jobs in `ci.yml` | CI |

## 4. Wrap-up machinery (epic wrap-up review)

| Coverage | Where | Verdict |
| --- | --- | --- |
| Watermark advance / delta window / charter renderer | `pkg/board` integration pins → `secrets-integration.yml` (`-tags=integration ./pkg/board/...`, path-gated on `pkg/board/**` + main pushes) | CI |
| wrapUpReviewer config + seeded role | handlers board-config integration pins → `secrets-integration.yml` | CI |
| Verdict recording / gap discipline (engine) | `pkg/board` suite | CI |
| Surface-visible wrap-up UI columns | board rail columns ride §1 | CI |

## 5. Turn-end lane (issues #2064/#2065, v0.44.15)

| Coverage | Where | Verdict |
| --- | --- | --- |
| Event emission, opencode leg (probe seam) | `cmd/workspace-agentd/board_turn_wiring_e2e_test.go` (untagged, no `-short` skip) → `ci.yml` test job | CI |
| Event emission, pi leg (journal) | `pi_board_turnend_test.go` (untagged) → `ci.yml` test job | CI |
| Advance ledger bounds | `board_turn_ledger_test.go` (untagged) → `ci.yml` | CI |
| Consumer due-mark pins (2065-A..G) | `pkg/board/due_mark_2065_integration_test.go` → `secrets-integration.yml` | CI |
| Production wiring (app tier) | `api/internal/app/board_due_mark_test.go` (untagged) → `ci.yml` test job | CI |
| Origin-plugin stamp parity | `ci.yml` `-tags=integration -run TestOriginPlugin ./pkg/agent/opencode/` | CI |

## 6. Born-red pins from the recent lanes: do they run in CI?

Every pin enumerated above executes in some workflow — none found
local-only. The two structural risks found instead:

1. **The #820 class (a suite that exists but no workflow executes)** is
   why the new envtest lifecycle step is EXPLICIT in `envtest.yml`
   rather than relying on path triggers — the workflow's own comments
   teach this lesson from `TestEnvtestRelayStaging`.
2. The pkg/board integration lane's PR trigger is the
   `secrets-integration.yml` changes-gate regex (`^pkg/board/` etc.);
   pushes to main always run it. The turn-end/wrapup pins therefore run
   on every main push and every touching PR — verified by reading the
   gate, not assumed.

## 7. Priority discipline (the rule this PR followed)

Coverage that would have caught each real bug took absolute priority
over speculative breadth: the envtest lifecycle suite and the drain
sweep guard the two .15 bug classes; the kind skew script guards the
real user path; the phone smoke is deliberately thin. No timing pins
tighter than the house discipline (the envtest convergence leg waits on
a 90s schema-reload window; the reconcile budgets are 60s with full
round-trip passes; the kind script waits use the 90s/180s class the
nightly already tolerates).
