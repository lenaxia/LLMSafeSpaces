# Worklog: Epic 68 — single-container e2e workflow for attachment rows E2/E10/E11

**Date:** 2026-08-27
**Session:** Close the last Epic 68 verification gap: the three cluster-level attachment e2e rows (E2 suspend/resume PVC persistence, E10 multi-tenant isolation, E11 pod-kill chaos) in `local/us-68-attachments-e2e.sh` had never executed — the nightly installs with `controller.agentdSidecar.enabled=true`, where uploads clean-fail by design (D1). Added a weekly + dispatch workflow that deploys the same chart in single-container mode so the script's sidecar gate passes through and the rows run.
**Status:** Complete

---

## Objective

Make E2/E10/E11 actually execute on a schedule + manual dispatch, without modifying the sidecar nightly: new `.github/workflows/e2e-attachments-single-container.yml` reusing the nightly's proven bootstrap verbatim (kind, images, cert-manager, Postgres/Redis, helm values) but installing with `controller.agentdSidecar.enabled=false`; minimal behavior-preserving touch-ups to the script's sidecar-gate messaging; validation (actionlint, helm render, bash -n); commit on `feat/epic-68-attachment-cluster-e2e`.

---

## Work Completed

### Workflow: e2e-attachments-single-container.yml (new)

- Triggers: `workflow_dispatch` + `schedule` Sundays 04:00 UTC (`0 4 * * 0`) — quiet slot, 2h clear of the daily e2e-nightly (06:00 UTC) and clear of the Monday 05:00 UTC US-2 L3 kind run.
- `concurrency: group: ${{ github.workflow }}` (no cancel-in-progress → manual dispatch queues behind an in-flight scheduled run; too expensive to run twice or cancel mid-flight).
- `permissions: contents: read` (checkout only; mirrors secrets-integration.yml convention).
- Bootstrap copied verbatim from the nightly: checkout@v6, setup-go@v6 (1.26.6, cache), kind v0.32.0 install, helm install script, `kind create cluster --config local/kind-cluster.yaml`, three docker builds with VERSION/COMMIT_SHA stamping, ghcr.io/lenaxia retag for the RuntimeEnvironment webhook allow-list, kind load ×3, cert-manager v1.16.0, credentials Secret before Postgres, `local/postgres-redis.yaml`, same helm values, rollout waits, failure-state dump, `if: always()` kind teardown. `timeout-minutes: 30` (nightly does strictly more work in the same budget).
- **Values diff vs nightly (the entire diff):** `controller.agentdSidecar.enabled=false` (set explicitly — it is today's default but the values.yaml migration-state note says the default flips once sidecar L3 passes; pinning keeps this workflow single-container regardless) and NO `controller.agentdDelivery.image`/`binarySHA256*` sets.
- **Omitted nightly steps, with justification:**
  - Local `registry:3` container + certs.d hosts.toml alias + `docker network connect` — existed solely for digest-pinned agentd delivery, which is sidecar-only: the chart fails the render unless `agentdSidecar.enabled=true` requires `agentdDelivery.image` (helm/templates/controller-deployment.yaml), the controller's delivery/sidecar startup validation returns nil when `!sidecarEnabled` (controller/internal/workspace/agentd_sidecar.go:83), and single-container mode runs the workspace-agentd binary baked into runtimes/base (COPY at runtimes/base/Dockerfile:259). The kind-cluster.yaml containerdConfigPatches are harmless without a registry (per that file's own comment).
  - agentd artifact build → push → RepoDigest extraction → binary-sha pinning (AGENTD_REF/AGENTD_BINARY_SHA) — same reason.
- **New step vs nightly: Seed RuntimeEnvironment python-3.11.** The nightly gets it from `local/test.sh` Test 3, which this workflow does not run; the attachments script seeds workspaces with `runtime: python:3.11` and `resolveRuntimeImage` falls back colon→dash to the CRD named `python-3.11` (controller/internal/workspace/runtime_resolver.go). Chart only seeds `base` (language multi). Manifest identical to local/test.sh:112-121, image `ghcr.io/lenaxia/llmsafespaces/runtime-base:ci` (webhook allow-list requires the ghcr.io/lenaxia/ prefix — nightly lines 92-95).
- **No LLM secrets, no gating:** the script has zero `LLM_*` references (grep-verified) — uploads + kubectl-exec filesystem assertions + suspend/resume/pod-kill against the API; no agent turns. The workflow must run without creds so the rows finally execute.
- e2e step env matches the nightly's attachments step (CLUSTER_NAME/CTX/NS; the script derives CTX itself and defaults PORTFWD_PORT=18081 — no clash with anything this workflow runs).
- Failure dump extended with `kubectl get workspaces -o wide` (the failure surface here is workspace pods, not just api/controller).

### Script touch-up: local/us-68-attachments-e2e.sh

