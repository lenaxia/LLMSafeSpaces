# Worklog NNNN — agentd image-volume delivery (#863)

**Date:** 2026-08-15
**PR:** (this PR)
**Issue:** #863

## What

Implements agentd overlay delivery: workspace pods receive the
`workspace-agentd` binary via a digest-pinned, read-only image volume
(KEP-4639) instead of relying on the copy baked into `runtimes/base`.
Platform agentd fixes now reach every workspace on any pod (re)creation
without rebuilding the runtime base or touching image-factory configs.
"Image is the workspace" is preserved — user deps/distro stay in the
factory image; only the platform supervisor moves to platform cadence.

## Components

- **Controller** (`agentd_overlay.go`, pod_builder, phase_active):
  image volume + RO mount + env pins (`AGENTD_IMAGE_VOLUME`,
  `LLMSAFESPACES_AGENTD_BINARY`, per-arch sha256) wired into buildPod;
  verify-failure detection (exit 81/82) in handleActive **before** the
  crashloop recovery branch (a digest mismatch cannot be fixed by
  restarts); `AgentdVerified` condition + one warning event +
  `llmsafespaces_workspace_agentd_verify_failures_total{outcome}` per
  failure episode; startup validation of the all-or-nothing flag
  contract (digest-pinned image + both 64-hex binary hashes).
- **Entrypoint** (`entrypoint-common.sh`): overlay branch verifies
  sha256 against the pod-spec pin before exec; mismatch → exit 81 (no
  fallback — silent fallback would be a downgrade attack), missing
  overlay → exit 82; failure details to `/dev/termination-log`.
  `entrypoint-opencode.sh` execs `${AGENTD_BIN} --supervise`.
- **Image** (`cmd/workspace-agentd/Dockerfile`): `FROM scratch`,
  static binary at the fixed path `/usr/local/bin/workspace-agentd`.
- **CI**: `build-agentd` (multi-arch) + `merge-agentd` jobs; extracts
  per-arch **binary** sha256s (chart pins the file hash, not the image
  digest) and prints the ready-to-paste `agentdDelivery` values block.
- **Helm**: `controller.agentdDelivery.{image,binarySHA256Amd64,binarySHA256Arm64}`
  with `required` guards (half-config fails the render); critical
  page-on-any alert `LLMSafeSpacesAgentdVerificationFailed`.
- **Docs**: `docs/operator/agentd-delivery.md` (enable, rollout,
  rollback, alert triage incl. tamper-vs-drift decision tree).

## Assumptions → validation

1. `ImageVolumeSource` exists in k8s.io/api v0.32.3 → **verified**
   (`go doc k8s.io/api/core/v1.ImageVolumeSource`). Chart floor
   `>=1.35` (PR #864) guarantees GA server-side; no capability probe.
2. The mount must be RO because init containers don't re-run per
   container restart → **locked by test**
   (`TestAgentdOverlay_MountIsReadOnly`).
3. Entrypoint contract change is backward-compatible with existing
   runtime images (AGENTD_BIN export; legacy path unchanged) →
   **verified** by reading both entrypoints; overlay env absent ==
   byte-for-byte legacy behavior.
4. Entrypoint verify behaves as specified → **verified live** against
   a real file: match execs, mismatch exits 81 with expected=/got=,
   missing exits 82 (all three reproduced in /tmp during development).
5. Fake-client status-mutation-after-Create is not visible to a
   subsequent Get in the same test → **disproved during TDD**; tests
   build pods fully-formed at Create time instead.

## Adversarial review (Phase 1→3)

- **Verify failure + crashloop interaction:** detection placed before
  recovery; test asserts phase stays Active and RestartCount untouched.
  Otherwise the workspace would burn its restart budget reproducing the
  same mismatch — real finding, fixed by placement.
- **Metric double-count across reconciles:** episode-idempotent
  (condition state comparison); delta-asserted in tests. False-alarm
  risk: a pod recreated with the same bad pin counts as a new episode —
  accepted, each episode = one new pod.
- **Silent fallback attack:** none — mismatch exits; only overlay
  *absent* (env unset) uses the baked binary, and the controller never
  sets the env without the volume.
- **Event spam:** one event per episode via Recorder; FakeRecorder
  asserted. Long CrashLoopBackOff does not re-emit on every reconcile.
- **Half-config rollout:** guarded twice (helm `required` + controller
  startup exit). Chart test asserts render failure.
- **gVisor/latency:** NOT validated live here (no cluster in this
  environment) — documented in #863 as the remaining validation leg
  with the pre-puller DaemonSet as fallback. Honest open item.

## Test evidence

`go test ./controller/... ./helm/ ./pkg/apis/...` — all green.
New: 11 controller tests (wiring × 4, detection × 6, config × 1) + 3
chart tests (helm-gated, skip locally without helm, run in CI Lint).
Entrypoint: three live legs reproduced locally.

## Deferred (tracked in #863)

- Live gVisor + resume-latency measurement; pre-puller if budget
  breached.
- Switching the fleet default to overlay mode (values stay empty until
  an operator opts in per cluster).
