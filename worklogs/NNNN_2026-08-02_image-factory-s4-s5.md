# Worklog NNNN — Image Factory S4-S5: POST /configs + callback + status derivation

**Date:** 2026-08-02
**Scope:** S4 (POST /configs + coalescing + dispatch-before-commit) and S5
(callback endpoint + atomic transitions). S4+S5 merged into a single PR (#619).

## Summary

Implemented the write-path vertical slice for the image factory: the
load-bearing `POST /v1/image-factory/configs` handler with coalescing and
dispatch-before-commit semantics, plus the authenticated callback endpoint
for GH Actions build results.

## Key decisions validated

1. **Dispatch-before-commit** (design #17): workflow dispatch happens BEFORE
   the config row commits. On dispatch failure, the row is never created —
   no orphaned "building" config with null gh_run_id. Validated by
   `TestIF_CreateConfig_DispatchFailureNoCommit`.
2. **Build coalescing** (design #16): before dispatching, checks for an
   existing in-flight or successful build for (hash, base_version). If found,
   links instead of dispatching. Validated by `TestIF_CreateConfig_CoalesceOnSucceeded`
   and `TestIF_CreateConfig_CoalesceOnInFlight`.
3. **Atomic transitions**: all multi-write operations (CreateConfigAndBuild,
   TransitionBuildSucceeded, TransitionBuildFailed) use single-transaction
   BeginTx/Commit. Verified at store level with sqlmock (7 tests: 4 happy + 3 rollback).
4. **Callback auth** (design #18): per-build callback_token compared via
   `subtle.ConstantTimeCompare`. Token from build A cannot mutate build B.
   Validated by `TestIF_Callback_TokenFromBuildA_DoesNotWorkOnBuildB`.
5. **Idempotent callback**: replay on a terminal build returns 204 without
   re-transitioning. Prevents double-write of known_failures.

## What was built

### S4 — POST /configs handler (`imagefactory_create.go`)
- 8-step sequence: bind→resolve→hash→blocklist-check→coalesce→dispatch→commit→respond
- `buildDispatcher` interface + `dispatchRequest` type
- `generateCallbackToken()` (crypto/rand 32 bytes hex)
- `CreateConfigAndBuild` store method (single tx for config+build)

### S5 — Callback endpoint (`imagefactory_callback.go`)
- `POST /internal/image-factory/builds/:id/callback` (token-authed, NOT JWT)
- `TransitionBuildSucceeded` / `TransitionBuildFailed` store methods (single tx)
- `buildStore` interface (ISP: different subset from imageFactoryStore)
- `failureExplainer` interface (S6 wires real LLM; S5 ships placeholder fallback)
- `ResolvedValues.Selection()` helper (recovers sorted IDs for known_failures)

## Review-driven fixes

1. Non-atomic CreateConfig+CreateBuild → `CreateConfigAndBuild` single tx
2. Non-atomic callback transitions → `TransitionBuildSucceeded/Failed` single tx
3. Dead code: `deriveBuildStatus`/`transitionBuild`/`statusResolver` removed
   (deferred to S8 when real GH Actions integration exists)
4. Value-receiver bug on CreateConfig/CreateBuild → pointer receivers
5. Auth ordering: userID checked before dispatcher nil check
6. Error-detail leakage: dispatch failure message sanitized
7. `newUUID` error no longer swallowed
8. `EmptySelection` test assertion made deterministic
9. `explainFailure` ctx/rv passthrough (contextcheck lint)
