# Worklog: #475 — scrapeRouterMetrics silent error swallowing

**Date:** 2026-07-12
**Session:** `RelayAdminHandler.scrapeRouterMetrics` silently returned a zero-valued `routerMetricsData{}` on each of 4 failure paths (request-build, HTTP transport, non-2xx response, response-read). The admin dashboard rendered zeros for `requestsToday`, `requests429Today`, `activeStreams` while looking healthy, hiding config and network errors from operators. The same anti-pattern flagged in #466 reviews as the cause of the original invisible-failure mode.
**Status:** Complete

---

## Objective

Make every failure path in `scrapeRouterMetrics` emit a Warn-level log line so operators can find the root cause without needing to read request latency as a proxy signal. Add a non-2xx response check that the original code omitted entirely.

---

## Work Completed

### `api/internal/handlers/relay_admin.go`

- Added `logger interfaces.LoggerInterface` field to `RelayAdminHandler`.
- Added `SetLogger(l interfaces.LoggerInterface)` setter, mirroring the `ModelsHandler.SetLogger` pattern. Documents the rationale (#475) and the nil-tolerance contract.
- Updated `scrapeRouterMetrics` to emit Warn on each of 4 paths:
  1. `http.NewRequestWithContext` failure (request build) — `relay router /metrics request build failed`
  2. `httpClient.Do` failure (HTTP transport) — `relay router /metrics scrape failed` (the #466 failure path)
  3. **NEW** `resp.StatusCode < 200 || >= 300` — `relay router /metrics returned non-2xx` (the original code never checked status code at all)
  4. `io.ReadAll` failure (response read) — `relay router /metrics response read failed`
  Every Warn includes the URL and the error/status for grep-friendly diagnostics.
- All 4 paths are nil-logger-safe (`if h.logger != nil`).

### `api/internal/app/app.go`

- Wired `relayAdminHandler.SetLogger(log)` after construction. Production logger is always set; the nil guard in the handler is defense-in-depth.

### `api/internal/handlers/relay_admin_test.go`

Three new tests + one new test helper (TDD: tests written first, confirmed RED against pre-fix code, then GREEN after fix):

- `relayWarnCapture` — minimal LoggerInterface implementation that records Warn and Error messages. Mirrors the `invLogCapture` pattern in `invitations_test.go`.
- `TestScrapeRouterMetrics_HTTPFailure_LogsWarn` — drives `/admin/relay/status` with a router URL pointing at port :1 (no listener); asserts a Warn was emitted and the message references the relay router or metrics scrape.
- `TestScrapeRouterMetrics_NonOKResponse_LogsWarn` — stands up a fake HTTP server returning 503; asserts Warn was emitted. This test would have failed against the pre-fix code which never inspected `resp.StatusCode`.
- `TestScrapeRouterMetrics_NilLogger_DoesNotPanic` — defense-in-depth: a nil logger MUST NOT crash the handler. Mirrors `TestInvitations_VerifyUser_NilLogger_DoesNotPanic`.

Tests seed a `relay-fleet` CR via `overrideList` so the `/status` handler doesn't early-return at the "no relays deployed" guard before reaching `scrapeRouterMetrics`.

---

## Key Decisions

1. **4 log paths, not 3.** The original issue listed 3 silent paths. Pre-fix code never inspected `resp.StatusCode` — a 503 from the router or a 404 from a wrong path silently parsed an empty body and returned zeros. Added the status-code check as part of this PR because it's the same class of bug (#475's "silent zero on dashboard") and the fix is in the same function.

2. **`Warn`, not `Error`.** The handler returns a 200 to the caller either way (the relay status endpoint is best-effort; CR-derived metrics still render). `Warn` is the right severity for "this lookup failed but the request itself succeeded." `Error` would page on-call unnecessarily.

3. **Nil-logger tolerance.** Production wiring always injects a logger. The nil guard exists because the handler is constructed inside an `if err == nil` branch in `app.go`; a future refactor that moves the construction could forget the `SetLogger` call. The nil guard prevents a crash on that path.

4. **Logger interface, not logger struct.** Matches the `ModelsHandler` pattern (existing convention). The struct field is `interfaces.LoggerInterface`, not `*logger.Logger` — keeps the handler testable with the minimal `relayWarnCapture` mock.

5. **No rate limiting on Warn.** The handler runs once per `/admin/relay/status` request (operator-initiated, low frequency). Sampled or rate-limited logging would hide the exact failures operators need to see. Unsamped Warn is correct.

---

## Assumptions stated and validated (Rule 7)

1. *The `/admin/relay/status` handler calls `scrapeRouterMetrics` unconditionally when a relay CR exists.* Validated by reading `relay_admin.go:225` — `routerMetrics := h.scrapeRouterMetrics(ctx)` is in the body of `handleStatus` after the `relays.Items[0]` retrieval, no conditional guard.
2. *The existing `ModelsHandler.SetLogger` pattern is the right injection model.* Validated by reading `models_handler.go:50,74` and `app.go:447` — same pattern (struct field + setter + app.go wiring). Consistent.
3. *`setupRelayRouter` returns a handler whose state can be modified post-construction.* Validated by reading the existing `TestRelayAdmin_*` tests, which call `overrideList` and `h.SetHTTPClient` on the returned handler. Same pattern.
4. *The non-2xx path is genuinely missing in the pre-fix code, not just unpruned.* Validated by reading lines 626-648 of the pre-fix file: `resp.StatusCode` is never read; the body is parsed regardless of status.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, fixed):** Initial test draft didn't seed a relay CR; the `/status` handler returned early at the "no relays deployed" guard (line 230) before reaching `scrapeRouterMetrics`. The Warn capture was empty because the function was never called. Phase 2 verdict: real test bug. Remediation: added `overrideList(relayMock, ...)` to seed a `relay-fleet` CR. Tests now reach the function under test.
- **Phase 2 false alarm initially considered:** "Does the new non-2xx path break the existing happy-path test that uses `setupRelayRouter` + a fake metrics server?" Validated: the existing happy-path test (if any) uses a fake server returning 200 with valid Prometheus output; the new `< 200 || >= 300` check passes 200 through unchanged. No regression. False alarm.
- **Phase 2 false alarm initially considered:** "Should the non-2xx path also be `< 200 || >= 300` or just `>= 300`?" Validated: HTTP 1xx informational responses are not real success codes for a metrics scrape; `< 200 || >= 300` is the standard "non-2xx" check. The router's `/metrics` endpoint only returns 200 on success. False alarm — the check is correct as written.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 60s -race -count=1 -run "TestScrapeRouterMetrics" -v ./api/internal/handlers/...
  pre-fix (no SetLogger method): compile failure (RED, expected)
  post-fix: 3/3 PASS

go test -timeout 60s -race -count=1 -run "TestRelayAdmin|TestScrape" ./api/internal/handlers/...
  ok — no regression in existing relay_admin tests
```

---

## Files Modified

- `api/internal/handlers/relay_admin.go` — added logger field, SetLogger setter, 4 Warn log lines + non-2xx check in scrapeRouterMetrics.
- `api/internal/app/app.go` — wired relayAdminHandler.SetLogger(log).
- `api/internal/handlers/relay_admin_test.go` — added relayWarnCapture helper + 3 new tests.

---

## Next Steps

1. Open this PR.
2. Closes #475.
