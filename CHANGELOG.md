# Changelog

All notable changes to LLMSafeSpaces are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security

- **G38 — ChangePassword now revokes all sessions (High).** The handler
  at `POST /api/v1/account/change-password` previously re-wrapped the
  DEK with the new password and updated the bcrypt hash but left every
  outstanding JWT — including the caller's — valid until natural expiry.
  A token stolen before the change kept reading secrets (the cached DEK
  in Redis was already evicted by `KeyService.ChangePassword`, but the
  JWT signature remained valid and a re-login re-cached the DEK under
  the new password, which the stolen token could then use). The handler
  now calls `auth.Service.RevokeAllUserSessions` after both the DEK
  re-wrap and the bcrypt update commit, writing the per-jti and per-hash
  revocation markers and clearing durable `jwt_sessions` rows — the same
  primitive `password-reset/confirm` already used (OWASP ASVS V2.5.2).
  Best-effort: a revocation failure is logged and the password change
  still reports success (the cryptographic change is irreversible).

- **G37 — Workspace env-var name blocklist (High).** The handler at
  `PUT /api/v1/workspaces/:id/env` accepted any POSIX-shaped env-var
  name, including `LD_PRELOAD`, `PATH`, `PYTHONPATH`, `BASH_ENV`,
  `DYLD_INSERT_LIBRARIES`, etc. Setting one of these via the env-secret
  mechanism would let a workspace owner compromise every process
  spawned in the pod (agentd, opencode, mise-installed interpreters) —
  a container-escape-equivalent in practice because the pod's single
  UID shares the same trust boundary. The new
  `pkg/validation.ValidateEnvVarName` enforces three rules at the API
  layer: POSIX shape (`[A-Za-z_][A-Za-z0-9_]*`), length ≤ 256, and not
  on a curated blocklist of ~30 dangerous names sourced from ld.so(8),
  bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. The same
  validator is now used by agentd's materialize-time check as defense-
  in-depth. Locale vars (`LANG`, `LC_ALL`, `TZ`) are intentionally NOT
  blocked — they don't execute code and users legitimately set them.

- **G35 — `/account/recover` per-route rate limit (High).** The
  recovery endpoint at `POST /api/v1/account/recover` was mounted on
  the root router behind only the global 100/min/IP rate limiter. While
  the recovery key is 128-bit random (brute-force is mathematically
  infeasible), the endpoint does Argon2id work to re-derive the DEK
  under the new password, making it a CPU-exhaustion DoS target. The
  new `PerRouteRateLimitMiddleware` (separate from the global
  `RateLimitMiddleware`) applies a stricter per-route limit (default
  20 tokens/burst 5, from the previously-dead-code `authRatePerMinute`
  /`authRateBurst` constants) using per-route bucket isolation
  (`<path>:<identity>` key) so a user hitting `/recover` cannot deplete
  the budget for other routes. The middleware is generic — future
  endpoints (e.g. G41 `/secrets/:id/reveal`) can be added to the same
  routes map.

- **G25 — Secret `value` field no longer logged (High).** The request
  logging middleware (`api/internal/middleware/logging.go`) masked
  sensitive JSON fields by name (`password`, `token`, `secret`, `key`,
  `apiKey`, `credit_card`) but NOT `value` — the field name used by
  the secrets API to carry the plaintext credential on
  `POST /api/v1/secrets` and `PUT /api/v1/secrets/:id`. A request to
  create a secret logged the plaintext API key in the application log,
  visible to anyone with log access. Two-layer fix: (1) added `value`
  to the default `SensitiveFields` list (defense in depth — catches
  any logged JSON with a `value` field, even on paths not in the skip
  list); (2) added `SkipPathPrefixes` to `LoggingConfig` and configured
  the default with the credential-bearing paths (`/api/v1/secrets`,
  `/api/v1/account`, `/api/v1/auth`, `/api/v1/admin/provider-credentials`)
  so bodies on those paths are never logged at all. Either layer alone
  prevents the leak.

