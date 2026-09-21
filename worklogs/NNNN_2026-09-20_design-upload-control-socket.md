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
4. **Idempotent re-apply per upload_id** — the timeout tail (supervisor completes after agentd gave up) converges instead of duplicating; hygiene reclaims the true orphans. *(Superseded in round 3 — D19 retries create new ids, the same id is never re-signaled, so the re-apply machinery was dead and removed; the 504 orphan completes-or-is-reclaimed, both terminal.)*

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

- Iterate the design PR with the reviewer to APPROVED; notify the orchestrator; implementation lanes split per §9 (agentd staging leg → supervisor upload_apply → API forwarding/wiring/stress harness → E2E un-skip).

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

## Verification (r1/r2 rounds)

Docs-only lane; no runtime tests. §7's test-plan rows and §6's proof specs are the implementation lanes' inherited contract.

## Review round 2 (design doc) — 2 new blockers + 5 minors, all introduced by my r2 edits, all fixed

- **The API "pass-through" claims were false**: the API handler collapses every non-201/413 agentd status into a fixed 502 (uploads.go:241-245) — my §4.6/§5.4 claimed reason strings pass through. Fixed: the design now specifies the REQUIRED API forwarding change (statuses + reason bodies verbatim; API reason enum widened) and §9's wiring PR is retitled accordingly ("API forwarding + wiring…", not "polish").
- **UPLOAD_STAGING_BUDGET had no enforcement point** (and §4.2 still said "the fraction"): admission formalized as two clauses — (A) reservedUploads+newBytes ≤ budget (the semaphore's enforcement point; defaults admit one 25 MiB upload by reservation, a 3×15 MiB storm peaks at 45 MiB), (B) credentialUsage+floor+reservations ≤ f_bavail. §6.1's residency pin now holds by construction.
- Minors: §4.7→§4.1.1 dangling ref; §8-Q2→item-3 cross-ref; D17→D19 (retry semantics); dest_margin_consumed given its export channel (additive A.1-legal dest_avail_after ack field; counter renamed to dest_outcomes covering rejections AND the success-path margin observation); Content-Length wording (cap+64 KiB envelope allowance, the safe direction); §6.2's unverifiable "V3" label replaced with the concrete row-family anchor.


## Review round 3 (design doc) — 2 new blockers (mine, from r3) + 4 minors, all fixed

- **The admission input did not exist on the wire**: the API→agentd forward is io.Pipe-chunked (no Content-Length; agentd learns sizes mid-copy today). Fixed with a second required API change: `X-LLS-Declared-Body-Bytes` on the hop (the client's declared multipart total — conservative upper bound, reservation reconciles down at completion) + a 411 gate on undeclared client bodies (clean rejection beats silent mid-stream truncation). §4.6/§5.4/§9.3 updated to three required API changes.
- **§6.1's residency pin was falsified by my own 504/abort paths**: fixed by pinning the lifecycle (unlink-before-release; reservations held until bytes leave the tmpfs; 504 holds until TTL scrub) and stating the two bounds separately (reserved ≤ budget ALWAYS by construction; walked ≤ budget in every no-crash lifecycle; the crash window bounded + transient + TTL-reclaimed, with clause B shrinking new admissions meanwhile).
- Minors: §6.6's matrix resized to the REACHABLE concurrency (1×/2×/4× + cap-boundary 429 characterization at the 5th; 25 MiB single-flight by clause A — the old 4×25/8× rows were unreachable under the doc's own defaults); the dead idempotent-re-apply machinery removed (retry creates a new id by D19 — never re-signaled; the 504 orphan completes-or-is-reclaimed, both terminal); `dest_disk_full` added to the widened API enum (never misrecorded as staging_full); the margin flag computed supervisor-side (it owns the margin env) as an explicit `margin_consumed` ack field.


## Review round 4 (design doc) — 1 blocker (incomplete propagation of my own r4 lifecycle fix) + 3 minors + 2 nits, all fixed

- **§3.5 still had the old abort order** ("reservation released → … hygiene reclaims") while §4.1/§6.1 had been corrected to unlink-before-release — the walked bound was falsifiable without a crash (a released-reservation partial + fresh admissions could walk 58 MiB on a 48 MiB budget). §3.5 now states the normative order (unlink → release → 507) and demotes the scrub to the crash backstop it is.
- §7's admission row gained the r4 wire-change contract (header propagation, 411 shape, lying declarations both directions); §3.1's diagram shows the 411 arm; the worklog Key-Decision-4 superseded pointer added; §4.1's scrub-arm parenthetical and §6.6's clause-(B) harness precondition (skip-DOWN with an explicit message, never silently measure 3×) stated.


## Review round 5 (design doc) — 4 minors + 1 nit, all fixed

- **§6.6's precondition omitted the credentialUsage term**: the worst-timing 4th-admission inequality is `credentialUsage + nonUploadUsage + 24 + 40 ≤ f_bavail` (on 96 MiB with C≈U: C+U ≤ 2 MiB); the harness now asserts the literal inequality, both terms, against the pre-storm walk.
- **The lying-declaration pin had no specifying mechanism and promised an undeliverable client shape**: the declared value is now a HARD READ-CAP agentd-side (over-read → 400 declared_length_exceeded), the header is MANDATORY at agentd (411 on missing/invalid — D14 makes direct :4097 calls adversary-reachable via the shared netns + pod-readable password, so the API-side 411 cannot stand alone), and the API forwarding list extends to the 4xx class so the shape reaches the client as itself.
- **§4.6's table gained the missing row** (mid-stream staging write abort → 507 staging write failed) and the remainder class is honest again (fixed-502, unchanged — no phantom "reason in the body").
- **The enum gained `staging_write_error` + `invalid_declared_length`** — each 507 class its own value, nothing misrecorded.
- **§4.1's scrub-arm comment reconciled** (two jobs: release the LIVE 504 hold's reservation; crash orphans hold none — bytes only).


