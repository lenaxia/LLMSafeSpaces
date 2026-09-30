# Worklog: SR-6B nightly-red root cause — pre-handler request-body buffering in the API middlewares

**Date:** 2026-09-30
**Session:** Triage the deterministic SR-6 upload-row failure on main's nightly (run 36740521434); reproduce, root-cause, fix red-first, verify.
**Status:** Complete (full-stack verification run in flight at session end)

---

## Objective

Nightly 36740521434 failed step "Run upload staging stress rows (design 0060 §6)" with
`FAIL: SR-6: the deterministic cap boundary failed (holders-ok=1 fifth=201 fifth-busy=0 retry=201)`.
Find the root cause, fix it red-first, and restore the nightly's SR-6 row to green at full stack.

---

## Work Completed

### Repro established from nightly history (no local kind — pod has no docker/kubectl)

`gh run view --log` across the last 25 nightlies: SR-6B failed **10/10 consecutive runs with a
byte-identical signature** (36135708380, 36148719177, 36214493133, 36216981147, 36222755701,
36246212235, 36326908065, 36459801703, 36593820056, 36740521434). The row has **never** been
green: #1567 (the trickled-holder determinism row) merged 2026-09-25 11:50 UTC; the first
nightly carrying it (36135708380, 12:54 UTC) failed. Red-on-arrival — NOT a regression from
v0.34.9/.10/.11 (first failure predates the v0.34.8 release commit). The .10/.11 tag bisect
was therefore moot; history resolved it. The last "green" (36064158866, 09-24) was the OLD
row shape and is consistent with the harness's own History note (pre-#1567 greens were
misattributed 429s).

### Instrumentation (commit 36a7a451)

The truncated report line could not seat the failure. Added to the SR-6B failure path:
per-holder status+body capture (the API rate limiter's 429 and agentd's staging_busy are
status-identical; only the body distinguishes), probe timing (probe-wait-ms), agentd's
`workspace_agentd_file_uploads_total` + staging gauges, the API's `llmsafespaces_uploads_total`,
and both log tails. Dispatched the nightly on the branch via the workflows API (webhook outage
sidestep). Run **36749714653** delivered the evidence.

### Root cause (proven, issue #1607)

**Corrections first (honesty):** my interim read "three non-ok holders failed admission" was
wrong twice over: `holders-ok=1` is the ALL-FOUR-DELIVERED value (any non-201 flips it to 0),
and all four holders returned 201 in every run. The row failed solely on `fifth=201`.

The API's **logging middleware** (`readAndReplaceBody`) did `io.ReadAll(c.Request.Body)`
BEFORE the handler ran, and the **error-handler middleware** did the same unbounded read a
second time (both global, both mounted **before auth**). A client-paced body never streams
through: the handler — and the agentd forward, and `Admit` — starts only after the ENTIRE
body has arrived at the API.

Proof from 36749714653's API request log:
- holders launched 17:35:23.44 (1MiB @ 64k/s = 16.0s); the **5th** (full speed) was received
  26.504 and **delivered 201** at 26.615
- the four holders' `Request received` lines appear only at **39.444–39.450 = T+16.0s** (the
  whole body), each completing ~40ms later with `duration:"16.039s"` — the middleware's own
  clock proves request start at 23.443
- agentd: `accepted=18`, `rejected_staging_busy` **absent** — the count cap never bound in the
  run; the 5th admitted into an empty cap

So #1567's determinism construction is sound at agentd but its premise (client pacing reaches
agentd's Admit through the API hop) was never true — `uploads.go`'s documented "NEVER buffered"
contract was silently voided by our own middleware. An unvalidated-assumption defect (Rule 7):
the row's needle pins verified the script's shape, never the stack's behavior — and the row
only executes in the nightly, so it never ran pre-merge.

### Fix (commit 3b5325d7, red-first)

Both middlewares now capture at most `requestBodyCaptureLimit` (4KiB) and replace the body
with a `MultiReader` stream: captured-prefix replay + live remainder (`captureRequestBody` /
`streamedBody`, shared). Restores the streaming contract; the trickle now reaches agentd and
holds an admission slot.

