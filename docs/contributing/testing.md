# Testing

Testing is the foundation of the project's quality bar. [Rule 0 (TDD)](rules.md#rule-0-test-driven-development-tdd) is mandatory, the cover floor is enforced in CI, and the `-race` flag is always on. This page covers the workflow, patterns, helpers, and the different test tiers.

## TDD workflow

```
1. Write test
2. Run test (must fail)
3. Write minimal code to pass
4. Run test (must pass)
5. Refactor if needed
```

**Definition of done:** a task is not done until it has been demonstrated to be integrated properly via passing e2e/integration tests. "It compiles", "unit tests pass", or "it works in isolation" do not satisfy this requirement.

### Test requirements (all mandatory)

- Multiple happy path tests
- Multiple unhappy path tests (errors, invalid inputs, boundary failures, dependency failures)
- Edge case coverage
- End-to-end integration tests that exercise the real wiring (router → service → K8s/DB/Redis or fakes thereof)
- Always use `-timeout` when running tests
- Tests must pass before marking work complete

## The canonical command

```bash
# All Go tests, with race detector and timeout
go test -timeout 90s -race ./...

# Or via make
make test
```

!!! danger "Always use `-timeout` and `-race`"
    `go test ./...` without a timeout can hang forever on a stuck test. The `-race` flag is mandatory — race conditions are bugs, and the race detector catches them at test time rather than in production. Never run the suite without both flags.

## Table-driven tests

Use table-driven tests with `t.Run()` for any function with multiple input cases. This is the project's standard pattern:

```go
func TestCreateWorkspace(t *testing.T) {
    tests := []struct {
        name    string
        req     types.CreateWorkspaceRequest
        wantErr bool
    }{
        {"valid workspace", types.CreateWorkspaceRequest{Runtime: "base", Name: "test"}, false},
        {"empty name", types.CreateWorkspaceRequest{Runtime: "base", Name: ""}, true},
        {"invalid storage size", types.CreateWorkspaceRequest{Runtime: "base", Name: "test", StorageSize: "-1"}, true},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, err := svc.CreateWorkspace(ctx, tt.req)
            if (err != nil) != tt.wantErr {
                t.Errorf("CreateWorkspace() error = %v, wantErr %v", err, tt.wantErr)
            }
        })
    }
}
```

Each case is named, so failures point at the exact scenario. Add cases for happy paths, unhappy paths, and edge cases in the same table.

## Mock conventions

| Concern | Location | Tool |
|---------|----------|------|
| Service mocks | `api/internal/mocks/` | `testify/mock` |
| Shared/root mocks | `mocks/` | `testify/mock` |
| Kubernetes mocks | `pkg/interfaces/kubernetes.go` interface | `testify/mock` |
| Database tests | — | `go-sqlmock` |
| Redis tests | — | `miniredis` (in-memory Redis) |

- Service mocks live in `api/internal/mocks/` and `mocks/` (root).
- Kubernetes mocks implement the interface from `pkg/interfaces/kubernetes.go`.
- Use `testify/mock` for mock generation.
- Database tests use `go-sqlmock` — no real Postgres needed for unit tests.
- Redis tests use `miniredis` — an in-memory Redis that's fast and deterministic.

## Cover floor

The project enforces a **50% coverage floor** in CI. Coverage below 50% fails the build.

```bash
# Run tests with coverage
make cover

# Or directly
go test -timeout 90s -race -cover ./...
```

50% is a floor, not a target. Critical paths (auth, secret handling, the proxy) should be well above it. The floor exists to catch regressions in coverage, not to license low coverage elsewhere.

## Integration tests

