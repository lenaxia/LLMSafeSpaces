# Security Report: Re-Validation Findings (G33-G47)

> **v0.3.0 update (2026-07-11):** G33, G34, and G39 are now **resolved**.
> The per-finding sections below are preserved verbatim for historical
> context; for the current status of each row, see `THREAT-MODEL.md` §9
> (gap table) which is now authoritative. Resolved findings:
>
> - **G33 (proxy IDOR):** the existing `WorkspaceAccessMiddleware` is
>   confirmed wired on the `idGroup` since the v2 design pass; the
>   "Open" status here was doc drift.
> - **G34 (proxy header forwarding):** closed by PR
>   [#513](https://github.com/lenaxia/LLMSafeSpaces/pull/513) — explicit
>   `copyRequestHeaders` allowlist (`Content-Type`, `Accept`,
>   `X-Request-ID`) + hop-by-hop strip in both directions.
> - **G39 (terminal WebSocket Origin):** closed by PR
>   [#515](https://github.com/lenaxia/LLMSafeSpaces/pull/515) —
>   `newCheckOriginChecker` same-origin-default + operator allowlist.

**Date:** 2026-06-12
**Validator:** Adversarial re-validation against code state
**Scope:** Gaps not covered by G1-G32
**Method:** Source code review — no design docs, no status updates, no inline comments. Only running code and tests.

---

## CRITICAL

### G33: Proxy routes have no workspace ownership check (IDOR)

**CWE:** CWE-639 (Authorization Bypass Through User-Controlled Key)
**Component:** API Proxy
**Status:** ✅ Resolved (v0.3.0) — see banner above. The `WorkspaceAccessMiddleware`
is confirmed wired on the `idGroup` (`router.go:291-292`); all proxy routes
inherit via `registerProxyRoutes(idGroup, ...)`. The "Open" status here was
doc drift from before the v2 design pass landed the middleware.

#### Description

Every proxy route — SendMessage, GetHistory, StreamEvents, AbortSession, DeleteSession, SendPromptAsync, QuestionReply, QuestionReject, PermissionReply, ListQuestions, ListPermissions — is accessible to any authenticated user for any workspace. There is no ownership verification.

The comment at `api/internal/server/router.go:824` says "All routes require authentication and ownership check (applied on the group)" but no such ownership middleware exists on the group.

#### Reproduction Steps

1. Authenticate as User A
2. `GET /api/v1/workspaces/<user-b-workspace-id>/session-events`
3. Receive real-time SSE events from User B's workspace
4. `POST /api/v1/workspaces/<user-b-workspace-id>/sessions/<session-id>/message` with arbitrary content
5. Message is delivered to User B's active agent session

#### Impact

Complete cross-tenant access: read conversation history, inject messages, subscribe to events, abort/delete sessions, reply to agent questions and permission requests on behalf of the workspace owner.

#### Root Cause

`api/internal/handlers/proxy.go:460-482` — `proxyToWorkspace` fetches workspace by ID from K8s, never checks `Labels["user-id"] == userID`. The `c.Get("workspace")` fallback at line 468-473 implies an ownership middleware was planned but never wired.

`api/internal/handlers/proxy.go:344-358` — `StreamEvents` subscribes to any workspace's events with no ownership check.

`api/internal/server/router.go:147-148` — workspaceGroup only applies `AuthMiddleware()`, no ownership middleware.

#### Evidence of Oversight (Not Design Choice)

- `api/internal/handlers/terminal.go:153-157` — HAS ownership check: `if ws.Labels["user-id"] != userID`
- `api/internal/handlers/models.go:123` — HAS ownership check with comment "Explicit ownership check before any pod communication"
- `api/internal/handlers/proxy.go:468` — has `c.Get("workspace")` fallback, implying a middleware was supposed to set it

#### Remediation

Add ownership check to `proxyToWorkspace` and `StreamEvents`, matching the terminal.go pattern:

```go
if workspace.Labels["user-id"] != userID {
    c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
    return
}
```

Add regression test: authenticate as User B, attempt to proxy to User A's workspace, assert 404.

---

### G34: Proxy forwards all client headers to sandbox pod

**CWE:** CWE-200 (Information Exposure)
**Component:** API Proxy
**Status:** ✅ Resolved (v0.3.0, PR #513) — see banner above. `proxy.go:465-471`
now calls `copyRequestHeaders` with an explicit allowlist (`Content-Type`,
`Accept`, `X-Request-ID`); hop-by-hop headers stripped in both directions.

#### Description

`api/internal/handlers/proxy.go:625-629` forwards every client request header to the sandbox pod before `SetBasicAuth` overwrites the Authorization header. This includes Cookie, Origin, Referer, X-Forwarded-For, and any custom headers.

#### Impact

A compromised sandbox pod (or malicious agent) can capture:
- Session cookies (if the browser sends them)
- CSRF tokens
- Internal routing headers (X-Forwarded-*, X-Real-IP)
- Custom headers injected by middleware or upstream proxies

The sandbox already has credentials via `/sandbox-cfg`, so this is additive rather than a new credential path. However, browser-scoped tokens that don't exist in K8s secrets (CSRF tokens, SSO cookies) leak into the sandbox's trust boundary.

#### Root Cause

`api/internal/handlers/proxy.go:625-629`:
```go
for k, vs := range c.Request.Header {
    for _, v := range vs {
        req.Header.Add(k, v)
    }
}
```

#### Remediation

Replace with an explicit allowlist of headers to forward (Content-Type, Accept, User-Agent at most), or strip sensitive headers (Cookie, Authorization, X-Forwarded-*) before forwarding.

---

## HIGH

### G35: RecoverAccount endpoint has no rate limiting

**CWE:** CWE-307 (Improper Restriction of Excessive Authentication Attempts)
**Component:** API Auth
**Status:** Open

#### Description

`POST /api/v1/account/recover` is registered on the root router (`api/internal/server/router.go:264`), outside the auth rate limiter group (20/min at lines 346-349). The endpoint requires only userID + recovery key with no CAPTCHA, no email verification, and no progressive backoff.

#### Root Cause

`api/internal/server/router.go:264`:
```go
router.POST("/api/v1/account/recover", cfg.RotateKeyHandler.RecoverAccount)
```

#### Remediation

Move behind the auth rate limiter, or add a dedicated stricter limiter (e.g. 3/hour per userID).

---

### G36: Workspace secrets not cleaned on deletion

**CWE:** CWE-226 (Sensitive Information in Resource Not Removed Before Reuse)
**Component:** Controller
**Status:** Open

#### Description

When a workspace is deleted, `handleTerminating` (`controller/internal/workspace/phase_terminating.go:15-62`) deletes the pod, PVC, and password secret (`workspace-pw-*`), but does NOT delete:
- `workspace-secrets-<id>` — contains the decrypted secrets manifest
- `workspace-creds-<id>` — contains credential data

The function `deleteEphemeralSecretsSecret` exists at `controller/internal/workspace/secrets.go:24-33` but is never called from `handleTerminating`. It IS called from `phase_active.go:113` and `phase_creating.go:111`, but not from the termination path.

`cleanupFailedWorkspaceSecrets` (secrets.go:42-58) deletes all three types but only for the Failed phase.

These secrets are created by the API server (not the controller), so they lack owner references. K8s garbage collection will not clean them.

#### Impact

Decrypted user secrets persist in K8s Secrets indefinitely after workspace deletion, accessible to any pod with namespace-scoped Secret read access.

#### Root Cause

`controller/internal/workspace/phase_terminating.go:32-38` — only deletes password secret, not secrets/creds secrets.

#### Remediation

Call `deleteEphemeralSecretsSecret` from `handleTerminating`. Also add deletion of `workspace-creds-*`. Add test verifying all three secret types are gone after workspace deletion.

---

### G37: No validation on workspace environment variable names

**CWE:** CWE-94 (Code Injection)
**Component:** API Secrets Handler
**Status:** Open

#### Description

`api/internal/handlers/secrets.go:573` — `SetWorkspaceEnv` accepts any string as an environment variable name with no blocklist. Users can set `LD_PRELOAD` (shared library injection), `PATH` (binary redirection), `PYTHONPATH`/`NODE_PATH` (code injection), `HOME` (config file redirection).

These variables affect the entire pod including opencode and agentd.

#### Root Cause

`api/internal/handlers/secrets.go:573-574`:
```go
for varName, value := range req.Vars {
    secretName := fmt.Sprintf("%s-env-%s", workspaceID, strings.ToLower(varName))
```

No validation on `varName`.

#### Remediation

Add a blocklist of dangerous env var names: `LD_PRELOAD`, `LD_LIBRARY_PATH`, `PATH`, `PYTHONPATH`, `NODE_PATH`, `HOME`, `HOSTNAME`, `SHELL`, `USER`, `LOGNAME`, `MAIL`, `LANG`, `TZ`. Also validate var names against the POSIX regex already used in `pkg/agentd/secrets/secrets.go:213-221` (`^[A-Za-z_][A-Za-z0-9_]*$`).

---

### G38: ChangePassword does not invalidate existing sessions

**CWE:** CWE-613 (Insufficient Session Expiration)
**Component:** API Auth
**Status:** Open

#### Description

`api/internal/handlers/secrets.go:782-817` — after password change, the bcrypt hash is updated and the DEK is re-wrapped, but existing JWT tokens remain valid until natural expiry. No call to `RevokeToken` for the current or other sessions. DEK cache entries keyed by existing session JTIs remain active.

An attacker who obtained a JWT (via G33 or other means) retains access even after the victim changes their password.

#### Root Cause

`api/internal/handlers/secrets.go:799-816` — no session revocation after successful password change.

#### Remediation

After successful password change, enumerate and revoke all active sessions for the user. At minimum, revoke the current session's JWT. The `RevokeToken` function already exists and works correctly (G18 fix verified).

---

## MEDIUM

### G39: Terminal WebSocket allows all origins

**CWE:** CWE-346 (Origin Validation Error)
**Component:** API Terminal
**Status:** ✅ Resolved (v0.3.0, PR #515) — see banner above. `terminal.go`
now uses `newCheckOriginChecker`: same-origin by default, plus operator
allowlist via `terminal.allowedOrigins` Helm value. Dead
`WebSocketSecurityMiddleware` and `RouterConfig.AllowedWebSocketOrigins`
removed; the gorilla Upgrader is the single enforcement point.

`api/internal/handlers/terminal.go:126` — `CheckOrigin: func(r *http.Request) bool { return true }`. The ticket system (single-use, 30s TTL) mitigates direct CSRF, but the WebSocket security middleware (`api/internal/middleware/security.go:206-271`) is not applied to the terminal route. A malicious site could use the user's HttpOnly cookie to fetch a ticket via authenticated POST, then open a cross-origin WebSocket.

**Remediation:** Apply the existing WebSocket origin validation middleware to the terminal route, or set `CheckOrigin` to validate against allowed origins.

---

### G40: Agentd user port (4097) has no application-layer auth

**CWE:** CWE-306 (Missing Authentication for Critical Function)
**Component:** Runtime (workspace-agentd)
**Status:** Open

`cmd/workspace-agentd/agent_reload.go:25-26` — comment states "Authentication: none at the application layer. The trust boundary is the Kubernetes NetworkPolicy." The endpoints `/v1/reload-secrets` (writes arbitrary secrets to disk) and `/v1/agent/reload` (disposes opencode instance) require no auth. The admin port (4098) correctly uses `requireBearerToken` middleware. If NetworkPolicy is misconfigured or disabled, any pod can inject credentials or kill the agent.

**Remediation:** Apply `requireBearerToken` to the user port endpoints. The middleware already exists at `cmd/workspace-agentd/main.go`.

---

### G41: No per-endpoint rate limit on RevealSecret

**CWE:** CWE-307 (Improper Restriction of Excessive Authentication Attempts)
**Component:** API Secrets Handler
**Status:** Open

`api/internal/server/router.go:245` — `/secrets/:id/reveal` behind global 100/min rate limit only. The handler requires password re-verification but has no rate limit on password attempts. An attacker with a stolen JWT can brute-force the user's password at this endpoint.

**Remediation:** Add per-endpoint rate limit (e.g. 5/min) on `/secrets/:id/reveal`.

---

### G42: SSE connection tracking has unbounded memory growth

**CWE:** CWE-400 (Uncontrolled Resource Consumption)
**Component:** API SSE Handler
**Status:** Open

`api/internal/handlers/stream_user_events.go:36-38` — `sseConnCounts` global map never pruned. Each unique `ClientIP()` creates a permanent entry. Over time this is a memory leak proportional to unique IPs seen.

**Remediation:** Add periodic cleanup of entries older than the reset window, or use a TTL-based cache.

---

### G43: IPv6 egress not covered by workspace NetworkPolicy

**CWE:** CWE-284 (Improper Access Control)
**Component:** Infrastructure (Helm Chart)
**Status:** Open

`charts/llmsafespace/templates/workspace-network-policy.yaml:120-130` — CIDR allowlist uses `0.0.0.0/0` (IPv4 only). IPv6 traffic to `::/0` is unrestricted on dual-stack clusters.

**Remediation:** Add IPv6 CIDR rules mirroring the IPv4 rules when dual-stack is detected, or document that the chart assumes IPv4-only clusters.

---

## LOW / INFORMATIONAL

### G44: Workspace pod-level SecurityContext missing RunAsNonRoot

**Component:** Controller
**Status:** Open

`controller/internal/workspace/pod_builder.go:309-333` — pod-level context sets RunAsUser/RunAsGroup/FSGroup but not RunAsNonRoot. Container-level `RunAsNonRoot: true` (line 109) prevents root execution today, but future sidecars or init containers added without explicit security contexts would not be blocked.

**Remediation:** Add `RunAsNonRoot: ptr.To(true)` to `buildPodSecurityContext()`.

---

### G45: Legacy source /sandbox-cfg/env in entrypoint

**Component:** Runtime
**Status:** Open

`runtimes/base/tools/entrypoints/entrypoint-opencode.sh:8-10` — sources `/sandbox-cfg/env` which is never created in current code. If a future change creates this file, its contents bypass the secrets package validation. Dead code.

**Remediation:** Remove lines 8-10.

---

### G46: Password file read failure is silent

**Component:** Runtime (workspace-agentd)
**Status:** Open

`cmd/workspace-agentd/main.go:753-757` — if `/sandbox-cfg/password` is missing, password is `""`, all opencode API calls will 401. Workspace becomes non-functional silently.

**Remediation:** Log at Error level and consider exiting with a non-zero code if the password file is missing and the workspace is Active.

---

### G47: Inference relay secret exposed as CLI arg in fallback path

**Component:** Infrastructure (Helm Chart)
**Status:** Open

`charts/llmsafespace/templates/controller-deployment.yaml:84-86` — fallback interpolates `.Values.inferenceRelaySecret` as a command-line argument, visible in `kubectl get pod -o yaml`.

**Remediation:** Remove the fallback path or force operators to use the managed secret mechanism exclusively.

---

## False Alarms Dismissed

| Claimed Finding | Verdict | Evidence |
|-----------------|---------|----------|
| AdminGuard broken (userRole never set) | **False** | `api/internal/services/auth/auth.go:937-938` — middleware does DB lookup and sets `c.Set("userRole", user.Role)` on every authenticated request |
| MCP SSE has no auth | **Unvalidated** | `cmd/mcp/` directory does not exist in this repo. Cannot verify. |
| SSRF via proxy | **False** | Pod IP sourced from CRD status, port is constant, path is hardcoded per handler. Verified at proxy.go:461,555,610. |
