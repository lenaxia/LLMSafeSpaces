# Worklog: #1604 — nightly M2/M4 "workspace never Active": capacity wall, not a v0.34.11 regression

**Date:** 2026-09-30
**Session:** TRIAGE lane (agent-w7, orchestrator workspace d8bed486). Nightly run 36740521434, step "Run the M2/M4 migration-fallback e2e (design 0061 §10)" — `✗ setup: workspace never Active`, suspected v0.34.11 secrets-policy reconcile regression (#1597/ac82cc1f).
**Status:** Complete (pending review)

---

## Objective

Determine whether v0.34.11's global-default secret-policy reconcile broke the workspace-creation path on main (the workspace never reached Active on the kind nightly), and fix per root cause.

## Verdict

**Not a v0.34.11 regression.** The failure is a requests-vs-allocatable capacity wall on the nightly's kind topology, exposed by a step that has never passed. The reconcile is innocent — see evidence.

## Evidence trail (all remote-verifiable; this pod has no docker/kind, so the run logs ARE the reproduction)

### 1. The regression question — settled by bisect-by-history

- v0.34.11 tagged 2026-09-29 03:12Z (`git log -1 --format=%ci v0.34.11`); v0.34.10 tagged 2026-09-27 17:26Z.
- The M2/M4 step first landed 2026-09-25 (14667878, PR #1566) and failed **7/7 executions** with the identical signature (`e2e07250-…-0001` stuck `Creating`; pod Pending; `FailedScheduling: 0/2 nodes are available: 1 Insufficient cpu, 1 node(s) had untolerated taint(s)`):
  - 36135708380, 36148719177 (09-25) — pre-v0.34.10 AND pre-v0.34.11
  - 36216981147, 36222755701, 36246212235 (09-26) — pre-both
  - 36459801703 (09-28) — post-v0.34.10, pre-v0.34.11
  - 36740521434 (09-30) — post-v0.34.11
  - (36593820056, 09-29, cancelled upstream of the step; 36214493133, 36326908065 skipped — the drill chain's own failures.)
  - Six of the seven predate the v0.34.11 tag; only 36740521434 postdates it. (Count corrected in the r1 review round — the original record said 6/6 and "three predate"; a mechanical re-tally of every nightly run's step conclusions found the 09-26 06:06 run 36222755701 omitted from the original table. Tally re-run via the Actions jobs API over all runs ≥ 09-25.)
- The reconcile is healthy in the failing run: CR status `CredentialsStaged=True` ("1 provider(s) staged at revision r9085d8bbf8c1", keyID hpke-g1) with `lastTransitionTime 16:49:15Z` — one second after the step's 16:49:14 start. Nothing in the secrets-policy path blocks or delays creation.

### 2. Root cause — the 2-node nightly cluster has ONE schedulable node

- Run 36740521434's failure dump (`kubectl get pods -o wide`): ALL pods — 7 standing workspace pods + api ×2 + controller + postgres + valkey + mock-llm — on `llmsafespaces-ci-worker`; zero non-static pods on the control-plane; the scheduler event names an untolerated taint on the other node.
- Standing workspace pods at M2/M4 start: 5 × us-70 secret-delivery suite (`e2e5d000-…-006..010`), 1 × drill (`e2e72500-…-1`), 1 × sweep (`e2e72600-…-1`). Each requests 500m CPU (workspace container default, `controller/internal/workspace/pod_builder.go:561`; the agentd sidecar sets no requests) = 3.5 CPU, plus infra requests, against ~4 CPU allocatable on the hosted runner's single schedulable node. The 8th workspace's 500m cannot fit → Pending forever → `wait_phase … 300` expires → `✗ setup: workspace never Active`; the controller's >5-min unschedulable recovery fires at 16:54:16.
- **Why one schedulable node:** the run-35550849959 adjudication (1fb6bb70, 09-20) added a worker to "double the allocatable ceiling" — but kind only strips the control-plane taint on SINGLE-node clusters: [kind v0.32.0 `kubeadminit/init.go:124-155`](https://github.com/kubernetes-sigs/kind/blob/v0.32.0/pkg/cluster/internal/create/actions/kubeadminit/init.go#L124) runs `kubectl taint nodes --all node-role.kubernetes.io/control-plane-` under "if we are only provisioning one node". Verified empirically: Sept 19 run 35437562027 (single-node era) shows everything on `llmsafespaces-ci-control-plane`; every 2-node run shows everything on the worker.
- Assumptions stated and validated (Rule 7):
  - "kind v0.32.0 keeps multi-node control-planes tainted" — validated from the pinned version's source (above) + the scheduler's own event + pod placement across runs.
  - "the kubeadm patch form works" — validated from the pinned version's source: patches match by `kind`, missing `apiVersion` is an explicit wildcard ([`resource.go:66-70`](https://github.com/kubernetes-sigs/kind/blob/v0.32.0/pkg/internal/patch/resource.go#L66)); the standard merge patch applies `taints: []` as a replace; kubeadm documents the explicit empty slice as the untaint mechanism. Governing schema: kind v0.32.0 selects `ConfigTemplateBetaV3` for node images < v1.36.0 ([`kubeadm/config.go:674-678`](https://github.com/kubernetes-sigs/kind/blob/v0.32.0/pkg/cluster/internal/kubeadm/config.go#L674)), so the nightly's v1.35.5 cluster runs **v1beta3** — and v1beta3's `NodeRegistrationOptions.Taints` ([`v1beta3/types.go:221-224`](https://github.com/kubernetes/kubernetes/blob/v1.35.5/cmd/kubeadm/app/apis/kubeadm/v1beta3/types.go#L221)) carries the guidance verbatim: "If you don't want to taint your control-plane node, set this field to an empty slice, i.e. `taints: []`" (v1beta4 carries textually identical wording — irrelevant here today, relevant if the node image moves to v1.36+). (Citation corrected in the r1 review round — the original record named v1beta4 as the governing schema.)
  - "500m per workspace pod" — validated: `seed_workspace` (local/lib/us70-common.sh:277) sets no resources; the pod describe in the run log shows QoS Burstable consistent with 500m/2000m.
  - Not validated mechanically (no kind on this pod): the green nightly run. The fix's effect is pinned structurally instead; the next nightly is the live verification.

## Fix

`local/kind-cluster-nightly.yaml`: append an `InitConfiguration` kubeadm patch registering the control-plane with `nodeRegistration.taints: []` — delivering the doubling the 09-20 retirement intended (both nodes schedulable → ~8 CPU requests-budget vs ~4.5 standing at M2/M4 time). Header comment corrected to record the real history (the old comment claimed the doubling unconditionally). The patch is byte-equivalent in effect to what kind itself does for single-node clusters — just declarative and version-independent of kind's internal single-node special case.

Red-first pin: `TestNightlyKindConfig_ControlPlaneUntainted` in `local/kind_cluster_config_test.go` — parses the config as YAML and requires a `kubeadmConfigPatches` document EXACTLY equal to `{kind: InitConfiguration, nodeRegistration: {taints: []}}` (apiVersion absent — kind silently discards non-matching patches, so an explicit one drifting from the generated template would no-op the fix; `taints` an explicit empty list — `null` re-defaults to the control-plane taint; superseded shape: the r0 pin was an indentation-tolerant text regexp, tightened in the r1 round) and requires the SHARED `local/kind-cluster.yaml` to carry NO `InitConfiguration` patch at all (blast radius: kind's single-node path already strips the taint; the pool's topology stays as calibrated). Observed red before the config change, green after; negative red-checks for the drift shapes (apiVersion added, `taints: null`, pre-fix config) each fail as designed; the file's existing pins (`TwoNodes` dind-escape strings, `StaysSingleNode`, `UsesNightlyKindConfig`) all still green.

## Out of scope (visible in the same runs, not this lane)

- SR-6 upload-stress row failure (step 18) — gated on #1518/#1524 landing per the workflow's own comment.
- The Sept-25 sweep K1 rows (36148719177).
- Steps 20-28 of the 09-30 run were skipped after step 18 failed (not armed with `!cancelled()`), which also means those legs' coverage vanished tonight — worth a look by whoever owns #1541 arming policy, but not a defect in this fix's scope.

## Adversarial self-review (Phase 1-3)

- "Untainting invites control-plane starvation from workspace pods on a 4-real-CPU runner": real trade, but identical to the pre-09-20 single-node posture (everything shared one node then) and explicitly accepted by the adjudication ("The wall is requests-vs-allocatable, not real CPU"). Documented, not changed.
- "Might kind silently drop a patch whose doc has only comments before `kind:`": YAML comments are legal inside the patch string; the parsed doc is exactly kind's documented example shape. Validated by parsing the file (yaml.safe_load) — patch #2 round-trips to the intended document.
- "Why not clean up earlier legs' workspaces instead?" — rejected: touches three other epics' harnesses, changes leg end-states, adds PVC-teardown latency, and the next appended leg re-hits the wall. The adjudication history already chose capacity for this wall class; this change makes that choice actually work.
- "Shared config drifting": pinned absent by the new test's second arm.

## Corrections (r1 review round, PR #1605 CHANGES_REQUESTED — record accuracy)

The reviewer verified the mechanics end-to-end and found the evidence trail itself wrong in two places; both corrected in this round:

1. **Governing kubeadm schema: v1beta3, not v1beta4.** kind v0.32.0 selects `ConfigTemplateBetaV3` for node images < v1.36.0 (`kubeadm/config.go:674-678`); the nightly's v1.35.5 image is v1beta3. The conclusion was unaffected (v1beta3's `Taints` doc is textually identical), but the original record cited the v1beta4 types file as governing — misleading for the next reader, e.g. at a v1.36+ node-image bump where kind really does switch templates and the v1beta4-gated `extraArgs` merge branch differs. Corrected above and in the PR/issue bodies.
2. **Run count: 7/7, not 6/6; six (not three) predate the v0.34.11 tag.** The original table omitted run 36222755701 (2026-09-26T06:06, step failed) — counted from recall instead of re-tallying. Re-tallied mechanically (Actions jobs API, every nightly run ≥ 09-25): 7 executions, 7 failures, 6 pre-tag. The disproof is stronger than originally claimed.
3. **Pin tightened** (reviewer's missing-test note, adopted as cheap): `TestNightlyKindConfig_ControlPlaneUntainted` now parses the YAML and asserts the patch document under `kubeadmConfigPatches` is EXACTLY `{kind: InitConfiguration, nodeRegistration: {taints: []}}` — apiVersion ABSENT and taints an explicit empty list. Rationale: kind v0.32.0 silently discards non-matching patches (`kubeyaml.go` drops the `matches` bool), so a text pin stays green if someone "corrects" the patch to declare an explicit apiVersion that misses the generated template. Negative red-checks exercised: apiVersion drift → FAIL; `taints: null` → FAIL; pre-fix config → FAIL; restored config → green. The already-pushed commit message's "three of the six" line is immutable without a force-push (forbidden); the PR body supersedes it.
