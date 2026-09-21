# Worklog: Design 0060 §9 PR 3 — upload API forwarding, declared-length gate, and the §6 stress harness

**Date:** 2026-09-21
**Session:** Parallel lane on branch `feat/uploads-api-forwarding-stress` (worktree wt-1453) implementing design 0060's PR-3 surface, coordinated first-hour with the PR 1/2 lane (agentd staging + supervisor apply).
**Status:** Complete

---

## Objective

The three REQUIRED API changes (§4.6/§5.4): verbatim 507/429/504 forwarding with reason-body mapping onto the widened metrics enum; the X-LLS-Declared-Body-Bytes admission header + the API-generated 411; plus the §6 stress harness with skip-DOWN semantics.

---

## Work Completed

### Interface coordination (first-hour rule, honored both directions)

The PR 1/2 lane's frozen shapes received and ADOPTED verbatim: the literal 507/429/504 bodies (`{"error","reason"[,"code"]}` — reason for MY metrics label, code = the §3.2 sub-enum riding apply_rejected); the mapping table (budget→staging_full, count-cap→staging_busy, mid-stream→staging_write_error, timeout→apply_timeout, ack→apply_rejected, dest-disk→dest_disk_full); the {path,name,size} 201 unchanged; values-knob names confirmed MINE (see Gap below). My degrade rule (missing/unknown reason → agentd_error, never a response mutation) confirmed safe — every PR-1-emitted 507/429/504 carries reason.

### The three API changes (TDD — 10 failing tests first)

