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
- The M2/M4 step first landed 2026-09-25 (14667878, PR #1566) and failed **6/6 executions** with the identical signature (`e2e07250-…-0001` stuck `Creating`; pod Pending; `FailedScheduling: 0/2 nodes are available: 1 Insufficient cpu, 1 node(s) had untolerated taint(s)`):
  - 36135708380, 36148719177 (09-25) — pre-v0.34.10 AND pre-v0.34.11
  - 36216981147, 36246212235 (09-26) — pre-both
  - 36459801703 (09-28) — post-v0.34.10, pre-v0.34.11
  - 36740521434 (09-30) — post-v0.34.11
  - (36593820056, 09-29, cancelled upstream of the step.)
- The reconcile is healthy in the failing run: CR status `CredentialsStaged=True` ("1 provider(s) staged at revision r9085d8bbf8c1", keyID hpke-g1) with `lastTransitionTime 16:49:15Z` — one second after the step's 16:49:14 start. Nothing in the secrets-policy path blocks or delays creation.

### 2. Root cause — the 2-node nightly cluster has ONE schedulable node

- Run 36740521434's failure dump (`kubectl get pods -o wide`): ALL pods — 7 standing workspace pods + api ×2 + controller + postgres + valkey + mock-llm — on `llmsafespaces-ci-worker`; zero non-static pods on the control-plane; the scheduler event names an untolerated taint on the other node.
- Standing workspace pods at M2/M4 start: 5 × us-70 secret-delivery suite (`e2e5d000-…-006..010`), 1 × drill (`e2e72500-…-1`), 1 × sweep (`e2e72600-…-1`). Each requests 500m CPU (workspace container default, `controller/internal/workspace/pod_builder.go:561`; the agentd sidecar sets no requests) = 3.5 CPU, plus infra requests, against ~4 CPU allocatable on the hosted runner's single schedulable node. The 8th workspace's 500m cannot fit → Pending forever → `wait_phase … 300` expires → `✗ setup: workspace never Active`; the controller's >5-min unschedulable recovery fires at 16:54:16.
- **Why one schedulable node:** the run-35550849959 adjudication (1fb6bb70, 09-20) added a worker to "double the allocatable ceiling" — but kind only strips the control-plane taint on SINGLE-node clusters: [kind v0.32.0 `kubeadminit/init.go:124-155`](https://github.com/kubernetes-sigs/kind/blob/v0.32.0/pkg/cluster/internal/create/actions/kubeadminit/init.go#L124) runs `kubectl taint nodes --all node-role.kubernetes.io/control-plane-` under "if we are only provisioning one node". Verified empirically: Sept 19 run 35437562027 (single-node era) shows everything on `llmsafespaces-ci-control-plane`; every 2-node run shows everything on the worker.
- Assumptions stated and validated (Rule 7):
  - "kind v0.32.0 keeps multi-node control-planes tainted" — validated from the pinned version's source (above) + the scheduler's own event + pod placement across runs.
  - "the kubeadm patch form works" — validated from the pinned version's source: patches match by `kind`, missing `apiVersion` is an explicit wildcard ([`resource.go:66-70`](https://github.com/kubernetes-sigs/kind/blob/v0.32.0/pkg/internal/patch/resource.go#L66)); the standard merge patch applies `taints: []` as a replace; kubeadm v1beta4 documents the explicit empty slice as the untaint mechanism ([`types.go:256-259`](https://github.com/kubernetes/kubernetes/blob/v1.35.5/cmd/kubeadm/app/apis/kubeadm/v1beta4/types.go#L256): "If you don't want to taint your control-plane node, set this field to an empty slice, i.e. `taints: []`").
  - "500m per workspace pod" — validated: `seed_workspace` (local/lib/us70-common.sh:277) sets no resources; the pod describe in the run log shows QoS Burstable consistent with 500m/2000m.
  - Not validated mechanically (no kind on this pod): the green nightly run. The fix's effect is pinned structurally instead; the next nightly is the live verification.

## Fix

`local/kind-cluster-nightly.yaml`: append an `InitConfiguration` kubeadm patch registering the control-plane with `nodeRegistration.taints: []` — delivering the doubling the 09-20 retirement intended (both nodes schedulable → ~8 CPU requests-budget vs ~4.5 standing at M2/M4 time). Header comment corrected to record the real history (the old comment claimed the doubling unconditionally). The patch is byte-equivalent in effect to what kind itself does for single-node clusters — just declarative and version-independent of kind's internal single-node special case.

Red-first pin: `TestNightlyKindConfig_ControlPlaneUntainted` in `local/kind_cluster_config_test.go` — requires the nightly config to carry the untaint patch (indentation-tolerant regexp, since the patch lives inside a `- |` literal block) and requires the SHARED `local/kind-cluster.yaml` to NOT carry it (blast radius: kind's single-node path already strips the taint; the pool's topology stays as calibrated). Observed red before the config change, green after; the file's existing pins (`TwoNodes` dind-escape strings, `StaysSingleNode`, `UsesNightlyKindConfig`) all still green.

## Out of scope (visible in the same runs, not this lane)

- SR-6 upload-stress row failure (step 18) — gated on #1518/#1524 landing per the workflow's own comment.
- The Sept-25 sweep K1 rows (36148719177).
- Steps 20-28 of the 09-30 run were skipped after step 18 failed (not armed with `!cancelled()`), which also means those legs' coverage vanished tonight — worth a look by whoever owns #1541 arming policy, but not a defect in this fix's scope.

## Adversarial self-review (Phase 1-3)

- "Untainting invites control-plane starvation from workspace pods on a 4-real-CPU runner": real trade, but identical to the pre-09-20 single-node posture (everything shared one node then) and explicitly accepted by the adjudication ("The wall is requests-vs-allocatable, not real CPU"). Documented, not changed.
- "Might kind silently drop a patch whose doc has only comments before `kind:`": YAML comments are legal inside the patch string; the parsed doc is exactly kind's documented example shape. Validated by parsing the file (yaml.safe_load) — patch #2 round-trips to the intended document.
- "Why not clean up earlier legs' workspaces instead?" — rejected: touches three other epics' harnesses, changes leg end-states, adds PVC-teardown latency, and the next appended leg re-hits the wall. The adjudication history already chose capacity for this wall class; this change makes that choice actually work.
- "Shared config drifting": pinned absent by the new test's second arm.