Two message-only edits, behavior-preserving in sidecar mode (both are comment/stderr text; assertions untouched):
- Header comment: "the rows require single-container mode" now names the workflow that executes them weekly.
- Sidecar-gate skip message: "tracked follow-up for the nightly config" (now stale — this session delivered it) → points at `e2e-attachments-single-container.yml`.
- Verified the gate itself needs no changes for single-container execution: it lists `.spec.containers[*].name` and skips only if an `agentd` container exists; the main container is named `workspace` in both modes (pod_builder.go:80), which `exec_ws` (`-c workspace`) depends on.

---

## Key Decisions

1. **Omit, don't replicate, the registry + digest-pinned agentd delivery.** Evidence chain: helm render fails `agentdSidecar.enabled requires agentdDelivery.image` (reproduced locally as a negative control); `validateAgentdSidecarConfig` returns nil for `!sidecarEnabled`; values.yaml documents "Leave everything empty for legacy baked-in mode"; agentd binary is COPY'd into runtimes/base. Fewer moving parts (no registry container, no digest extraction) = fewer weekly failure modes.
2. **Explicit `agentdSidecar.enabled=false`** rather than relying on the default: values.yaml says the default will flip to true after the sidecar L3 suite passes; an implicit default would silently turn this workflow into a skip-everything run on that day.
3. **Seed RuntimeEnvironment in the workflow, not the script.** The script's contract is "complements local/test.sh (same harness conventions)" — it assumes a cluster where test.sh (or equivalent) has run. Duplicating the seed inside the script would double-seed on any cluster where both run and change sidecar-mode behavior (the gate exits before any workspace seeding wait, but the seed would still happen — pointless kubectl applies). The workflow is the right owner for "cluster this script runs against".
4. **Concurrency = self-serialization only, documented honestly.** The task asked for a group that never overlaps the sidecar nightly or itself. GitHub concurrency groups only synchronize workflows that share a group name; the nightly declares none and I was forbidden to modify it — cross-workflow exclusion is unenforceable from this file alone. Mitigations: the Sun 04:00 slot is 2h clear of the nightly's daily 06:00, and the runs are isolated hosted runners (no shared kind cluster/docker state; same CLUSTER_NAME is per-runner). The group does prevent self-overlap (dispatch during a scheduled run queues).
5. **Keep setup-go@v6 verbatim** although no step invokes `go` directly: mirroring the proven nightly exactly (incl. module cache validation of go.sum) beats trimming a cheap step and risking an untested divergence.
6. **No `cancel-in-progress`**: both scheduled and manual runs are ~full budget; queuing preserves the scheduled run's artifacts/teardown semantics.

---

## Blockers

None. Cannot execute the workflow in this sandbox (no GitHub Actions runner) — expected; deliverable validated statically + via helm template.

---

## Tests Run

| Command | Result |
|---|---|
| `actionlint .github/workflows/e2e-attachments-single-container.yml` (actionlint 1.7.7, downloaded to /tmp) | **CLEAN** (nightly also clean as baseline) |
| YAML parse (PyYAML): triggers/concurrency/permissions/timeout/15 steps; heredoc step renders at column 0 | OK |
| `helm template` with the workflow's EXACT value set (`--kube-version 1.35.0`, matching the kind node v1.35.5 / chart kubeVersion floor) | **RENDER OK, 37 resources, zero agentdDelivery refs** |
| Negative control: `helm template --set controller.agentdSidecar.enabled=true` (no delivery image) | **fails the render as expected** (proves delivery sets are sidecar-only) |
| `bash -n local/us-68-attachments-e2e.sh` (after touch-up) | clean |
| `grep -c "LLM" local/us-68-attachments-e2e.sh` | 0 (no secrets gating needed) |
| repolint via pre-commit hook on commit | run at commit time (see below) |

Not run locally (impossible in sandbox): the workflow itself on a runner; `local/us-68-attachments-e2e.sh` against a live kind cluster (needs docker + kind; sandbox has neither wired for a full deploy). The script is byte-identical in logic to the one shipped in US-68.6 (only stderr/comment text changed).

---

## Next Steps

- First scheduled run (Sun 04:00 UTC) or a manual dispatch will execute E2/E10/E11 for the first time; watch for workspace-Active timing on the 180s/240s/300s waits — they mirror nightly-observed timings but the rows themselves are unproven at cluster scale.
- If the sidecar control-socket write op (design doc deviation 1 follow-up) ever makes uploads work in sidecar mode, this workflow and the script gate can be retired in favor of the nightly running the rows directly.
- When the agentdSidecar default flips (post-L3), this workflow is unaffected (explicit false), but the NIGHTLY's behavior is unchanged too (it sets true explicitly) — no action needed, just keep both pinned.

---

## Files Modified

- .github/workflows/e2e-attachments-single-container.yml (new)
- local/us-68-attachments-e2e.sh (comment + skip-message text only)