Red tests (verified failing on pre-fix code): `TestLoggingMiddleware_SlowBodyStreamsToHandler`
(handler must start while a slow body is still arriving — structurally impossible when the
middleware ReadAll's to EOF), `TestLoggingMiddleware_OversizedBodyCaptureNotLogged`
(oversized captures must not log body bytes — the truncated-string branch bypasses JSON field
masking; logs the declared size instead), `TestErrorHandlerMiddleware_BodyCaptureBoundedStreamsThrough`
(second seat; its red-state is implied by the same mechanism — TDD order was red→fix on the
logging seat, the error-handler pin landed with its fix in the same commit; noted honestly).

Bonus kills (both real, both filed in #1607): the unauthenticated memory-exhaustion amplifier
(pre-auth unbounded buffer on every route) and megabytes of multipart binary in request logs.

---

## Key Decisions

- **4KiB capture, not zero.** Logging fidelity for JSON APIs is preserved (masked-field
  logging needs the parseable body; 4KiB covers real request DTOs). Oversized captures log
  `request_body_size` = declared ContentLength and NO bytes — a raw-string branch would leak
  unmasked field values, so bytes are skipped entirely rather than truncated. Rationale to be
  carried in the PR body per orchestrator instruction.
- **Fix the middleware, not the row.** Alternative rejected: enlarge the holders/probe timing
  to outrun the buffer — that keys the row to an implementation detail (capture size) and
  leaves the product streaming contract broken. The middleware fix makes the row's ORIGINAL
  design valid.
- **Not touched:** `validation.go`'s `io.ReadAll` (per-route opt-in via `validationModel`,
  not on the upload path — flagged in #1607 for its owners); the #1539 skip-override in the
  harness (if the fixed streaming lets the p95 guard pass, the override's "tighten on fix"
  note becomes actionable — separate lane).
- **Issue placement:** new issue #1607 (the buffering was never filed); cross-reference on
  #1541 (the lane this unblocks). #1532 is an unrelated family.

---

## Blockers

- GitHub webhook delivery outage: the PR gets no CI/review until recovery. Branch pushed,
  nightly dispatched via the API for full-stack verification.
- No local kind in this pod (no docker/kubectl) — verification rides branch-dispatched
  nightly runs (~50min each).

### Verification run 1 (36758872210): mis-adjudicated as a flake — actually a second defect

The fix's first full-stack run failed the upload row from SR-1 onward with transport-level
000s — including the BODYLESS reload-secrets POST — while the next step's e2e passed against
the same API. I adjudicated it an infra flake (port-forward death) and backed that with a
unit-level storm test that passed. **Verification run 2 (36766849622) failed BYTE-IDENTICALLY
— deterministic, so the flake call was wrong and the unit disproof was testing the wrong
layer** (direct TCP; loopback socket buffers absorb what the tunnel cannot).

### The second peeled layer: RST on early refusals (fix 2, f9ee2b60)

Real mechanism, both runs: with the bounded capture, agentd's at-Admit refusals arrive
MID-STREAM for the first time. The API writes the 507 while ~24MiB of the client's declared
body is unread; a server closing with unread receive-buffer bytes emits RST — clobbering the
in-flight response (curl 000) and killing the step's port-forward tunnel (every later request
on the step 000'd). The old unbounded `io.ReadAll` had accidentally consumed every body before
any response existed — masking this hazard since the route shipped. The route's
"never buffered" contract was broken TWICE: the buffer hid the RST hazard; fixing one exposed
the other.

Fix: `scheduleUploadBodyDrain` (uploads.go) — after an early response, flush, then discard
exactly ContentLength − consumed (`countingBody` wrapper), memory-free, deadline-bounded
(10s) for stalled senders; chunked bodies keep the old behavior. Red test:
`TestUpload_EarlyRefusalDrainsClientBody` — a 25MiB body (at the cap) through a synchronous
`io.Pipe` so socket buffers cannot absorb it; fails on prior code with
`io: read/write on closed pipe`.

TDD stumble, owned: the first drain implementation deferred inside the helper — the drain ran
BEFORE the handler (consumed the body, flushed an empty 200). The existing shape-table tests
caught it immediately (400s became 200s); fixed with the returned-closure idiom
(`defer scheduleUploadBodyDrain(c)()`).

### Verification run 3 (36774677154, commit f9ee2b60): GREEN

```
✓ SR-1: concurrent storm complete + terminal-clean (delivered=2 refused=4 other=0 total=6)
✓ SR-6: 5th-concurrent 429 boundary observed DETERMINISTICALLY (4 trickled holders;
  5th=429/staging_busy; retry-after-release delivered; holders-ok=1 fifth=429 fifth-busy=1
  retry=201 probe-wait-ms=3004)
✓ upload stress harness: all rows passed (loud skips: 2)
```

The §6.6 boundary exercised deterministically for the first time since #1567 landed. Remaining
skips are known lanes, not regressions: the #1539 serialization guard still fires
(706ms > 610ms — its own follow-up; with uploads now streaming, the residual seat is worth a
fresh triage there), and SR-3 awaits the PR 1/2 fault seam. The upload row no longer blocks
the downstream nightly lane (#1541).

---

## Tests Run

- `go test ./api/internal/middleware/...` — green (incl. 3 new pins)
- `go test ./api/internal/handlers/` — green (incl. 2 new storm/drain pins; the shape table caught the drain defer bug red)
- `go test ./api/...` — 42 packages, all green
- `go test -run TestUploadStress ./local/` — green (harness pin suite + new diagnostic pins)
- Nightly on branch, 4 dispatches: 36749714653 (instrumentation — evidence), 36758872210 +
  36766849622 (fix 1 — exposed layer 2, byte-identical failures), **36774677154 (fix 2 —
  SR-6 green deterministically; full harness pass)**

---

## Next Steps

1. PR review + merge (webhook-recovery-gated). 2. Post-merge: the #1539 guard's residual
   706ms seat deserves fresh triage now that uploads stream end-to-end (its skip note says
   "tighten on fix"). 3. `validation.go`'s per-route `io.ReadAll` (opt-in routes, flagged in
   #1607) for its owners. 4. Post-merge bot assigns this worklog's number.

---

## Files Modified

- `api/internal/middleware/logging.go` — bounded capture + stream-through (+ shared captureRequestBody/streamedBody)
- `api/internal/middleware/error_handler.go` — bounded capture (second seat)
- `api/internal/middleware/tests/logging_test.go` — 2 red-first pins + chunkedBody test reader
- `api/internal/middleware/tests/error_handler_test.go` — second-seat pin
- `api/internal/handlers/uploads.go` — early-response body drain (countingBody + scheduleUploadBodyDrain)
- `api/internal/handlers/uploads_forwarding_test.go` — concurrent early-refusal storm pin + drain red test
- `local/us-1500-upload-stress-e2e.sh` — SR-6B failure legibility (holder bodies, timing, evidence dump)
- `local/us_1500_upload_stress_script_test.go` — pins for the diagnostics
- `COORDINATE.md` — active claim row
- Issue #1607 filed (+ follow-up comment with layer 2 + green run); #1541 cross-comment; worklog (this file)
