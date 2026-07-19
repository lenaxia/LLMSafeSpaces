# Worklog: SendPromptAsync redirects to queue when non-empty (backend FIFO fix)

**Date:** 2026-07-19
**Session:** Close the residual race window left by the frontend fix in PR #563. The frontend check (`queue.queuedMessages.length > 0` in `handleSend`) covers the common case but cannot catch the window where the client's queue view is stale (the `refreshQueue` poll hasn't landed yet).
**Status:** Complete

---

## Objective

Add a server-side guard in `SendPromptAsync` that checks `queueSvc.Len()` and redirects to `Enqueue` when non-empty. The authoritative source of queue state is Redis; only the API can read it. The frontend fix in PR #563 closes most of the race but cannot close all of it because the client polls `refreshQueue` on a SSE-event-triggered cadence, not on every send.

---

## Background: the residual race window

PR #563 (frontend-only fix, merged 2026-07-19) added `queue.queuedMessages.length > 0` to `handleSend`'s queue-or-send decision. This closes the common case where the user clicks send during the busy→idle transition and the queue pill is still visible.

But the client's queue view is **eventually consistent**:

1. Session transitions busy→idle.
2. SSE event arrives → frontend calls `reconcileOnIdle` → `refreshQueue` polls GET /queue.
3. During the network RTT for step 2, the client's `queue.queuedMessages` is empty even though Redis still has pending messages.
4. User clicks send → `handleSend` sees `queue.queuedMessages.length === 0` → routes to `doSendNow` (direct POST /prompt).
5. Server receives the direct POST, marks session busy, forwards to opencode.
6. The drain goroutine that was spawned by step 1 dequeues the queued message and tries to forward it; depending on timing, either:
   - The drain wins (409 → requeue), the direct send's reply gets persisted, the drained message gets persisted later → drain message has LATER `info.time.created` → renders as "most recent" on reload.
   - The direct send wins (drain hits 409), same outcome.

Either way the user observes the drained message rendering AFTER the direct send on reload, even though they typed it first.

---

## Work Completed

### `api/internal/handlers/proxy_handlers.go`

Added the queue-length check in `SendPromptAsync` after the active-session guard:

```go
if h.queueSvc != nil {
    n, err := h.queueSvc.Len(c.Request.Context(), wid, sid)
    if err != nil {
        h.logger.Warn("SendPromptAsync: queue Len check failed; proceeding with direct send",
            "error", err.Error(), "workspaceID", wid, "sessionID", sid)
    } else if n > 0 {
        h.redirectPromptToQueue(c, wid, sid)
        return
    }
}
```

**Fail-open policy:** if `Len()` errors (Redis transient), the request proceeds with a direct send and a warning log. Rejecting legitimate traffic for a queue-length probe failure would be worse than a possible FIFO miss.

### `redirectPromptToQueue` (new method)

Reads the prompt body, extracts text from all text parts, enqueues with the same validation as `EnqueueMessage`, publishes the `queue.update/enqueued` SSE event, fires `drainQueuedMessage` immediately (so the redirected message does not wait for the next idle SSE event — the session is already idle), and returns the same 202 + `messageID` response shape as `EnqueueMessage`.

**Body cap:** wraps `c.Request.Body` with `http.MaxBytesReader(c.Writer, c.Request.Body, 100_000+1024)` before `io.ReadAll`. Mirrors the pattern at `proxy.go:275`. Without this cap a client could force the API to allocate an arbitrarily large buffer before the 100KB text check rejects it — a trivial DoS vector.

### `extractPromptText` (new helper)

Parses the prompt body shape `{parts: [{type: "text", text: "..."}, ...]}` and concatenates all text parts. Tool parts are dropped (no analog in the queue). Returns an error only on invalid JSON; empty text is returned as `""` so the caller can apply its own empty-check policy.

---

## Key Decisions

1. **Server-side guard, not client-side only.** The client's queue view is eventually-consistent; only the API can read Redis authoritatively. The backend guard is the only way to catch the residual window with certainty.

2. **Fail-open on `Len()` error.** Rejecting a direct send because we couldn't check the queue length would convert every Redis hiccup into user-visible breakage. The worst-case outcome of fail-open is a possible FIFO miss (the original bug), not a hard outage. The frontend check still catches the common case.

3. **Cap body before reading.** The 100KB text limit check runs AFTER `io.ReadAll`. Without a `MaxBytesReader` cap, a malicious client could force the API to allocate gigabytes before the limit check rejects. The cap is 100KB + 1KB slack for JSON overhead.

4. **Reuse the opencode prompt body shape, don't introduce a new one.** The redirect path reads the SAME body the client would have sent to `/prompt` (`{parts: [{type: "text", text: "..."}]}`). No new wire format introduced; if the client ever sends a richer prompt (multiple parts, future types), the redirect path degrades gracefully (extracts whatever text it can, drops the rest).

5. **Fire `drainQueuedMessage` from the redirect path, not just from `EnqueueMessage`.** Without this the redirected message would sit in Redis until the next idle SSE event (which will not come — the session is already idle). Mirrors `EnqueueMessage`'s idle-drain behavior at `proxy_handlers.go:851-853`.

6. **Did not unit-test the `Enqueue` failure path directly.** Forcing `queueSvc.Enqueue` to error requires either a closed Redis connection or a mock interface — both invasive for a single error-return branch. The branch is one line (`return`) with a generic 500 response; if it ever does something more interesting, a test can be added then. Fail-open behaviour on `Len()` errors IS tested (TestSendPromptAsync_FailOpenWhenLenErrors).

---

## Verification

