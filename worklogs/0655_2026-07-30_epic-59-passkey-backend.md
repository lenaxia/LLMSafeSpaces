# Worklog: Epic 59 — Passkey-only login (backend ceremony + handlers)

**Date:** 2026-07-30
**Status:** Implementation — ready for review (draft PR #605)
**Depends on:** Epic 58 (merged #604 — server-KEK DEK foundation)

## What shipped

**Schema** — migrations 000009 (`user_passkeys`), 000010 (`user_recovery_codes`), 000011 (`dek_source` + 'passkey' value).

**Ceremony service** (`api/internal/services/passkey/service.go`) — wraps `go-webauthn/webauthn` v0.17.4:
- BeginRegistration/FinishRegistration: attestation verification + credential persistence + recovery-code generation (10 × 20-char, bcrypt cost 12, unambiguous alphabet).
- BeginLogin/FinishLogin: assertion verification + sign-count update.
- ConsumeRecoveryCode: bcrypt constant-time validation, single-use consumption.
- Challenge store: crypto-random token, single-use (delete-on-consume), 5-min TTL. CacheSessionStore adapts the existing Redis CacheService.

**HTTP handlers** (`api/internal/handlers/passkey.go`):
- POST /api/v1/auth/passkey/register/{begin,finish} — new user signup: create user (unusable bcrypt hash), provision dek_source='passkey' DEK, return session token + recovery codes.
- POST /api/v1/auth/passkey/login/{begin,finish} — assertion verification, issue session token via auth.Service.IssueTokenAndUnlockDEK.

**Router/app.go wiring** — PasskeyHandler on RouterConfig; routes under /api/v1/auth/passkey/. Feature-gated on `cfg.Passkey.RPID` non-empty (nil handler = routes not registered).

**Encryption-tier wiring** — `dek_source='passkey'` shares Epic 58's server-KEK tier (same provider, same unwrap path). `dekSourceIsServerWrapped()` treats both identically. `InitializeUserKeysServerKEK` parameterized by dekSource.

**Tests** — 14 TDD unit tests: constructor validation, recovery-code generation (distinctness, length, alphabet, bcrypt verification), challenge generation + single-use lifecycle, BeginLogin error paths (user not found, no passkey), ConsumeRecoveryCode (valid/invalid/not-found), SessionStore round-trip.

## What remains for Phase 2 (separate PR)

- Frontend: register page defaults to passkey, "Use password" explicit opt-in; login page passkey-first.
- Browser E2E tests (Playwright) for the full register→login→secret-store ceremony.
- Helm chart values for passkey.RPID/RPOrigins.
- `/auth/config` to advertise passkey support to the frontend.

## Verification

- `go build ./...`, `go vet ./...`, `gofmt`, `goimports` clean.
- `go test ./api/internal/services/passkey/` — 14 tests PASS.
- `go test ./pkg/secrets/`, `./api/internal/services/{auth,sso}/` — PASS (Epic 58 regression).
