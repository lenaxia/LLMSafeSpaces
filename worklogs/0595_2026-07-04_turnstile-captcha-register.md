# 0595 — 2026-07-04 — Cloudflare Turnstile CAPTCHA on /register

## What

Add Cloudflare Turnstile CAPTCHA to `POST /api/v1/auth/register` as a
full-stack feature: backend middleware (`api/internal/middleware/turnstile.go`),
config wiring (`api/internal/config/config.go`), router integration
(`api/internal/server/router.go`), frontend widget component
(`frontend/src/components/auth/TurnstileWidget.tsx`), form integration
(`frontend/src/components/auth/RegisterForm.tsx`), and chart values
(`charts/llmsafespaces/values.yaml` + api/frontend deployment templates).

Feature-flagged behind `turnstile.enabled` in chart values. When
disabled, the middleware is not installed on the route at all — the
change is a strict no-op for existing deployments.

## Why

Without a CAPTCHA on the register endpoint, an attacker can spam
account creation via a script. The rate-limit at the Cloudflare edge
(from PR-less session 4 work) caps volume but not per-attempt cost —
5 req / 10 s per IP is enough for a distributed botnet to burn through
in aggregate. Turnstile forces a per-request browser-side proof of
work that raises the cost of automated signup by ~100×.

## How

### Backend

`middleware.Turnstile()` is a gin middleware that intercepts requests
before `authSvc.Register` is called. It extracts the token from the
`cf-turnstile-response` header (preferred) or `cfTurnstileResponse`
form field (form-encoded callers only — the frontend uses JSON so
header path is the only production path). Token + secret are POSTed
to Cloudflare's siteverify endpoint over HTTPS with a 5s timeout;
`success:false` OR any transport/HTTP error results in 401
`turnstile_failed` with a `reason` code for the client to react to.

Fail-closed in every branch:
  * missing token → 401 (reason=missing_token)
  * verify HTTP error / timeout → 401 (reason=verify_unavailable)
  * verify 5xx → 401 (reason=verify_unavailable)
  * verify success=false → 401 (reason=rejected)
  * config missing secret → 401 (reason=no_secret_configured)

Additionally the config layer has a startup fail-closed guard:
`config.Load` returns an error when `Turnstile.Enabled=true` and
`Turnstile.SecretKey==""`. No path allows a running server with
an enabled-but-unsecret Turnstile config.

### Frontend

`TurnstileWidget.tsx` wraps Cloudflare's client script with a
lifecycle-safe React component. The script is loaded once and cached
across mounts (module-level singleton promise). Widget state is
cleaned up on unmount via `window.turnstile.remove(widgetId)`. When
`siteKey` is empty (Turnstile disabled server-side), the component
renders `null` — no script fetch, no CDN dependency.

`RegisterForm.tsx` renders the widget when `env.turnstileSiteKey`
is non-empty. Submit button is disabled until the widget issues a
token. On backend `turnstile_failed` error, the token is cleared so
the user re-challenges before retrying.

The Cloudflare widget in "managed" mode is invisible for
well-behaved browsers (no CAPTCHA UI), interactive for bot-suspected
sessions. UX friction is minimal for real users.

### Chart

`turnstile.enabled` in values.yaml gates all wire-up:

```yaml
turnstile:
  enabled: false          # default
  siteKey: ""             # public; injected into env.json
  secretKey:
    existingSecret: llmsafespaces-credentials
    key: turnstile-secret
  verifyURL: "https://challenges.cloudflare.com/turnstile/v0/siteverify"
```

Secret is loaded via `secretKeyRef` on the API deployment — never
rendered into a ConfigMap or a static env block. Site key is public
and injected into the frontend's runtime `env.json` via
`docker-entrypoint.sh` (a new `TURNSTILE_SITE_KEY` env var).

## Assumptions + validation

- **Assumption**: Cloudflare's siteverify endpoint is stable across
  Free and paid plans. **Validation**: docs at
  https://developers.cloudflare.com/turnstile/get-started/server-side-validation/
  are versioned under v0; endpoint URL has been stable since
  Turnstile GA (2023-09).
