# Worklog: M1 crash-loud arming (design 0061 §3, implementation PR2)

**Date:** 2026-09-23
**Session:** Design 0061's first mechanism, per the recorded order after PR1 (#1552, the posture RBAC fixes). The #1548 split-brain's class — an enabled-but-inert staging binary booting green — becomes structurally undeployable: a DISTINCT exit code the CrashLoop carries.
**Status:** Complete

---

## Objective

Exit 85 (the doctrine ladder's fifth rung: 81/82 agentd verify, 83/84 opencode verify) for "deployed relay-only but not armed"; the enable line as the armed contract; no new armed-state notion (the existing SetupRelayStaging conjunction); the existing 30s guard budget as the window; flag-off byte-identical.

## Work Completed

- `controller/internal/controller/controller.go`: `RelayStagingNotArmedExitCode = 85` with the ladder + #1548-class doc block; `ArmingStartupGuardWindow = 30s` (the design's named constant — the guard's existing budget, now the single source: `context.WithTimeout(..., ArmingStartupGuardWindow)`; NO new timer machinery).
- `controller/main.go`: the staging-error path exits with the constant (not bare 1) and the refusal message says "(not armed)".
- Unit pins (`controller_arming_test.go`): 85-is-distinct-from-81–84; the flag-off nil-nil shape with a NIL manager (safe iff the !enabled branch returns before any mgr deref — a future reorder panics the test instead of every flag-off boot); the window==guard-budget semantic constant.
- Source-truth pins (`local/m1_arming_source_test.go`, the release-smoke precedent): main.go's wiring exits THE constant with no surviving bare Exit(1) at the site; the constant's value + ladder comment + guard-reads-the-window bindings; the parity reconciliation contract recorded (the #1548 provenance PR's verifyRelayStagingParity takes THIS code when it merges — the pin asserts the contract is carried at the constant until then).

## The four design shapes — where each is pinned

1. unarmable→85-in-window: the wiring (source-truth pin) + the window (the guard's existing fail-loud matrix, staging_guard_client_test.go, now reading the named constant).
2. armed→line+exit-0: the line exists (controller.go:164); the release-smoke binary markers pin its literal; the posture gate asserts it cluster-side (PR5).
3. parity→85: the reconciliation contract (the parity PR is not yet merged on main; its failure path reconciles onto this constant — noted here and pinned as the contract).
4. flag-off→byte-identical: the nil-nil unit pin.

## Key Decisions

1. NO new armed-state — the design's §3 scoping: the existing conjunction, made un-skippable by giving its failure a name (the code) and its success an existing contract (the line).
2. Source-truth pins for the main() wiring (not unit-runnable) — the established local/ precedent; the needles match across line-wraps after the first red (two needles failed on wrapped comments — fixed to unwrapped substrings, a small instance of the needle-must-match-the-artifact class).

## Blockers

None.

## Tests Run

- `go test ./controller/...` — all green (incl. the three new unit pins).
- `go test ./local/ -run TestM1_` — 3/3 source-truth pins green.
- `go build ./...` — clean.

## Next Steps

1. Review; then PR3 per the recorded order: M2 fallback + both counters.

## Files Modified

- `controller/internal/controller/controller.go` (the constant, the window constant, the guard reads it)
- `controller/main.go` (exit 85, the not-armed refusal)
- `controller/internal/controller/controller_arming_test.go` (new, 3 pins)
- `local/m1_arming_source_test.go` (new, 3 source-truth pins)

## r1 — the seam; the shapes executed

- **The decision seam** (the reviewer's ask, the opencodeOverlayDecision precedent): `RelayStagingExitCodeFor(err) int` — nil→0, ANY enabled failure→85 (one code, one meaning, the design's armed conjunction); main.go exits THROUGH it; the mapping is unit-tabled (flag/config/guard classes).
- **The shapes EXECUTED** (arming_behavior_test.go, hermetic — no envtest dependency): the manager stub (embedded nil interface: only the three touched methods; a GetConfig panic means production reached around the seam — by design); the guard-client seam (`newStartupGuardClient`, the house DI pattern) letting the fake client serve the pub-Secret read + mint-key create; ARMED = stub router + valid pub Secret + fake API → non-nil cfg + exit-0 mapping + the line literal pinned IN THIS TREE (the r1 finding: the release-smoke citation was dangling — that branch is unmerged; this is the armed contract's only in-tree coverage until it or the gate lands); UNARMABLE = dead router → error WITHIN the window (elapsed-bounded — "not 1, not a hang" now tested), refusal language, 85 via the seam.
- **The two inaccurate PR-body/worklog claims corrected**: the fail-loud matrix does NOT read the named window (the diff touched no guard test — the WINDOW is bound by the constant + the guard's source; the behavioral test bounds the elapsed); the release-smoke markers do not exist on main (the literal pin moved here).
- The grep-window finding: the refusal→exit call is now a TIGHT window (the refusal line through the adjacent exit — no foreign Exit(1) can trip it).

## r2 — the sink chase, honestly recorded

- **Finding 1 (the armed-line claim): the sink capture is NOT POSSIBLE in this package.** I built the real logr capture (a minimal LogSink, SetLogger, assert) — and a direct probe after SetLogger captured NOTHING: the package binary's controller-runtime root logger is already fulfilled before any test runs (a dependency's init wins the race; SetLogger is then a no-op Fulfill). The empirical record is in the test's comment. Taken instead the reviewer's alternative branch: the test renamed to what it proves (ArmedReturnsConfig) and the LINE pinned STRUCTURALLY — ArmedLineLivesInArmedPath scopes to SetupRelayStaging's FUNCTION BODY and asserts the literal sits AFTER ValidateRelayStagingStartup's success — deletion AND relocation both fail (a file-level grep catches only deletion).
- **Finding 2 (the var-derived bound): fixed** — the elapsed bound is a literal 35s (the design's 30s window + slack), decoupled from ArmingStartupGuardWindow so a var regression cannot silently loosen it; the commit-message overstatement corrected by this entry ("not 1, not a hang" holds; bounded-STALL also caught now).
- **Finding 3 (the tautology): folded** — the unit twin's comment now points at the source pin (the genuine guard-reads-the-window binding); the assertion retained only as the semantic-constant documentation.
- Also: the M4 lane (worker wt-1453) coordinated the hunk-map for the CredentialsStaged condition — relay_batch.go split agreed (their read-only accessor vs my build-flow/reason-constant/counters), BuildDegrade gets NO new field (reason-string parity), and MY SIDE OWES THE PINNED CONTRACT: the migration-mode fallback returns a non-nil BuildDegrade with Reason=relay_fallback_delivery (recorded here as an M2 requirement).
