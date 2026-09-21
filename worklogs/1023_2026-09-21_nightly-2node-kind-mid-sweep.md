# Worklog: Nightly class retirement — 2-node kind + AC-11 mid sweep (run 35550849959 adjudication)

**Date:** 2026-09-21
**Session:** Adjudicated BOTH: the 2-node kind lever (three consecutive one-node capacity walls — AC-1c, AC-13 wave 2, AC-11 — is a class retirement) PLUS the AC-17-era five-workspace verified sweep before AC-11. Lane claimed at issuecomment-5754640213. A compute force-refresh cut the workspace mid-lane; resumed with pins green and work intact (nothing uncommitted was lost — the branch was mid-implementation, all changes re-verified after resume).
**Status:** Complete — PR open, iterating review

---

## Objective / Work Completed

1. **`local/kind-cluster-nightly.yaml` (NEW)** — the shared shape PLUS one worker node (same digest-pinned node image on both; identical serviceSubnet 10.217/iptables/certs.d settings). The nightly's single hosted node topped out at ~10 standing workspace pods of requests; the wall is requests-vs-allocatable (idle row workspaces stack without failing scheduling), so a second node doubles the ceiling. The SHARED `kind-cluster.yaml` stays single-node — the pool is calibrated on exactly that topology (dind nesting, #1244-class sensitivities) and the weekly attachments run shares it; a blast-radius pin enforces this.
2. **`.github/workflows/e2e-nightly.yml`** — cluster created from the nightly config, with the why-comment. The registry certs.d wiring already iterates `kind get nodes` (pinned so a future editor can't hardcode one node and strand the worker).
3. **`local/us-70-secret-delivery-e2e.sh`** — the mid sweep before AC-11: removes the AC-17/AC-F/Chaos legs' five standing row workspaces (ids 1-5, census-verified: they were exactly the margin ws-010 lacked) using the same verified machinery (typed delete, loud failure, bounded-poll verification, no unobserved-state claims, selection-failure = warn + state-unknown). AC-11 and later rows re-seed what they need.
4. Pins (TDD red→green): `kind_cluster_config_test.go` (≥2 nodes, control-plane+worker, same digest-pinned image on all nodes, shared-shape settings retained, shared config stays single-node — blast radius; nightly workflow uses the nightly config + node-generalizing loop) and the mid-sweep structural + executable trio (clean verified-gone / wedged dies / nothing-standing states exactly that) on the grammar-enforcing fake kubectl.

### Key decisions

1. **Nightly-specific config over editing the shared one** — the pool's calibrated single-node dind topology must not be re-topologized by a nightly-lane change; enforced by `TestSharedKindConfig_StaysSingleNode`.
2. **Worker node, not a second control-plane** — the API lives on the control plane (extraPortMappings stay there); a worker doubles allocatable with zero HA semantics to reason about.
3. **Mid sweep as a third inline block** (not a shared function): the three sweeps differ in range, message, and failure policy (pre-wave dies / mid dies / post warns); a parameterized helper would speculate abstraction over exactly one stable shape per site (Rule 4). All three carry the identical verified machinery, and the executable pins prove each.
4. **p95 soft-budget observation** — NOT fixed here, per adjudication; flagged in the PR body for the harness owner (AC-13 printed ✓ with p95=33323ms against P95_BUDGET_MS=30000).

### Assumptions stated and validated (Rule 7)

- The wall is requests-vs-allocatable — from three runs' FailedScheduling evidence + idle-standing census pods.
- The nightly's registry wiring generalizes to N nodes — verified: the post-create loop iterates `kind get nodes`; `kind load docker-image` loads all nodes; pinned by test.
- ids 1-5 are the complete standing set at AC-11 — from run 35550849959's census (0001-0005); later rows re-seed via seed_workspace's pre-clean.

---

## Blockers

None.

## Tests Run

- New pins RED pre-implementation → GREEN post (kind-config trio + mid-sweep structural + executable trio).
- `go test -count=1 -timeout 300s ./local/` — **ok** (26.6s).
- `bash -n` clean; both YAML files validated.

## Next Steps

- APPROVED → orchestrator merges + dispatches → expected: first COMPLETE us-70 run in history (AC-11/AC-5/AC-6/AC-4 on 2 nodes with real sweeping) → the four-run-blocked cascade (R1–R9 + every remaining suite).

## Files Modified

- `local/kind-cluster-nightly.yaml` — NEW, 2-node nightly topology.
- `.github/workflows/e2e-nightly.yml` — nightly config + why-comment.
- `local/us-70-secret-delivery-e2e.sh` — the AC-11 mid sweep.
- `local/kind_cluster_config_test.go` — NEW, kind-config + blast-radius + wiring pins.
- `local/us70_harness_script_test.go` — mid-sweep structural + executable pins.
- `worklogs/1023_2026-09-21_nightly-2node-kind-mid-sweep.md` — this worklog.
