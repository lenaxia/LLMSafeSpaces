# LLMSafeSpace Threat Model

**Status:** Active
**Scope:** Full system — API, Controller, Runtime, Frontend, Infrastructure

---

## 1. System Overview

LLMSafeSpace is a Kubernetes-native platform that runs AI agents (opencode serve) in isolated sandbox pods. Users interact via REST API, SSE streaming, MCP protocol, or React frontend. The system manages credentials, workspaces (PVC-backed), and sandbox lifecycle.

### Trust Boundaries

```
┌─────────────────────────────────────────────────────────────────────────┐
│ EXTERNAL (Untrusted)                                                    │
│  • End users (browser, SDK, MCP client)                                 │
│  • LLM providers (OpenAI, Anthropic, etc.)                              │
│  • Package registries (PyPI, npm, GitHub)                               │
│  • Mise tool registry (jdx/mise releases on GitHub)                     │
└────────────────────────────┬────────────────────────────────────────────┘
                             │ TLS / JWT / API Key
                             ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ BOUNDARY 1: Ingress → API Server                                        │
│  • Authentication (JWT + API key)                                        │
│  • Rate limiting (global 100/min default)                                │
│  • Input validation + body size limits                                   │
│  • CORS enforcement (explicit allow-list, no wildcard)                   │
│  • Security headers (CSP, HSTS, X-Frame-Options, Permissions-Policy)    │
└────────────────────────────┬────────────────────────────────────────────┘
                             │ Internal HTTP / K8s API
                             ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ BOUNDARY 2: API Server → Kubernetes Cluster                             │
│  • RBAC (ServiceAccount, namespace-scoped by default)                    │
│  • CRD operations                                                        │
│  • Secret management                                                     │
│  • Proxy to sandbox pods (pod IP:agentd port, plain HTTP — G4)          │
└────────────────────────────┬────────────────────────────────────────────┘
                             │ Pod network / K8s API
                             ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ BOUNDARY 3: Controller → Sandbox Pods                                   │
│  • Pod creation with hardened security context                           │
│  • Credential injection via init containers                              │
│  • NetworkPolicy default-deny ingress + egress allow-list (shipped)      │
│  • PVC lifecycle                                                         │
└────────────────────────────┬────────────────────────────────────────────┘
                             │ Filesystem / Network
                             ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ BOUNDARY 4: Sandbox Pod → External World                                │
│  • Agent (opencode serve) executes LLM-directed actions                  │
│  • Egress to LLM APIs (always allowed)                                   │
│  • Egress to allowlisted domains (NetworkPolicy-enforced)                │
│  • Credential access (tmpfs-mounted, never on PVC)                       │
│  • No SA token automounted                                               │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Assets (What We Protect)

| Asset | Sensitivity | Location | Impact if Compromised |
|-------|-------------|----------|----------------------|
| User LLM API keys | Critical | K8s Secret → tmpfs in pod (`/sandbox-cfg`) | Financial loss, unauthorized API usage |
| User SSH keys / Git tokens | Critical | K8s Secret → tmpfs in pod | Source code theft, supply chain attack |
| User DEK (data encryption key) | Critical | Redis session cache (memory) | All user secrets decryptable |
| User password hash (bcrypt cost 12) | High | PostgreSQL | Offline brute-force → credential access |
| JWT signing key | Critical | API server config/env | Full impersonation of any user |
| PostgreSQL credentials | Critical | K8s Secret (auto-generated) | Full database access |
| Redis credentials | High | K8s Secret (auto-generated) | Session hijacking, cache poisoning |
| Server master KEK (root of trust) | Critical | File mount `/var/run/secrets/llmsafespaces/master-secret` (US-50.1 default, mode 0440); legacy env var is a deprecated opt-in (`masterSecret.deliveryMethod=env`) | All at-rest credentials decryptable — admin/org LLM API keys, org SSO client secrets, API-key DEKs, Redis-cached user DEKs |
| Workspace PVC data | Medium | Kubernetes PV | User code/data exposure |
| Agent conversation history | Medium | opencode state in pod (`/workspace`) | Intellectual property leak |
| Controller ServiceAccount token | High | Pod automount (namespace-scoped by default) | Namespace-scoped CRD/Secret/Pod manipulation |
| API ServiceAccount token | High | Pod automount | Workspace-namespace Secret + CRD CRUD |
| etcd data (K8s Secrets at rest) | Critical | etcd storage | All credentials if unencrypted |
| Frontend session (JWT in browser) | High | cookie (HttpOnly, Secure) | Account takeover until expiry |

---

## 3. Threat Actors

| Actor | Capability | Motivation |
|-------|-----------|-----------|
| **Malicious user** | Authenticated, owns workspaces | Escape sandbox, access other tenants' data, steal credentials |
| **Compromised agent** | Code execution inside sandbox pod | Exfiltrate data, pivot to cluster, mine crypto |
| **Malicious LLM output** | Prompt injection via tool responses | Manipulate agent to exfiltrate, escalate, or destroy |
| **Malicious assistant content (browser)** | LLM emits markdown/HTML rendered in user's browser | Exfiltrate JWT from browser via crafted content if sanitization is bypassed |
| **Network attacker** | MITM on pod-to-pod traffic (G4: plain HTTP) | Credential interception, data exfiltration |
| **Compromised API server** | Full API memory + DB access | Access all active session DEKs, impersonate users |
| **Compromised controller** | K8s SA with Secret/Pod CRUD | Read credentials, create pods (namespace-scoped by default) |
| **Cluster admin (insider)** | kubectl access to all namespaces | Read Secrets, exec into pods |
| **Supply chain attacker** | Compromised opencode binary, Go dependency | Backdoor in all sandbox pods |

---

## 4. Attack Trees

### 4.1 Credential Theft

```
Goal: Steal user's LLM API key
├── [1] From sandbox pod (attacker = compromised agent)
│   ├── [1.1] Read /sandbox-cfg/secrets.json (init container writes plaintext)
│   │   └── Mitigation: tmpfs-backed emptyDir (pod_builder.go:136-139),
│   │                   main container mount read-only, runs as UID 1000
│   ├── [1.2] Read /tmp/agent-config.json (materialized by agentd)
│   │   └── Mitigation: File created with mode 0600 (pkg/agentd/secrets
│   │                   atomicWrite uses O_CREATE|O_TRUNC, 0o600).
│   │                   Same-UID processes can still read — residual risk.
│   ├── [1.3] Read environment variables (env-secret type)
│   │   └── Mitigation: /proc/self/environ readable by same user —
│   │                   ACCEPTED RISK (G3)
│   ├── [1.4] Exfiltrate via allowed egress domain
│   │   └── Mitigation: Redaction library exists (pkg/redact, 16 rules)
│   │                   but is NOT wired into the agent output pipeline.
│   │                   NetworkPolicy restricts egress destinations.
│   └── [1.5] Exfiltrate via DNS tunneling
│       └── Mitigation: External DNS resolvers reachable on port 53 (G30);
│                       audit logging; DNS rate limiting (operator responsibility)
├── [2] From API server (attacker = compromised API)
│   ├── [2.1] Read K8s Secrets directly (API SA has Secret read access)
│   │   └── Mitigation: Namespace-scoped Role
│   │                   (charts/llmsafespace/templates/rbac.yaml:234-285);
│   │                   etcd encryption at rest (operator responsibility)
│   ├── [2.2] Read DEK from Redis session cache
│       └── Mitigation: Redis auth required; auto-generated password
│                       (values.yaml:276-278); datastore NetworkPolicy
│                       restricts ingress (chart_test.go:419-470)
│   ├── [2.3] Read master KEK from /proc/1/environ (env-var delivery)
│   │   └── Mitigation: 🟢 Fixed (US-50.1) — default delivery is now a read-only
│   │       file mount at /var/run/secrets/llmsafespaces/master-secret (mode 0440,
│   │       subPath; api-deployment.yaml:112-130). The env-var path is a
│   │       deprecated opt-in (masterSecret.deliveryMethod=env). The file
│   │       loader fails closed on a mis-mounted/short active file
│   │       (secrets_adapters.go:525-571; app.go:1012-1017 deprecation Warn).
│   ├── [2.4] Read master KEK from API process memory (process compromise)
│   │   └── Mitigation: 🟡 Partial — under the default local providers
│   │       (Static/Sealed) the unsealed key lives in API memory for the pod's
│   │       lifetime; a process-level attacker calls Decrypt exactly as the
│   │       application does (pkg/secrets/root_key.go:136-151). KMS/Vault
│   │       Transit (H3) was deferred at Epic 50; **Epic 57 shipped AWS KMS
│   │       (US-57.1) and GCP KMS (US-57.3)** — under a cloud-KMS provider the
│   │       key material never leaves the HSM, so the attacker cannot recover
│   │       it for offline decrypt after the RCE is evicted; every decrypt is
│   │       independently logged in CloudTrail / Cloud Audit Logs. The value is
│   │       exfiltration-limitation + audit, not prevention of in-process abuse
│   │       while the RCE is live (pkg/secrets/README.md §Threat model).
│   └── [2.5] KEK compromise → mass credential decryption (blast radius)
│       └── Mitigation (partial) — zero-downtime rotation is now supported
│           end-to-end at the provider layer (US-50.4 multi-key StaticKeyProvider,
│           US-50.3 key_version columns, US-50.6 rotation-aware write path). The
│           operational rotate-kek CLI (US-50.5) is pending. Without rotation,
│           one compromised KEK decrypts every row it wraps. Domain separation
│           (US-50.7, merged) further narrows blast radius: the api_keys provider
│           now derives from purpose "master-kek" rather than reusing the Redis
│           DEK-cache key ("dek-cache"), so a Redis compromise cannot help unwrap
│           Postgres api_keys DEKs.
├── [3] From database (attacker = SQL injection or DB compromise)
│   ├── [3.1] Read wrapped_dek from user_keys table
│   │   └── Mitigation: Useless without password (HKDF-derived KEK)
│   └── [3.2] Read ciphertext from user_secrets table
│       └── Mitigation: AES-256-GCM encrypted; useless without DEK
├── [4] From etcd (attacker = cluster admin or etcd breach)
│   ├── [4.1] Read K8s Secret objects (plaintext if etcd unencrypted)
│   │   └── Mitigation: Operator MUST configure etcd encryption (A1)
│   └── [4.2] Read controller SA token → impersonate controller
│       └── Mitigation: Namespace-scoped by default (rbac.scope: "namespace");
│                       bound SA tokens (short-lived)
└── [5] From browser (attacker = malicious assistant content)
    ├── [5.1] XSS via crafted markdown bypassing rehype-sanitize
    │   └── Mitigation: rehype-sanitize default schema
    │                   (frontend/src/components/chat/MessagePart.tsx:74,84);
    │                   needs explicit fuzz testing (RT-7.9)
    └── [5.2] Token theft via leaked Authorization header to attacker domain
        └── Mitigation: API CORS hardened (explicit allow-list, no wildcard)
