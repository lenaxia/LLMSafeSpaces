# Worklog: Image Factory dispatch error logging + outage root cause

**Date:** 2026-08-03
**Session:** Diagnosed the production 503 on POST /image-factory/configs, identified the true root cause (GitHub App missing Actions:Write), and landed the observability fix.
**Status:** Complete

---

## Objective

Find why `POST /api/v1/image-factory/configs` returns `503 "failed to dispatch build"` in production (v0.8.2), and prevent the diagnostic gap from recurring.

---

## Work Completed

### Root-cause investigation (live cluster + GitHub API)

- Fast-forwarded local from stale `deb10648` to `704471b7` (v0.8.2) — confirmed all image-factory files exist (admin, dispatcher, explainer, workflow, frontend).
- Verified cluster state: pod runs `api:0.8.2`, ConfigMap `ghDispatcher` present (`owner=lenaxia repo=LLMSafeSpaces workflowId=image-build.yml`), env vars `LLMSAFESPACES_IMAGE_FACTORY_APP_ID`/`_APP_PRIVATE_KEY` mounted from `image-factory-app-credentials` secret.
- Found the live 503 in pod logs at `15:30:07` — **636 ms** duration (proving `Dispatch()` actually ran, ruling out nil dispatcher).
- Extracted App credentials from K8s secret, walked the full GitHub App auth flow out-of-band: JWT mint ✓ → installation `150863674` ✓ → token `201` ✓ → `GET workflow` `200` (active) → `POST workflow_dispatch` → **`403 "Resource not accessible by integration"`**.
- Installation permissions: `{"actions":"read","contents":"read","metadata":"read","packages":"write"}` — `actions` is read-only; needs read+write.

### Code fix — Fix B (observability)

- `imagefactory.go` — added `logger` field + `SetLogger()` + `HasLogger() bool` (mirrors `pod_bootstrap.go` precedent for swallowed-error gaps).
- `imagefactory_create.go` — dispatch-failure path logs the wrapped error via `h.logger.Error(...)` before returning 503 (nil-guarded for tests).
- `app.go` — wires `imageFactoryHandler.SetLogger(log)`.
- Handler regression test: `TestIF_CreateConfig_DispatchFailureLogsError` (asserts wrapped error reaches logger via `assert.ErrorIs`).
- App-level wiring guard: `TestImageFactoryHandler_LoggerWired` (mirrors `TestPodBootstrapHandler_LoggerWired`).
- `captureIFLogger` test mock implements all 7 `LoggerInterface` methods.
- Refactored `newIFRouterWithDispatcher` to delegate to shared `newIFRouterForHandler`.

### Iteration on CI/review feedback

- **Lint failure:** `behaviour` → `behavior` (US English, repo convention).
- **Bot review REQUEST CHANGES:** added `HasLogger()`, app-level wiring test, worklog.
- **Bot re-review:** APPROVE.
- **SDK canary (S-RATE-LIMIT):** fails on this PR and consistently on main (7/10 recent runs) — non-blocking, timing-sensitive, unrelated to image-factory code.

---

## Key Decisions

1. **Follow `pod_bootstrap.go` precedent (not `admin_session.go`)** — The bot review corrected the initial convention citation: `admin_session.go`/`agent_roles.go` inject logger via constructor, while `pod_bootstrap.go` uses `SetLogger` + `HasLogger` + app-level wiring test. The latter is the right pattern for this bug class (handler constructed without logger, wired separately in app.go).
2. **Nil-guard the logger** — Preserves existing tests (`TestIF_CreateConfig_DispatchFailureNoCommit`) that construct the handler without `SetLogger`. The trade-off (silent no-op if unwired) is mitigated by `HasLogger()` + the wiring test.
3. **Fix A (permission grant) is out of scope** — It's a GitHub UI action requiring a repo admin; can't be done in code. Documented in the handoff doc.

---

## Blockers

None for this PR. **Production is still broken** until Fix A is applied: GitHub App `llmsafespaces-builder` settings → Actions = Read and write → `kubectl -n llmsafespaces rollout restart deploy/llmsafespaces-api`.

---

## Tests Run

- `go build ./...` — pass
- `go vet ./internal/handlers/... ./internal/app/...` — pass
- `gofmt -l` on all changed files — clean
- `go test ./internal/handlers/ -run 'IF_|ImageFactory'` — pass (incl. new regression)
- `go test ./internal/app/ -run TestImageFactoryHandler_LoggerWired` — pass
- CI: Lint ✓, Test (full + race) ✓, Test (-short + coverage) ✓, all builds ✓, SDK Contract ✓
- CI: SDK Integration canary ✗ (pre-existing flake on main, non-blocking)

---

## Next Steps

1. **Apply Fix A** (production): GitHub App `llmsafespaces-builder` → Repository permissions → Actions = Read and write. Then `kubectl -n llmsafespaces rollout restart deploy/llmsafespaces-api`.
2. **Merge PR #639** (squash).
3. **Tag v0.8.3** and bump `talos-ops-prod` Helm release so the logging fix reaches prod.
4. **Verify end-to-end:** retry create-and-build from the UI → expect `201` → `building` → callback → `ready`.

---

## Files Modified

- `api/internal/handlers/imagefactory.go` — `logger` field, `SetLogger`, `HasLogger`, import
- `api/internal/handlers/imagefactory_create.go` — log dispatch error before 503
- `api/internal/handlers/imagefactory_create_test.go` — `captureIFLogger`, `TestIF_CreateConfig_DispatchFailureLogsError`, router helper refactor
- `api/internal/app/app.go` — wire `SetLogger(log)`
- `api/internal/app/secrets_wiring_test.go` — `TestImageFactoryHandler_LoggerWired`
- `design/0048_2026-08-03_image-factory-handoff.md` — handoff doc (root-cause investigation, verified state, Fix A/B)
- `worklogs/NNNN_2026-08-03_image-factory-dispatch-logging.md` — this worklog
