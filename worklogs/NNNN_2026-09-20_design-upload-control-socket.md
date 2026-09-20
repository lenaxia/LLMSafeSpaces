# Worklog: design 0060 — upload delivery in sidecar mode (stage-and-signal)

**Date:** 2026-09-20
**Session:** design/upload-control-socket — the design-doc-first lane for #1500 (the owner-funded control-socket upload delivery leg), grounded in the #1497 investigation
**Status:** In Progress — design PR in review (HOLD class; implementation lanes follow approval)

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


---

## Review round 1 (design doc) — 5 findings, all real, all fixed

- **The `*.part` mischaracterization (blocking)**: the existing scrub class globs `*.tmp` (uploads.go:277), and my §5.2 claimed it covered destination `.part` files — false; an implementation lane following it would leak PVC `.part` files forever. Fixed by unifying on `.tmp` everywhere (staged `<id>.tmp`, destination `<uuid>-<name>.tmp` — matching the existing single-container temp contract exactly), making the hygiene claims true as written.
- **§3.1 vs §4.4 ordering contradiction**: resolved normatively — pre-copy statfs gate; pre-rename (gating): fsync + size/sha verify; post-rename (observability): statfs margin-consumed counter.
- **§6 A.1 non-conformance**: the test plan pinned unknown-param REJECTION; 0051 A.1 mandates unknown-key tolerance (forward compatibility). Row now pins tolerance, cites A.1.
- **§4.6 metric-name drift + R6 gap**: real names used (workspace_agentd_file_uploads_total{workspace_id,outcome}; API-side llmsafespaces_uploads_total unchanged); added the missing bytes-in/out counters ({direction=staged_in|copied_out}); stated the supervisor→agentd export mechanism (outcomes ride the upload_apply ack — agentd is the single metrics authority).
- **D14 hardening (robustness)**: added §4.1.1 — POSIX perms are NOT a boundary (agent shares uid+gid with the supervisor); integrity rests on sha/size verification; junk planting reduces admission capacity via f_bavail (the safe direction — clean 507s, never mid-stream ENOSPC, unless junk lands post-admission, which aborts cleanly per §3.5).

## Owner amendment folded in (round 2, protocol-shaping — not bolt on)

Stress testing is now a first-class section (§6) with six invariants, each naming mechanism + proof method + acceptance signal: (6.1) measured streaming residency via gauge pin under max-concurrency near-cap storms; (6.2) credential resync DURING an upload storm completes unaffected (the cross-feature isolation the design exists for — V3 of the invariant matrix); (6.3) backpressure with throttled consumer (window bounds, flat RSS); (6.4) disk-margin edges incl. the TOCTOU interleave; (6.5) mid-stream kills at chunk boundaries (atomicity + reclamation + credential intactness); (6.6) ack-path throughput/latency characterization with a relative regression guard. §3.5 added: the flow-control decision the stress spec SHAPED — implicit TCP backpressure via the synchronous streaming chain, explicit window signaling deliberately REJECTED (nothing is decoupled, so nothing can run away), with the mid-stream-ENOSPC abort class specified. Defaults reconciled with the /analyze proposal on the issue (48 MiB budget / 24 MiB floor — one 25 MiB upload by reservation, ≥47 MiB credential headroom); Q1 (507 vs 429) and Q3 (gVisor run) absorbed into §8.

## Tests Run (r1/r2)

Docs-only lane; no runtime tests. §7's test-plan rows and §6's proof specs are the implementation lanes' inherited contract.