- **G36 — Workspace secrets cleaned on deletion (High).** The
  workspace controller's `handleTerminating`
  (`controller/internal/workspace/phase_terminating.go`) deleted the
  pod, PVC, and `workspace-pw-*` Secret but NOT `workspace-creds-*`,
  which persisted indefinitely after workspace deletion. The existing
  `cleanupFailedWorkspaceSecrets` primitive (already used for the
  Failed-phase path in `recovery.go`) knows how to delete both
  `workspace-creds-*` and `workspace-pw-*`; this PR wires it into the
  graceful-termination path too. Best-effort (failures logged, not
  propagated — the workspace is already being torn down and the
  finalizer must still release). `handleDeletion` (the CRD-deletion
  entry point) inherits the fix automatically because it calls
  `handleTerminating`.

- **G28 — Workspace bind handler reclassified as Accepted (was High/Open).**
  The threat-model row originally flagged "PUT /workspaces/:id/bindings
  returns 204 but K8s Secret is never created." Epic 35 (secretless
  injection) removed the durable K8s Secret path entirely; the
  architecture now persists bindings to PostgreSQL and the init
  container fetches them via `/internal/v1/pod-bootstrap` at boot. The
  live HTTP push to running pods is best-effort; `ErrNoRunningPod` is
  an accepted, documented transient state. The "no-op for first-time
  delivery" is the intended behavior in the new architecture. Added
  `TestSecretService_G28_BindingsSurviveNoPodState` to lock the
  persistence invariant — bindings survive the no-pod window and are
  visible to the bootstrap read path (`GetBindings`) when the pod
  eventually boots.

### Threat model reconciliation

A fresh audit of the 50 gaps in
`design/stories/epic-17-security-review/THREAT-MODEL.md` against the
current code found 6 rows whose status no longer matched reality. This
entry reconciles the threat model without changing any production code.

- **G29 — Path-traversal `mount_path` accepted by API → 🟢 Fixed (was Medium/Open).**
  The threat-model row claimed `POST /api/v1/secrets` accepts
  `mount_path = "../../etc/passwd"`. `validateMountPath` was added at
  `pkg/secrets/secret_service.go:582` (Bug 13 in worklog 0085); it is
  called from line 563 BEFORE secret creation. Stale row corrected.
- **G45 — Legacy `source /sandbox-cfg/env` in entrypoint → 🟢 Fixed (was Low/Open).**
  US-35.7 moved the env-secret source path to `/sandbox-runtime/secrets-env`.
  The legacy `/sandbox-cfg/env` source no longer exists in
  `entrypoint-opencode.sh`. Stale row corrected.
- **G50 — Decrypt operations not audited → 🟢 Fixed (was Medium/Open).**
  The threat-model row claimed `NewAuditedProvider` had zero call sites.
  US-50.12 wired it at three production sites in `api/internal/app/app.go`:
  `app.go:408` (providerCredsProv), `app.go:409` (orgCredsProv),
  `app.go:624` (apiKeyProv). Every Decrypt on those providers now logs
  to `secret_audit_log`. Stale row corrected.
- **G4 — No mTLS between API and sandbox pods → 🟡 Accepted (was Medium/Open).**
  Real gap, but the fix requires either a service mesh (Linkerd/Istio)
  or per-workspace certificate provisioning — both substantial
  infrastructure additions outside the scope of threat-model-gap fixes.
  Compensating controls: NetworkPolicy default-deny, RFC1918/CGNAT
  egress filter, explicit header allowlist (G34 fix), per-request
  basic-auth rotation. Operator runbook: deploy Linkerd or Istio in
  `inject` mode on the LLMSafeSpaces namespace to close this gap
  without code changes.
- **G30 — Egress NetPol allows external DNS resolvers → 🟡 Accepted (was Medium/Open).**
  Real gap; standard Kubernetes `NetworkPolicy` cannot restrict DNS by
  destination domain. Closing requires Cilium FQDN policies, Calico
  `GlobalNetworkPolicy`, or a custom filtering resolver — operator
  infrastructure decisions. Compensating controls: workspace pods use
  cluster DNS by default; egress allowlist blocks RFC1918/CGNAT; DNS
  exfil bandwidth is naturally limited.
