# Worklog: #465 — Redis TLS support

**Date:** 2026-07-12
**Session:** The API's Redis client config had no TLS field. Operators deploying against AWS ElastiCache with TransitEncryptionEnabled, GCP Memorystore with TLS, or any self-hosted Redis with TLS got `context deadline exceeded` because the client connected in plaintext to a TLS-speaking server. The H3 note in `values.yaml` already acknowledged plaintext caching was a security regression; this PR closes the gap.
**Status:** Complete

---

## Objective

Add `tls` and `insecureSkipVerify` fields to the Redis config (Go + Helm chart), wire them into the cache client's `redis.Options`, and document the operator path. No new dependencies — `crypto/tls` is stdlib and `go-redis` already accepts `TLSConfig`.

---

## Work Completed

### Config (`api/internal/config/config.go`)

- Added `TLS bool`, `InsecureSkipVerify bool` fields to the `Redis` struct.
- Added `isTruthy(s string) bool` helper — parses common boolean string representations ("1", "true", "yes", "on", case-insensitive) for env-var toggles. Viper's `AutomaticEnv` doesn't auto-coerce env strings to bool; the helper makes `LLMSAFESPACES_REDIS_TLS=true` work.
- Wired `LLMSAFESPACES_REDIS_TLS` and `LLMSAFESPACES_REDIS_INSECURE_SKIP_VERIFY` env-var overrides in `applyEnvOverrides`.

### Cache client (`api/internal/services/cache/cache.go`)

- When `cfg.Redis.TLS` is true, sets `opts.TLSConfig = &tls.Config{ServerName: cfg.Redis.Host, InsecureSkipVerify: cfg.Redis.InsecureSkipVerify, MinVersion: tls.VersionTLS12}`.
- TLS 1.2 minimum (explicit for clarity; matches go-redis default).

### Helm chart

- `helm/values.yaml`: added `tls: false` and `insecureSkipVerify: false` to the `redis:` block, with comment block explaining when to enable each.
- `helm/templates/configmap-api.yaml`: renders `tls:` and `insecureSkipVerify:` lines into the API config.

### Tests (TDD)

- `TestNewCache_TLS_ConnectsAndPings` (cache package): stands up a TLS-enabled miniredis via `miniredis.RunTLS`, points the cache service at it with `cfg.Redis.TLS=true`, asserts ping succeeds, does a SET+GET round-trip. Pre-fix: connection timed out (plaintext client to TLS server).
- `TestNewCache_TLS_DisabledFallsBackToPlaintext` (cache package): regression guard. Default `cfg.Redis.TLS=false` must still produce a plaintext client.
- `TestIsTruthy` (config package): 17 table cases covering accepted truthy/falsy values.
- `selfSignedCert()` helper: generates a runtime RSA-2048 self-signed cert for the test server (avoids embedding PEM bytes that can break on copy-paste).

### Docs

- `docs/reference/helm-values.md`: added `redis.tls` and `redis.insecureSkipVerify` rows.

---

## Key Decisions

1. **Two fields (`tls` + `insecureSkipVerify`), not a `url` field.** The issue suggested an optional `rediss://` URL shorthand. Considered and rejected — the chart already uses discrete `host/port/password/db` fields; adding a URL that overrides them creates two sources of truth. The two-field approach is the minimal change that solves the actual problem (ElastiCache TLS) without restructuring the config.

2. **`ServerName` set from `cfg.Redis.Host`.** This is what makes SNI and cert validation work. The operator sets `redis.host` to the ElastiCache endpoint; the client uses that hostname for both DNS resolution and TLS SNI/cert-verification. No separate `tls.serverName` field needed.

3. **`insecureSkipVerify` exposed.** It's a footgun (man-in-the-middle), but the alternative is requiring every operator with a self-signed cert to either (a) fork the chart, (b) inject a CA into every API pod, or (c) deploy a stunnel sidecar. For homelab/dev it's the right escape hatch. The comment block says "DEV ONLY" three times. Production should use CA-signed certs.

4. **`isTruthy` in config, not cache.** The function lives in `config` because that's where env-var parsing happens. Cache package consumes the parsed bool. The function is unexported (test-only visibility via same-package test in `config_test.go`).

