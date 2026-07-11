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
│   │   └── Mitigation: Residual — the unsealed key lives in API memory for the
│   │       pod's lifetime; a process-level attacker calls Decrypt exactly as
│   │       the application does (pkg/secrets/root_key.go:136-151). KMS/Vault
│   │       Transit (H3) is deferred by design — it limits exfil + adds audit,
│   │       it does not prevent in-process abuse (epic-50 README §Deferred).
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
| G3 | env-secret readable via /proc/self/environ | Medium | 🟡 Accepted | `entrypoint-opencode.sh:13-14` sources `/tmp/secrets-env` into agent env | Accepted risk; prefer secret-file type; document for operators. |
| G4 | No mTLS between API and sandbox pods | Medium | 🔴 Open | `api/internal/handlers/proxy.go:610` — `http://%s:%d%s`, no TLSClientConfig | Implement mTLS via per-workspace cert or service mesh (Linkerd/Istio). |
| G5 | ~~Controller SA cluster-wide Secret access~~ | — | 🟢 **Fixed** | `values.yaml:460` defaults `rbac.scope: "namespace"`; `chart_test.go:696` regression | Default is namespace-scoped. Cluster scope is opt-in. Even in cluster mode, no mutating verbs on secrets/pods (chart_test.go:1411). |
| G6 | No per-endpoint rate limit on secrets | Medium | 🔴 Open | `router.go:237-256` — `/api/v1/secrets/*` behind global 100/min only; no stricter limit on `/secrets/:id/reveal` | Apply stricter per-endpoint rate limit (e.g. 10/min) on POST /secrets/:id/reveal. |
| G7 | SSE streams bypass injection-detection blocking | Low | 🟡 Accepted | Streaming endpoints cannot be blocked mid-stream; injection detector runs in non-streaming path only | Accepted: SSE is unidirectional; block action applies to non-streaming JSON responses. |
| G8 | ~~First-user-admin auto-promotion race~~ | — | 🟢 **Fixed** | `auth.go:570-576` — uses atomic SQL CTE; role promotion is atomic in the INSERT statement; no CountUsers→INSERT race | Fixed via database-layer atomicity. |
| G9 | opencode/gh binary downloaded without checksum verification | Medium | 🔴 Open | `runtimes/base/Dockerfile:142-154` (opencode), `Dockerfile:166-172` (gh) — `curl --fail` over TLS only, no checksum or Sigstore verification | opencode upstream does not publish checksums. GitHub CLI publishes `.sha256` — should be verified. Implement cosign at admission time. |
| G10 | Redis session cache not encrypted at rest | Low | 🟡 Accepted | Redis persistence is operator-configured | Document operator requirement: disable RDB/AOF persistence or enable disk encryption. |
| G11 | ~~No Pod Security Admission enforcement~~ | — | 🟢 **Fixed** | `namespace.yaml:20-25` sets `pod-security.kubernetes.io/enforce=restricted`; `values.yaml:19` defaults `podSecurityEnforce: "restricted"` | PSA labels enforce restricted profile on workspace namespace. |
| G12 | ~~Proxy ResponseHeaderTimeout 300s~~ | — | 🟢 **Fixed** | `proxy.go:128` — `ResponseHeaderTimeout: 60 * time.Second`; streaming endpoints bypass this client entirely | Reduced from 300s to 60s for non-streaming requests. |
| G13 | Account lockout keyed on email only (DoS vector) | Medium | 🔴 Open | `auth.go:686` — `lockoutKey := fmt.Sprintf("lockout:%s", email)` — attacker who knows victim email can lock them out from any IP | Add IP component to lockout key, or use progressive delays + CAPTCHA. |
| G14 | No egress request body inspection | High | 🟡 Accepted | No code path inspects outbound HTTP request bodies from sandbox pods | Accepted residual risk; minimize allowedDomains; document. |
| G15 | ~~Sandbox emptyDir is disk-backed, not tmpfs~~ | — | 🟢 **Fixed** | `pod_builder.go:136-143` — `sandbox-cfg` and `tmp` volumes use `StorageMediumMemory` with explicit size limits (4Mi, 64Mi) | All credential-bearing emptyDir volumes are tmpfs-backed with size limits. |
| **G16** | **No NetworkPolicy templates ship with the chart** | Critical | 🟢 **Fixed** | Pre-fix: no NetworkPolicy in chart | Chart ships `workspace-network-policy.yaml` with default-deny ingress and egress allow-list. `networkPolicy.enabled` defaults to `true`. Regression: 5 helm-render tests. |
| **G17** | **SA token automounted in sandbox pod** | High | 🟢 **Fixed** | Pre-fix: no `AutomountServiceAccountToken` field → defaulted to true | `pod_builder.go:196` sets `AutomountServiceAccountToken: &falseVal`. Regression: `security_test.go:51-63`. |
| **G18** | **JWT revocation broken (cache key mismatch)** | High | 🟢 **Fixed** | Pre-fix: RevokeToken wrote `token:<jti>`, ValidateToken read `token:<hash>` — keys never collided | `auth.go:276-281` writes both `token:<hash>` and `token:<jti>`. `auth.go:368-376,407-411` checks both. `/auth/logout` calls `RevokeToken` (router.go:462). Regression: 6 tests in `auth_revocation_test.go`. |
| G19 | ~~mise installs runtimes without attestation~~ | — | 🟢 **Fixed** | `Dockerfile:269,277` sets `MISE_GITHUB_ATTESTATIONS=1` | mise verifies Sigstore-backed GitHub attestations on every tool install. |
| **G20** | **Credential files written without atomic mode 0600** | Medium | 🟢 **Fixed** | Pre-fix: entrypoint used `>` with no chmod | `pkg/agentd/secrets` uses `os.OpenFile(path, O_CREATE|O_TRUNC, 0o600)`. Regression: `TestG20_AllFilesCreatedWithMode0600`. |
| G21 | `/sandbox-cfg/password` mode 0644 | Medium | 🔴 Open | `pod_builder.go:350` — `cp /mnt/secrets/password/password /sandbox-cfg/password`; Secret `defaultMode: 420` (0644) preserved by `cp` | Replace `cp` with `install -m 0600` in the init-container credScript. Distinct from G20 (different code path). |
| G22 | ~~EnableServiceLinks leaks namespace topology~~ | — | 🟢 **Fixed** | `pod_builder.go:203` sets `EnableServiceLinks: &falseVal`. Regression: `security_test.go:490-499`. |
| G23 | `/workspace` PVC mount lacks `nosuid` | Medium | 🟡 Accepted | PVC mount lacks `nosuid,nodev` mount options | Documented in NOTES.txt:180-198 as operator responsibility via StorageClass mountOptions. Mitigated by runAsNonRoot + NoNewPrivs + cap-drop ALL. |
| G24 | ~~No seccompProfile on workspace pod~~ | — | 🟢 **Fixed** | `pod_builder.go:329-331` sets `SeccompProfile: RuntimeDefault` at pod level. Regression: `security_test.go:505-515`. |
| G25 | Secret value field logged unredacted in API request bodies | High | 🟢 **Fixed** | Pre-fix: `logging.go:41` `SensitiveFields` was `["password", "token", "secret", "key", "apiKey", "credit_card"]` — missing `"value"`. The secrets API carries the plaintext credential in `{"name":"...", "value":"sk-..."}`; that body was logged verbatim. | Two-layer fix. (1) Added `"value"` to `DefaultLoggingConfig.SensitiveFields` — defense in depth catching any logged JSON with a `value` field, even on paths not in the skip list. (2) Added `SkipPathPrefixes` to `LoggingConfig` and configured the default with credential-bearing paths (`/api/v1/secrets`, `/api/v1/account`, `/api/v1/auth`, `/api/v1/admin/provider-credentials`) so bodies on those paths are never logged at all. Either layer alone prevents the leak. Regression coverage: `TestLoggingMiddleware_G25_SecretsPathBodyNotLogged`, `TestLoggingMiddleware_G25_SkipPathPrefixes_MatchesNestedPaths`, `TestLoggingMiddleware_G25_SkipPathPrefixes_DoesNotMatchUnrelatedPaths`, `TestLoggingMiddleware_G25_ValueFieldInSensitiveFields`. |
| G26 | ~~Default Postgres/Redis passwords~~ | Critical | 🟢 **Fixed** | `values.yaml:276-278` auto-generates 32-char random passwords on install. Datastore NetworkPolicies restrict ingress (chart_test.go:419-470). |
| G27 | ~~Login response timing reveals registered emails~~ | — | 🟢 **Fixed** | `auth.go:698-701,709` — dummy bcrypt `CompareHashAndPassword` runs on DB-error and user-not-found paths. All failure branches return identical timing and same generic error message. |
| G28 | Workspace bind handler is a no-op for first-time secret delivery | High | 🟡 **Accepted** | Original row (now stale) claimed: "`PUT /api/v1/workspaces/<id>/bindings` returns 204 but K8s Secret is never created." **Epic 35 (secretless injection) removed the durable K8s Secret path entirely** — `EnsureSecretsManifest` is gone (`secrets.go:414-418` comment documents the removal). The architecture now: (1) `SetBindings` persists bindings to PostgreSQL `user_secret_bindings` inside an advisory-locked transaction (`pg_secret_store.go:301`); (2) the live HTTP push via `agentpush.Service.Push` is best-effort — `ErrNoRunningPod` is documented at `agentpush.go:70-75` as an accepted, transient state; (3) the init container fetches credentials at boot via `/internal/v1/pod-bootstrap`, which calls `GetBindings` to resolve what to inject. The "no-op for first-time delivery" was the intended behavior in the new architecture: bindings are durable in PostgreSQL, and first-time delivery happens at pod boot. | **Accepted**: the architecture intentionally defers first-time delivery to pod boot. The bindings are the durable source of truth; the live push is opportunistic. Risk: a workspace that never boots (stuck in Pending forever) never receives its bindings — but that workspace also has no pod to receive them, so the bindings are correctly idle. Invariant regression: `TestSecretService_G28_BindingsSurviveNoPodState` locks the persistence guarantee (SetBindings → GetBindings round-trip survives the no-pod window). The bootstrap path that consumes the bindings is independently covered by `TestPodBootstrap_ValidToken_ReturnsSecrets`. |
| G29 | Path-traversal `mount_path` accepted by API | Medium | 🔴 Open | API `POST /api/v1/secrets` accepts `mount_path = "../../etc/passwd"` with HTTP 201 | Materialize-time validation in `pkg/agentd/secrets/secrets.go:277-296` blocks the real exploit. API should reject up-front with same `filepath.Clean + filepath.Rel` check. |
| G30 | Egress NetPol allows external DNS resolvers (e.g. 8.8.8.8:53) | Medium | 🔴 Open | "DNS to kube-dns" and "0.0.0.0/0 except RFC1918" rules are OR-ed — port 53 to 8.8.8.8 allowed by second rule | Standard NetPol limitation. Use Cilium FQDN policies or Calico GlobalNetworkPolicy. Enables DNS exfil/tunnelling. |
| G31 | ~~Frontend ingress lacks CSP and X-Frame-Options~~ | — | 🟢 **Fixed** | `values.yaml:580-585` configures CSP `frame-ancestors 'none'`, X-Frame-Options DENY, HSTS, X-Content-Type-Options, Referrer-Policy on frontend ingress. |
| G32 | No per-user workspace quota | Low | 🟡 Accepted | `POST /api/v1/workspaces` accepts unbounded creates | Intentional for single-tenant. Multi-tenant SaaS should add `MAX_WORKSPACES_PER_USER`. |
| **G33** | **Proxy routes have no workspace ownership check (IDOR)** | Critical | 🟢 **Fixed** | ~~`proxy.go:460-482` fetches workspace by ID without checking `Labels["user-id"] == userID`~~. `WorkspaceAccessMiddleware` (`api/internal/middleware/workspace_access.go`) is now wired on the `idGroup` (`router.go:287-288`), which all proxy routes inherit via `registerProxyRoutes(idGroup, ...)` (`router.go:327`). The middleware resolves the workspace, checks `WorkspaceOwner{UserID, OrgID}` against the caller's identity, and rejects with 403 on mismatch. List/Create endpoints (no `:id`) are scoped by owner in the handler. | Closed by wiring the existing middleware. Regression: `TestWorkspaceAccessMiddleware_WiredOnIdGroup_Forbidden`, `TestWorkspaceAccessMiddleware_AuthorizedReachesHandler`, and the rest of the `TestWorkspaceAccessMiddleware_*` battery in `api/internal/server/router_workspace_access_test.go:93-274`. Full historical analysis in `security-report-g33-g47.md`. |
| **G34** | **Proxy forwards all client headers to sandbox pod** | Critical | 🟢 **Fixed** | ~~`proxy.go:625-629` forwards Cookie, Origin, Referer, X-Forwarded-* and all custom headers to sandbox before SetBasicAuth overwrites Authorization~~. `proxy.go:470` now calls `copyRequestHeaders` (`proxy_helpers.go`), an explicit allowlist (`Content-Type`, `Accept`, `X-Request-ID`) — Cookie/Origin/Referer/X-Forwarded-*/custom headers are dropped. Hop-by-hop headers (RFC 7230 §6.1 + Upgrade) stripped in both directions via `hopByHopHeaders`. `Accept-Encoding` deliberately not forwarded (Go's transport handles gzip transparently). | PR [#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513). Regression: `TestProxy_G34_CallerAuthorizationNotForwarded` (e2e through real ProxyHandler). |
| **G35** | **RecoverAccount endpoint has no rate limiting** | High | 🟢 **Fixed** | Pre-fix: `router.go:549` mounted `POST /api/v1/account/recover` on the root router, behind only the global 100/min/IP rate limiter. The recovery key is 128-bit random (brute-force infeasible) but the endpoint does Argon2id work, making it a CPU-exhaustion DoS target. | New `PerRouteRateLimitMiddleware` (`api/internal/middleware/per_route_rate_limit.go`) applies a stricter per-route limit on top of the global limiter, with per-route bucket isolation (`<path>:<identity>` key) so users hitting `/recover` cannot deplete the budget for other routes. Default: 20 tokens/burst 5 (from the previously-dead-code `authRatePerMinute`/`authRateBurst` constants, now wired). Generic middleware — future endpoints (e.g. G41 `/secrets/:id/reveal`) can be added to the same routes map. Regression coverage: `TestRouter_G35_RecoverAccountRateLimited` (wiring), plus 5 unit tests in `per_route_rate_limit_test.go` covering bucket isolation, disabled-config, unprotected-paths-passthrough, and nil-service no-op. |
| **G36** | **Workspace secrets not cleaned on deletion** | High | 🟢 **Fixed** | Pre-fix: `phase_terminating.go:40-46` deleted only `workspace-pw-*`; `workspace-creds-*` persisted indefinitely after workspace deletion. The threat-model row referenced `deleteEphemeralSecretsSecret` which does not exist by that name — the actual primitive is `cleanupFailedWorkspaceSecrets` (`secrets.go:33`), which was already used in `recovery.go` for the Failed-phase path but not the graceful-termination path. | `handleTerminating` now calls `r.cleanupFailedWorkspaceSecrets(ctx, workspace)` after the explicit password-secret delete. Best-effort (failures logged, not propagated — the workspace is already being torn down and the finalizer must still release). `handleDeletion` (the CRD-deletion entry point) inherits the fix because it calls `handleTerminating`. Regression coverage: `TestHandleTerminating_G36_DeletesCredentialsSecret`, `TestHandleTerminating_G36_DoesNotDeleteOtherWorkspaceSecrets`. |
| **G37** | **No validation on workspace env var names** | High | 🟢 **Fixed** | Pre-fix: `api/internal/handlers/workspace_env.go:SetWorkspaceEnv` accepted any POSIX-shaped env-var name; the materialize-time `validateVarName` (`pkg/agentd/secrets/secrets.go:222`) checked only regex + length, no dangerous-name blocklist. A workspace owner could set `LD_PRELOAD`, `PATH`, `PYTHONPATH`, `BASH_ENV`, `DYLD_INSERT_LIBRARIES`, etc. via the env-secret mechanism and compromise every process spawned in the pod. The threat-model row's claim of a parallel agentd check at `pkg/agentd/secrets/secrets.go:277-296` was incorrect — that range is the path-traversal check for `mount_path`, not env-var names. | New shared `pkg/validation.ValidateEnvVarName` enforces POSIX shape, length ≤ 256, and a curated blocklist of ~30 dangerous names sourced from ld.so(8), bash(1), Python, Node, Ruby, Perl, Java, and glibc docs. The API handler validates every name up front (fail-fast, no partial application); agentd's `validateVarName` now delegates to the same validator (defense-in-depth). Locale vars (`LANG`, `LC_ALL`, `TZ`) are intentionally NOT blocked — they don't execute code. Regression coverage: `TestSetWorkspaceEnv_RejectsBlockedNames`, `TestSetWorkspaceEnv_RejectsBlockedNamesCaseInsensitive`, `TestSetWorkspaceEnv_RejectsInvalidPOSIXNames`, `TestSetWorkspaceEnv_RejectsMixedBatch_NoPartialApply`, `TestSetWorkspaceEnv_AcceptsLocaleNames`, plus `TestValidateEnvVarName_*` in pkg/validation. |
| **G38** | **ChangePassword does not invalidate existing sessions** | High | 🟢 **Fixed** | Pre-fix: `secrets.go:597-632` (`RotateKeyHandler.ChangePassword`) called `KeyService.ChangePassword` (which evicts the caller's cached DEK and durable `jwt_sessions` row at `key_service.go:859-882`) and updated the bcrypt hash, but never revoked the JWT signatures themselves — stolen pre-change tokens stayed valid until natural expiry. | Handler now calls `auth.Service.RevokeAllUserSessions` after both the DEK re-wrap and bcrypt update commit, via a new optional `SessionRevoker` interface wired in `app.go` (`SetSessionRevoker`). Best-effort: revocation failure is logged and the change still reports success — the cryptographic change is irreversible. Mirrors the password-reset flow's existing revocation pattern (`password_reset.go:309-315`). Regression coverage: `TestChangePassword_RevokesAllSessionsOnSuccess`, `TestChangePassword_RevokerErrorIsNonFatal`, `TestChangePassword_WrongPasswordDoesNotRevoke`, `TestChangePassword_NoRevokerWired_StillSucceeds`, `TestChangePassword_Unauthenticated_Returns401`, and the extended `TestE2E_RealAuth_ChangePassword` which proves the pre-change JWT is rejected immediately after the change while a freshly-issued post-change JWT still works. |
| G39 | ~~Terminal WebSocket allows all origins~~ | Medium | 🟢 **Fixed** | ~~`terminal.go:126` — `CheckOrigin: func(r *http.Request) bool { return true }`; WebSocket security middleware not applied to terminal route~~. `terminal.go` now uses `newCheckOriginChecker` (`terminal.go:130-200`): same-origin by default (Origin host:port == request Host), plus an operator-controlled allowlist via `terminal.allowedOrigins` Helm value. Non-browser clients (no Origin) are accepted — they authenticate via the single-use ticket, not cookies. The dead `WebSocketSecurityMiddleware` and `RouterConfig.AllowedWebSocketOrigins` plumbing (the latter was never read by the router) have been removed; the gorilla Upgrader is the single enforcement point. | PR [#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515). Regression: `TestTerminal_G35_CrossOriginUpgradeRejected` and the 9-case `TestCheckTerminalOrigin_*` table. |
| G40 | Agentd user port (4097) has no application-layer auth | Medium | 🔴 Open | `agent_reload.go:25-26` — "Authentication: none"; `/v1/reload-secrets` writes arbitrary secrets; `requireBearerToken` middleware exists but not applied to user port | Apply `requireBearerToken` to user port endpoints. |
| G41 | No per-endpoint rate limit on RevealSecret | Medium | 🔴 Open | `router.go:245` — `/secrets/:id/reveal` behind global 100/min only; enables password brute-force | Add stricter per-endpoint limit (e.g. 5/min). |
| G42 | SSE connection tracking has unbounded memory growth | Medium | 🔴 Open | `stream_user_events.go:36-38` — `sseConnCounts` global map never pruned | Add periodic cleanup of stale entries. |
| G43 | IPv6 egress not covered by workspace NetworkPolicy | Medium | 🔴 Open | `workspace-network-policy.yaml:120-130` — CIDR allowlist uses `0.0.0.0/0` only; IPv6 `::/0` unrestricted | Add IPv6 rules or document IPv4-only assumption. |
| G44 | Workspace pod-level SecurityContext missing RunAsNonRoot | Low | 🔴 Open | `pod_builder.go:309-333` — container-level has it but pod-level doesn't; future sidecars could run as root | Add `RunAsNonRoot: ptr.To(true)` to `buildPodSecurityContext()`. |
| G45 | Legacy `source /sandbox-cfg/env` in entrypoint | Low | 🔴 Open | `entrypoint-opencode.sh:8-10` sources file that is never created; bypasses secrets validation if ever created | Remove dead code. |
| G46 | Password file read failure is silent | Low | 🔴 Open | `cmd/workspace-agentd/main.go:753-757` — empty password if file missing; workspace non-functional silently | Log at Error level, consider non-zero exit. |
| G47 | Inference relay secret exposed as CLI arg | Low | 🔴 Open | `controller-deployment.yaml:84-86` — fallback interpolates secret as command-line argument | Remove fallback path. |
| **G48** | **Master KEK delivered as env var (exposed via /proc/1/environ)** | High | 🟢 **Fixed** | Pre-fix: `api-deployment.yaml` projected `LLMSAFESPACES_MASTER_SECRET` into the pod env, readable via `/proc/1/environ` by any same-UID process. | US-50.1: default delivery is now a read-only file mount at `/var/run/secrets/llmsafespaces/master-secret` (mode 0440, subPath). `secrets_adapters.go:525-571` reads `LLMSAFESPACES_MASTER_SECRET_FILE` (colon-separated for the rotation window). Legacy env path is a deprecated opt-in (`masterSecret.deliveryMethod=env`) with a startup Warn (`app.go:1017`). Regression: `chart_master_secret_test.go:121-200`. |
| G49 | ~~No operational KEK rotation capability (rotating is destructive)~~ | High | 🟢 **Fixed** | Pre-fix: rotating the master KEK orphaned every Postgres ciphertext. Foundation shipped: `StaticKeyProvider` multi-key decrypt (`root_key.go:62-118`, US-50.4); `key_version` columns on `api_keys` + `org_sso_configs` (migrations 42/43, US-50.3); rotation-aware write path populates active version on encrypt (US-50.6). | `rotate-kek` CLI ships at `cmd/rotate-kek/main.go` (153 lines: old/new master key loading, per-purpose key derivation, Postgres + Redis connections, `RotationCoordinator`, dry-run, resume-from, multi-table support). Operational runbook is the only remaining piece (track separately as a doc task, not a security gap). |
| G50 | Decrypt operations are not audited (exfiltration via legitimate API undetectable) | Medium | 🔴 Open | `NewAuditedProvider` (`pkg/secrets/audited_provider.go:42-73`, US-50.12) wraps a `RootKeyProvider` and logs every Decrypt to `secret_audit_log` (fire-and-forget, never logs plaintext/ciphertext/key material). **Not yet wired into production decrypt paths** — `NewAuditedProvider` has zero call sites anywhere: `rg "NewAuditedProvider\("` returns a single hit (the constructor definition at `audited_provider.go:50`), and the two test references (`audited_provider_test.go:56,163`) are `&AuditedProvider{}` struct literals that bypass the constructor (so it is itself untested). `AdminKeyDeriver` still exists (`pkg/secrets/credential_store.go:81`), so Layer 2 callers still do `DecryptSecret(deriveServerKey(label), ct)` directly with no single decrypt chokepoint to hook. | Wiring depends on US-50.2 (unify the two crypto layers under `RootKeyProvider` — `AdminKeyDeriver` removal). US-50.2 is **not yet merged** (distinct from US-50.7 domain separation, which has merged). Per README-LLM.md Rule 0, the unwrapped component does not yet provide coverage; `secret_audit_log` currently records only secret CRUD, not decrypts. |

---

## 6. STRIDE Analysis

| Component | Spoofing | Tampering | Repudiation | Info Disclosure | DoS | Elevation |
|-----------|----------|-----------|-------------|-----------------|-----|-----------|
| **API Auth** | JWT forgery (mitigated: HMAC-only signing); API key theft | Token replay (mitigated: dual-key revocation) | No audit of failed auth | ~~Secret values logged unredacted (G25)~~ 🟢 Fixed | Account lockout abuse (G13); ~~no rate limit on recovery (G35)~~ 🟢 Fixed | ~~Sessions survive password change (G38)~~ 🟢 Fixed |
| **Proxy** | Workspace ID spoofing — ~~**NO OWNERSHIP CHECK (G33)**~~ 🟢 Fixed | Response tampering (plain HTTP — G4); ~~header injection to sandbox (G34)~~ 🟢 Fixed | No per-request audit trail | ~~All client headers forwarded to sandbox (G34)~~ 🟢 Fixed (explicit allowlist) | Connection exhaustion (mitigated: limits) | ~~Cross-tenant access via proxy (G33)~~ 🟢 Fixed |
| **Controller** | SA token theft (mitigated: bound tokens) | CRD manipulation (mitigated: webhooks) | Actions not individually audited | Namespace-scoped by default; ~~secrets persist after deletion (G36)~~ 🟢 Fixed | CRD spam (mitigated: quotas) | Namespace-scoped SA |
| **Sandbox Pod** | N/A (no auth within pod) | PVC data corruption | No file-level audit | Credential in env (G3 accepted); tmpfs-backed (G15 fixed); ~~env var injection (G37)~~ 🟢 Fixed; agentd user port unauthenticated (G40) | Resource exhaustion (mitigated: limits) | Container escape (mitigated: seccomp, caps; G1 accepted) |
| **Database** | SQL injection (mitigated: pgx parameterized) | Data corruption (mitigated: transactions) | No query audit log | Wrapped DEK exposure (mitigated: AES-256-GCM); credential rows now carry `key_version` for rotation (US-50.3); authorized-decrypt exfiltration undetectable — audit wrapper built but not wired (G50) | Connection exhaustion | N/A |
| **Redis** | Auth bypass (mitigated: auto-generated password, datastore NetworkPolicy) | Cache poisoning | No operation audit | DEK in memory (G10 accepted) | Memory exhaustion; SSE tracking leak (G42) | N/A |
| **Frontend** | Session theft via XSS (mitigated: rehype-sanitize — needs fuzzing) | DOM tampering (mitigated: React auto-escape) | No client audit | JWT in HttpOnly Secure cookie | UI freeze via huge messages | N/A |
| **Workspace Network** | Cross-tenant traffic (mitigated: NetworkPolicy) | N/A | NetworkPolicy events not audited | DNS exfil via external resolvers (G30); IPv6 unrestricted (G43) | N/A | N/A |

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
| A7 | Container images from trusted registry | Dockerfile review | **Partial** | Base image uses tag-only `debian:bookworm-slim` (not digest-pinned). opencode and gh downloaded over TLS without checksum verification (G9). mise uses MISE_GITHUB_ATTESTATIONS=1. AWS CLI has full PGP verification. |
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
| Security gaps (G1–G50) | 50 | 26 | 16 | 8 |

**Open gaps (require remediation):** G4, G6, G9, G13, G21, G29, G30, G40–G47, G50

**Accepted risks (documented rationale):** G1, G3, G7, G10, G14, G23, G28, G32

> G33 (proxy IDOR) and G34 (proxy header forwarding) — previously the
> Critical open gaps — are now **Fixed** as of the v0.3.0 network
> hardening sweep (PRs [#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513),
> [#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515)). G39
> (terminal WebSocket Origin) and G49 (operational KEK rotation — the
> `rotate-kek` CLI at `cmd/rotate-kek/main.go` ships) also closed. The
> highest-severity remaining open gaps are now G35 (RecoveryAccount no
> rate limiting) and G50 (decrypt audit not wired into production paths).

> **v2.2 count correction:** the prior summary (v2.1) reported 18 Fixed / 22 Open; a row-by-row recount of the table showed 16 Fixed / 24 Open. The recount is folded into this revision alongside the G48–G50 additions. Counts now reconcile exactly (17 + 26 + 7 = 50).

---

## 11. Revision History

| Version | Change |
|---------|--------|
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