## Review round 6 (design doc) — 2 blockers of the claims-without-landing class + 1 minor + 1 nit, all fixed

- **The r5 enum/forwarding "fixes" existed only in THIS worklog**: my r6 python edit replaced a sentence whose original didn't match, the replace silently missed, and my verification grep hit OTHER lines — I recorded the fix without verifying the strings landed in the doc. Second occurrence of the class this session; the check is now grep-the-specific-strings-in-the-target-file before writing any worklog claim about them.
- Landed for real: the enum gains staging_write_error + invalid_declared_length (§4.6(b)); the 411/400 delivery story is honest per the reviewer's sharpening — the 411 is API-GENERATED (no forwarding needed), the 400 declared_length_exceeded is DIRECT-`:9097`-PATH-ONLY (the API pipes only the LimitReader-bounded part, so the declared bound always covers the API hop; only the D14 direct caller can over-read) — the forwarding list stays 507/429/504 by design, and §4.6's table row + §7's pins state the real delivery points.
- §6.6's precondition is now definition-free and executable: assert `f_bavail_pre − 30 MiB ≥ C + 24 MiB + 10 MiB` measured pre-storm (the literal worst-timing clause-(B) substitution; the old C+U arithmetic double-counted against f_bavail's own exclusions three different ways).
- §3.1's diagram step 1 shows the 411/400 admission arms.


## Review round 7 (design doc) — 2 blockers + 2 minors + 1 nit

- **§6.6's precondition was 30 MiB weak (third recurrence of this exact defect)**: the correct worst-timing substitution includes BOTH the staged bytes (reducing f_bavail) AND the still-held reservations (the clause-B term) — hold-until-bytes-leave double-counts by design. Now pinned as `f_bavail_pre ≥ C + 94 MiB`, with the double-counting named as the semantics. My round-6 record's "all fixed" header and "literal substitution" claim were false — the new standard is re-derive-the-arithmetic, not re-read-the-sentence (the grep lesson needed its numeric twin).
- **The ack-failure class had no pinned delivery**: now mapped — agentd 507 with the §3.2 error code in the reason body, forwarded verbatim, labeled `apply_rejected` (integrity mismatches reach the client as themselves; fine granularity lives in the agentd counter); supervisor `busy` maps to 429 `staging_busy`.
- Minors: agentd counter gains `rejected_declared_invalid`/`rejected_declared_exceeded` (the D14-visible admission-input traffic — R6's visibility for exactly the adversarial class the read-cap gates); the round-6 record's `:9097` typo corrected here (the port is 4097).
- Nit: the diagram's 400 arm moved to step 2 (mid-read detection, matching §4.1).
- Context fold-in: §6.3 gained the WEDGED-consumer extreme (alive-but-spinning, #1507's autopsy shape) as the backpressure row's limit case — window bounded, apply timeout bounding the hold, 504 tail + hygiene reclaiming.


## Review round 8 (design doc) — 1 pre-existing blocker surfaced by the skeptical pass + 2 minors

- **The destination-side hygiene claim was FALSE in sidecar mode**: I had verified the scrub CALL's existence (sidecar_mode.go:163) but never its EFFICACY — the sidecar's /workspace is RO (its own comment concedes the no-op), and the only live call site (main.go:182) is the single-container path sidecar pods never reach. The issue thread's own /analyze comment had documented this gap and specified the fix; my doc asserted the opposite. Fixed: §9.2's supervisor lane ADDS the uid-1000 destination scrub (boot + TTL, the existing *.tmp glob); §4.3/§5.2 state the real gap being closed; §7 pins the destination-scrub row. Lesson recorded: verify EFFICACY (trace the call through its execution environment), not existence.
- Diagram 411-arm cite corrected to §4.1; the PR body rewritten to the head doc (the .part/idempotency claims were stale from revision 1).


## Review round 9 (design doc) — 1 minor + 2 nits (convergence)

- §4.3's false absolute corrected: the supervisor is the only CONTROL-PLANE component that can write the destination dir — the in-pod agent shares its uid (§4.1.1) and is the D14 adversary, not a hygiene authority.
- §5.2: "no PVC `.tmp` survives" → "survives INDEFINITELY" (reclaimed at boot/TTL, not instantly).
- Destination scrub's TTL pinned to the same `UPLOAD_STAGING_TTL`.


## Review round 10 (design doc) — 1 minor ("one word from approval")

- The r10 superlative still had a falsifier class: platform-init (PVC root RW, uid 1000) and workspace-setup (subPath RW) are boot-phase control-plane writers of the same directory. "STANDING" qualifier + boot-window carve-out clause added. PR body's revision label refreshed.