5. **Runtime cert generation in tests, not embedded PEM.** Initial draft embedded PEM constants; they had a copy-paste truncation issue (the word "ferrari" appeared mid-key from an earlier autocomplete). Switched to runtime generation via `crypto/rsa` + `crypto/x509` — slower (RSA-2048 keygen is ~30ms) but always parses.

---

## Assumptions stated and validated (Rule 7)

1. *`go-redis` v8 accepts `TLSConfig` in `redis.Options`.* Validated by reading the `redis.Options` struct in the vendored `go-redis/v8` package — `TLSConfig *tls.Config` field is present.
2. *`miniredis` v2 supports TLS via `RunTLS`/`StartTLS`.* Validated by reading `miniredis.go:130,180,199` in the vendored module — `RunTLS(cfg *tls.Config)` and `StartTLS(cfg *tls.Config)` both exist.
3. *The chart's configmap render format is YAML-bool-compatible (`true`/`false` literals).* Validated by rendering the chart and confirming `tls: false` and `tls: true` parse as YAML booleans (Viper reads them correctly).
4. *No existing Redis env-var binding for TLS exists.* Validated by `grep "LLMSAFESPACES_REDIS" api/internal/config/config.go` — only `_PASSWORD` was bound pre-fix.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, fixed):** Initial test draft used `tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))` with embedded PEM constants. The constants had a copy-paste error ("ferrari" mid-key). Phase 2 verdict: real test-infra bug. Remediation: replaced with runtime cert generation via `selfSignedCert()`. Slower but always parses.
- **Phase 1 finding (real, fixed):** `TestIsTruthy` was originally in the cache package, but `isTruthy` is unexported in `config`. Cache package couldn't compile (undefined symbol). Phase 2 verdict: real compile error. Remediation: moved `TestIsTruthy` to `config_test.go` (same-package access).
- **Phase 2 false alarm initially considered:** "Does TLS 1.2 minimum break any current ElastiCache deployments?" Validated: ElastiCache supports TLS 1.2+ on all current node types; the previous implicit minimum was also 1.2 (go-redis default). False alarm.
- **Phase 2 false alarm initially considered:** "Should the env-var path `LLMSAFESPACES_REDIS_TLS` also be wired into the api-deployment template for parity with `LLMSAFESPACES_REDIS_PASSWORD`?" Validated: the password env var exists because it's a secret (must not be in configmap). TLS is not a secret — it renders fine in the configmap. Adding an env var for TLS would duplicate the value (configmap + env); Viper's merge order is configfile < env, so the env would win, but it's unnecessary complexity. False alarm — configmap-only is correct.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 30s -race -count=1 -run "TestNewCache_TLS|TestIsTruthy" -v \
  ./api/internal/services/cache/... ./api/internal/config/...
  post-fix: 4/4 PASS (TLS connect, TLS disabled regression, isTruthy 17 cases)

go test -timeout 30s -race -count=1 ./api/internal/services/cache/...
  ok — no regression in existing cache tests

go test -timeout 80s -race -count=1 ./helm/...
  ok — chart tests green (configmap renders TLS fields correctly)
```

---

## Files Modified

- `api/internal/config/config.go` — added TLS + InsecureSkipVerify fields; added isTruthy helper; added LLMSAFESPACES_REDIS_TLS/INSECURE_SKIP_VERIFY env bindings.
- `api/internal/services/cache/cache.go` — TLSConfig on redis.Options when cfg.Redis.TLS=true.
- `api/internal/services/cache/cache_test.go` — TestNewCache_TLS_ConnectsAndPings, TestNewCache_TLS_DisabledFallsBackToPlaintext, selfSignedCert helper.
- `api/internal/config/config_test.go` — TestIsTruthy (17 table cases).
- `helm/values.yaml` — tls + insecureSkipVerify fields under redis:.
- `helm/templates/configmap-api.yaml` — render tls + insecureSkipVerify lines.
- `docs/reference/helm-values.md` — added redis.tls + redis.insecureSkipVerify rows.

---

## Next Steps

1. Open this PR.
2. Closes #465.
