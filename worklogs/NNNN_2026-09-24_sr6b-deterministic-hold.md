# Worklog: SR-6B made deterministic — gauge-verified trickled holders force the admission overlap the racy row hoped for

**Date:** 2026-09-24
**Session:** The SR-6 nightly recurrence (run 36049595521, new shape delivered=5 refused=0) — the cap-precedence fix was correct but the e2e row was load-racy; this lane ports the handler-level pin's hold-4-then-5th shape to the full stack
**Status:** Complete

---

## Objective

Make the SR-6B row (design 0060 §6.6's characterized boundary: the 5th concurrent upload's literal 429) deterministic at full stack. The recurrence: 5 SIMULTANEOUS fires only produce >4 overlapping admissions when the runner is slow — on a fast runner each upload completes and releases before the 5th admits (delivered=5 refused=0, the cap never binds). Do NOT retune to "all delivered is fine" — §6.6's boundary must be exercised deterministically.

---

## Work Completed

### The triage (the row was load-racy; no product defect)

- Run 36049595521 (with #1563's precedence fix in-tree): `delivered=5 refused=0 has429=0` — the cap never bound because no admission windows overlapped. The prior runs' refusals were an artifact of slow applies holding reservations longer; the fast-runner world exposes the row's assumption ("5 simultaneous fires ⇒ overlap") as luck.
- Verdict: the ROW needed the deterministic seam; the product needs nothing (the handler-level pin `TestStagedUpload_CountCapBusy429` already proves the admission shape in isolation).

### The seam: NONE NEEDED — the trickled-holder shape

- Design §4.1's reservation-before-acceptance: a reservation is held from Admit until ack/abort — i.e., for the WHOLE body transfer. A client-trickled upload (`curl --limit-rate`) therefore holds its reservation for a runner-independent, client-controlled window.
- The timing budget verified against the product: agentd's `defaultUploadBodyTimeout` = 5 minutes (uploads.go:52); the API hop deliberately has NO server-level body timeout (app.go:1527-1535, SSE posture; ReadHeaderTimeout=10s touches headers only). A ~16s trickle is safely inside every deadline on the path.

### The row (replaces the 5-simultaneous-fire block)

1. **4 holders**: 1MiB bodies at `--limit-rate 64k` ≈ 16s hold each; 4×1MiB reserved + the 5th's 10MiB = 14MiB ≤ the 48MiB budget, so the COUNT CAP is the only clause that can bind the 5th (the boundary this row characterizes — not budget, not clause B).
2. **Gauge-verified held**: poll `workspace_agentd_upload_staging_reserved_bytes` until ≥ 4×1MiB (30s budget) — proof of four held reservations before the 5th fires, not hope. Window-failure fails the row loud.
3. **The 5th, full speed**: demand the LITERAL 429.
4. **Holders all deliver 201** after their trickles finish (they are valid uploads; a non-201 holder means the window closed early — failed explicitly).
5. **Retry-after-release** (§4.2: "clean, retryable"): a full-speed upload AFTER the holders finish must deliver 201 — the slot reopened.
6. The pass condition requires ALL of: held=1, fifth=429, holders-ok=1, retry=201.

---

## Key Decisions

- **Client-side throttle, no product seam.** The orchestrator's fallback (a copy-throttle injection seam) is unnecessary: §4.1's reservation-before-acceptance + curl's --limit-rate already give a controllable hold window through the REAL full stack (API → agentd → stager), exercising the true admission path.
- **1MiB holders, not 10MiB**: keeps reserved+5th at 14MiB ≤ budget so ONLY the cap can bind — the row cannot be satisfied by a budget 507 (the r4 lesson), and the hold arithmetic (1MiB/64k ≈ 16s) stays far from every deadline on the path.
- **The gauge check is the port of the handler pin's shape**: hold 4 (there: direct Admit; here: trickled bodies verified via the reserved gauge), THEN the 5th.

---

## Blockers

None.

---

## Tests Run

- `bash -n local/us-1500-upload-stress-e2e.sh` — syntax ok.
- `go test -count=1 -run 'TestHarnessExecuteSmoke_RepoWide/us' ./local/` — ok (the harness smoke gate).
- Full-stack proof: the next nightly (the row runs in kind; the local environment has no kind cluster). Expected: `SR-6: 5th-concurrent 429 boundary observed DETERMINISTICALLY (4 holders gauge-verified held; literal 429; retry-after-release delivered; ...)`.

---

## Next Steps

- Watch the next nightly's SR-6B line; the SR-6 latency SKIP-DOWN (#1539 known issue) is a separate designed skip and unaffected.
- #1564 (the #1561 parse-boundary PR) is parked at r2-remediation-pushed awaiting review; resume on orchestrator signal.

---

## Files Modified

- `local/us-1500-upload-stress-e2e.sh` — the SR-6B row: trickled holders + gauge verify + 5th-429 + retry-after-release; the two-round failure history in the comment
- `worklogs/NNNN_2026-09-24_sr6b-deterministic-hold.md` — this worklog
