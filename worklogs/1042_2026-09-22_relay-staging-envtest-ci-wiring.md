# Worklog: US-72.5 unit 0 — relay staging envtest matrix wired into CI + stale-RV leg repair

**Date:** 2026-09-22
**Session:** Execute the #820 plan addendum (issue comment 5768990194) as US-72.5's first PR: `TestEnvtestRelayStaging_ConditionsMatrix` (shipped in #1448) was never wired into `.github/workflows/envtest.yml` and was red the first time it actually ran against a real API server. Wire it, drive it green, record the corrected root-cause mechanism and the disposition of the remaining unwired suites. The default flip must not ride on a controller test matrix CI has never executed.
**Status:** Complete — PR #1531

---

## Objective

Wire the US-72.3 relay staging conditions matrix (`-run TestEnvtestRelayStaging`) into the envtest workflow, repair the failure it surfaced on first execution, and record why the failure had never been caught anywhere before the wiring.

## Work Completed

- `.github/workflows/envtest.yml`: new step `Run envtest relay staging conditions matrix` (`go test -timeout 120s -race -tags envtest ./controller/internal/workspace/ -run TestEnvtestRelayStaging -v`), placed after the US-72.4 relayRevision step. The workflow now executes five suites total.
- `controller/internal/workspace/staging_envtest_test.go`: the leg-3 pub-restore repair — re-get the Secret, then restore the payload on the fresh object.
- Verified the #1529 shared-fixture repair (`makeRelayWorkspace` gaining `spec.storage.size: 10Gi`) present on this base; no change needed.

## Key Decisions

- **Root cause of the red run (corrected per review r2 — the first recording of this was WRONG):** the conditions-matrix leg 2 (`Update` of a shape-invalid pub Secret) bumps the API server's `resourceVersion`; leg 3 then restored the pub from the ORIGINAL in-memory `pubSec` captured before leg 2 — a stale-RV write the real apiserver rejects (`Operation cannot be fulfilled on secrets "llm-relay-hpke-pub": the object has been modified`). Fix: re-get-then-restore. **Why it never surfaced before (the corrected mechanism):** `staging_envtest_test.go` carries `//go:build envtest`, so the file never compiled into ANY run other than an envtest-tagged one — and no CI workflow executed this suite (the envtest workflow pinned only its then-four steps; the `pkg/repolint` envtest-wiring guard is satisfied at PACKAGE granularity, so the unwired test function was invisible to it). The earlier claim that "the fake client does not enforce optimistic concurrency" is FALSE for the pinned controller-runtime (v0.20.3): the fake client stamps `ResourceVersion="1"` on Create (pkg/client/fake/client.go:331), allows unconditional update only when RV is empty (:476, :1304-1316), and returns `NewConflict` on any RV mismatch (:488-492) — the exact leg sequence reproduces the Conflict on the stock fake builder. The suite simply never ran anywhere: not under the fake client (build tag excludes the file), not under envtest (never wired).
- **Follow-up disposition (out of scope here — tracked):** three more envtest suites in `controller/internal/workspace` are likewise unwired — `TestEnvtestPlatformInit_*`, `TestEnvtestRecoveryExhausted_*`, `TestEnvtestAgentdSidecar_*`/`TestEnvtestUS4B_*` — same root-cause CLASS (build-tagged files CI never executes; the package-granular wiring guard cannot see them). Filed on #820 as an owner-decision item (per-function guard extension vs wire+green): https://github.com/lenaxia/LLMSafeSpaces/issues/820#issuecomment-5769972896. Not implemented here — the addendum's scope was the relay staging matrix only.

## Blockers

None.

## Tests Run

- `KUBEBUILDER_ASSETS=$(setup-envtest use 1.31.x -p path) go test -count=1 -tags envtest ./controller/internal/workspace/ -run TestEnvtestRelayStaging -v` (real envtest assets 1.31.0) — red before the fix (stale-RV Conflict at staging_envtest_test.go:108), PASS after (6.6s).
- Same command with `-race`, plus `-run 'TestEnvtestRelayStaging|TestEnvtestRelayRevision|TestEnvtestAgentdPins'` — ok (29.7s).
- `go test -count=1 ./controller/internal/workspace/` (full untagged suite) — ok (68s).
- `go test -count=1 ./pkg/repolint/` — ok (envtest-wiring guard re-validated against the workflow edit).
- `go vet` (+`-tags envtest`) and `gofmt -l` on the package — clean.

## Next Steps

- US-72.5 story PR (runbook + rollback drill + canary rehearsal) rides on this merge; the default flip (unit C) is gated on the first green nightly drill run.
- The three still-unwired suites: owner decision on #820 (guard extension vs wire+green).

## Files Modified

`.github/workflows/envtest.yml`, `controller/internal/workspace/staging_envtest_test.go`, this worklog.
