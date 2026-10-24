# Worklog: #1915 — reap-net coverage gap: engine-authorized and DAG-leg carrier deletes origin-mark before effects

## Charter

Issue #1915 (split from #1901's close-out Observations §1): the
mark-before-effects discipline existed ONLY in the Scheduler's
`propagateCarrierDelete`; two leg families ran the same delete effects
UNMARKED, so a mid-effects failure left ghost rows the #1901 reap net
structurally cannot admit:

1. `deleteSessionAuthorized` (engine.go:1462 @ cdde5e72) — the
   caller-neutral core behind the routine PreserveOnFailure-success
   cleanup, the retry-intermediate cleanup, and the DAG reconciler's
   node-retry cleanup (engine.go:601).
2. The DAG reconciler's `propagateDeletedSession` (engine.go:617) —
   BOTH envelope legs (errorCode + delivered).

Structural cause: `ReconcilerStore` (engine.go:54) lacked
`RecordSessionOrigin`/`DeleteSessionOrigin` — they lived only on the
Scheduler's store seam (engine.go:87-91). Branch
`fix/1915-reap-origin-marking` off origin/main @ cdde5e72 (post-#1897
squash b52f0dab — current main IS its final shape; no rebase needed).

## Pre-work validation (owner-mandated, packeted before the red pin)

- (a) Gap reproduced on pristine main: 5 demonstration tests (kept at
  /tmp/opencode/repro1915/, removed from the tree) — positive control
  (routine carrier leg marks kind='carrier' → PASS) + 4 gap legs, all
  RED exactly per the issue (both authorized-delete legs; both
  envelope legs): effects ran (propagator called, agentd 204/route
  confirmed) with ZERO carrier marks ever recorded →
  `ListCarrierOriginMarks` empty → the discriminator
  (proxy_session_index.go:130 `marks[sessionID]`) false →
  kept_for_operator persists.
- (b) Already-fixed sweep clean: main since 10-20 touches this area
  only via #1897 (b52f0dab) and #1913 (d91945b4) — the cited base,
  neither marks these legs. Open PRs #1921/#1916/#1904 target other
  lanes (sibling fleet's #1916/#1904 untouched per hazard).
- (c) Structural confirm: `pkg/workflows.Store` already implements
  both origin verbs (store.go:1250/1315) — widening ReconcilerStore
  needs zero store-package change.

## Ground truth at lane start

The #1901 worklog's (d) residual named this class: "carriers whose
propagation NEVER ran ... leave no mark → kept_for_operator". #1915 is
the complement — legs where propagation DID run but unmarked. The
effects (`PropagateSessionDeleted`, proxy_handlers.go:836) return void
(soft failures logged internally): an index-write failure is only
observable as the ghost row surviving. The mark written BEFORE the
effects is then the only admission data; the clean AFTER the effects
is what keeps origin rows from accumulating per fire. In the
shared-outage window (effects' index writes fail AND the mark clean
fails — same pool), the mark must SURVIVE: fail-safe direction.

## The changes (red d923914a → green)

- `ReconcilerStore` widened with `RecordSessionOrigin` /
  `DeleteSessionOrigin` (comments cite #1915; concrete store already
  conforms — store.go:1250/1315).
- New NARROW `SessionOriginStore` interface (just the two origin
  verbs; orchestrator GO refinement 1) beside `SessionDeletePropagator`
  — satisfied structurally by SchedulerStore, ReconcilerStore, and the
  concrete store; nil ⇒ pre-#1915 unmarked behavior.
- `deleteSessionAuthorized` gains an `origins SessionOriginStore` seam
  param (caller-neutral kept): CONFIRMED (204) deletes mark
  kind='carrier' → effects → clean; refused deletes (session survived)
  write nothing. `originForPurpose` maps the purpose label to the
  honest origin value (routine cleanups → 'routine', DAG node-retry →
  'workflow'); the mark's Title carries the purpose for provenance.
  Both call sites updated: `deleteRoutineSessionAuthorized` passes
  `s.Store`; the DAG node-retry call passes `r.Store`.
- `propagateDeletedSession` wraps its effects in the same
  mark→effects→clean (origin 'workflow', kind 'carrier', title
  "workflow ephemeral carrier"), gated on `r.Store != nil` — mirroring
  `propagateCarrierDelete` (engine.go:1219-1241) line-for-line in
  shape: best-effort mark (logs, effects still run), best-effort
  clean (logs, mark survives on failure — the fail-safe).

NO reap-side changes: `ListCarrierOriginMarks`, the bypass admits at
proxy_session_index.go:130/305, DefaultReapGuard, the #1850
spawn-window grace, and the #1865 probe-veto semantics are untouched —
this lane only widens the WRITER side to the same policy-deleted
population the bypass already admits.

## Red-first evidence + mutation recipes

RED d923914a (pins only, against unmodified main): the four gap pins
FAIL on exactly the ordering observable —
`TestExecuteRoutine_AuthorizedDelete_MarksCarrierOriginBeforeEffects`,
`TestExecuteNode_SupersededCleanup_MarksCarrierOriginBeforeEffects`,
`TestReconciler_DAGEnvelopeTeardown_MarksCarrierOriginBeforeEffects`
(both envelope legs in one pin),
`TestExecuteRoutine_AuthorizedDeleteMidOutage_MarkSurvivesForReap`;
the invariant pin
(`TestExecuteRoutine_AuthorizedDeleteFailed_NoOriginMark`) GREEN at
red — a refused 502 delete writes no carrier mark and the survivor's
kind='session' verdict (written by the completion-tail artifacts path,
engine.go:1173-1175) is untouched. All five GREEN after the fix.

Recipes: (1) move either `RecordSessionOrigin` call after the
`PropagateSessionDeleted` call → that leg's ordering probe RED (the
probe consults the store AT effects time — that is the pin's subject);
(2) drop the `origins != nil` / `r.Store != nil` gates → no red (they
are degradation paths, pinned only by nil-wiring non-panic behavior);
(3) mark before the 204 gate (eager marking) → the 502 invariant pin
RED (marks exist only for confirmed deletes); (4) make the clean
conditional on effects success → unreachable (the propagator returns
void — the clean IS unconditional by design; the survive pin encodes
the outage window instead).

## Observations (disclosed, NOT touched)

1. The mark/clean pair is now written three times (carrier tail,
   authorized core, envelope propagator) — a shared helper is
   tempting but each site's comments carry leg-specific rationale;
   left inline deliberately (minimal fix; a future refactor can
   consolidate once #1915 settles).
2. Residual (accepted, same class as #1901's (d)): a DB blip that
   fails ONLY the effects' index writes but recovers before the clean
   leaves the mark cleaned and the ghost unmarked — identical residual
   on the routine carrier path (propagateCarrierDelete); this lane
   brings the legs to parity, not beyond.
3. The transient mark is visible to `ListSessionOrigins`-backed
   sidebar display for its ~seconds lifetime — origin/title fields are
   honest ('routine'/'workflow', purpose / "workflow ephemeral
   carrier") so the transient row, if ever rendered, reads sanely.

## Verification

- `go test ./api/...` — all 42 packages green (incl. the full
  workflows suite, 87s; handlers; app).
- Red run evidence: 4 gap pins RED @ d923914a, all GREEN post-fix;
  invariant pin green at both ends.
- gofmt / goimports / golangci-lint (pre-commit, new-only) clean;
  repolint clean (worklog sentinel NNNN → bot numbers it).
