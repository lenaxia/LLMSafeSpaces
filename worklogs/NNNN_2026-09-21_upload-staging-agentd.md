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