Unit tests alone are not sufficient ([Rule 0](rules.md#rule-0-test-driven-development-tdd)). The project requires integration tests that exercise the real wiring.

### Secrets integration tests (Postgres + Redis)

The secrets subsystem has integration tests that run against real Postgres and Redis instances. These validate the encrypted store end-to-end: DEK derivation, AES-256-GCM encryption, audit logging, and reveal.

```bash
# From the api directory — requires Postgres + Redis reachable
go test -timeout 120s ./api/internal/services/database/... -run Secret
```

### Controller envtest

The controller uses [`envtest`](https://book.kubebuilder.io/reference/envtest.html) — a real Kubernetes API server (etcd + kube-apiserver) started locally for each test run. This validates reconcilers, webhooks, and CRD schemas against a real control plane without a full cluster.

```bash
cd controller
go test -timeout 120s -race ./...
```

envtest catches the failure modes unit tests miss: incorrect CRD schemas, webhook admission failures, status subresource errors, and reconciliation races.

## E2E suite

The end-to-end suite lives under `tests/` and runs against a real cluster (typically `kind`). It exercises the full path: API → controller → workspace pod → agent.

```bash
cd local

# Bootstrap the cluster first
./bootstrap.sh

# Run the e2e suite (9 tests)
LLM_BASE_URL=https://your-llm/v1 \
LLM_API_KEY=sk-... \
LLM_MODEL=default \
./test.sh
```

The `LLM_*` env vars enable the prompt round-trip and patch-part stripping checks. Without them, the suite skips the tests that require a live LLM.

## Frontend tests

The frontend (React 19 + TypeScript + Vite) has two test tiers:

### Unit tests (vitest)

```bash
cd frontend
npm test            # vitest run
npm run typecheck   # tsc --noEmit
```

Vitest is configured in `vitest.config.ts`. Tests live alongside source as `*.test.tsx` / `*.test.ts`.

### E2E tests (Playwright)

```bash
cd frontend
npx playwright test
```

Playwright specs live under `frontend/tests/e2e/`. They drive a real browser against a running frontend + API. Example: `register-turnstile.spec.ts` covers the Turnstile CAPTCHA happy path and three unhappy paths with a stubbed Cloudflare siteverify.

## Auth tests

The auth subsystem has a dedicated e2e surface because of its security sensitivity:

```bash
# Go tests covering register, login, API key CRUD, and security controls
go test -race ./api/internal/server/... \
  -run "TestRegister|TestLogin|TestCreateAPIKey|TestListAPIKeys|TestDeleteAPIKey|TestAPIKeyEndpoints"

# Shell script against a running server
./local/test-auth.sh http://localhost:8080
```

These validate: bcrypt hashing, email enumeration prevention, password-never-in-response, API key secrets stripped on list, body size limits, sanitized binding errors, and the Turnstile CAPTCHA on `/register`.

## Test helpers and fakes

| Helper | Purpose |
|--------|---------|
| `go-sqlmock` | In-process SQL mock for database unit tests |
| `miniredis` | In-memory Redis for cache/rate-limit/lockout tests |
| `envtest` | Real kube-apiserver + etcd for controller integration tests |
| `testify/mock` service mocks | `api/internal/mocks/`, `mocks/` |
| Fake IdP helpers | `api/internal/handlers/org_sso_idp_helpers_test.go` — a fake OIDC IdP with JWKS for SSO tests |

When a test depends on an external service, provide a mock or fake — never depend on the real thing in unit/integration tests (the e2e suite is the exception, by design).

## What not to do

- [ ] **No tests that don't assert anything.** A test that runs code without checking the outcome is documentation, not a test.
- [ ] **No flaky tests.** If a test passes sometimes and fails others, fix it or remove it. Timeouts, sleep-based waits, and shared mutable state without synchronization are the usual causes.
- [ ] **No hacked tests.** If a test fails, fix the root cause — never adjust the test to make it pass. See [Rule 5](rules.md#rule-5-zero-technical-debt).
- [ ] **No tests without `-timeout`.** A hung test with no timeout hangs the whole suite.
- [ ] **No unwired code.** Code that is built but never called from a live request path is dead code. Either wire it (with an integration test proving the wiring) or remove it.

## CI

CI runs the full suite on every push to `main` and on every PR:

```bash
go test -timeout 90s -race ./...
make lint
make cover   # enforces the 50% floor
```

CI also builds and pushes images to `ghcr.io/lenaxia/llmsafespaces/{api,controller,base,frontend}:dev` on every push to `main` (see `.github/workflows/ci.yml`).

## Next

- [Engineering Rules](rules.md) — Rule 0 (TDD) in full
- [Development Workflow](development.md) — running tests locally
- [Worklogs](worklogs.md) — recording what tests were run
