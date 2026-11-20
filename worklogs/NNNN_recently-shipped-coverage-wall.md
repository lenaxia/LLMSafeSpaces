# Worklog: recently-shipped coverage wall — the .15 bug classes pinned at the integ/e2e tiers

Task `tsk-robust-pivot-invokes`. Owner direction 2026-11-17: "make sure
we have integration and e2e tests around the recently-shipped features
— and the bug classes they just exposed — so they work consistently."
The .15 bugs prove per-story pins existed but missed real user paths:
the board rail shipped with no orientation/viewport-change test (fixed
with the rail in #2087/.16), and the workspace refresh/start starvation
(PR #2088: CRD-skew starving the legacy-harness backfill arm and the
phase machine) passed every unit pin.

## What landed

1. **The gap matrix** (`reports/audits/recently-shipped-coverage-matrix.md`):
   the per-feature inventory (CI / nightly / local / absent) for the
   recently-shipped surface — board rail, workspace lifecycle, harness
   bind-once, wrap-up machinery, turn-end lane — with the finding that
   every recent lane's born-red pins already execute in some workflow
   (none local-only), and the two structural gaps: no real-apiserver
   execution of the 2088 class (fake-client interceptor only), and no
   rail drain-state/phone-smoke execution beyond the all-done shape.

2. **The envtest lifecycle suite**
   (`controller/internal/workspace/lifecycle_envtest_test.go`, `//go:build
   envtest`) against a REAL apiserver serving the PINNED pre-.15 CRD
   (`testdata/legacycrd/`, captured from tag v0.44.14 — the
   harness-less shape a normally-upgraded cluster serves because helm
   never upgrades crds/):
   - LC-1 pruning-is-real: a typed harness write is ACCEPTED and the
     field ABSENT on read-back (the exact trap, proven against the
     apiserver rather than simulated by the fake interceptor);
   - LC-2 start completes under skew (the widened .15 symptom);
   - LC-3 the RefreshWorkspaceCompute write (restartGeneration bump +
     the #1505 force marker) recycles the pod and returns to Active;
   - LC-4 suspend/resume preserves state (pod gone, PVC RETAINED,
     resume → Active);
   - LC-5 convergence once the CRD is patched to the .15 shape (the
     hot reload a `kubectl apply` performs), including the probe-wait
     for the apiserver's schema reload (90s generous window).
   Wired into `envtest.yml` as an EXPLICIT step (the #820 drift class:
   a suite no workflow executes is not a suite). **Born-red verified**:
   on pre-#2088 main (`dc5632044^`) LC-2/3/4 starve — no pod is ever
   built, no phase transition fires (>1000 reconciles, no convergence) —
   and on current main all five legs pass (~35s).

3. **The kind-tier skew e2e** (`scripts/workspace-lifecycle-skew-e2e.sh`
   + `.github/workflows/workspace-lifecycle-integration.yml`, the
   us2-kind pattern): pre-.15 CRD applied BEFORE helm, controller-lean
   chart installed `--skip-crds` (the skew survives), then the
   owner-visible path — LC-K1 start → Active, LC-K2 refresh → recycle →
   Active (generation observed), LC-K3 suspend/resume with PVC
   retention, LC-K4 the operator's CRD apply converging the harness
   stamp (controller restart = the crash-safe attempt-map reset).
   PR-path-gated on the phase-machine surface + weekly + dispatch.
   Pinned by `scripts/workspace_lifecycle_script_test.go` (bash -n,
   the skew wiring order, the fixture drift guard, the us2 incident
   classes re-pinned, every leg present).

4. **The rail drain-state wall**
   (`frontend/tests/e2e/board-rail-drain.spec.ts`): every drain state —
   each of the four columns drained ALONE while the rest stay loaded,
   the fully-empty board, and a column draining LIVE on reload — with
   measured portrait width fractions inside the fixed-basis bands, the
   skeleton affordance ("—"), and the drop zone surviving (a drained
   column is still a drop target). Closes the gap where only the
   all-done shape was pinned (a backlog-only or in_review-only collapse
   passed every existing pin).

5. **The phone-viewport smoke**
   (`frontend/tests/e2e/board-mobile-smoke.spec.ts`, thin but broad per
   the direction): task kebab opens on-screen at 375px and its Pause
   action fires the POST; the done cap's show-all affordance reachable
   from the done page, drawer opens with all 13 and closes; the project
   kebab's archive action fires; settings surfaces render with NO
   horizontal overflow (the #1629 class).

## Rules honored

- Coverage that would have caught each real bug over speculative
  breadth (the two .15 classes dominate; the smoke is deliberately
  thin).
- No flaky-by-design tests: the envtest legs use bounded 60s reconcile
  budgets with full real-apiserver round trips; the CRD hot-reload
  gets a 90s probe window; the kind waits reuse the nightly's 90–300s
  class; no sleep-based assertions anywhere.
- Every added test fails meaningfully on the bug it guards: the envtest
  legs were run RED against `dc5632044^` (pre-fix main) before being
  run green on main — the spot-verification record.

## Evidence

- envtest: `go test ./controller/internal/workspace/ -tags envtest -run
  TestEnvtestLifecycle` — 5/5 PASS (~35s); born-red 3/4 legs FAIL on
  pre-#2088 main (LC-1 correctly passes there: it pins the apiserver,
  not the fix).
- Born-red RE-VERIFICATION (2026-11-20, the exact reproduction): on
  `8ed843500` (= `dc5632044^`, pre-#2088 main; the test file copied in
  with a no-op `ageBackfillWindow` shim — the seam postdates the fix and
  only LC-5 uses it) `TestEnvtestLifecycleSkew_StartCompletes` FAILS at
  67s (`lifecycle leg "start builds the pod" did not converge` — no pod
  is ever built); the same test PASSES in 6.4s on this branch's head.
  The widened .15 symptom is pinned red-on-pre-fix, green-on-fix.
- scripts: `go test ./scripts/ -run TestWorkspaceLifecycle` — 6/6 PASS.
- frontend: board-rail-drain + board-mobile-smoke specs parse and list
  (12 tests); full execution rides the PR's Playwright CI
  (e2e-pr.yml runs the suite on frontend-touching PRs) — the
  authoring sandbox suspends between turns, killing long-lived
  browser runs, so the authoritative gate is CI.
- `make fmt-check` clean; `go vet -tags envtest` clean.
