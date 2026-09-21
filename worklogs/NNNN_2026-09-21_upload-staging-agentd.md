# Worklog: design 0060 implementation PR 1 — the agentd staging leg

**Date:** 2026-09-21
**Session:** feat/upload-staging-agentd — §9 PR 1 of the approved design 0060 (squash 729f1586): the sidecar-mode upload staging leg
**Status:** Complete (this PR; the supervisor apply leg is PR 2)

---

## Objective

Implement design 0060 §4.1–§4.3 + §4.6 agentd-side: budgeted staging admission (two-clause), the declared-bytes admission input (hard read-cap + mandatory header), the unlink-before-release reservation lifecycle, boot+TTL hygiene with reservation finalization, the staging observability surfaces, and the handler flow with the control-socket apply seam. Single-container mode byte-unchanged (nil stager → the direct path).

---

## Work Completed

- `cmd/workspace-agentd/upload_staging.go` (new): `uploadStager` (admission controller + staging surface owner) — two-clause `Admit` (A: reserved+new ≤ budget; B: credentialUsage+floor+reservations ≤ statfs f_bavail; statfs failure rejects, the safe direction); `stageStream` (256 KiB window, sha256 incremental, hard read-cap at declared → `errDeclaredExceeded`, `.tmp`+same-fs rename, 0640); `ReconcileDown` (declared → actual at completion); `abortStaged`/`ackStaged` (unlink BEFORE release — the §3.5 ordering); `scrubStagingDir` (age-gated TTL + ttl=0 boot; finalizes held reservations — the §4.3 two-job scrub arm); `startStagingSweeper` (ticker + gauge snapshot); `handleStagedUpload` (the §3.1 flow: 411/413 admission gates, 507/429 classes, 504 timeout that HOLDS staged+reservation per §3.3, apply-error mapping with the §3.2 sub-codes riding `code`); `parseDeclaredBodyBytes` (missing/invalid → 411); env knobs per the §8 defaults (names cross-checked with worker 2's values-file lane).
- `uploads.go`: `uploadErrorResponse` +`Reason`/`+Code` (omitempty — the worker-2-pinned wire shape); `writeUploadErrorClass`; the handler branches to the staged flow when a stager is present (signature +stager/+apply); new outcomes enum.
- `ops_metrics.go`: staging gauges (bytes/reserved/files/credential) + `workspace_agentd_upload_bytes_total{direction=staged_in|copied_out}` (§4.6).
- `control_client.go`: `UploadApply` (closed-enum error mapping via controlClientError; transport errors wrap the cause so the DeadlineExceeded class is detectable).
- `server.go`: `serverDeps.uploadStager/uploadApply`; a stager without its apply seam de-wires (clean-fail, never stage-what-cannot-deliver).
- `sidecar_mode.go`: the sidecar wiring — stager + boot scrub + sweeper + apply via the control client (no client → stager stays unwired; today's clean-fail continues until PR 2 lands the server side).

## Interface coordination (worker 2, first hour — done)

Pinned verbatim for their PR 3: the error-body JSON (error+reason+code, literals sent), 507/429/504-only forwarding, their values-file knob ownership with my exact env names + defaults.

---

## Key Decisions

1. DI over mode flags: the stager rides `serverDeps` — single-container never constructs one (§5.1's byte-unchanged guarantee is structural, not conditional).
2. The apply seam is a func type — PR 2's server side lands independently; the interim (PR1-without-PR2) state is exactly today's behavior (uploads clean-fail; the socket answers method_unknown → apply_rejected).
3. The sweeper rides the process lifetime (buildSidecarDeps has no shutdown ctx; a ticker goroutine that dies with the process — same as the other sidecar loops).

### Assumptions stated and validated (Rule 7)

- `buildUserMux` is shared by both modes → the handler-level branch is the right seam (validated: server.go:452 is the only registration).
- The §3.2 uuid-shape claim: the staged name is always `uuid.NewString()` output here; the supervisor-side regex validation is PR 2's job.
- Test-visible masking risk: clause A alone can be masked by clause B in narrow-number tests — caught during mutation runs and closed with the dedicated clause-A-binding pin.

---

## Blockers

None. PR 2 (supervisor `upload_apply` + destination scrub) next.

---

## Tests Run

- `go test -run 'TestStaging|TestStagedUpload' ./cmd/workspace-agentd/` — 20 tests green.
- Mutations, each witnessed red then restored green: clause A removed (dedicated binding pin), over-read check removed (2 fails), timeout arm disabled (504 pin), scrub-finalize removed (held-reservation pin).
- Full `go test ./cmd/workspace-agentd/` — ok (281s), existing uploads suite untouched-green (all direct-path call sites nil-stager).
- `go build ./cmd/workspace-agentd/`, `go vet`, `gofmt` — clean.

---

## Next Steps

- PR 2: the supervisor `upload_apply` method (closed-param validation, statfs write-time gate, bounded-window copy+verify+rename, additive ack fields) + the uid-1000 destination scrub (boot+TTL over the existing `*.tmp` glob — closing the sidecar-mode gap §4.3 names).

---

## Files Modified

- `cmd/workspace-agentd/upload_staging.go` — new: the staging leg
- `cmd/workspace-agentd/upload_staging_test.go` — new: 20 tests + mutation-verified pins
- `cmd/workspace-agentd/uploads.go` — error body +Reason/+Code; handler branch; outcomes enum
- `cmd/workspace-agentd/ops_metrics.go` — staging gauges + bytes counters
- `cmd/workspace-agentd/control_client.go` — UploadApply
- `cmd/workspace-agentd/server.go` — deps fields + call-site wiring
- `cmd/workspace-agentd/sidecar_mode.go` — the sidecar wiring (stager/scrub/sweeper/apply)
- `cmd/workspace-agentd/uploads_test.go` — call sites updated for the new signature (nil stager)
- `worklogs/NNNN_2026-09-21_upload-staging-agentd.md` — this worklog


---

## Review round 1 (7 findings, all fixed; the cross-lane envelope divergence folded in)

- **BLOCKING — the 2s control-client deadline defeated the 60s apply bound** (the reviewer reproduced it empirically): `call` fixed `SetDeadline(now+2s)`, so a >2s supervisor copy died as a conn i/o timeout → misrouted to abort+507 apply_rejected, unlinking mid-copy, and `UPLOAD_APPLY_TIMEOUT_MS` was dead in production. Fixed: `callTimeout` (per-invocation deadline); `UploadApply` bounds the connection by the caller's ctx deadline AND normalizes ctx-expiry to the timeout class regardless of the surface error. Pinned permanently by `TestUploadApplyClient_BoundedByContextDeadline` (the reproduction, in-tree) + the closed-enum mapping pin.
- **§4.6 literal deviation**: the 400 over-read carried reason `invalid_declared_length`; the design pins `declared_length_exceeded` for the 400 (the 411 is the invalid_declared_length carrier). Fixed + re-pinned.
- **`busy` mapping missing**: mapApplyError now routes the supervisor's `busy` → 429 staging_busy (§4.6), pinned.
- **Dead branch on the ack path**: the dest-margin observation implemented — `workspace_agentd_upload_dest_outcomes_total{code}` counted from the ack's supervisor-computed flag (never inferred); pinned by TestStagedUpload_MarginObserved (an observation, not a rejection).
- **`staging_scrubbed` absent**: scrub activity now records the outcome at both scrub sites.
- **§4.1.1 dir contract**: `ensureStagingDir` (0750) established at boot BEFORE the scrub; the gid-1000 dependency stated and validated (the sidecar runs gid 1000 by pod spec — process inheritance, not fsGroup; the supervisor shares gid 1000 so 0640 staged files are group-readable across the boundary).
- Minor: `copied_out` now counts the ack's verified size; the ack Unmarshal swallow stays consistent with sibling methods (noted, not changed).
- **Cross-lane (worker 2's coordination catch)**: the declared value on the hop is the envelope-INCLUSIVE client Content-Length — my bare `declared > cap` gate would have 413'd exactly-at-cap files. Now mirrors the API's 64 KiB allowance, pinned both arms (commit 76d9cb41, pre-review).

## Tests Run (r1)

- `go test -run 'TestStaging|TestStagedUpload|TestUploadApplyClient'` — 24 tests green (the production client seam now has its own wire tests).
- Full `./cmd/workspace-agentd/` — ok (274s); golangci-lint 0 issues.


## Review round 2 (3 findings + 3 minors, all fixed)

- **BLOCKING — the bounded-wait fix was inverted** (min instead of the ctx-as-bound; the reviewer reproduced a 3s copy within a 5s budget dying at the 2s conn deadline): UploadApply now takes the ctx deadline AS the bound (only deadline-less ctx falls back to the 2s default). The r1 flaky pin (a coin-flip clock race between the conn arm and the ctx timer) replaced with deterministic classification: callDeadline reports whether ITS OWN armed deadline fired (os.ErrDeadlineExceeded on the round trip) — when that arm was ctx-derived, it IS the timeout class by construction. Two pins: the reviewer's reproduction (300ms copy / 5s budget → SUCCESS) and the beyond-budget arm (→ timeout class), both -count=5 stable.
- **Dest-outcome rejections were dead code**: the dest_disk_full/checksum/size rejection path now counts on workspace_agentd_upload_dest_outcomes_total (the metric's own advertised contract).
- **§7's race test** (concurrent admission never exceeds clause A): added TestStagingAdmission_ConcurrentNeverExceedsBudget (N goroutines racing Admit at a tight budget; max observed reserved ≤ budget).
- Minors: staging_scrubbed moved onto the injected seam (RecordScrubbed); the boot ensureStagingDir error logs loudly (rides to the request-time 507 seam); the envelope-allowance comment now states the direct-path cap honestly moved to maxBytes+64KiB.
