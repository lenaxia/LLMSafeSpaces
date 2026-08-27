# Worklog: SDK contract gates — Hurl blocking, expectedPaths removal, PACKAGES accuracy

**Date:** 2026-08-26
**Session:** P2 of epic #1032 — #1044 (Hurl blocking), #1045 (expectedPaths removal), #1049 (version/claims accuracy)
**Status:** Complete

---

## Objective

Make every SDK-contract gate actually gate: the Hurl suite was `continue-on-error`, the validator carried a second hand-maintained route list, and PACKAGES.md made false claims.

---

## Work Completed

### #1044 — Hurl suite fixed and made blocking
- **Root cause discovered**: Prism 5.x serves spec paths WITHOUT the `/api/v1` server prefix, so every request in the suite 404'd in CI — the suite has never been green; `continue-on-error` masked a 100%-red gate. Additionally hurl 5.0.1 does not support `isObject`/`isArray`/`notExists` predicates — three files never parsed.
- Verified locally: installed node 22 + prism-cli + hurl 5.0.1, booted the mock, ran the suite, iterated to green (all 10 files PASS).
- Fixes: `{{base_url}}/api/v1/…` → `{{base_url}}/…` (7 files); unsupported predicates → `exists`/`isNumber`/`isBoolean`/`count`; value-level asserts (mock-generated) → type-level (values are canary territory); error responses exercised via Prism's `Prefer: code=N`; abort status corrected to the documented 200.
- New coverage: `workflows.hurl` (Epic 64 CRUD + 202 run + cancel + boolean `enabled` — #1035 regression pin), `passkeys.hurl` (ceremony shapes + documented 400/401), history pagination params + `X-Next-Cursor` header (#1039 pin).
- CI: `continue-on-error` removed from the Hurl step, comment rewritten to document the mock semantics.

### #1045 — expectedPaths removed
- Deleted the hand-maintained `expectedPaths` list, `validateRouteCoverage`, and `TestSpec_Completeness` from `sdks/validate`. Route parity has exactly one authority now: `TestOpenAPIRouterContract` (spec ↔ router, both directions, every handler wired) — a strict superset, since it also catches spec-side orphans and method drift. Both gates run in the same blocking CI jobs (`go test ./...`, `make openapi-validate`).
- `sdks/validate` keeps structure only: required fields, security schemes, $refs, operationId uniqueness.

### #1049 — PACKAGES.md accuracy
- Removed the false "84 paths" count and the false "SDK versions match the platform version" claim; documented the actual policy (spec/SDKs semver the API surface independently of platform release versions) and named the authoritative parity gate.

---

## Key Decisions

1. **Type-level asserts in the Hurl suite** — a Prism mock generates example values; asserting specific strings tests the generator, not the contract. Value fidelity stays with the live canaries.
2. **`Prefer: code=N` for error contracts** — validates the documented error responses EXIST and match the Error schema, without pretending the mock can route 404s by ID.
3. **Local verification before flipping the gate** — installed the exact CI toolchain (prism-cli, hurl 5.0.1) locally and iterated to green rather than flipping `continue-on-error` blind and bouncing CI.

---

## Blockers

None.

---

## Tests Run

- Full Hurl suite (10 files) PASS against local Prism mock with hurl 5.0.1 (exact CI versions)
- `sdks/validate` suite green post-removal; `make openapi-validate` ✓
- `TestOpenAPIRouterContract` ✓ (unchanged, still authoritative)

---

## Next Steps

1. PR this branch (closes #1044, closes #1045, closes #1049).
2. #1047 SDK history pagination (Go/TS/Python/Java) — spec params now documented.
3. #1046 SDK MCP-server CRUD ×4.
4. Watch CI on this PR: first-ever blocking run of the Hurl suite (validated locally; CI should match since versions are pinned).

---

## Files Modified

- `.github/workflows/ci.yml` — Hurl step blocking + rewritten rationale
- `sdks/tests/contract/*.hurl` — prefix/predicate/assert fixes + new `workflows.hurl`, `passkeys.hurl`
- `sdks/validate/main.go`, `sdks/validate/spec_completeness_test.go` — expectedPaths removal
- `sdks/PACKAGES.md` — claims accuracy + versioning policy
