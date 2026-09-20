# Worklog: Nightly AC-1d unblock — relay-router egress lever on the nightly helm install + trap-pin hardening

**Date:** 2026-09-20
**Session:** Triage-driven remediation (no issue — nightly run 35513961442's AC-1d failure, orchestrator-adjudicated): add the one egress lever the AC-1d row's design requires to the nightly's helm install, and fold in the queued trap-pin hardening from the #1486 (ea6146ac) merge verification.
**Status:** Complete — PR open, iterating review

---

## Objective

us-70 AC-1d ("credential-backed TURN resolves against a mock upstream, synchronous V1 route") timed out from the workspace container on every destination form (ClusterIP, endpoint pod-IP; `Connection timed out after 5001ms` ×3) while a plain pod got 200 and DNS resolved — the default workspace egress posture (blockedEgressCIDRs includes 10.0.0.0/8, "sandbox pods should never reach in-cluster services") doing its designed job. The row's own mock manifest documents its admission mechanism: relay-router labels riding the podSelector egress allow rendered by `networkPolicy.allowRelayRouterEgress`. The pool sets that lever and its AC-1d passes; the nightly set neither lever (the row — added in 75a3f802/#1313 — first became reachable in the nightly only after #1486 unblocked AC-1c). Consequence: every downstream suite (automation R1–R9, #1342, #1452, #1455, #1417, revisions, dev-preview, us-63, benchmarks) blocked three consecutive nightlies.

---

## Work Completed

- `.github/workflows/e2e-nightly.yml`: `--set networkPolicy.allowRelayRouterEgress=true` added to the helm install with a why-comment naming AC-1d and the evidence pair (nightly 35513961442 vs pool run 35509103926 — same day, same head-era code: nightly workspace=000000 plain-pod=200; pool workspace=200, AC-1d PASS).
- `local/us70_harness_script_test.go`: `TestUS70AC1D_MockEgressLeverInNightly` — pins the lever AND the why-comment (so it can't be "cleaned up" as unused).
- `local/us68_attachments_script_test.go`: trap pin hardened from substring to an uncommented line-start match (`(?m)^trap cleanup EXIT$`) — the #1486 merge verification caught that a commented `# trap cleanup EXIT` passes a substring pin; mutation-tested locally (comment the line → pin FAILS; restored).

### Lever choice (selector vs /32)

Selected `allowRelayRouterEgress=true` over the `extraEgressCIDRs` 10.217.200.200/32 pre-grant: (a) exact parity with the pool's established pattern (us-70-delivery-pool.yml:380) — one proven-in-CI behavior; (b) the mock pod carries the relay-router labels PRECISELY to ride this rule — the mechanism the row was authored around; (c) the /32 pins a kind-serviceSubnet literal into the workflow (fragile against kind-cluster.yaml changes) and covers only the ClusterIP form.

### Assumptions stated and validated (Rule 7)

- The pool's green AC-1d proves the lever suffices under the same sidecar-mode + cluster shape — verified from pool run 35509103926's log (workspace=200 on all probes, `AC-1d PASS`, `allowRelayRouterEgress=true` in its helm args).
- No other nightly row depends on RFC1918 egress staying closed for workspace pods — the lever admits only pods SELECTED as relay-router (the mock labels itself that way deliberately); no other in-cluster destination becomes reachable.
- The trap pin's mutation-resistance — proven empirically (comment mutation → FAIL), reverted cleanly.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s ./local/ -run 'TestUS70AC1D|TestUS68CleanupDeletesSeededWorkspaces'` — lever pin RED pre-fix, GREEN post-fix; trap pin green + mutation-red proven.
- `go test -timeout 300s ./local/` (full package) — **ok** (22.8s).
- `e2e-nightly.yml` YAML-validated.

---

## Next Steps

- Iterate review to APPROVED; orchestrator merges — effective from main for tomorrow's nightly (09:17Z slot + queue lag). Expected: AC-1d PASS → the full downstream cascade (R1–R9 first real arbitration incl. R8/R9 first-run, #1342, #1452, #1455, revisions, dev-preview, us-63, benchmarks) finally executes.
- Next watch: verify AC-1d, then arbitrate the newly-reached suites.

---

## Files Modified

- `.github/workflows/e2e-nightly.yml` — the egress lever + why-comment.
- `local/us70_harness_script_test.go` — the lever pin.
- `local/us68_attachments_script_test.go` — trap-pin hardening (line-start anchor).
- `worklogs/NNNN_2026-09-20_nightly-ac1d-egress-lever.md` — this worklog.
