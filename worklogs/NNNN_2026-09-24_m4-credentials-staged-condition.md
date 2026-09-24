# Worklog: M4 — CredentialsStaged CRD condition (design 0061 §6)

**Date:** 2026-09-24
**Session:** Parallel lane on branch `feat/0061-m4-credentials-staged-condition` (wt-1453).
**Status:** Complete

## Objective

Design 0061 §6 (M4): a degraded relay batch surfaces on the Workspace CRD — visible to the UI and alertable — not only in API bootstrap logs. The existing `CredentialsStaged` condition (US-72.3), fed by the batch outcome: `False/<reason>` on a degrade, `True` on the next clean batch, via the existing status path (`UpdateStatus`). No new condition type, no new surface. Flag-off never writes (the W15 condition-absent contract).

## Work Completed

TDD (service writer 5 tests + handler hook 5 tests, both sides RED-first):

- `Service.ReportRelayBatchOutcome(ctx, workspaceID, degradeReason)` — the condition writer (new file `api/internal/services/workspace/credentials_staged.go`): get → upsert condition → `UpdateStatus`, under `RetryOnConflict` (the resume path's lane). Transition semantics mirror the controller's `setCondition` (health.go): same status+reason refreshes the message WITHOUT bumping `LastTransitionTime` — the API's batch writes and the controller's staging writes cannot flap the transition clock. The reason is an INPUT (builder owns the vocabulary): `relay_staging_not_ready` now, M2's `relay_fallback_delivery` later, uninterpreted.
- The pod-bootstrap hook (`api/internal/handlers/pod_bootstrap.go`, after the existing degrade Warn at the `BuildWorkspaceBatch` outcome): fires only when the sink is wired AND the injector implements `RelayOnlyEnabled() bool` returning true (the `bootstrapManifestSource` optional-seam pattern — every existing test double lacks the method, so pre-M4 behavior is byte-identical). Best-effort by design: a write failure logs and never gates the boot.
- `SecretService.RelayOnlyEnabled()` — the flag accessor (the only relay_batch.go touch; the M1/M2 lane owns the rest of that file).
- Production wiring in `app.go` (SetPromptService pattern): `podBootstrapHandler.SetRelayOutcomeSink(wsSvc)` when the concrete workspace service exists.

## Key Decisions

1. **Reason-as-input seam (coordinated with the M1/M2 lane)**: the hook keys on `degrade != nil` — non-nil degrade (any reason) → False/<reason>, nil → True. M2's migration-mode fallback must surface `relay_fallback_delivery` through the existing reason channel (non-nil `*BuildDegrade`), per the hunk-map exchange; M2's enrichment field is declined (reason-string parity suffices) so both PRs stay closed to each other.
2. **Optional seams over constructor changes**: sink via setter + flag via type assertion keeps every existing handler test double and mixed-fleet injector untouched — the W15 flag-off contract falls out of the seam's absence, not a runtime check alone.
3. **Best-effort visibility, never a boot gate**: the condition write is user-facing surface on an object the controller owns; its failure logs (Warn) and the batch still delivers — the boot path's existing contract.

## Tests Run

- Service writer: 5/5 (degrade False/reason, clean True, transition-time preservation, RetryOnConflict, missing-CR error).
- Handler hook: 5/5 (flag-on degrade writes reason, flag-on clean writes empty, flag-off never writes, legacy injector never writes, sink error still bootstraps 200).
- Mutation-verified: disabling the flag gate fails exactly the two positive arms (the three negative arms pin the no-write behavior); removing the write fails the writer assertions.
- Full suites: handlers 110s, workspace services, pkg/secrets, app — ok. gofmt clean, golangci-lint (new-from-rev) 0 issues.

## Blockers

None.

## Next Steps

- Review loop to APPROVED; M4 merges per the recorded order (M1 → M2 → M4 → gate → e2e).
- On M2's PR: verify the seam (fallback surfaces a non-nil BuildDegrade with Reason=relay_fallback_delivery); pin the real constant then if needed.

## Files Modified

- `api/internal/services/workspace/credentials_staged.go` — the condition writer (new)
- `api/internal/services/workspace/credentials_staged_test.go` — writer tests (new)
- `api/internal/handlers/pod_bootstrap.go` — the hook + the two optional seams + setter
- `api/internal/handlers/pod_bootstrap_relay_condition_test.go` — hook tests (new)
- `pkg/secrets/relay_batch.go` — `RelayOnlyEnabled` accessor only (the coordinated hunk)
- `api/internal/app/app.go` — production sink wiring
- `worklogs/NNNN_2026-09-24_m4-credentials-staged-condition.md`
