# Worklog NNNN — single-coordinate (Renovate-friendly) agentd pins (#863 follow-up)

**Date:** 2026-08-16
**PR:** (this PR)
**Issue:** #863 (follow-up to the image-volume delivery in #872)

## Problem

The #863 contract required three values to move in lockstep:
digest-pinned image + two per-arch binary sha256s. Dependency bots
(Renovate, Dependabot-class tooling) update a docker **tag+digest
pair** — one line — but nothing can compute the sha256 of a file inside
an image. A bot-driven digest bump with stale hashes would exit-81 every
workspace pod fleet-wide (the page-on-any alert firing on every Renovate
merge). The pins are operator-side values, so the fix must make the
Renovate-visible surface self-contained.

## Design

ONE Renovate-updatable coordinate:

```
controller.agentdDelivery.image: ghcr.io/.../agentd:dev@sha256:<digest>
```

CI (merge-agentd) stamps the per-arch binary sha256s onto the image
INDEX as OCI annotations (`dev.llmsafespaces/agentd.sha256-amd64/arm64`,
plus `dev.llmsafespaces/version`). Annotations are part of the manifest,
so the digest covers them — desync is structurally impossible. At
controller startup (before the manager starts, fail-fast), any unset
hash is resolved from the index annotations via an anonymous registry
read (go-containerregistry), with:

- **ConfigMap cache** (`llmsafespaces-agentd-pins`, POD_NAMESPACE,
  RBAC already granted for configmaps): on fetch failure the cache
  satisfies startup ONLY if recorded for the SAME digest; a stale-digest
  cache is rejected (that rejection IS the desync guard).
- **Flag overrides** (`--agentd-binary-sha256-*`): break-glass, per-arch,
  always win over annotations; both-or-neither enforced at validation
  and Helm render.
- Entrypoint verify/exit codes/conditions/events/alerts: **unchanged**.

## Assumptions → validation

1. buildx supports `--annotation index:key=value` on `imagetools
   create` → verified against buildx docs syntax used by the action
   ecosystem; will be proven by the first post-merge merge-agentd run
   (the job is non-PR-only — the known limitation that bit the five
   export-step iterations; the shell-syntax guard now covers rendering).
2. ghcr anonymous reads of public image indexes work from clusters →
   consistent with the free-tier/public-repo posture; the cache plus
   overrides cover restricted-egress clusters.
3. Annotations survive later tag-adding `imagetools create` calls → not
   required: merge-agentd creates the index WITH annotations fresh each
   run from build digests.
4. Renovate helm-values + docker digests handle `image: repo:tag@digest`
   in values → documented recipe; verified against Renovate docs
   patterns already used elsewhere in the org (`docker:pinDigests`).
5. Dependabot cannot manage this → stated honestly in docs; the
   CI-printed block remains the manual path (now image-only).

## Tests

- `agentd_pins_test.go`: annotation extraction (happy/missing), cache
  write on success, cache fallback on outage, stale-cache rejection,
  flag-override precedence (full + partial), image-only validation,
  partial-override rejection.
- Chart: image-only renders exactly ONE flag (the Renovate form);
  one-sided hash override fails render; hashes-without-image still
  fails; full pin renders all three.
- Existing 18 #863 tests unchanged and green — downstream wiring is
  oblivious to the pin origin.

## Adversarial notes

- Registry outage at FIRST-EVER boot (no cache): controller exits with
  a clear error telling the operator to pin hashes manually — fail
  closed, acceptable (no fleet can have been running overlay mode
  without a prior successful resolve).
- Annotation stripping by a mirror: overrides exist for exactly this;
  documented as break-glass.
- The printed CI block now leads with the image-only form; hashes shown
  commented as overrides.
