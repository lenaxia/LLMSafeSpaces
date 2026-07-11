# Changelog

All notable changes to LLMSafeSpaces are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-07-11

Network hardening sweep + KMS-backed master KEK foundation + Go security bump.

### Security

- **G34 — proxy header allowlist (Critical).** The workspace reverse proxy
  previously forwarded every client request header (Cookie, Origin, Referer,
  X-Forwarded-*, arbitrary custom headers) verbatim into the tenant pod. Now
  uses an explicit allowlist (`Content-Type`, `Accept`, `X-Request-ID`) and
  strips RFC 7230 hop-by-hop headers in both directions. `Accept-Encoding` is
  deliberately not forwarded — Go's http.Transport handles gzip transparently.
  ([#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513))

- **G39 — terminal WebSocket Origin check (High).** The terminal WebSocket
  upgrader accepted any Origin (`CheckOrigin: return true`), enabling
  cross-site WebSocket hijacking from a malicious page in a browser holding
  the user's session ticket. Now defaults to same-origin only, with an
  operator-controlled allowlist (`terminal.allowedOrigins` Helm value).
  Removed the dead `WebSocketSecurityMiddleware` and
  `RouterConfig.AllowedWebSocketOrigins` plumbing — the gorilla `Upgrader`
  is now the single enforcement point.
  ([#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515))

- **CORS hardening.** `security.allowedOrigins=["*"]` combined with
  `security.allowCredentials=true` is now rejected at config load with an
  actionable error. The combo is forbidden by the CORS spec (Fetch §3.2.1)
  because it would let any website read authenticated responses from this API
  in a victim's browser. Browsers reject the combo client-side; we now also
  fail closed at boot.
  ([#516](https://github.com/lenaxia/LLMSafeSpaces/pull/516))

- **NetworkPolicy CGNAT drift (chart/controller parity).** The chart-side
  default `blockedEgressCIDRs` was missing `100.64.0.0/10` (CGNAT),
  `127.0.0.0/8` (loopback), and `224.0.0.0/4` (multicast) that the
  controller-side list already had. The CGNAT gap specifically affected
  managed Kubernetes offerings (AKS default VNet, some EKS configs, k3s
  default flannel) where `100.64/10` is the pod CIDR — workspace pods on
  such clusters could reach internal pods/services in the CGNAT range,
  defeating cross-tenant isolation.
  ([#517](https://github.com/lenaxia/LLMSafeSpaces/pull/517))

- **runtimeClass webhook admin-gate (S51.2 closure).** The Workspace CRD's
  `spec.runtimeClass` field (Epic 51 S51.1, used for per-workspace gVisor
  opt-out) was documented as "admin-gated, not tenant-selectable" but the
  webhook enforcement was deferred to S51.2. Without it, any user with
  workspace create/update RBAC could set `spec.runtimeClass="runc"` to
  escape gVisor via direct kubectl. The workspace validating webhook now
  rejects `spec.runtimeClass` unless the object carries the annotation
  `llmsafespaces.dev/allow-runtime-class-override: "true"`, applied via
  cluster-admin RBAC.
  ([#518](https://github.com/lenaxia/LLMSafeSpaces/pull/518))

- **JWT iss/aud claims.** JWTs now carry explicit `iss` and `aud` claims,
  minted from `auth.jwtIssuer` / `auth.jwtAudience` (default
  `"llmsafespaces"`), and validated on every parse. Pre-fix tokens carried
  only `sub/jti/exp/iat`, so any service sharing the same HMAC secret could
  mint accepted tokens. Pre-fix tokens are rejected after this change; tokens
  are short-lived (24h default) so rotation is fast.
  ([#519](https://github.com/lenaxia/LLMSafeSpaces/pull/519))

- **Epic 57 US-57.1 — CompositeProvider + prefix-aware local providers
  (foundation for KMS-backed master KEK).** The first of three PRs closing
  the largest unbuilt gap in the threat model: API-pod RCE → permanent KEK
  exfiltration for offline DB decrypt. This PR lands the dispatch mechanism
  (`CompositeProvider` with prefix-sniffing ciphertext routing) and the
  prefix-aware local providers (`lkms:v1:` prefix on new writes, legacy
  un-prefixed blobs still decrypt via fallback). Subsequent PRs add AWS KMS
  and GCP KMS providers and the `migrate-kek` CLI.
  ([#510](https://github.com/lenaxia/LLMSafeSpaces/pull/510) design,
  [#511](https://github.com/lenaxia/LLMSafeSpaces/pull/511) composite,
  [#512](https://github.com/lenaxia/LLMSafeSpaces/pull/512) AWS KMS provider + Go 1.25.12 bump)

### Infrastructure / Tooling

- **Go 1.25.12.** Bumped from 1.25.11 to fix `GO-2026-5856`
  (Encrypted Client Hello privacy leak in `crypto/tls`).
  ([#512](https://github.com/lenaxia/LLMSafeSpaces/pull/512))

- **Supply chain hardening.** Release images are now cosign-signed
  (keyless OIDC, Rekor transparency log). Trivy image scanning runs
  on every built OCI image. Renovate `docker:pinDigests` opens PRs to
  pin Dockerfile FROM lines to immutable digests.
  ([#534](https://github.com/lenaxia/LLMSafeSpaces/pull/534))

- **Documentation site.** New MkDocs Material site at
  https://lenaxia.github.io/LLMSafeSpaces/ — 32 pages across 7
  sections (Getting Started, Operator Guide, Architecture, API
  Reference, Contributing, Reference). Docs-maintenance runbook
  documents content inventory, drift triggers, and procedures.
  ([#527](https://github.com/lenaxia/LLMSafeSpaces/pull/527),
  [#529](https://github.com/lenaxia/LLMSafeSpaces/pull/529),
  [#530](https://github.com/lenaxia/LLMSafeSpaces/pull/530))

- **Chart path cleanup.** `charts/llmsafespaces/` → top-level `/helm`.
  Zero impact on consumers (chart registry URL unchanged).
  ([#526](https://github.com/lenaxia/LLMSafeSpaces/pull/526))

- **New chart values:** `terminal.allowedOrigins`, `auth.jwtIssuer`,
  `auth.jwtAudience`. See the security entries above for usage.

- **Helm chart NetworkPolicy default** now includes the full
  controller-side private-or-internal CIDR set (CGNAT/loopback/multicast),
  not just RFC1918 + link-local.

## [0.2.2] - 2026-07-07

## [0.2.1] - 2026-07-06

## [0.2.0] - 2026-07-06

## [0.1.0] - 2026-07-04

[Unreleased]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.2...v0.3.0
[0.2.2]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/lenaxia/LLMSafeSpaces/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/lenaxia/LLMSafeSpaces/releases/tag/v0.1.0
