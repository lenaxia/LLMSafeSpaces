# Worklog: design 0060 — upload delivery in sidecar mode (stage-and-signal)

**Date:** 2026-09-20
**Session:** design/upload-control-socket — the design-doc-first lane for #1500 (the owner-funded control-socket upload delivery leg), grounded in the #1497 investigation
**Status:** Design proposed (HOLD-class PR; implementation lanes follow approval)

---

## Objective

Design the upload delivery leg for sidecar mode with the owner's binding directive as normative: disk/memory safety as first-class citizens — the design must be incapable of causing disk-full conditions. Design-doc first per the delegation; Appendix-A socket-semantics class.

---

## Work Completed

- Read the issue's six requirements + all inputs: the #1497 chain, control_socket.go (v1 semantics, A.4 rule, the two standing invariants, `refresh_files` — the mid-lifecycle staged-files precedent), #1165 R2b (stage-and-pull precedent), the shared `/sandbox-runtime` topology (verified LIVE: 96 MiB tmpfs RW in the workspace container; sidecar mount at agentd_sidecar.go:197), Epic 68 D16 gate order + cap/timeout defaults, the boot/TMP scrub class, supervise_opencode's ownership of the control-socket server.
- Wrote `design/0060_2026-09-20_upload-control-socket-delivery.md`: **stage-and-signal** — sidecar stages upload bytes on the budgeted shared tmpfs (reservation-before-acceptance admission with a credential floor; byte-weighted semaphore = the admission itself + a small concurrency cap), one new closed-param control method `upload_apply` (additive under v1; A.4 capability-equivalence argued), synchronous ack held through the supervisor's bounded-window copy, per-side same-fs renames bracketing the cross-fs copy (the "temp+rename equivalence across the boundary"), supervisor-side statfs write-time gate + post-write verification (the authoritative TOCTOU answer — statfs is ground truth the CRD ratio is not), boot+TTL hygiene extending the existing scrub class, and the full observability table (usage/reserved gauges, rejection counters by class). Requirement-by-requirement mapping in §4; test plan; 4-PR implementation sequencing; open questions for the implementation lanes.
- Corrected one arithmetic error pre-commit (the apply-timeout vs API-stream-timeout rationale — the windows never compose because staging completes before signaling).

---

## Key Decisions

1. **Stage-and-signal over stage-and-pull** — the shared tmpfs already exists, is budgeted by the directive itself, has established cross-uid semantics, and keeps the control socket small-JSON. The HTTP-pull alternative adds a second authenticated surface for the same bytes.
2. **Admission IS the semaphore** — byte-weighted by construction; a separate count cap handles fd/window pressure. Reject-before-first-byte makes half-filled-refusals impossible.
3. **statfs as the authoritative destination gate** — the supervisor cannot (and must not) read the CRD; the filesystem is ground truth, the margin absorbs concurrent writers, the counter makes margin consumption visible.
4. **Idempotent re-apply per upload_id** — the timeout tail (supervisor completes after agentd gave up) converges instead of duplicating; hygiene reclaims the true orphans.

### Assumptions stated and validated (Rule 7)

- `/sandbox-runtime` is RW in both containers in sidecar mode — verified live (mount in this pod's workspace container) + controller source (agentd_sidecar.go:197).
- The pod network namespace is shared (sidecar reaches 127.0.0.1:4099) — verified: the sidecar's control CLIENT and refresh_files flow depend on it (supervise_opencode.go:115 serves).
- API cap 25 MiB / 5-min stream timeout / agentd body deadline — verified (api uploads.go:49-54).

---

## Blockers

None — awaiting design review (HOLD class).

---

## Tests Run

None — design-doc lane; the doc carries the test plan (§6) the implementation lanes inherit.

---

## Next Steps

- Iterate the design PR with the reviewer to APPROVED; notify the orchestrator; implementation lanes split per §8 (agentd staging leg → supervisor upload_apply → wiring/observability → E2E un-skip).

---

## Files Modified

- `design/0060_2026-09-20_upload-control-socket-delivery.md` — the design (new)
- `worklogs/NNNN_2026-09-20_design-upload-control-socket.md` — this worklog
