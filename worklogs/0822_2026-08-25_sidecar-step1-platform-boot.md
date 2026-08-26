
## Addendum (2026-08-26): L3 caught what L0–L2 could not

The first kind execution of the K1–K13 world (v0.21.0 tag, run 06:35 UTC)
failed at pod boot: `platform-init` CrashLoopBackOff, exit 128 —
`stat /agentd/usr/local/bin/workspace-agentd: no such file or directory`.
The platform init containers ran the overlay binary's PATH without
mounting the `agentd` image volume (`wireAgentdOverlay` wires main+sidecar
only; the new containers were added outside it). No unit/envtest layer
validates mount-vs-command consistency, so only a real kubelet could see
it — the L3 tier doing exactly its job.

Fix: `agentd` volume (RO) added to all three platform containers; pinned
by `TestPlatformInit_AgentdVolumeMountedEverywhere` (both modes, init +
main containers). Shipped as v0.21.1.

Same session, deploy-side lesson recorded: the agentdDelivery digest must
be the OCI **index** digest (`Docker-Content-Digest` of the tag), not
`manifests[0].digest` (the amd64 child) — the wrong form produces ghcr's
misleading "Accept header does not support OCI manifests" at controller
pin resolution.
