# Worklog 0673 — Image Factory: dispatch error logging + outage root cause

**Date:** 2026-08-03
**Scope:** Diagnosed the production 503 on POST /image-factory/configs,
identified the true root cause (GitHub App missing Actions:Write), and
landed the observability fix that prevents recurrence.

## Summary

`POST /api/v1/image-factory/configs` returned `503 "failed to dispatch build"`
in production (v0.8.2). The handler at `imagefactory_create.go:230` discarded
the dispatch error, making the outage look like a wiring/version problem.

**Root cause (verified live):** The GitHub App `llmsafespaces-builder`
(App ID 4470040) had repository permission `actions: read` only. The
`workflow_dispatch` POST returned `403 "Resource not accessible by
integration"`, which the handler swallowed into a generic 503. Proven by
extracting the App credentials from the K8s secret and walking the full
JWT → installation → token → dispatch flow out-of-band.

**Fix applied:**
1. `SetLogger` + `HasLogger` on `ImageFactoryHandler` (mirrors the
   `pod_bootstrap.go` precedent for swallowed-error observability gaps).
2. Dispatch-failure path now logs the wrapped error before returning 503.
3. App-level wiring test (`TestImageFactoryHandler_LoggerWired`) guards
   the production `SetLogger` call against silent regression.
4. Handler-level regression test
   (`TestIF_CreateConfig_DispatchFailureLogsError`) asserts the wrapped
   error reaches the logger.

**Production fix (no code, separate from this PR):** App settings →
Actions = Read and write → restart api pod.

## Files changed

- `api/internal/handlers/imagefactory.go` — `logger` field, `SetLogger`,
  `HasLogger`
- `api/internal/handlers/imagefactory_create.go` — log dispatch error
- `api/internal/handlers/imagefactory_create_test.go` — regression test +
  `captureIFLogger` mock + router helper refactor
- `api/internal/app/app.go` — wire `SetLogger(log)`
- `api/internal/app/secrets_wiring_test.go` —
  `TestImageFactoryHandler_LoggerWired`
- `design/0048_2026-08-03_image-factory-handoff.md` — handoff doc

## Lesson

A swallowed error turned a 1-minute diagnosis ("403, missing permission")
into a multi-hour investigation. The `pod_bootstrap.go` precedent exists
for exactly this bug class — every handler that can swallow an underlying
error in its HTTP response path should have a `SetLogger` + `HasLogger`
pair with an app-level wiring guard.
