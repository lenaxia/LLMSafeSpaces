# Worklog: CORS hardening — wildcard+credentials fail-closed

**Date:** 2026-07-11
**Session:** Refuse the unsafe CORS combo at config load. Third of the network hardening sweep targeted at v0.3.0.
**Status:** Complete

---

## Objective

`api/internal/middleware/security.go:160-176` happily emits `Access-Control-Allow-Origin: <Origin>` and `Access-Control-Allow-Credentials: true` together when the operator configures `AllowedOrigins=["*"]` and `AllowCredentials=true`. That combination is forbidden by the CORS spec (Fetch §3.2.1) because it would let any website read authenticated responses from this API in a victim's browser. Browsers reject the combo client-side, but relying on browser enforcement is not a security posture — a misconfigured deploy would silently produce broken CORS responses with no server-side signal.

Goal: refuse the unsafe combo at boot, fail-closed, with an actionable error.

---

## Work Completed

### TDD: failing tests first (`api/internal/config/security_validate_test.go`)

- `TestValidateSecurity_RejectsWildcardWithCredentials` — primary case.
- `TestValidateSecurity_AllowsWildcardWithoutCredentials` — wildcard alone is fine.
- `TestValidateSecurity_AllowsExplicitOriginsWithCredentials` — normal authenticated-deploy shape.
- `TestValidateSecurity_AllowsEmptyConfig` — chart default.
- `TestValidateSecurity_WildcardAmongOtherOriginsAlsoRejected` — `["*", "https://app.example.com"]` still triggers the guard.
- `TestLoad_FailClosedOnWildcardCredentialsCombo` — e2e through `Load(tmpfile)`.

Verified red before implementing.

### Implementation

- `api/internal/config/config.go`:
  - New `validateSecurity(*Config) error` helper, invoked from `Load` after the existing Turnstile guard. Same pattern as `applyTurnstileEnv`.
  - New sentinel `errCORSWildcardWithCredentials` with an actionable message naming the conflicting keys.
- `charts/llmsafespaces/values.yaml`: expanded the `security:` comment block to warn operators about the fail-closed guard and link the spec rationale.

---

## Key Decisions

1. **Fail at boot, not at request time.** Mirrors the Turnstile fail-closed guard pattern. A boot-time error is loud, unambiguous, and surfaces the misconfiguration immediately rather than producing silently-broken CORS responses in production.
2. **Don't try to "fix" the config silently.** Stripping the wildcard or forcing credentials off would hide a real operator intent mismatch. Refuse and explain.
3. **Sentinel error, not `fmt.Errorf` inline.** Matches `errRedirectBaseURLNotSet` and friends in the SSO service; testable via `errors.Is` if a caller wants to distinguish.

---

## Assumptions stated and validated (Rule 7)

1. *The single `config.Load` caller propagates the error correctly.* Validated by reading `api/cmd/api/main.go:24-28` — `os.Exit(1)` with `fmt.Fprintf(os.Stderr, …)`.
2. *Both file and env-var config paths reach `validateSecurity`.* Validated by reading `config.go:282-285` — env overrides are applied before the `Load` returns, and `validateSecurity` runs after. Both paths covered.
3. *The chart default (`allowCredentials: false`) means default deploys are unaffected.* Validated by reading `charts/llmsafespaces/values.yaml:117` and `DefaultSecurityConfig()` in `security.go:66`.
4. *No existing test or production config sets both `*` and credentials=true.* Validated by grepping `values.yaml`, `config.yaml`, and tests — `AllowCredentials: true` only appears in tests with explicit origin lists.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s -run "TestValidateSecurity|TestLoad_FailClosedOnWildcardCredentialsCombo" ./api/internal/config/
→ ok  github.com/lenaxia/llmsafespaces/api/internal/config  0.005s

go test -timeout 120s -short ./api/internal/config/... ./api/internal/middleware/...
→ ok  ./api/internal/config        0.013s
→ ok  ./api/internal/middleware    0.064s
→ ok  ./api/internal/middleware/tests  0.083s

go build ./...   → clean
gofmt -l <changed>  → clean
goimports -l <changed .go files>  → clean
```

---

## Next Steps

1. Open this PR for review.
2. After approval + merge: NetworkPolicy CGNAT drift, runtimeClass webhook gate, JWT iss/aud, doc reconciliation, v0.3.0 release.

---

## Files Modified

- `api/internal/config/config.go` (new `validateSecurity`, new sentinel error, wired into `Load`)
- `api/internal/config/security_validate_test.go` (new file — TDD test battery)
- `charts/llmsafespaces/values.yaml` (expanded `security:` comment block to document the guard)
- `worklogs/NNNN_2026-07-11_cors-wildcard-credentials-guard.md` (this entry)
