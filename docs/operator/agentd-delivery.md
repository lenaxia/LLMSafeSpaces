# Agentd Overlay Delivery (Image Volume)

Operational guide for delivering `workspace-agentd` via a digest-pinned
image volume (#863), instead of the binary baked into `runtimes/base`.

## What it is

When enabled, every workspace pod receives:

- an **image volume** (`/agentd`) whose content is the standalone
  `ghcr.io/lenaxia/llmsafespaces/agentd` image — a `FROM scratch`
  artifact containing only the `workspace-agentd` binary,
- mounted **read-only** on the workspace container,
- with the binary's **per-arch sha256** pinned in the pod spec env,
- verified by the entrypoint before exec (`sha256sum` vs pin).

The workspace image still owns everything user-facing (distro, packages,
opencode). Only the platform supervisor binary moves to platform cadence:
any pod (re)creation — launch, resume-from-suspend, controller-initiated
restart — pulls the **currently pinned** agentd. "The image is the
workspace" is preserved.

## Enabling

Set ONE value — the image, in the canonical tag+digest form every
digest-aware dependency bot manages:

```yaml
controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:<manifest-digest>
```

That single coordinate carries the whole verify contract: CI stamps the
per-arch binary sha256s onto the image index as OCI annotations (covered
by the digest — they cannot desync), and the controller resolves them at
startup, caching the result in the `llmsafespaces-agentd-pins`
ConfigMap so a registry outage at controller boot does not brick
startup. The entrypoint sha256 verify, exit codes, conditions, and
alerts are unchanged from the original design.

Requirements (enforced at `helm template` and controller startup):

- `image` must be **digest-pinned** — a floating tag defeats both
  reproducibility and the verify contract,
- `binarySHA256Amd64`/`binarySHA256Arm64` are OPTIONAL per-image
  overrides (break-glass): set BOTH or NEITHER,
- the binary path inside the image is fixed:
  `/usr/local/bin/workspace-agentd`.

Leave `image` empty for legacy mode (baked-in binary) — nothing renders,
no volume is mounted, the entrypoint behaves exactly as before.

## Keeping it updated (Renovate / Dependabot)

**Renovate — first-class.** Because the pin is one docker tag+digest
line, Renovate's `helm-values` manager maintains it automatically. In
the repo holding your Helm values:

```json
{
  "extends": ["config:recommended", "docker:pinDigests"],
  "packageRules": [
    {
      "description": "agentdDelivery digest bumps are self-contained",
      "matchManagers": ["helm-values"],
      "matchDepNames": ["ghcr.io/lenaxia/llmsafespaces/agentd"],
      "automerge": true,
      "automergeType": "pr"
    }
  ]
}
```

A digest bump is SAFE BY CONSTRUCTION: the binary hashes are derived
from the new digest's annotations at controller startup, never
independently updated. There is deliberately nothing else for a bot to
touch.

**Dependabot — not applicable.** Dependabot has no manager for Helm
values files or generic image digests. For manual updates, the
`merge-agentd` CI job prints the exact image line for its commit (its
"Print Helm values block" step); copy that single line.

**Break-glass override.** If you must pin hashes explicitly (e.g. an
air-gapped mirror stripping annotations), set both:

```yaml
controller:
  agentdDelivery:
    image: <mirror>/agentd:dev@sha256:<digest>
    binarySHA256Amd64: "<64-hex>"
    binarySHA256Arm64: "<64-hex>"
```

CI's merge job output includes the current binary hashes alongside the
digest. An override pair overrides annotations per-arch; a one-sided
pair fails the Helm render.

## Rollout / rollback

Roll **forward** by updating the three values to a newer digest and
running `helm upgrade`. The controller Deployment restarts; **existing
pods keep their old pin** (pod specs are immutable) and pick up the new
agentd the next time their pod is recreated.

Roll **back** the same way: pin the old digest back. Pods created in the
meantime verify against their own pod-spec pins, so a mixed-version fleet
is safe — each pod's entrypoint only ever trusts its own immutable spec.

To force a single workspace onto the new agentd immediately: suspend and
resume it (or bump `spec.restartGeneration`).

## Behavior on verification failure

| Entrypoint outcome | Exit | What happens |
|---|---|---|
| sha256 matches pin | 0 | exec overlay binary; pod sets `AgentdVerified=True` |
| sha256 mismatch | **81** | refuse to exec — **no fallback**; CrashLoopBackOff |
| pinned binary missing | **82** | refuse to exec; CrashLoopBackOff |
| verify disabled (legacy) | — | baked-in binary, no condition |

On 81/82 the controller:

- sets the Workspace condition `AgentdVerified=False`
  (`AgentdVerificationFailed` / `AgentdOverlayMissing`),
- emits a Warning event on the Workspace with **expected vs observed
  digest** and the node name,
- increments
  `llmsafespaces_workspace_agentd_verify_failures_total{outcome,node,agentd}`,
- deliberately does **not** enter the crashloop recovery machinery —
  restarting cannot fix a digest mismatch; the workspace stays in its
  current phase (Active or Creating) with the condition set, requeued at
  a slow cadence.

A Prometheus alert (`LLMSafeSpacesAgentdVerificationFailed`, severity
**critical**, fires on any increase) is shipped in the chart.

## Investigating a firing alert

Triage by comparing the observed digest from the event message:

- **Observed == the sha256 of another agentd release you know** →
  rollout drift: the pod spec pin and the registry content disagree
  (usually a bad values update). Fix the pins; recycle the pod.
- **Observed == your pin, but pin != image content** → the values block
  was assembled from mismatched CI runs. Re-copy the block from a single
  `merge-agentd` step output.
- **Unknown digest** → tampering or corruption. Treat as a security
  incident: the workspace refused to run the swapped binary, which is
  the designed outcome. Preserve the pod (`kubectl get pod -o yaml`) and
  the event, then investigate the node.

## gVisor note

Image volumes are served through the runsc gofer as read-only mounts;
first-pod-on-node pull cost applies per digest. The agentd image is
~25 MB per arch. If resume latency on gVisor nodes ever breaches budget,
a chart-gated pre-puller DaemonSet is the fallback (deliberately not
built until measured — see #863).