```

### 4.2 Sandbox Escape

```
Goal: Break out of sandbox pod to access cluster resources
├── [1] Container escape
│   ├── [1.1] Kernel exploit (CVE in container runtime)
│   │   └── Mitigation: RuntimeDefault seccomp profile
│   │                   (pod_builder.go:329-331); Drop ALL caps;
│   │                   AllowPrivilegeEscalation: false; regular patching (A3)
│   ├── [1.2] Exploit writable paths (/tmp, /workspace, /home/sandbox)
│   │   └── Mitigation: Read-only root filesystem (pod_builder.go:108);
│   │                   /tmp and /sandbox-cfg are tmpfs-backed with size limits
│   │                   (pod_builder.go:136-143); noexec NOT enforced on
│   │                   emptyDir volumes (G1 — K8s limitation, mitigated by
│   │                   seccomp + cap-drop + NoNewPrivs)
│   └── [1.3] Abuse capabilities
│       └── Mitigation: Drop ALL capabilities on ALL containers
│                       (pod_builder.go:111,395,417);
│                       AllowPrivilegeEscalation: false (pod_builder.go:110,394,416)
├── [2] Network escape
│   ├── [2.1] Access K8s API server
│   │   └── Mitigation: Egress NetworkPolicy blocks RFC1918 CIDRs
│   │                   (workspace-network-policy.yaml:120-130);
│   │                   SA token NOT automounted (pod_builder.go:196)
│   ├── [2.2] Access other pods in namespace
│   │   └── Mitigation: Default-deny ingress NetworkPolicy
│   │                   (workspace-network-policy.yaml:18-65)
│   ├── [2.3] Access node metadata (169.254.169.254)
│   │   └── Mitigation: blockedEgressCIDRs includes 169.254.0.0/16
│   │                   (values.yaml:403-407)
│   └── [2.4] Access Redis/PostgreSQL directly
│       └── Mitigation: Datastore NetworkPolicy restricts ingress
│                       (chart_test.go:419-470); auto-generated passwords
├── [3] Kubernetes API abuse
│   ├── [3.1] SA token automount in sandbox pod
│   │   └── Mitigation: AutomountServiceAccountToken=false
│   │                   (pod_builder.go:196, security_test.go:51-63)
│   └── [3.2] Exploit mounted secrets/configmaps
│       └── Mitigation: Only /sandbox-cfg (tmpfs emptyDir) and /workspace (PVC)
│                       and password Secret mounted; EnableServiceLinks=false
│                       (pod_builder.go:203) prevents service env leaks
└── [4] Resource exhaustion (DoS)
    ├── [4.1] Fork bomb / CPU exhaustion
    │   └── Mitigation: Resource limits (CPU/memory); PID limits
    ├── [4.2] Fill PVC storage
    │   └── Mitigation: Storage quotas; ephemeral storage limits
    └── [4.3] Open excessive network connections
        └── Mitigation: Connection limits in NetworkPolicy; conntrack limits
```

### 4.3 Cross-Tenant Data Access

```
Goal: User A accesses User B's workspace/credentials
├── [1] API-level
│   ├── [1.1] IDOR — guess workspace ID (UUID)
│   │   └── Mitigation: Ownership check on every API call; UUIDv4 unguessable
│   ├── [1.2] JWT manipulation (change user_id claim)
│   │   └── Mitigation: JWT signature verification (HMAC-SHA256);
│   │                   alg-confusion check enforces SigningMethodHMAC only
│   ├── [1.3] API key of another user
│   │   └── Mitigation: API keys per-user; bcrypt-hashed in DB; lsp_ prefix
│   └── [1.4] Replay revoked JWT
│       └── Mitigation: RevokeToken writes both token:<hash> and token:<jti>
│                       (auth.go:276-281); ValidateToken checks both
│                       (auth.go:368-376, 407-411); /auth/logout calls
│                       RevokeToken (router.go:462)
├── [2] Kubernetes-level
│   ├── [2.1] All workspaces in same namespace (label-based isolation only)
│   │   └── Mitigation: Per-workspace NetworkPolicy (default-deny ingress);
│   │                   ownership labels; controller enforces
│   ├── [2.2] PVC access from another pod
│   │   └── Mitigation: RWO access mode; one pod per workspace; controller
│   │                   enforces
│   └── [2.3] Secret name guessing (workspace-secrets-{uuid})
│       └── Mitigation: RBAC restricts Secret access to controller/API SA only
└── [3] Proxy-level
    ├── [3.1] Proxy to another user's pod IP
    │   └── Mitigation: Proxy resolves pod IP from CRD owned by authenticated
    │                   user; sandboxOwnershipMiddleware enforces
    └── [3.2] Session ID collision
        └── Mitigation: UUIDv4 session IDs; session-to-workspace binding
