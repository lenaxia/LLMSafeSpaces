# Worklog: S5 overlay validation kind suite (design 0053)

**Date:** 2026-08-31
**Session:** Implement design 0053 §7 S5 — the kind-level validation suite for overlay delivery, wired to CI. The "flip defaults" decision is explicitly deferred to a green run (see Next Steps).
**Status:** Complete (suite shipped; green-run + flip pending)

---

## Objective

Everything S3/S4 changed needs a real kubelet to validate: verify-failure surfacing, the stripped base + both overlays booting end-to-end, cold-node resume pull cost (opencode ~10× agentd), and design 0051's open gVisor item — image volumes under `runsc`.

---

## Work Completed

- `local/s5-overlay-validation.sh` (new, modeled on the us2-kind-integration harness — same registry/certs.d pattern, controller-lean chart, webhook-endpoint wait):
  - **S5.1/S5.1b** — missing-pin render legs (no pins → agentdDelivery mandatory; agentd-only → opencodeDelivery mandatory), run pre-cluster (pure Helm surface).
  - **S5.2** — capstone: workspace Active on the stripped base + both overlays; asserts PID1 = pinned overlay agentd (`/proc/1/cmdline`), opencode serving on :4096, and the supervisor-written `/sandbox-runtime/bin/redact` wrapper redacting through a real pipe (**closes S2's deferred cluster-e2e debt**).
  - **S5.3** — wrong agentd break-glass pin (tampered 64-hex) → supervisor exit 81 → `AgentdVerified=False` condition + `AgentdVerificationFailed` event.
  - **S5.4** — wrong opencode pin → exit 83 → `OpencodeVerified=False` condition.
  - **S5.5** — resume-path pull cost: suspend → `crictl rmi` BOTH overlay images from the node → activate; measured seconds → `$GITHUB_STEP_SUMMARY`; asserted ≤ `RESUME_BUDGET` (default 120s).
  - **S5.6** — gVisor leg: runsc installed into the kind node (gvisor apt repo), containerd handler registered, RuntimeClass created, workspace on `runtimeClassName: gvisor` → Active + opencode serving. Env-gated skip (`S5_SKIP_GVISOR=1`) reports FAIL-with-reason, not silent green — the flip decision requires a real runsc result.
- `.github/workflows/s5-overlay-validation.yml` (new): workflow_dispatch + Sundays 06:00 UTC (after the attachments slot, before US-2 L3), failure diagnostics for every S5 workspace shape.

## Key Decisions

- **Break-glass tamper instead of artifact tampering** for S5.3/S5.4: flipping the chart's `binarySHA256*` override exercises the real controller→pin→supervisor→exit-code→condition path without rebuilding images; `helm upgrade` restores correct pins after each leg.
- **Explicit binary pins required in-kind** (same as us2): with image-only pins the controller's startup annotation resolution dials the registry from inside its pod, where `localhost:$REG_PORT` does not resolve — the certs.d alias is node-level containerd only.
- **S5.5 evicts via `crictl rmi` on the kind node** — the only way to force the cold pull the design asks to measure (node cache otherwise absorbs it).
- **gVisor leg fails loudly on skip**: a skipped runsc result must not read as green for the flip decision.
- **No default flipped in this PR**: S5's flip is gated on THESE results (design §7). The candidate flip inventory is in Next Steps.

## Assumptions validated

- The tamper SHAs are exactly 64 hex-shaped chars (`printf … | head -c 64`) — pass chart + controller validation shape while guaranteeing mismatch.
- `Workspace.spec.suspend` + `runtimeClassName` exist on the CRD (verified: `workspace_types.go` S51.1/S3 reconciler paths; the us2 suite creates workspaces the same way).
- The redact wrapper assert covers S2's S5 deferral: real pod, real pipe, built-in rule set (no `/sandbox-cfg` config needed).

## Blockers

None (script cannot run on the authoring sandbox — no docker; CI executes it).

## Tests Run

- `bash -n local/s5-overlay-validation.sh` — clean.
- Workflow YAML parsed; repolint suite green (no layout/rules regressions from the new files).

## Next Steps

- **Run the suite**: `gh workflow run s5-overlay-validation.yml` (workflow_dispatch) on the merged PR; triage any red leg from the diagnostics step.
- **Flip decision** (from a green run, in preference order):
  1. `controller.agentdSidecar.enabled` default `false → true` — the 0051/0055 target topology, unblocked by US-70.1 (#1164); requires the us2 L3 suite green alongside.
  2. Promote `bookworm@2026.08.0` to the fleet default on existing clusters (admin upsert — the seed deliberately does not move operator defaults, #936).
  3. S5.5's measured number → update the documented ~22s resume figure (README-LLM currently carries the measured pre-overlay number).
- **gVisor on CI**: if the ubuntu-latest runner cannot host runsc (nested virtue constraints), move S5.6 to a self-hosted/gvisor-capable runner or a GKE-based leg — the loud-skip keeps this honest in the meantime.

## Files Modified

- `local/s5-overlay-validation.sh` (new)
- `.github/workflows/s5-overlay-validation.yml` (new)
- `worklogs/NNNN_2026-08-31_s5-overlay-validation.md` (this file)
