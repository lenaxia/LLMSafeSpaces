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
  own resourceNames-scoped RBAC grant added in the chart — the earlier
  "already granted" claim was FALSE with relay + free-models disabled,
  empirically confirmed by review): on fetch failure the cache
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

## Tests (final state, round 2)

- `agentd_pins_test.go` (cachedPinResolver + ResolvePinsWithCache):
  annotation extraction (happy/missing), cache write on success, cache
  fallback on outage, stale-cache rejection, malformed-cache rejection,
  manual-pin short-circuit (valid hex verbatim; invalid hex rejected),
  image-only resolution via the cluster path, namespace fallback
  ordering. (An earlier revision of this worklog listed
  flag-override-precedence tests that no longer exist — those covered
  deleted dead code and were replaced, not renamed.)
- `agentd_pins_integration_test.go`: local in-process OCI registry —
  fetchIndexAnnotations happy round-trip (digest-only AND tag+digest
  forms), unreachable registry, tag-only ref rejected; CI↔Go
  annotation-key consistency guard against .github/workflows/ci.yml
  (all three keys via the Go constants).
- Chart: image-only renders exactly one flag (Renovate form); full pin
  renders three; one-sided override (BOTH directions) fails render;
  hashes-without-image fails; agentd-pins RBAC grant renders with
  delivery enabled + relay/free-models disabled, absent in legacy mode.
- Existing #863 tests unchanged and green — downstream wiring is
  oblivious to the pin origin.

## Adversarial notes

- Registry outage at FIRST-EVER boot (no cache): controller exits with
  a clear error telling the operator to pin hashes manually — fail
  closed, acceptable (no fleet can have been running overlay mode
  without a prior successful resolve).
- Review round 1 majors fixed in the follow-up commit: ctx threaded
  into remote.Index via remote.WithContext (30s boot timeout now bounds
  the fetch; ggcr defaults to context.Background otherwise); dead
  resolvers (registryPinResolver, resolveAgentdPins) deleted and their
  tests rewritten against the production path; cache-read errors no
  longer misreported as "no cache exists" (RBAC hint included); local
  ggcr-registry integration test added for fetchIndexAnnotations; CI↔Go
  annotation-key consistency guard added (fleet-wide-boot-failure class).
- Annotation stripping by a mirror: overrides exist for exactly this;
  documented as break-glass.
- The printed CI block now leads with the image-only form; hashes shown
  commented as overrides.

## Review round 2 addendum

- ResolvePinsWithCache made injectable (resolvePinsFromCluster).
- Cache identity compares DIGESTS, not full refs.
- docs "three values" rollout line corrected; CI-printed image line is
  digest-only.
- annotationVersion constant now used by the CI guard test.

## Review round 3 addendum (mutation findings)

Round 2's test claims were partially FALSE — the reviewer's mutations
proved the image-only cluster path, the ns fallback, and the sameDigest
re-tag scenario were NOT covered despite claims. Corrected:

- The misleading fake-client near-duplicate test DELETED (renamed
  honestly: TestResolvePinsFromCluster_ConfigErrorSurfaces covers
  ordering only). The cluster path is now covered by
  agentd_pins_envtest_test.go (build tag envtest; drives the real
  ResolvePinsWithCache against a real API server; POD_NAMESPACE
  fallback proven by cache landing in the set namespace).
- sameDigest: table test + re-tag-during-outage resolver test,
  mutation-verified red/green locally before claiming.
- Mirrored one-sided Go validation row added.
- RBAC chart test pins exact verbs, resourceNames scoping, and the
  separate unscoped create rule — not just the ConfigMap name.
- errFetchUnavailable → exported ErrAgentdPinsUnavailable with a real
  errors.Is consumer in main.go (targeted manual-pin hint on outage vs
  broken-image messaging); import hack removed.

## Review round 4 addendum (claims vs execution — the recurring failure)

Round 3 again shipped false claims: both envtest tests failed as
written (test 1 never created the llmsafespaces namespace so the cache
write no-op'd; test 2 swapped only loadConfig, sending the production
fetcher to real ghcr.io — non-hermetic and unpassable anywhere), and
"runs in the envtest workflow" was false (no step, and the paths
filter excluded the directory entirely). The reviewer ran the tests;
the author had not. Corrections:

- Both envtest tests fixed as diagnosed and EXECUTED locally against
  real envtest 1.31 assets before this commit (PASS 4.88s/4.35s).
- envtest.yml: new step runs the suite; paths now includes
  controller/internal/workspace/**.
- The earlier "all suites green incl. go vet -tags envtest" statement
  was meaningless (vet type-checks, never runs tests) — recorded here
  so the pattern is explicit: never claim a test result without the
  execution that produced it.