### Tests added (17 new test cases)

In `api/internal/handlers/proxy_queue_test.go`:

1. **`TestSendPromptAsync_RedirectsToQueueWhenNonEmpty`** — the main regression test. Pre-populates queue with message A, marks session idle, calls SendPromptAsync with message B. Asserts:
   - Response is 202 (not 200 from `proxyToWorkspace`).
   - `messageID` present in response body.
   - **FIFO ordering:** captures prompt_async body order, asserts `["message A", "message B"]` — A reaches opencode BEFORE B. This is the actual user-visible invariant the fix is supposed to preserve; the prior draft only asserted `Len >= 1` which would have passed even if B were dequeued first.
   - Verified RED without the fix: returns 200, calls prompt_async directly with message B, drain goroutine sends A second → wrong order.

2. **`TestSendPromptAsync_ProceedsWhenQueueEmpty`** — empty queue + idle session. Asserts the direct send path still works (200 OK, prompt_async called). Ensures the guard does not false-positive on empty queues.

3. **`TestSendPromptAsync_RedirectRejectsInvalidBody`** — table-driven test covering 5 unhappy paths in `redirectPromptToQueue`:
   - Malformed JSON → 400 "invalid request body"
   - Empty parts array → 400 "text must not be empty"
   - Tool-only parts → 400 "text must not be empty" (tool parts dropped to empty)
   - Text part with empty string → 400 "text must not be empty"
   - Text > 100KB → 400 "text exceeds 100KB limit"
   - Each case also asserts the pre-existing queue message is unchanged (no side effects on rejection).

4. **`TestSendPromptAsync_FailOpenWhenLenErrors`** — closes the miniredis server before the test to force `Len()` to error. Asserts:
   - Response is 200 (direct send proceeded), NOT 5xx.
   - `prompt_async` was called — the fail-open path took the direct-send branch.

5. **`TestExtractPromptText`** — unit tests for the body parser, 8 cases:
   - Single text part
   - Multiple text parts (concatenation semantics)
   - Tool-only parts (dropped to empty)
   - Mixed text and tool parts (only text kept)
   - Empty parts array
   - Missing parts field
   - Malformed JSON (error)
   - Valid JSON but wrong shape (parts is string, error)

### Test runs

```
go test -timeout 60s -run "TestSendPromptAsync|TestExtractPromptText" -v ./api/internal/handlers/
  17/17 PASS

go test -timeout 120s -count=1 -run "Queue|Prompt|Enqueue|Drain|ExtractPromptText" ./api/internal/handlers/
  ok — full queue/prompt/drain battery green, no regressions

go build ./api/...   — clean

golangci-lint run ./api/internal/handlers/...   — no new issues in proxy_handlers.go or proxy_queue_test.go
  (25 pre-existing staticcheck issues in unrelated test files, unchanged)
```

### Pre-existing failures (not caused by this PR)

Two tests in `pod_bootstrap_e2e_test.go` fail on `main` without my changes:
- `TestE2E_PasswordReset_FullPurgeThenBoot_NoProviders`
- `TestE2E_PasswordReset_PurgeThenBoot_NoResurrect`

Both are about provider credential resurrection after password reset — unrelated to the queue path. Verified by stashing my changes and re-running on `main`: same two failures.

---

## Files Modified

- `api/internal/handlers/proxy_handlers.go` — `SendPromptAsync` queue check; `redirectPromptToQueue` + `extractPromptText` helpers.
- `api/internal/handlers/proxy_queue_test.go` — 17 new test cases (regression + unhappy-path + unit).
- `worklogs/0642_2026-07-19_sendpromptasync-queue-nonempty-redirect.md` — this worklog.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real, addressed):** initial draft of `redirectPromptToQueue` used `io.ReadAll(c.Request.Body)` with no cap. AI reviewer flagged this as a DoS vector (client could force arbitrarily large allocation before the 100KB check rejected). Phase 2 verdict: real. Remediation: added `http.MaxBytesReader(c.Writer, c.Request.Body, 100_000+1024)` before the read, mirroring `proxy.go:275`.
- **Phase 1 finding (real, addressed):** initial regression test asserted `Len >= 1` after the redirect, which would have passed even if the redirected message B were dequeued before pre-existing message A. AI reviewer flagged as "assertion doesn't prove FIFO order." Phase 2 verdict: real. Remediation: rewrote to capture `prompt_async` body order and assert `["message A", "message B"]` — proves the actual user-visible invariant.
- **Phase 1 finding (real, addressed):** initial draft shipped without unhappy-path test coverage for `redirectPromptToQueue`. AI reviewer flagged 5 distinct error branches with zero coverage. Phase 2 verdict: real (hard gate). Remediation: added `TestSendPromptAsync_RedirectRejectsInvalidBody` (5 table-driven cases) + `TestSendPromptAsync_FailOpenWhenLenErrors` + `TestExtractPromptText` (8 cases).
- **Phase 2 false alarm initially considered:** "Does the `MaxBytesReader` cap break the existing direct-send path?" Validated: no — the cap is applied only inside `redirectPromptToQueue`, which runs only when the queue is non-empty AND the session is idle. The direct-send path (`proxyToWorkspace`) applies its own cap at `proxy.go:275` and is unaffected. False alarm.

---

## Blockers

None.

---

## Next Steps

1. Open this PR.
2. After merge, deploy to production.
3. The two pre-existing `pod_bootstrap_e2e_test.go` failures (password reset provider resurrection) are unrelated but block the full CI suite from going green on `main`. Worth investigating separately — they predate this PR and any PR opened against current `main`.
