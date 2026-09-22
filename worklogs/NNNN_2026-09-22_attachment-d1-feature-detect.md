# Worklog: Attachment D1 gate — feature-detect for design 0060 (run 35697148238)

**Date:** 2026-09-22
**Session:** The scoreboard dispatch found the D1 contract superseded: design 0060's full stack (#1515 staging + #1516 forwarding + #1518 supervisor apply) landed between dispatches and sidecar uploads now SUCCEED. The D1 clean-fail assertion was the pre-0060 contract. Adjudicated: feature-detect (my recommendation, endorsed).
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

The gate now distinguishes THREE upload shapes:
- **201** → design 0060 landed (stage-and-signal works) — falls through and runs E2/E10/E11 FULLY in sidecar mode (the new design's coverage, gained for free)
- **502/503** → pre-0060 clean-fail (the proxy/sidecar's DESIGNED upload-unavailable response — the old D1 contract) — loud skip, no files, exit 0
- **anything else** (500, 000, 4xx, corrupt) → hard die ("neither designed-success nor designed-clean-fail") — the detector distinguishes designed outcomes from broken ones, never absorbing the latter

The fleet is mid-transition (nightly=main has the 0060 stack; pool/weekly=deployed prod does not) — the feature-detect keeps the harness correct on BOTH surfaces and self-adapts when prod catches up. The weekly single-container workflow is unchanged.

Pins: the existing executable gate test extended from two legs to FOUR (pre-0060 503 → skip; 0060 201 → fall-through; broken 500 → die; single-container → fall-through). The old "upload accepted must die" D1 test is superseded by the 201 fall-through leg. Structural pins updated for the new gate shape.

### Key decision: 502/503 (not all 5xx) as the clean-fail class

500 IS 5xx — a blanket `5*` would absorb genuine internal errors into the designed-clean-fail skip. The pre-0060 sidecar's upload-unavailable response is specifically 502 (proxy) / 503 (sidecar) — those are the designed shapes; 500 is always broken.

---

## Blockers

None.

## Tests Run

- `go test -count=1 -timeout 300s ./local/` — **ok** (26.7s; includes the four-leg gate test + the ExecuteSmoke suite).
- `bash -n` clean.

## Files Modified

- `local/us-68-attachments-e2e.sh` — the three-way feature-detect gate.
- `local/us68_attachments_script_test.go` — the four-leg executable test + structural pins.
- `worklogs/NNNN_2026-09-22_attachment-d1-feature-detect.md` — this worklog.