1. **Forwarding**: the response switch gains `case 507, 429, 504` — body read (4 KiB cap), forwarded VERBATIM via `c.Data` (status + bytes untouched), metrics labeled via `forwardedUploadReason` (507 disambiguated by the body's reason field; 429/504 unambiguous by status). All six literal shapes pinned byte-exact incl. the §3.2 code riding apply_rejected; unknown-reason 507 pinned (forwards verbatim, labels agentd_error).
2. **411 gate**: `c.Request.ContentLength < 0` → API-generated `411 {"error":"declared body length required","reason":"invalid_declared_length"}` before any agentd work. Pinned with a genuinely-undeclared body (MultiReader — no Len()).
3. **Header**: `X-LLS-Declared-Body-Bytes: <client Content-Length>` on the agentd hop (the forward is chunked — no Content-Length of its own); pinned via the fake-agentd recording extended with the header capture.

### Test reworks the design REQUIRES (behavior changes on the wire)

The 411 gate deliberately supersedes undeclared-body behavior: `CapChunkedOverrun_CutAtLimit` became `CapChunkedOverrun_UndeclaredNow411` (the declared lying-small mid-stream cut stays pinned by ContentLengthSpoof; declared honestly-over-cap by CapRejectedLocally). `Streams_WithoutFullBuffering` now DECLARES via a fixed-boundary measuring pass (streaming itself unchanged — the producer still blocks until agentd consumes); `ClientDisconnect` declares plausibly (its subject is abort propagation, not the gate). `doUpload` propagates known lengths through opaqueReader (declared = the honest-client default; undeclared tests use length-less readers).

### The §6 stress harness (`local/us-1500-upload-stress-e2e.sh` + pin test + nightly + smoke)

Rows: SR-1 measured residency (gauge sampler pins max staging_bytes AND max_reserved ≤ 48 MiB across a 6×24MiB storm — gauge pins, not code-path asserts), SR-2 isolation (forced resync mid-storm; spawnedRev advances; credential_bytes never regresses), SR-4 disk edges (in-workspace fill to the margin; 507 dest_disk_full), SR-5 kills (three phases; destination .tmp reclaim), SR-6 latency baseline + the §6.6 clause-(B) precondition by direct substitution with EXPLICIT skip-DOWN to 3×. SR-3 (backpressure) and all staging-dependent rows skip-DOWN LOUDLY until the PR 1/2 lane lands (mechanism detection: the staging gauge's absence). The script carries the #1474-r4 no-subshell api() contract verbatim; the pin suite holds the rows; the ExecuteSmoke table gains the row (its depth pin is the shim-stable skip line — SR-A legitimately fails under shims, which the smoke's fail-gate branch absorbs).

---

## Key Decisions

1. **Forward-verbatim + label-locally**: the client sees agentd's body byte-for-byte; the reason field is consumed ONLY for metrics. A parse failure degrades the label, never the response.
2. **Harness runs against DEFAULTS** — the §6 budget/429 examples assume the default 48MiB/4 on the 96 MiB tmpfs, so no knob override is needed on the kind cluster.
3. **GAP flagged (not built, per lane boundaries)**: the values-file knobs have NO deploy plumbing — nothing today flows env from helm/controller to the workspace pod's agentd container (no pass-through exists in pod_builder; the knobs act via env on a pod neither the chart nor the controller config reaches). PR 1 carries the parsing; the plumbing (controller config + pod env) is unassigned cross-cutting surface — flagged to the orchestrator for routing, not silently built here.

---

## Blockers

None. The knob-plumbing gap (above) and the SR-3 fault-seam dependency (skip-DOWN by design) are recorded, not blocking.

---

## Tests Run

- RED-first: 10 failing (the six shape subtests + unknown-reason + 411 + header + more) before implementation.
- `go test ./api/internal/handlers/ ./api/internal/services/metrics/ ./local/` — ALL green (handlers 120s full suite incl. the reworked streaming/cap tests; metrics; local incl. the new pin suite + smoke).
- `go build ./api/...` ok; `go vet`, gofmt, goimports, golangci-lint (new-from-rev) — clean.
- Mutation (copy-based): removing the forwarding case's verbatim pass-through (replace c.Data with a fixed 502) fails the shape table — verified during development.

---

## Next Steps

- Review loop to APPROVED; ping the PR 1/2 lane for the promised cross-check of the code map.
- When PR 1/2 merges: the skip-DOWN rows activate on the next nightly automatically (gauge detection); SR-3 needs their fault seam.

---

## Files Modified

- `api/internal/handlers/uploads.go` — forwarding arm, reason parser, 411 gate, header
- `api/internal/handlers/uploads_forwarding_test.go` — new: the shape table + gates
- `api/internal/handlers/uploads_test.go` — recording extended (declared-body capture); design-superseded test reworks; doUpload declaration semantics
- `api/internal/services/metrics/metrics.go` — enum + help widened
- `local/us-1500-upload-stress-e2e.sh` — new: the §6 harness
- `local/us_1500_upload_stress_script_test.go` — new: structural pins
- `local/e2e_smoke_repo_wide_test.go` — smoke row
- `.github/workflows/e2e-nightly.yml` — harness registration
- `worklogs/NNNN_2026-09-21_uploads-api-forwarding-stress.md` — this worklog

## Review Round 1 (nine harness defects — all fixed; the handler was already sound)

The reviewer reproduced every defect by execution. All nine fixed in the harness rewrite:

1. **SR-A 415-vs-411**: the row sent JSON content-type — the media gate 415s before the 411. Now sends multipart content-type with a length-less body (the shape the handler actually 411s).
2. **grep -c line-counting**: `storm_report()` now iterates result FILES (one per upload, written by concurrent jobs) — terminal counting is per-outcome.
3. **Vacuous gauge sampler**: the background subshell's variables never reached the parent. The sampler now writes to a FILE; the parent aggregates with awk.
4. **statf → stat -f**: corrected (statf doesn't exist); TOTAL/BLOCK unified into the fill math.
5. **SR-5 triple-defect**: container-ID kill via `crictl ps -q --name agentd` (pod name never worked); the tautological `-ge 0` assertion replaced by terminal-outcome checking; the partial-visibility assertion now counts `.tmp` artifacts (the real §6.5 surface).
6. **SR-6 constant**: the check, the message, and the pin now ALL say the design's C+94 MiB (98_560_614 bytes).
7. **trap clobber**: the sampler kill + fill cleanup ride the SAME EXIT trap as workspace cleanup — one cleanup function, no clobbering.
8. **SR-2 not-mid-storm**: the three uploads now run CONCURRENTLY and the resync fires 1s in (uploads still staging); outcomes asserted via storm_report.
9. **Concurrency absent**: SR-1 fires all six uploads concurrently (backgrounded with staggered starts); SR-2 likewise.

The pin suite was hardened alongside (file-backed sampler needle, concurrent-fire needle, container-ID needle, stat -f needle + a NotContains for statf, terminal-outcome needles); three stale needles from the first draft's text cost several edit cycles (the recurring needle-alignment lesson: write needles FROM the final script text, never from the draft that produced them).

## Review Round 2 (six more — the deepest catch: broken awk numerics)

1. SR-A sent random bytes with a multipart content-type — the file-part LOCATOR 400s before the 411. The row now sends WELL-FRAMED multipart (boundary + file-part headers + 256 bytes + closing boundary) with no declared length — the exact shape the handler 411s (verified by the reviewer AND by TestUpload_DeclaredBodyGate_ChunkedClientBody_411).
2. `98_560_614` in awk: gawk has no underscore digit separators — lexed as string "98" (unset-var concat), making the comparison lexicographic. AND the number was wrong (94 MiB = 98,566,144, not 98,560,614). Now `98566144` plain.
3. The precondition read /workspace (GB-scale PVC) — vacuous. Now reads /sandbox-runtime (the 96 MiB tmpfs the clause-B admission actually conditions on).
4. SR-2 hit a nonexistent route (/me/workspaces/:id/reload-secrets → 404, discarded). Correct route (/api/v1/workspaces/:id/reload-secrets); rev BEFORE captured; the "advanced" assertion now distinguishes advanced-vs-held (a no-op resync legitimately holds the rev).
5. SR-5's terminal set omitted 502 (the API's transport mapping for a killed agentd) — inverted failure mode. Added; the .tmp assertion is TTL-honest (bounded ≤ kills, not zero — the default TTL is 15 min per §4.3).
6. §6.6's "concurrency" was serial. Now: a genuinely concurrent wall-clock storm + the 5th-concurrent-429 boundary row.

Two unit tests added (the r1-carried gaps): unparseable-body 507 (the Unmarshal failure branch — forwards verbatim, labels agentd_error) and the >4 KiB truncation pin (bounded read; the reason field at the body's END is cut → parse fails → agentd_error — the documented pairing).

## Review Round 3 (five assertion-binding findings)

1. SR-6's boundary check `*"refused="*` matched every well-formed report (refused=0 included) — now the COUNT is parsed (sed) and asserted `-ge 1`.
2. SR-2's route-fired check was in the comment but not the code — `api_status == 200` now asserted after the resync POST.
3. SR-5's §6.5 PRIMARY invariant (no non-.tmp partial) is now a real pre-kill vs post-settle listing comparison (comm -13 on the sorted listings).
4. SR-6's regression guard is an ASSERTION: concurrent wall ≤ 2 × single × count.
5. storm_report prints `total=` and every consumer checks it — a storm losing result files fails completeness.
All pins aligned; three needle-alignment cycles on the refused-count pin (backtick-escaping in raw strings — the recurring lesson).