```

### 4.4 Prompt Injection / Agent Manipulation

```
Goal: Manipulate agent to perform unauthorized actions
├── [1] Indirect injection via tool output
│   ├── [1.1] Malicious content in fetched web page
│   │   └── Mitigation: Injection detection (not yet wired — design only);
│   │                   redaction library exists but not in pipeline
│   ├── [1.2] Malicious content in git repo
│   │   └── Mitigation: Agent-level defense (opencode's own guardrails)
│   └── [1.3] Malicious content in package metadata
│       └── Mitigation: mise uses MISE_GITHUB_ATTESTATIONS=1 (Dockerfile:269);
│                       opencode binary has no checksum verification (G9)
├── [2] Direct injection via user input
│   ├── [2.1] User crafts prompt to bypass agent guardrails
│   │   └── Mitigation: Out of scope (user attacking their own agent)
│   └── [2.2] Shared workspace — User A injects via workspace files
│       └── Mitigation: Workspaces are single-owner; no sharing in V2
└── [3] Exfiltration via agent
    ├── [3.1] Agent instructed to curl secrets to external URL
    │   └── Mitigation: NetworkPolicy restricts egress; no egress body
    │                   inspection — ACCEPTED RISK (G14)
    └── [3.2] Agent encodes secrets in DNS queries
        └── Mitigation: External DNS resolvers reachable on port 53 (G30);
                        DNS audit logging; accepted residual risk
```

### 4.5 Frontend XSS / Browser-Side Compromise

```
Goal: Steal user's JWT or perform actions in user's browser session
├── [1] Stored XSS via assistant message content
│   ├── [1.1] Malicious markdown bypasses rehype-sanitize default schema
│   │   └── Mitigation: rehype-sanitize on all ReactMarkdown usage
│   │                   (frontend/src/components/chat/MessagePart.tsx:74,84);
│   │                   default schema strips on*, javascript:, data: URIs;
│   │                   needs explicit fuzz testing (RT-7.9)
│   ├── [1.2] Tool output rendered as <pre> — no XSS surface
│   │   └── Mitigation: <pre> renders as text, not HTML; React auto-escapes
│   └── [1.3] Dangerous part types (HTML, raw)
│       └── Mitigation: Only known part types rendered (text/thinking/
│                       tool_use/tool_result/error); unknown returns null
├── [2] Reflected XSS via API error responses rendered in UI
│   └── Mitigation: API errors are text-only; React JSX auto-escapes;
│                   no v-html / dangerouslySetInnerHTML in chat components
└── [3] Clickjacking
    └── Mitigation: Frontend ingress sets CSP frame-ancestors 'none' and
                    X-Frame-Options DENY (values.yaml:580-585);
                    API security middleware sets same headers
                    (middleware/security.go:104,107)
```

---

## 5. Identified Gaps & Residual Risks

All gaps below have been verified against the codebase. Each entry cites exact file:line evidence.

**Status legend:**
- 🔴 **Open** — present in codebase, awaiting fix.
- 🟡 **Accepted** — risk accepted with documented rationale and compensating controls.
- 🟢 **Fixed** — remediated with regression test that prevents reintroduction.

| # | Gap | Severity | Status | Verified By | Fix / Recommendation |
|---|-----|----------|--------|-------------|----------------------|
| G1 | No `noexec` on emptyDir mounts | Low | 🟡 Accepted | `pod_builder.go:136-143` — tmpfs-backed but no `noexec` enforcement | K8s does not support `noexec` on emptyDir natively. Mitigated by RuntimeDefault seccomp + Drop ALL caps + NoNewPrivs + tmpfs (not disk). Accept with documented rationale. |
| **G2** | **Entrypoint shell injection via secret values** | High | 🟢 **Fixed** | Pre-fix: `entrypoint-common.sh:78` — single quote in PLAINTEXT escaped the literal | Secret materialization moved into `pkg/agentd/secrets` (typed Go package, atomic 0600 writes, `filepath.Rel` path traversal check). Bash entrypoint is a 35-line shim. Regression: 26 tests including 13-payload bash-subprocess corpus. |
| G3 | env-secret readable via /proc/self/environ | Medium | 🟡 Accepted | `entrypoint-opencode.sh:9-11` sources `/sandbox-runtime/secrets-env` into agent env (file:line updated post-US-35.7; original row cited the legacy `/tmp/secrets-env` path). The env vars are exported to the agentd+opencode child processes and readable via `/proc/self/environ` by any same-UID process. | Accepted risk; prefer `secret-file` type; document for operators. |
| G4 | No mTLS between API and sandbox pods | Medium | 🟡 **Accepted** | `api/internal/handlers/proxy.go:450` — `targetURL := fmt.Sprintf("http://%s:%d%s", podIP, opencodePort, targetPath)`, no TLSClientConfig. Verified still open against current code (line moved from 610 → 450 in a prior refactor). | **Accepted with documented operator responsibility.** mTLS between API server and workspace pods requires either (a) a service mesh (Linkerd, Istio) that injects mTLS transparently, or (b) per-workspace certificate provisioning (controller issues a cert per pod, API trusts the CA). Both are substantial infrastructure additions outside the scope of threat-model-gap fixes. Compensating controls: (1) `NetworkPolicy` default-deny ingress restricts which pods can reach the API↔pod traffic path; (2) the workspace network is RFC1918/CGNAT-filtered (`workspace-network-policy.yaml`); (3) the proxy uses an explicit header allowlist (G34 fix) so a MITM cannot inject headers; (4) the basic-auth password injected per-request is rotated per workspace. Operator runbook: deploy Linkerd or Istio in `inject` mode on the LLMSafeSpaces namespace to close this gap without code changes. Reclassify to Fixed when the chart ships a service-mesh reference implementation. |
| G5 | ~~Controller SA cluster-wide Secret access~~ | — | 🟢 **Fixed** | `values.yaml:460` defaults `rbac.scope: "namespace"`; `chart_test.go:696` regression | Default is namespace-scoped. Cluster scope is opt-in. Even in cluster mode, no mutating verbs on secrets/pods (chart_test.go:1411). |
| G6 | ~~No per-endpoint rate limit on secrets~~ | Medium | 🟢 **Fixed** | Pre-fix: `/api/v1/secrets/:id/reveal` was behind the global 100/min/IP limiter only. | G41 fix (same code path) added the route to `PerRouteRateLimitConfig.Routes` in `DefaultRouterConfig` with 5/min + burst 5. G6 and G41 are duplicates — both closed by the same change. Regression: `TestRouter_G41_RevealSecretRateLimited`. |
| G7 | SSE streams bypass injection-detection blocking | Low | 🟡 Accepted | Streaming endpoints cannot be blocked mid-stream; injection detector runs in non-streaming path only | Accepted: SSE is unidirectional; block action applies to non-streaming JSON responses. |
| G8 | ~~First-user-admin auto-promotion race~~ | — | 🟢 **Fixed** | `auth.go:570-576` — uses atomic SQL CTE; role promotion is atomic in the INSERT statement; no CountUsers→INSERT race | Fixed via database-layer atomicity. |
| G9 | opencode/gh binary downloaded without checksum verification | Medium | 🟡 **Accepted** | `runtimes/base/Dockerfile:142-154` (opencode) uses `curl --fail` over TLS only, no checksum. opencode upstream (`anomalyco/opencode`) does not publish checksums or Sigstore signatures — no verification is possible without upstream support. **gh CLI now verifies checksum** (G9 partial fix): the Dockerfile downloads the consolidated `gh_<version>_checksums.txt` file published by `cli/cli` releases, greps for the specific tarball hash, and compares before extracting. | **Accepted (opencode part):** upstream doesn't publish checksums. Compensating controls: release images are cosign-signed (image-level provenance via GitHub Actions OIDC + Rekor); Trivy image scanning on every release; Renovate digest-pinning opens PRs to pin Dockerfile FROM tags. **Fixed (gh part):** checksum verification via `checksums.txt` added in the Dockerfile. Reclassify to Fixed when opencode upstream publishes checksums or when the chart ships cosign binary verification at admission time. |
| G10 | Redis session cache not encrypted at rest | Low | 🟡 Accepted | Redis persistence is operator-configured | Document operator requirement: disable RDB/AOF persistence or enable disk encryption. |
| G11 | ~~No Pod Security Admission enforcement~~ | — | 🟢 **Fixed** | `namespace.yaml:20-25` sets `pod-security.kubernetes.io/enforce=restricted`; `values.yaml:19` defaults `podSecurityEnforce: "restricted"` | PSA labels enforce restricted profile on workspace namespace. |
| G12 | ~~Proxy ResponseHeaderTimeout 300s~~ | — | 🟢 **Fixed** | `proxy.go:128` — `ResponseHeaderTimeout: 60 * time.Second`; streaming endpoints bypass this client entirely | Reduced from 300s to 60s for non-streaming requests. |
| G13 | ~~Account lockout keyed on email only (DoS vector)~~ | Medium | 🟢 **Fixed** | Pre-fix: `auth.go:1000` — `lockoutKey := fmt.Sprintf("lockout:%s", email)` — attacker who knows victim email can lock them out from any IP. | New `WithClientIP(ctx, ip)` context-propagation helper; router sets it before calling `Login`. Lockout key now includes the client IP: `lockout:<email>:<ip>`. An attacker from a different IP cannot trigger the victim's lockout. Callers without `WithClientIP` fall back to email-only keying (backward compat). Regression: `TestLogin_G13_AttackerFromDifferentIPCannotLockVictim`, `TestLogin_G13_SameIPLockoutStillWorks`, `TestLogin_G13_NoIPContextFallsBackToEmailOnly`. |
| G14 | No egress request body inspection | High | 🟡 Accepted | No code path inspects outbound HTTP request bodies from sandbox pods | Accepted residual risk; minimize allowedDomains; document. |
| G15 | ~~Sandbox emptyDir is disk-backed, not tmpfs~~ | — | 🟢 **Fixed** | `pod_builder.go:136-143` — `sandbox-cfg` and `tmp` volumes use `StorageMediumMemory` with explicit size limits (4Mi, 64Mi) | All credential-bearing emptyDir volumes are tmpfs-backed with size limits. |
| **G16** | **No NetworkPolicy templates ship with the chart** | Critical | 🟢 **Fixed** | Pre-fix: no NetworkPolicy in chart | Chart ships `workspace-network-policy.yaml` with default-deny ingress and egress allow-list. `networkPolicy.enabled` defaults to `true`. Regression: 5 helm-render tests. |
| **G17** | **SA token automounted in sandbox pod** | High | 🟢 **Fixed** | Pre-fix: no `AutomountServiceAccountToken` field → defaulted to true | `pod_builder.go:196` sets `AutomountServiceAccountToken: &falseVal`. Regression: `security_test.go:51-63`. |
| **G18** | **JWT revocation broken (cache key mismatch)** | High | 🟢 **Fixed** | Pre-fix: RevokeToken wrote `token:<jti>`, ValidateToken read `token:<hash>` — keys never collided | `auth.go:276-281` writes both `token:<hash>` and `token:<jti>`. `auth.go:368-376,407-411` checks both. `/auth/logout` calls `RevokeToken` (router.go:462). Regression: 6 tests in `auth_revocation_test.go`. |
| G19 | ~~mise installs runtimes without attestation~~ | — | 🟢 **Fixed** | `Dockerfile:269,277` sets `MISE_GITHUB_ATTESTATIONS=1` | mise verifies Sigstore-backed GitHub attestations on every tool install. |
| **G20** | **Credential files written without atomic mode 0600** | Medium | 🟢 **Fixed** | Pre-fix: entrypoint used `>` with no chmod | `pkg/agentd/secrets` uses `os.OpenFile(path, O_CREATE|O_TRUNC, 0o600)`. Regression: `TestG20_AllFilesCreatedWithMode0600`. |
| G21 | ~~`/sandbox-cfg/password` mode 0644~~ | Medium | 🟢 **Fixed** | Pre-fix: `pod_builder.go:495` used `cp /mnt/secrets/password/password /sandbox-cfg/password`; `cp` preserves the source K8s Secret's `defaultMode: 420` (0644), leaving the password world-readable in the pod filesystem. | Replaced `cp` with `install -m 0600` in the init-container credScript. `install -m 0600` sets the mode atomically with the copy, so the file is never briefly world-readable even on slow filesystems. Regression: `health_test.go:TestE2E_Reconcile_*` now asserts the `install -m 0600` line in the rendered script. |
| G22 | ~~EnableServiceLinks leaks namespace topology~~ | — | 🟢 **Fixed** | `pod_builder.go:203` sets `EnableServiceLinks: &falseVal`. Regression: `security_test.go:490-499`. |
| G23 | `/workspace` PVC mount lacks `nosuid` | Medium | 🟡 Accepted | PVC mount lacks `nosuid,nodev` mount options | Documented in NOTES.txt:180-198 as operator responsibility via StorageClass mountOptions. Mitigated by runAsNonRoot + NoNewPrivs + cap-drop ALL. |
| G24 | ~~No seccompProfile on workspace pod~~ | — | 🟢 **Fixed** | `pod_builder.go:329-331` sets `SeccompProfile: RuntimeDefault` at pod level. Regression: `security_test.go:505-515`. |
| G25 | Secret value field logged unredacted in API request bodies | High | 🟢 **Fixed** | Pre-fix: `logging.go:41` `SensitiveFields` was `["password", "token", "secret", "key", "apiKey", "credit_card"]` — missing `"value"`. The secrets API carries the plaintext credential in `{"name":"...", "value":"sk-..."}`; that body was logged verbatim. | Two-layer fix. (1) Added `"value"` to `DefaultLoggingConfig.SensitiveFields` — defense in depth catching any logged JSON with a `value` field, even on paths not in the skip list. (2) Added `SkipPathPrefixes` to `LoggingConfig` and configured the default with credential-bearing paths (`/api/v1/secrets`, `/api/v1/account`, `/api/v1/auth`, `/api/v1/admin/provider-credentials`) so bodies on those paths are never logged at all. Either layer alone prevents the leak. Regression coverage: `TestLoggingMiddleware_G25_SecretsPathBodyNotLogged`, `TestLoggingMiddleware_G25_SkipPathPrefixes_MatchesNestedPaths`, `TestLoggingMiddleware_G25_SkipPathPrefixes_DoesNotMatchUnrelatedPaths`, `TestLoggingMiddleware_G25_ValueFieldInSensitiveFields`. |
| G26 | ~~Default Postgres/Redis passwords~~ | Critical | 🟢 **Fixed** | `values.yaml:276-278` auto-generates 32-char random passwords on install. Datastore NetworkPolicies restrict ingress (chart_test.go:419-470). |
| G27 | ~~Login response timing reveals registered emails~~ | — | 🟢 **Fixed** | `auth.go:698-701,709` — dummy bcrypt `CompareHashAndPassword` runs on DB-error and user-not-found paths. All failure branches return identical timing and same generic error message. |
| G28 | Workspace bind handler is a no-op for first-time secret delivery | High | 🟡 **Accepted** | Original row (now stale) claimed: "`PUT /api/v1/workspaces/<id>/bindings` returns 204 but K8s Secret is never created." **Epic 35 (secretless injection) removed the durable K8s Secret path entirely** — `EnsureSecretsManifest` is gone (`secrets.go:414-418` comment documents the removal). The architecture now: (1) `SetBindings` persists bindings to PostgreSQL `user_secret_bindings` inside an advisory-locked transaction (`pg_secret_store.go:301`); (2) the live HTTP push via `agentpush.Service.Push` is best-effort — `ErrNoRunningPod` is documented at `agentpush.go:70-75` as an accepted, transient state; (3) the init container fetches credentials at boot via `/internal/v1/pod-bootstrap`, which calls `GetBindings` to resolve what to inject. The "no-op for first-time delivery" was the intended behavior in the new architecture: bindings are durable in PostgreSQL, and first-time delivery happens at pod boot. | **Accepted**: the architecture intentionally defers first-time delivery to pod boot. The bindings are the durable source of truth; the live push is opportunistic. Risk: a workspace that never boots (stuck in Pending forever) never receives its bindings — but that workspace also has no pod to receive them, so the bindings are correctly idle. Invariant regression: `TestSecretService_G28_BindingsSurviveNoPodState` locks the persistence guarantee (SetBindings → GetBindings round-trip survives the no-pod window). The bootstrap path that consumes the bindings is independently covered by `TestPodBootstrap_ValidToken_ReturnsSecrets`. |
| G29 | ~~Path-traversal `mount_path` accepted by API~~ | Medium | 🟢 **Fixed** | Pre-fix threat-model row claimed: "API `POST /api/v1/secrets` accepts `mount_path = '../../etc/passwd'` with HTTP 201." That was true when the row was written, but `validateMountPath` was added at `pkg/secrets/secret_service.go:582-608` (Bug 13 in worklog 0085). It is called from `secret_service.go:563` BEFORE secret creation — rejects empty, absolute, the bare base dir, and any path whose `filepath.Clean + filepath.Rel` resolves outside the notional secrets base. Wraps `ErrInvalidMetadata` so callers map to 400. Defense in depth: the in-pod materializer's `resolveMountPath` (`pkg/agentd/secrets/secrets.go:286-307`) independently enforces the same rule at materialize time. | No code change required — already fixed. Threat-model row updated to reflect the existing validator. |
| G30 | Egress NetPol allows external DNS resolvers (e.g. 8.8.8.8:53) | Medium | 🟡 **Accepted** | "DNS to kube-dns" and "0.0.0.0/0 except RFC1918" rules are OR-ed — port 53 to 8.8.8.8 allowed by second rule. Verified still open against `helm/templates/workspace-network-policy.yaml`. | **Accepted with documented operator responsibility.** Standard Kubernetes `NetworkPolicy` cannot restrict DNS by destination domain (only by IP/CIDR). Closing this requires either (a) Cilium FQDN policies (`cilium.io/v2 CiliumNetworkPolicy` with `toFQDNs`), (b) Calico `GlobalNetworkPolicy` with domain rules, or (c) a custom DNS resolver that filters queries. All three are operator-infrastructure decisions, not application code. Compensating controls: (1) workspace pods use the cluster DNS (`kube-dns`) by default for service discovery; (2) the egress allowlist already blocks RFC1918/CGNAT; (3) DNS exfil bandwidth is naturally limited (DNS queries are small, ~512 bytes). Operator runbook: if your CNI is Cilium, apply a `CiliumNetworkPolicy` that restricts `toFQDNs` to a curated allowlist. If your CNI is Calico, use `GlobalNetworkPolicy` with `destination.domains`. Document IPv4-only operation if neither is available. Reclassify to Fixed if/when the chart ships Cilium and Calico reference policies. |
| G31 | ~~Frontend ingress lacks CSP and X-Frame-Options~~ | — | 🟢 **Fixed** | `values.yaml:580-585` configures CSP `frame-ancestors 'none'`, X-Frame-Options DENY, HSTS, X-Content-Type-Options, Referrer-Policy on frontend ingress. |
| G32 | No per-user workspace quota | Low | 🟡 Accepted | `POST /api/v1/workspaces` accepts unbounded creates | Intentional for single-tenant. Multi-tenant SaaS should add `MAX_WORKSPACES_PER_USER`. |
| **G33** | **Proxy routes have no workspace ownership check (IDOR)** | Critical | 🟢 **Fixed** | ~~`proxy.go:460-482` fetches workspace by ID without checking `Labels["user-id"] == userID`~~. `WorkspaceAccessMiddleware` (`api/internal/middleware/workspace_access.go`) is now wired on the `idGroup` (`router.go:287-288`), which all proxy routes inherit via `registerProxyRoutes(idGroup, ...)` (`router.go:327`). The middleware resolves the workspace, checks `WorkspaceOwner{UserID, OrgID}` against the caller's identity, and rejects with 403 on mismatch. List/Create endpoints (no `:id`) are scoped by owner in the handler. | Closed by wiring the existing middleware. Regression: `TestWorkspaceAccessMiddleware_WiredOnIdGroup_Forbidden`, `TestWorkspaceAccessMiddleware_AuthorizedReachesHandler`, and the rest of the `TestWorkspaceAccessMiddleware_*` battery in `api/internal/server/router_workspace_access_test.go:93-274`. Full historical analysis in `security-report-g33-g47.md`. |
| **G34** | **Proxy forwards all client headers to sandbox pod** | Critical | 🟢 **Fixed** | ~~`proxy.go:625-629` forwards Cookie, Origin, Referer, X-Forwarded-* and all custom headers to sandbox before SetBasicAuth overwrites Authorization~~. `proxy.go:470` now calls `copyRequestHeaders` (`proxy_helpers.go`), an explicit allowlist (`Content-Type`, `Accept`, `X-Request-ID`) — Cookie/Origin/Referer/X-Forwarded-*/custom headers are dropped. Hop-by-hop headers (RFC 7230 §6.1 + Upgrade) stripped in both directions via `hopByHopHeaders`. `Accept-Encoding` deliberately not forwarded (Go's transport handles gzip transparently). | PR [#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513). Regression: `TestProxy_G34_CallerAuthorizationNotForwarded` (e2e through real ProxyHandler). |
| **G35** | **RecoverAccount endpoint has no rate limiting** | High | 🟢 **Fixed** | Pre-fix: `router.go:549` mounted `POST /api/v1/account/recover` on the root router, behind only the global 100/min/IP rate limiter. The recovery key is 128-bit random (brute-force infeasible) but the endpoint does Argon2id work, making it a CPU-exhaustion DoS target. | New `PerRouteRateLimitMiddleware` (`api/internal/middleware/per_route_rate_limit.go`) applies a stricter per-route limit on top of the global limiter, with per-route bucket isolation (`<path>:<identity>` key) so users hitting `/recover` cannot deplete the budget for other routes. Default: 20 tokens/burst 5 (from the previously-dead-code `authRatePerMinute`/`authRateBurst` constants, now wired). Generic middleware — future endpoints (e.g. G41 `/secrets/:id/reveal`) can be added to the same routes map. Regression coverage: `TestRouter_G35_RecoverAccountRateLimited` (wiring), plus 5 unit tests in `per_route_rate_limit_test.go` covering bucket isolation, disabled-config, unprotected-paths-passthrough, and nil-service no-op. |
| **G36** | **Workspace secrets not cleaned on deletion** | High | 🟢 **Fixed** | Pre-fix: `phase_terminating.go:40-46` deleted only `workspace-pw-*`; `workspace-creds-*` persisted indefinitely after workspace deletion. The threat-model row referenced `deleteEphemeralSecretsSecret` which does not exist by that name — the actual primitive is `cleanupFailedWorkspaceSecrets` (`secrets.go:33`), which was already used in `recovery.go` for the Failed-phase path but not the graceful-termination path. | `handleTerminating` now calls `r.cleanupFailedWorkspaceSecrets(ctx, workspace)` after the explicit password-secret delete. Best-effort (failures logged, not propagated — the workspace is already being torn down and the finalizer must still release). `handleDeletion` (the CRD-deletion entry point) inherits the fix because it calls `handleTerminating`. Regression coverage: `TestHandleTerminating_G36_DeletesCredentialsSecret`, `TestHandleTerminating_G36_DoesNotDeleteOtherWorkspaceSecrets`. |
| **G37** | **No validation on workspace env var names** | High | 🟢 **Fixed** | Pre-fix: `api/internal/handlers/workspace_env.go:SetWorkspaceEnv` accepted any POSIX-shaped env-var name; the materialize-time `validateVarName` (`pkg/agentd/secrets/secrets.go:222`) checked only regex + length, no dangerous-name blocklist. A workspace owner could set `LD_PRELOAD`, `PATH`, `PYTHONPATH`, `BASH_ENV`, `DYLD_INSERT_LIBRARIES`, etc. via the env-secret mechanism and compromise every process spawned in the pod. The threat-model row's claim of a parallel agentd check at `pkg/agentd/secrets/secrets.go:277-296` was incorrect — that range is the path-traversal check for `mount_path`, not env-var names. | New shared `pkg/validation.ValidateEnvVarName` enforces POSIX shape, length ≤ 256, and a curated blocklist of ~30 dangerous names sourced from ld.so(8), bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. The API handler validates every name up front (fail-fast, no partial application); agentd's `validateVarName` now delegates to the same validator (defense-in-depth). Locale vars (`LANG`, `LC_ALL`, `TZ`) are intentionally NOT blocked — they don't execute code. Regression coverage: `TestSetWorkspaceEnv_RejectsBlockedNames`, `TestSetWorkspaceEnv_RejectsBlockedNamesCaseInsensitive`, `TestSetWorkspaceEnv_RejectsInvalidPOSIXNames`, `TestSetWorkspaceEnv_RejectsMixedBatch_NoPartialApply`, `TestSetWorkspaceEnv_AcceptsLocaleNames`, plus `TestValidateEnvVarName_*` in pkg/validation. |
| **G38** | **ChangePassword does not invalidate existing sessions** | High | 🟢 **Fixed** | Pre-fix: `secrets.go:597-632` (`RotateKeyHandler.ChangePassword`) called `KeyService.ChangePassword` (which evicts the caller's cached DEK and durable `jwt_sessions` row at `key_service.go:859-882`) and updated the bcrypt hash, but never revoked the JWT signatures themselves — stolen pre-change tokens stayed valid until natural expiry. | Handler now calls `auth.Service.RevokeAllUserSessions` after both the DEK re-wrap and bcrypt update commit, via a new optional `SessionRevoker` interface wired in `app.go` (`SetSessionRevoker`). Best-effort: revocation failure is logged and the change still reports success — the cryptographic change is irreversible. Mirrors the password-reset flow's existing revocation pattern (`password_reset.go:309-315`). Regression coverage: `TestChangePassword_RevokesAllSessionsOnSuccess`, `TestChangePassword_RevokerErrorIsNonFatal`, `TestChangePassword_WrongPasswordDoesNotRevoke`, `TestChangePassword_NoRevokerWired_StillSucceeds`, `TestChangePassword_Unauthenticated_Returns401`, and the extended `TestE2E_RealAuth_ChangePassword` which proves the pre-change JWT is rejected immediately after the change while a freshly-issued post-change JWT still works. |
| G39 | ~~Terminal WebSocket allows all origins~~ | Medium | 🟢 **Fixed** | ~~`terminal.go:126` — `CheckOrigin: func(r *http.Request) bool { return true }`; WebSocket security middleware not applied to terminal route~~. `terminal.go` now uses `newCheckOriginChecker` (`terminal.go:130-200`): same-origin by default (Origin host:port == request Host), plus an operator-controlled allowlist via `terminal.allowedOrigins` Helm value. Non-browser clients (no Origin) are accepted — they authenticate via the single-use ticket, not cookies. The dead `WebSocketSecurityMiddleware` and `RouterConfig.AllowedWebSocketOrigins` plumbing (the latter was never read by the router) have been removed; the gorilla Upgrader is the single enforcement point. | PR [#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515). Regression: `TestTerminal_G35_CrossOriginUpgradeRejected` and the 9-case `TestCheckTerminalOrigin_*` table. |
| G40 | Agentd user port (4097) has no application-layer auth | Medium | 🟡 **Accepted** | `cmd/workspace-agentd/agent_reload.go:23-26` documents: "Authentication: none at the application layer. The trust boundary is the Kubernetes NetworkPolicy which allows only the API server pod to reach the workspace pod on port agentd.AgentdPort (4097)." `/v1/reload-secrets` and `/v1/agent/reload` on the user port accept any request that reaches the port. | **Accepted with documented operator responsibility.** The trust boundary is the NetworkPolicy at `helm/templates/workspace-network-policy.yaml` — only the API server pods can reach workspace pods on port 4097. Adding `requireBearerToken` at the application layer is defense-in-depth, but: (1) the API server already authenticates per-request before calling agentd; (2) a workspace tenant cannot reach another tenant's pod (cross-tenant NetworkPolicy default-deny); (3) the per-workspace SA token (Epic 35) provides identity at the bootstrap layer. Compensating controls are sufficient for single-tenant and namespace-isolated multi-tenant deployments. Reclassify to Fixed if/when a deployment topology emerges where the NetworkPolicy trust boundary is insufficient (e.g. shared-namespace multi-tenant with per-tenant API servers). |
| G41 | ~~No per-endpoint rate limit on RevealSecret~~ | Medium | 🟢 **Fixed** | Pre-fix: `/api/v1/secrets/:id/reveal` was behind the global 100/min/IP limiter only. The endpoint takes the user's password as input to re-authenticate before decrypting — without a per-endpoint cap, a single IP can attempt 100 password guesses per minute. | Added `/api/v1/secrets/:id/reveal` to `PerRouteRateLimitConfig.Routes` in `DefaultRouterConfig` (the infrastructure shipped in G35's PR #538). 5/min + burst 5 matches the legitimate-user pattern (re-reveal several secrets in quick succession) while making brute-force impractical (5 attempts/min → 7,200/day; bcrypt cost 12 makes each attempt ~250ms → 30min CPU per 7,200 guesses). Regression: `TestRouter_G41_RevealSecretRateLimited`. |
| G42 | ~~SSE connection tracking has unbounded memory growth~~ | Medium | 🟢 **Fixed** | Pre-fix: `stream_user_events.go:38-40` — `sseConnCounts` global map never pruned. Every distinct client IP that ever attempted a connection left a permanent entry, unbounded over the process lifetime. | `sseConnAllowed` now opportunistically prunes expired entries on every call. Sweep is O(N) where N is the current entry count; acceptable because N is bounded by the per-IP rate limit (long-lived entries are pruned the moment they expire). Avoids a separate goroutine and the lifecycle complexity it would add. Regression: `TestSSEConnAllowed_G42_PrunesStaleEntries`. |
| G43 | ~~IPv6 egress not covered by workspace NetworkPolicy~~ | Medium | 🟢 **Fixed** | Pre-fix threat-model row claimed: "IPv6 `::/0` unrestricted." This is incorrect. The workspace egress NetworkPolicy has `policyTypes: [Egress]`, which default-denies ALL egress not explicitly allowed. The `allowedEgressCIDRs: [0.0.0.0/0]` matches IPv4 only (Kubernetes `ipBlock` CIDRs are address-family-specific). IPv6 traffic is denied by omission — no egress rule matches `::/0`. | No code change required. The implicit default-deny already restricts IPv6. Threat-model row corrected to reflect actual NetworkPolicy behavior. |
| G44 | ~~Workspace pod-level SecurityContext missing RunAsNonRoot~~ | Low | 🟢 **Fixed** | Pre-fix: `buildPodSecurityContext` set RunAsUser/RunAsGroup/FSGroup/SeccompProfile but NOT RunAsNonRoot. Every container today sets it explicitly at the container level, but a future sidecar added without its own SecurityContext would inherit the pod default (nil) and could run as root. | Added `RunAsNonRoot: &runAsNonRoot` to `buildPodSecurityContext`. The kubelet enforces RunAsNonRoot by refusing to start any container that resolves to UID 0, so pod-level setting makes the guarantee structural rather than per-container. Regression: `TestG44_PodSecurityContextHasRunAsNonRoot`. |
| G45 | ~~Legacy `source /sandbox-cfg/env` in entrypoint~~ | Low | 🟢 **Fixed** | Pre-fix threat-model row claimed: "`entrypoint-opencode.sh:8-10` sources file that is never created; bypasses secrets validation if ever created." US-35.7 moved the env-secret source path from `/sandbox-cfg/env` (never created) to `/sandbox-runtime/secrets-env` (tmpfs, written by the materializer). The legacy path no longer exists. | No code change required — already fixed. Threat-model row updated. Current entrypoint at `runtimes/base/tools/entrypoints/entrypoint-opencode.sh:9-11` sources only the correct path. |
| G46 | ~~Password file read failure is silent~~ | Low | 🟢 **Fixed** | Pre-fix: `cmd/workspace-agentd/main.go:134-140` (file:line in original row was stale; actual location is `readAgentPassword`) read the password file, logged a Warn on error, and returned an empty string. The workspace would start silently non-functional — opencode without auth, every proxy request fails basic-auth. | `readAgentPassword` now logs at Error and calls `os.Exit(1)` on file-read failure. The pod enters CrashLoopBackOff, which is the correct signal — the workspace cannot recover without operator intervention (recreate the workspace, or fix the Secret mount). Happy-path regression: `TestReadAgentPassword_HappyPath`. The error path (os.Exit) is documented; not unit-testable without subprocess execution. |
| G47 | ~~Inference relay secret exposed as CLI arg~~ | Low | 🟢 **Fixed** | Pre-fix: `controller-deployment.yaml:108` rendered `--inference-relay-secret={{ .Values.inferenceRelaySecret }}` directly into the controller's container args when `externalSecret` was not configured. The plaintext secret was visible in `kubectl get pod -o yaml`, monitoring scrapers, and audit logs. | Removed the plaintext fallback. Operators who set `inferenceRelaySecret` without configuring `externalSecret.create` or `externalSecret.existingSecret` now get a `helm template`-time error (`{{ fail "G47: ..." }}`) with an actionable remediation message. Forces operators onto the env-var/K8s-Secret path. Regression: `TestControllerArgs_G47_NoPlaintextRelaySecretFallback` (fail-fast) and `TestControllerArgs_G47_EnvVarPathStillWorks` (legitimate path). |
| **G48** | **Master KEK delivered as env var (exposed via /proc/1/environ)** | High | 🟢 **Fixed** | Pre-fix: `api-deployment.yaml` projected `LLMSAFESPACES_MASTER_SECRET` into the pod env, readable via `/proc/1/environ` by any same-UID process. | US-50.1: default delivery is now a read-only file mount at `/var/run/secrets/llmsafespaces/master-secret` (mode 0440, subPath). `secrets_adapters.go:525-571` reads `LLMSAFESPACES_MASTER_SECRET_FILE` (colon-separated for the rotation window). Legacy env path is a deprecated opt-in (`masterSecret.deliveryMethod=env`) with a startup Warn (`app.go:1017`). Regression: `chart_master_secret_test.go:121-200`. |
| G49 | ~~No operational KEK rotation capability (rotating is destructive)~~ | High | 🟢 **Fixed** | Pre-fix: rotating the master KEK orphaned every Postgres ciphertext. Foundation shipped: `StaticKeyProvider` multi-key decrypt (`root_key.go:62-118`, US-50.4); `key_version` columns on `api_keys` + `org_sso_configs` (migrations 42/43, US-50.3); rotation-aware write path populates active version on encrypt (US-50.6). | `rotate-kek` CLI ships at `cmd/rotate-kek/main.go` (153 lines: old/new master key loading, per-purpose key derivation, Postgres + Redis connections, `RotationCoordinator`, dry-run, resume-from, multi-table support). Operational runbook is the only remaining piece (track separately as a doc task, not a security gap). |
| G50 | ~~Decrypt operations are not audited~~ | Medium | 🟢 **Fixed** | Pre-fix threat-model row claimed: "`NewAuditedProvider` has zero call sites anywhere." That was true when the row was written, but US-50.12 wired `NewAuditedProvider` at three production sites in `api/internal/app/app.go`: `app.go:408` (providerCredsProv), `app.go:409` (orgCredsProv), `app.go:624` (apiKeyProv). Every Decrypt call on those providers now logs to `secret_audit_log` (fire-and-forget, never logs plaintext/ciphertext/key material). | No code change required — already fixed. Threat-model row updated. The `secret_audit_log` table now records decrypts for the three provider types, making authorized-decrypt exfiltration detectable. |

---

## 6. STRIDE Analysis

| Component | Spoofing | Tampering | Repudiation | Info Disclosure | DoS | Elevation |
|-----------|----------|-----------|-------------|-----------------|-----|-----------|
| **API Auth** | JWT forgery (mitigated: HMAC-only signing); API key theft | Token replay (mitigated: dual-key revocation) | No audit of failed auth | ~~Secret values logged unredacted (G25)~~ 🟢 Fixed | Account lockout abuse (G13); ~~no rate limit on recovery (G35)~~ 🟢 Fixed | ~~Sessions survive password change (G38)~~ 🟢 Fixed |
| **Proxy** | Workspace ID spoofing — ~~**NO OWNERSHIP CHECK (G33)**~~ 🟢 Fixed | Response tampering (plain HTTP — G4 accepted); ~~header injection to sandbox (G34)~~ 🟢 Fixed | No per-request audit trail | ~~All client headers forwarded to sandbox (G34)~~ 🟢 Fixed (explicit allowlist) | Connection exhaustion (mitigated: limits) | ~~Cross-tenant access via proxy (G33)~~ 🟢 Fixed |
| **Controller** | SA token theft (mitigated: bound tokens) | CRD manipulation (mitigated: webhooks) | Actions not individually audited | Namespace-scoped by default; ~~secrets persist after deletion (G36)~~ 🟢 Fixed | CRD spam (mitigated: quotas) | Namespace-scoped SA |
| **Sandbox Pod** | N/A (no auth within pod) | PVC data corruption | No file-level audit | Credential in env (G3 accepted); tmpfs-backed (G15 fixed); ~~env var injection (G37)~~ 🟢 Fixed; agentd user port unauthenticated (G40 accepted) | Resource exhaustion (mitigated: limits) | Container escape (mitigated: seccomp, caps; G1 accepted) |
| **Database** | SQL injection (mitigated: pgx parameterized) | Data corruption (mitigated: transactions) | No query audit log | Wrapped DEK exposure (mitigated: AES-256-GCM); credential rows now carry `key_version` for rotation (US-50.3); ~~authorized-decrypt exfiltration undetectable — audit wrapper built but not wired (G50)~~ 🟢 Fixed (AuditedProvider wired at app.go:408,409,624) | Connection exhaustion | N/A |
| **Redis** | Auth bypass (mitigated: auto-generated password, datastore NetworkPolicy) | Cache poisoning | No operation audit | DEK in memory (G10 accepted) | Memory exhaustion; SSE tracking leak (G42) | N/A |
| **Frontend** | Session theft via XSS (mitigated: rehype-sanitize — needs fuzzing) | DOM tampering (mitigated: React auto-escape) | No client audit | JWT in HttpOnly Secure cookie | UI freeze via huge messages | N/A |
| **Workspace Network** | Cross-tenant traffic (mitigated: NetworkPolicy) | N/A | NetworkPolicy events not audited | DNS exfil via external resolvers (G30 accepted); ~~IPv6 unrestricted (G43)~~ 🟢 Fixed (IPv6 denied by NetworkPolicy default-deny) | N/A | N/A |

---

## 7. Data Flow Diagram (Security-Relevant)

```
User ──[HTTPS/JWT]──► API Server ──[K8s API/SA token]──► K8s API Server
                           │                                    │
                           │ [HTTP/pod-IP:agentd — plain text]   │ [etcd]
                           ▼                                    ▼
                      Sandbox Pod                          K8s Secrets
                           │                              (credential store)
                           │ [HTTPS/API key]                    │
                           ▼                                    │
                      LLM Provider                              │
                                                                  │
User ──[HTTPS/JWT]──► API Server ──[pgx/TLS]──► PostgreSQL     │
                           │                    (user metadata,  │
                           │                     wrapped DEKs)   │
                           │                                    │
                           └──[go-redis/auth]──► Redis          │
                                                (session DEKs,   │
                                                 rate limits,    │
                                                 cache)          │
```

---

## 8. Assumptions (with Validation Evidence)

Per `README-LLM.md` Rule 7, every assumption must be validated. Where validation is not yet possible (operator runtime config), the assumption is flagged as a deployment-time precondition.

| # | Assumption | Validation Method | Status | Evidence / Action Required |
|---|-----------|-------------------|--------|----------------------------|
| A1 | etcd encryption at rest enabled | Pre-flight check at install time | **Unvalidated** | No chart guard exists. Document requirement in NOTES.txt. |
| A2 | NetworkPolicy CNI installed and functioning | Chart ships NetworkPolicy resources | **Validated** | `workspace-network-policy.yaml` ships with chart; `networkPolicy.enabled: true` by default. No preflight check that CNI actually enforces policies. |
| A3 | Node OS patched, container runtime current | Operator responsibility | **Unvalidated** | Document minimum K8s version (>=1.29) in chart NOTES.txt. |
| A4 | TLS termination at ingress | Helm chart values | **Validated** | `values.yaml:565` defaults `tls: true`. Operator must provide cert or use cert-manager. |
| A5 | Redis not exposed outside cluster | Service type review | **Validated** | Chart does not create a Redis service. Document network requirement. Datastore NetworkPolicy restricts ingress (chart_test.go:447-470). |
| A6 | PostgreSQL not exposed outside cluster | Service type review | **Validated** | Same as A5. Datastore NetworkPolicy restricts ingress (chart_test.go:419-443). |
| A7 | Container images from trusted registry | Dockerfile review | **Partial** | Base image uses tag-only `debian:bookworm-slim` (not digest-pinned). opencode downloaded over TLS without checksum verification (G9 — accepted; upstream doesn't publish checksums). gh CLI checksum verified via `checksums.txt` (G9 partial fix). mise uses MISE_GITHUB_ATTESTATIONS=1. AWS CLI has full PGP verification. |
| A8 | JWT signing keys rotated periodically | Code search | **Refuted (JWT); Partial (KEK)** | JWT signing keys: no rotation primitives in code; sourced from config at startup (restart-with-new-secret only). Master KEK: zero-downtime rotation is now supported at the provider layer — multi-key `StaticKeyProvider` (`root_key.go:82-109`, US-50.4), `key_version` columns (US-50.3), rotation-aware write path (US-50.6). The operational `rotate-kek` CLI (US-50.5) is the remaining piece. |
| A9 | rehype-sanitize default schema is sufficient for LLM output | Bypass fuzz testing | **Unvalidated** | Needs fuzz testing with known XSS bypass corpora (RT-7.9). |
| A10 | Operator deploys etcd, K8s, CNI per chart documentation | Documentation completeness | **Unvalidated** | Chart README lists requirements. No automated preflight check. |

---

## 9. Out-of-Scope (Explicitly Documented)

| Risk | Owner | Mitigation Reference |
|------|-------|---------------------|
| LLM provider security | OpenAI/Anthropic/etc. | Operator selects providers |
| opencode binary internals | upstream anomalyco/opencode | Pin version; track CVE feeds |
| Physical/social engineering | Operator | Out of scope |
| etcd encryption at rest | K8s operator | Documented (A1) |
| Node OS hardening | Cluster admin | Documented (A3) |
| gVisor runtime availability | Cluster admin | Optional defense-in-depth |

---

## 10. Implementation Status Summary

| Category | Total | Fixed | Open | Accepted |
|----------|-------|-------|------|----------|
| Security gaps (G1–G50) | 50 | 38 | 0 | 12 |

**Open gaps (require remediation):** _(none)_

**Accepted risks (documented rationale):** G1, G3, G4, G7, G9, G10, G14, G23, G28, G30, G32, G40

> G33 (proxy IDOR) and G34 (proxy header forwarding) — previously the
> Critical open gaps — are now **Fixed** as of the v0.3.0 network
> hardening sweep (PRs [#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513),
> [#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515)). G39
> (terminal WebSocket Origin) and G49 (operational KEK rotation — the
> `rotate-kek` CLI at `cmd/rotate-kek/main.go` ships) also closed. The
> G38, G37, G35, G25, G36 High-severity gaps were closed in the
> 2026-07-11 security sweep (PRs [#536](https://github.com/lenaxia/LLMSafeSpaces/pull/536)–[#540](https://github.com/lenaxia/LLMSafeSpaces/pull/540));
> G28 was reclassified as Accepted (architecture changed in Epic 35,
> PR [#541](https://github.com/lenaxia/LLMSafeSpaces/pull/541)). G29,
> G45, and G50 were reclassified as Fixed (stale rows — the validators
> and AuditedProvider wiring already existed, PR
> [#542](https://github.com/lenaxia/LLMSafeSpaces/pull/542)). G4, G30,
> and G40 were reclassified as Accepted (operator-side infrastructure
> dependencies). The highest-severity remaining open gaps are now G13
> (account-lockout DoS) and G21 (`/sandbox-cfg/password` mode 0644).

> **v2.2 count correction:** the prior summary (v2.1) reported 18 Fixed / 22 Open; a row-by-row recount of the table showed 16 Fixed / 24 Open. The recount is folded into this revision alongside the G48–G50 additions. Counts now reconcile exactly (17 + 26 + 7 = 50).

---

## 11. Revision History

| Version | Change |
|---------|--------|
| 2.14 | **Epic 60 (2026-07-12) removed the entire G47 surface.** The CF Worker relay was deleted (Zen blocks CF Worker IPs); the chart values (`inferenceRelayURL`, `inferenceRelaySecret`, `cloudflare.*`), the controller flag (`--inference-relay-secret`), the env var (`INFERENCE_RELAY_SECRET`), and the Helm Hook Job (`relay-secret-sync`) all went with it. G47 had been 🟢 Fixed since rev 2.10; the underlying surface is now gone entirely. The self-hosted InferenceRelay fleet (Epic 42) uses per-VM tokens managed by the router and never had a path-segment secret. No recount — gap count unchanged at **38 Fixed / 0 Open / 12 Accepted**. |
| 2.13 | Closed G43 (stale — IPv6 already restricted by NetworkPolicy default-deny; `0.0.0.0/0` matches IPv4 only). Reclassified G9 as Accepted (opencode upstream doesn't publish checksums; gh CLI checksum verification added as partial fix). **All 50 gaps resolved: 38 Fixed / 0 Open / 12 Accepted.** |
| 2.12 | Closed G13 (account lockout keyed on email only). New `WithClientIP(ctx, ip)` context-propagation helper; router sets it before calling Login. Lockout key now includes the client IP (`lockout:<email>:<ip>`). An attacker from a different IP cannot trigger the victim's lockout. Callers without WithClientIP fall back to email-only keying (backward compat). Counts: 36/3/11 → **37/2/11**. |
| 2.11 | Epic 57 shipped AWS KMS (US-57.1) + GCP KMS (US-57.3) + `migrate-kek` cross-provider CLI (US-57.2). Attack tree [2.4] (Read master KEK from API process memory) reclassified from "Residual / KMS deferred" to 🟡 Partial — under a cloud-KMS provider the key material never leaves the HSM, so the attacker cannot recover it for offline decrypt after the RCE is evicted and every decrypt is independently logged in CloudTrail / Cloud Audit Logs. The in-process-abuse-during-live-RCE property is unchanged (exfiltration-limitation + audit, not prevention). No gap-table recount — no gaps changed status; this update reflects the now-shipped mitigation option for an existing partial row. `pkg/secrets/README.md` threat-model matrix and `epic-50` "Deferred — External Providers (H3)" section (now resolved) updated to match. |
| 2.10 | Closed 7 code-fixable gaps (G6/G41 duplicates, G21, G42, G44, G46, G47). G6+G41: added `/api/v1/secrets/:id/reveal` to `PerRouteRateLimitConfig.Routes` (5/min + burst 5) using the middleware shipped in G35's PR #538. G21: replaced `cp` with `install -m 0600` in pod_builder.go credScript so the password file is mode 0600 regardless of source Secret defaultMode. G42: opportunistic prune of stale entries in `sseConnAllowed` on every call. G44: added `RunAsNonRoot: &true` to `buildPodSecurityContext` so future sidecars inherit non-root default. G46: `readAgentPassword` now logs Error + os.Exit(1) on file-read failure (was Warn + empty string → silently non-functional workspace). G47: removed plaintext fallback for `--inference-relay-secret` in controller-deployment.yaml; operators who set `inferenceRelaySecret` without `externalSecret` now get a helm-template-time error. Counts: 26/16/8 → **33/9/8**. |
| 2.9 | Reclassified G28 from Open to Accepted. Original row claimed "K8s Secret is never created" but Epic 35 (secretless injection) removed the durable K8s Secret path entirely. Architecture now: bindings persist to PostgreSQL (advisory-locked transaction), live HTTP push is best-effort (ErrNoRunningPod is documented transient state), init container fetches credentials at boot via /internal/v1/pod-bootstrap. The "no-op for first-time delivery" is the intended behavior in the new architecture. Added `TestSecretService_G28_BindingsSurviveNoPodState` to lock the persistence invariant. Counts: 26 Fixed / 16 Open / 8 Accepted. |
| 2.8 | Closed G36 (workspace secrets cleaned on deletion). `handleTerminating` now calls `r.cleanupFailedWorkspaceSecrets(ctx, workspace)` after the explicit password-secret delete. The primitive was already used in `recovery.go` for the Failed-phase path; this PR extends it to graceful termination. `handleDeletion` inherits the fix automatically. Threat-model row's reference to `deleteEphemeralSecretsSecret` corrected — the actual function name is `cleanupFailedWorkspaceSecrets`. Counts: 26 Fixed / 17 Open / 7 Accepted. |
| 2.7 | Closed G25 (secret `value` field no longer logged). Two-layer fix: (1) added `"value"` to `DefaultLoggingConfig.SensitiveFields`; (2) added `SkipPathPrefixes` to `LoggingConfig` with credential-bearing paths (`/api/v1/secrets`, `/api/v1/account`, `/api/v1/auth`, `/api/v1/admin/provider-credentials`) so bodies on those paths are never logged. Either layer alone prevents the leak. Counts: 25 Fixed / 18 Open / 7 Accepted. |
| 2.6 | Closed G35 (/account/recover per-route rate limit). New `PerRouteRateLimitMiddleware` (`api/internal/middleware/per_route_rate_limit.go`) applies a stricter per-route limit (default 20 tokens/burst 5, from the previously-dead-code `authRatePerMinute`/`authRateBurst` constants) on top of the global limiter, with per-route bucket isolation (`<path>:<identity>` key). Generic middleware — future endpoints (e.g. G41 `/secrets/:id/reveal`) can be added to the same routes map. Counts: 24 Fixed / 19 Open / 7 Accepted. |
| 2.5 | Closed G37 (workspace env-var name blocklist). New shared `pkg/validation.ValidateEnvVarName` enforces POSIX shape, length ≤ 256, and a curated blocklist of ~30 dangerous names (LD_PRELOAD, PATH, PYTHONPATH, BASH_ENV, DYLD_INSERT_LIBRARIES, etc.) sourced from ld.so(8), bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. The API handler validates every name up front (fail-fast, no partial application); agentd's `validateVarName` now delegates to the same validator (defense-in-depth). Locale vars (LANG, LC_ALL, TZ) are intentionally NOT blocked. Counts: 23 Fixed / 20 Open / 7 Accepted. |
| 2.4 | Closed G38 (ChangePassword session revocation). `RotateKeyHandler.ChangePassword` now invokes `auth.Service.RevokeAllUserSessions` after both the DEK re-wrap and bcrypt update commit, via a new optional `SessionRevoker` interface wired in `app.go`. Mirrors the password-reset flow's OWASP ASVS V2.5.2 revocation pattern. Best-effort (revocation failure logged, change still reports success). New unit tests + extended e2e regression proving the pre-change JWT is rejected immediately after the change. Counts: 22 Fixed / 21 Open / 7 Accepted. |
| 2.3 | v0.3.0 network hardening sweep reconciliation. Closed 4 gaps: G33 (proxy IDOR — `WorkspaceAccessMiddleware` confirmed wired on `idGroup` since the v2 design pass; the stale "Open" status was doc drift), G34 (proxy header forwarding — replaced with explicit `copyRequestHeaders` allowlist + hop-by-hop strip via `proxy_helpers.go`, PR #513), G39 (terminal WebSocket Origin — `CheckOrigin: return true` replaced with `newCheckOriginChecker` same-origin-default + operator allowlist via `terminal.allowedOrigins`, dead `WebSocketSecurityMiddleware` + `RouterConfig.AllowedWebSocketOrigins` removed, PR #515), G49 (operational KEK rotation — the `rotate-kek` CLI at `cmd/rotate-kek/main.go` ships; previously listed as Open because the row text said "CLI pending", but the CLI was merged and the row wasn't updated). Counts: 21 Fixed / 22 Open / 7 Accepted. The previous "Critical open gaps" callout (G33, G34) is removed — the highest-severity remaining open gaps are now G35 (RecoveryAccount no rate limiting) and G50 (decrypt audit not wired into production paths). STRIDE Proxy row updated to reflect closed items. |
| 2.2 | Synced with Epic 50 (master KEK hardening) landings (worklogs 0460, 0504, 0505, 0513, 0514, 0515). Added master KEK as an explicit critical asset (§2). Attack tree 4.1 gains nodes [2.3]–[2.5]: `/proc/1/environ` exposure now closed by the file-mount default (US-50.1, 🟢 G48); in-memory KEK exposure documented as residual with KMS/Vault deferred; KEK blast radius now bounded by rotation primitives (US-50.3/.4/.6) with the `rotate-kek` CLI pending, and narrowed by US-50.7 domain separation (api_keys provider moved off the Redis DEK-cache purpose). New gaps: G48 (KEK env delivery, Fixed), G49 (operational KEK rotation, Open — provider/columns/write-path shipped, CLI pending), G50 (decrypt audit, Open — `AuditedProvider` shipped but **not wired** into production decrypt paths; wiring awaits US-50.2 unification — `AdminKeyDeriver` still present at `credential_store.go:81`). Assumption A8 split: JWT rotation still refuted, KEK rotation now partial. STRIDE Database row updated (key_version + G50 detection gap). Recounted the gap table (prior summary was stale: 18/22 reported vs 16/24 actual) — now reconciles at 17 Fixed / 26 Open / 7 Accepted / 50 Total. |
| 2.1 | Added 15 new gaps (G33-G47) from adversarial re-validation. Critical: G33 (proxy IDOR — no ownership check), G34 (all client headers forwarded to sandbox). High: G35 (RecoveryAccount no rate limit), G36 (secrets persist after deletion), G37 (env var name injection), G38 (sessions survive password change). Full report in security-report-g33-g47.md. STRIDE table updated with new findings. Implementation status updated. |
| 2.0 | Full rewrite against verified code state. 12 gaps updated from stale "Open" to reflect actual fixed status (G5, G8, G11, G12, G15, G18, G19, G22, G24, G26, G27, G31). Attack trees updated to reflect current mitigations. STRIDE table updated. Assumptions re-validated against code. Trust boundaries updated. Removed stale file:line references to deleted controller.go code (now pod_builder.go). |
| 1.4 | Phase C remediation (worklogs 0095-0116). 19 of 32 G-findings claimed closed. |
| 1.3 | Pentest Phases 3-7 complete (worklogs 0088-0092). 12 new gaps surfaced (G21-G32). |
| 1.2 | Added Status column to gap table. G2, G16, G17, G18, G20 marked Fixed. |
| 1.1 | All gaps verified against code with file:line evidence; added G15-G20; assumptions A1-A10. |
| 1.0 | Initial threat model created. |