- **Assumption**: `Bearer public`-equivalent bypass doesn't exist —
  Turnstile requires a real token per request. **Validation**:
  live probe with fake token returns 401
  `invalid-input-response` (verified on safespaces.thekao.cloud
  2026-07-04).
- **Assumption**: The 5s HTTP timeout is enough for the AWS US-West
  → Cloudflare hop under normal conditions. **Validation**: latency
  spot-check with `curl -w '%{time_total}'` shows ~150ms typical.
  5s absorbs a 30× spike.

## Deployment

Provisioning is out-of-band, once:
1. Cloudflare Turnstile → Add site (managed mode).
2. AWS Secrets Manager: create-secret `llmsafespaces/turnstile-secret`
   with the SECRET key, tagged `llmsafespaces:role=app-secret`.
3. CDK: set `llmsafespaces:turnstileSecretArn` and
   `llmsafespaces:turnstileSiteKey` in `cdk.context.json`.
4. ops-prod: `turnstile.enabled: true` in the llmsafespaces HR values;
   ExternalSecret pulls the secret into `llmsafespaces-credentials`.

Deployed to safespaces.thekao.cloud 2026-07-04. Live verification:
- No token → HTTP 401 `{"error":"turnstile_failed","reason":"missing_token"}`
- Invalid token → HTTP 401 `{"error":"turnstile_failed","reason":"rejected","detail":"invalid-input-response"}`
- `env.json` served includes `turnstileSiteKey: 0x4AAAAAADvViBYSywlB8kIb`

## Tests

- **Middleware unit** (`api/internal/middleware/turnstile_test.go`, 9 tests):
  valid-passes, missing-token-401, cloudflare-rejects-401,
  cloudflare-unreachable-fails-closed, cloudflare-5xx-fails-closed,
  form-field-fallback, header-precedence, X-Forwarded-For-remoteip,
  no-secret-fails-closed.
- **Config unit** (`api/internal/config/config_test.go`, 4 tests):
  default-disabled, enabled-with-secret-valid,
  enabled-without-secret-fails-closed, verify-url-override.
- **Router integration**
  (`api/internal/server/router_auth_turnstile_test.go`, 5 tests):
  valid-token-reaches-handler, missing-token-blocks-handler,
  invalid-token-blocks-handler, verify-server-down-fails-closed,
  disabled-skips-middleware. Guards against regressions of the
  `if turnstile.Enabled { ... }` router branch.
- **Frontend unit** (`frontend/src/components/auth/TurnstileWidget.test.tsx`,
  7 tests + `RegisterForm.test.tsx` enabled-path added, +4 tests):
  widget lifecycle, submit-blocked-until-token, token-forwarded,
  turnstile_failed-clears-token.

## Rollback

Set `turnstile.enabled: false` in ops-prod chart HR values → HR
upgrade → middleware removed from route → no code deploy needed.
Total rollback time: ~90 seconds (Flux reconcile + helm upgrade +
pod rollout).

## Non-goals in this PR

- Turnstile on `/login` — different threat model (credential-stuffing
  already gated by CF rate-limit + api/internal/services/auth lockout).
- Turnstile on other public endpoints (`/auth/lookup`,
  `/sso/domains`) — enumeration-safe by design; adding CAPTCHA
  friction would degrade legitimate login-discovery UX with no
  clear benefit.

## Session context

Originally implemented + direct-pushed to main 2026-07-04 (bypassing
PR review). Reverted from main and reopened as PR #501 for proper
review. The automated PR reviewer (github-actions) flagged four
required gaps in the initial version, all addressed in the follow-up
commit on this branch:
1. Router integration tests (proving the middleware is wired to /register).
2. TurnstileWidget unit tests.
3. RegisterForm enabled-path unit tests.
4. This worklog entry.

Non-blocking review points also addressed:
- `containsAny` helper replaced with `strings.Contains`.
- `clientIP` docstring clarified (deliberately bypasses gin's
  TrustedProxies model because the extracted IP is only used as a
  Cloudflare fraud-scoring hint, not for access control).
- `TurnstileRouterConfig` docstring strengthened to explain why it's
  separate from `middleware.TurnstileConfig` (isolates the test-only
  `HTTPClient` field).
- `extractTurnstileToken` form-field fallback comment clarified —
  only works for form-encoded bodies, header path is the JSON caller
  contract.
