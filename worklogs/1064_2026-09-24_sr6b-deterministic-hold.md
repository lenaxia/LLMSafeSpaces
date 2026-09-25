# Worklog: SR-6B made deterministic — trickled holders force the admission overlap the racy row hoped for

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
2. **Settle margin** (3s): orders the probe inside the 16s window's steady state — NOT the determinism (the trickled bodies are).
3. **The 5th, full speed — status AND body**: the literal 429 must carry reason `staging_busy` (the API forwards agentd's 429 body verbatim; the API's global rate limiter also emits status-identical 429s on /uploads — status-only assertion regressed r4's discrimination lesson). Self-verifying post-hoc: if the holders failed to hold, the 5th delivers 201 and the row fails loud.
4. **Holders all deliver 201** after their trickles finish (a non-201 holder means the window closed early — failed explicitly).
5. **Retry-after-release** (§4.2: "clean, retryable"): a full-speed upload AFTER the holders finish must deliver 201 — the slot reopened.
6. The pass condition requires ALL of: fifth=429∧staging_busy, holders-ok=1, retry=201.

### r1 review round (both findings + the minor)

- **The gauge gate was tick-luck, CUT**: the original gated "held" on `workspace_agentd_upload_staging_reserved_bytes` — push-only on the sweep's 10-minute tick (`RecordGauges` from the sweep loop, upload_staging.go:533), frozen ≈boot inside the row's 16s window. Replaced with the LIVE signal: the 5th's own body (above). No product work needed (the alternative — pushing gauges on Admit/Release — remains available if a future row wants a read-before-fire gate).
- **The pin suite was RED, fixed**: the structural pins still carried the deleted `SR6B_HAS_429`; replaced with needles for the new shape (`--limit-rate 64k`, `SR6B_5TH_BUSY`, `staging_busy`, `upload_bytes_with_body`, `SR6B_HOLDERS_OK`, `SR6B_RETRY`, the precondition gate). The r1 lesson on my own validation: the smoke-filter (`TestHarnessExecuteSmoke_RepoWide/us`) did not cover the pin suite — this round ran the FULL `./local/` package.
- **Kill-orphans**: the r1 cut killed already-waited PIDs (dead, recycled-PID hazard) — r2 deleted the loop entirely (the holders are waited before any fail branch; the row's failure already ended their scope).

### r2 review round (all four findings)

- **The retry capture was red-on-arrival**: `upload_bytes`' outfile form writes the file INSTEAD of printing (args-silence), so the r1 stdout-capture left `SR6B_RETRY` always-empty — the pass condition was unreachable. Fixed by UNIFORM capture semantics (r2's suggestion): the 5th and the retry both read their status from the res files (`SR6B_STATUS=$(cat res-5)`, `SR6B_RETRY=$(cat res-retry)`), pinned by needles so the stdout form can't silently return.
- **Self-contradicting comment**: the History-II block still advertised "VERIFIED HELD via the reserved_bytes gauge" against the r1-correction note; both trimmed to what stands.
- **Dead kill loop deleted**: the failure path killed PIDs it had already `wait`ed (recycled-PID hazard, no effect).
- **Documentation swept**: worklog title/Key-Decisions/Files-Modified and the PR title/body no longer reference the cut gauge mechanism; one expected-nightly-line.
- `go test -count=1 ./local/` — ok (19s).

---

## Key Decisions

- **Client-side throttle, no product seam.** The orchestrator's fallback (a copy-throttle injection seam) is unnecessary: §4.1's reservation-before-acceptance + curl's --limit-rate already give a controllable hold window through the REAL full stack (API → agentd → stager), exercising the true admission path.
- **1MiB holders, not 10MiB**: keeps reserved+5th at 14MiB ≤ budget so ONLY the cap can bind — the row cannot be satisfied by a budget 507 (the r4 lesson), and the hold arithmetic (1MiB/64k ≈ 16s) stays far from every deadline on the path.
- **The held verification is the 5th's own body** (the handler pin's hold-4-then-5th shape ported): a reserved-gauge gate was cut in r1 — the gauge pushes on the sweep's 10-min tick only, so a read-before-fire gate was tick-luck.

---

## Blockers

None.

---

## Tests Run

- `bash -n local/us-1500-upload-stress-e2e.sh` — syntax ok.
- `go test -count=1 ./local/` — ok, the FULL package (31s; the r1-cut smoke filter masked the red pin suite — not repeated).
- Expected next-nightly line: `SR-6: 5th-concurrent 429 boundary observed DETERMINISTICALLY (4 trickled holders; 5th=429/staging_busy; retry-after-release delivered; holders-ok=1 fifth=429 fifth-busy=1 retry=201)`.

- Full-stack proof: the next nightly (the row runs in kind; the local environment has no kind cluster). The expected line is line 70 above — the ONE expected line (r3 deleted the stale duplicate that sat here still advertising the cut "gauge-verified held" mechanism).

---

## Next Steps

- Watch the next nightly's SR-6B line; the SR-6 latency SKIP-DOWN (#1539 known issue) is a separate designed skip and unaffected.
- #1564 (the #1561 parse-boundary PR) is parked at r2-remediation-pushed awaiting review; resume on orchestrator signal.

---

## Files Modified

- `local/us-1500-upload-stress-e2e.sh` — the SR-6B row: trickled holders + the 5th's 429/staging_busy body assertion + retry-after-release; the failure history in the comment
- `worklogs/1064_2026-09-24_sr6b-deterministic-hold.md` — this worklog