- **G40 — Agentd user port (4097) has no application-layer auth → 🟡 Accepted (was Medium/Open).**
  Real defense-in-depth gap; the trust boundary is the NetworkPolicy
  (`workspace-network-policy.yaml`) — only API server pods can reach
  workspace pods on port 4097. Adding `requireBearerToken` at the
  application layer is defense-in-depth that the existing controls
  make redundant for the documented deployment topologies.

Threat model counts: 26 Fixed / 16 Open / 8 Accepted →
**29 Fixed / 10 Open / 11 Accepted** (50 total). Revision 3.0.

The remaining 10 open gaps are tracked in the threat model under
"Open gaps (require remediation)": G6, G9, G13, G21, G41, G42, G43,
G44, G46, G47. Subsequent PRs will close the code-fixable ones.

- **G6/G41 — `/secrets/:id/reveal` per-route rate limit (Medium).**
  The reveal endpoint at `POST /api/v1/secrets/:id/reveal` accepts
  the user's password as input to re-authenticate before decrypting.
  Without a per-endpoint cap, a single IP could attempt 100 password
  guesses per minute against the global limiter. The route is now in
  `DefaultRouterConfig.PerRouteRateLimitConfig.Routes` at 5/min + burst
  5 — matches the legitimate-user pattern (re-reveal several secrets in
  quick succession) while making brute-force impractical. Closes both
  G6 and G41 (duplicate rows for the same gap). Regression:
  `TestRouter_G41_RevealSecretRateLimited`.

- **G21 — `/sandbox-cfg/password` mode 0600 (Medium).** The init
  container's `cp /mnt/secrets/password/password /sandbox-cfg/password`
  preserved the K8s Secret's `defaultMode: 420` (0644), leaving the
  opencode basic-auth password world-readable in the pod filesystem.
  Replaced with `install -m 0600` so the mode is set atomically with
  the copy. Regression: the existing `TestE2E_Reconcile_*` test now
  asserts the `install -m 0600` line in the rendered credScript.

- **G42 — SSE connection tracking prunes stale entries (Medium).** The
  `sseConnCounts` global map grew monotonically — every distinct
  client IP that ever attempted a connection left a permanent entry.
  `sseConnAllowed` now opportunistically prunes expired entries on
  every call (O(N) sweep, N bounded by per-IP rate limit). Regression:
  `TestSSEConnAllowed_G42_PrunesStaleEntries`.

- **G44 — Pod-level RunAsNonRoot (Low).** `buildPodSecurityContext`
  set RunAsUser/RunAsGroup/FSGroup/SeccompProfile but not
  RunAsNonRoot. Every container today sets it explicitly at the
  container level, but a future sidecar without its own
  SecurityContext would inherit the pod default (nil) and could run
  as root. Added `RunAsNonRoot: &true` at the pod level — the kubelet
  enforces it by refusing to start any container resolving to UID 0.
  Regression: `TestG44_PodSecurityContextHasRunAsNonRoot`.

- **G46 — Silent password file read failure now fatal (Low).**
  `readAgentPassword` previously logged a Warn on file-read error and
  returned an empty string. The workspace would start silently non-
  functional — opencode without auth, every proxy request fails basic-
  auth. Now logs at Error and calls `os.Exit(1)` so the pod enters
  CrashLoopBackOff, surfacing the failure as a pod-level signal.
  Regression: `TestReadAgentPassword_HappyPath` (error path uses
  os.Exit, documented as not unit-testable without subprocess exec).

- **G47 — Inference relay secret no longer exposed as CLI arg (Low).**
  The Helm chart's plaintext fallback
  `--inference-relay-secret={{ .Values.inferenceRelaySecret }}`
  rendered the secret into the controller's container args, visible
  in `kubectl get pod -o yaml` and audit logs. Removed the fallback;
  operators who set `inferenceRelaySecret` without configuring
  `externalSecret.create` or `externalSecret.existingSecret` now get
  a `helm template`-time error with an actionable remediation message.
  Regression: `TestControllerArgs_G47_NoPlaintextRelaySecretFallback`
  (fail-fast) and `TestControllerArgs_G47_EnvVarPathStillWorks`
  (legitimate path).

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
