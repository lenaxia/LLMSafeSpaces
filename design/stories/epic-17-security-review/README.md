# Epic 17: Security Review & Penetration Test Plan

**Status:** Pre-Pentest Remediation Complete; Live Re-Pentest Pending
**Author:** mikekao
**Depends On:** Epics 6, 8, 10 (core platform must be functional)
**Threat Model:** [THREAT-MODEL.md](./THREAT-MODEL.md)

---

## Objective

Conduct a red-team security assessment of LLMSafeSpace covering all trust boundaries, credential flows, sandbox isolation, multi-tenant separation, and frontend rendering. Produce actionable findings with severity ratings, remediation guidance, and automated regression tests for every Critical/High finding.

This epic is structured as a Validator-Loop epic per `README-LLM.md` §Multi-Agent Workflow: every finding goes through skeptical validator → triage → remediation → re-validation cycles until zero real findings remain.

---

## Scope

### In Scope

| Layer | Components | Focus |
|-------|-----------|-------|
| API Server | Auth, proxy, handlers, middleware, MCP server | AuthN/AuthZ bypass, injection, SSRF, IDOR, revocation |
| Controller | Reconcilers, pod spec generation, RBAC, finalizers | Privilege escalation, CRD manipulation, leaked SA tokens |
| Sandbox Runtime | Entrypoints, credential materialization, opencode, agentd, mise | Container escape, credential theft, network escape, supply chain |
| Frontend (browser) | React UI, markdown/code rendering, JWT storage | XSS via assistant content, CSP, clickjacking, token exfiltration |
| Infrastructure | Helm chart, RBAC, NetworkPolicy, Secrets, ingress | Misconfig, over-permissioned SA, missing policies, default-deny absence |
| Crypto | Key wrapping, JWT, password hashing, redaction | Weak crypto, key leakage, bypass, revocation correctness |
| Data Stores | PostgreSQL, Redis, etcd (K8s Secrets) | Injection, unauthorized access, data at rest |
| Build & Supply Chain | Base image, opencode binary, mise binary, Go deps | Image signing, checksum verification, SBOM, attestation |

### Out of Scope (with Mitigation Owner)

| Risk | Owner | How It's Tracked |
|------|-------|------------------|
| LLM provider security (OpenAI, Anthropic) | LLM provider | Operator selects providers; documented in deployment guide |
| opencode binary internals | upstream `anomalyco/opencode` | Pin version per release; track upstream CVEs |
| Physical/social engineering | Operator | Documented in deployment guide |
| Browser zero-days (Chrome, Firefox, Safari) | Browser vendor | Out of scope; require modern browsers |

---

## Pre-Pentest Remediation Status

### Fixed Gaps (verified in code with regression tests)

| Gap | Fix Location | Regression Test |
|-----|-------------|-----------------|
| G2 — entrypoint shell injection | `pkg/agentd/secrets` replaces bash; entrypoint is 35-line shim | 26 tests in `pkg/agentd/secrets/secrets_test.go`, `cmd/workspace-agentd/secrets_test.go` |
| G5 — controller SA cluster-wide | `values.yaml:460` defaults `rbac.scope: "namespace"` | `chart_test.go:696` |
| G8 — first-user-admin race | `auth.go:570-576` atomic SQL CTE | Verified in code |
| G11 — no PSA enforcement | `namespace.yaml:20-25` sets enforce=restricted | Verified in code |
| G12 — proxy timeout 300s | `proxy.go:128` reduced to 60s | Verified in code |
| G15 — emptyDir disk-backed | `pod_builder.go:136-143` tmpfs + size limits | `security_test.go:99-200` volume footprint |
| G16 — no NetworkPolicy | `workspace-network-policy.yaml` ships with chart | `chart_test.go:129-299` (5 tests) |
| G17 — SA token automount | `pod_builder.go:196` sets false | `security_test.go:51-63` |
| G18 — JWT revocation broken | `auth.go:276-281` dual-key; `router.go:462` logout calls RevokeToken | `auth_revocation_test.go` (6 tests) |
| G19 — mise no attestation | `Dockerfile:269,277` MISE_GITHUB_ATTESTATIONS=1 | Verified in Dockerfile |
| G20 — credential file permissions | `pkg/agentd/secrets` atomic 0600 writes | `TestG20_AllFilesCreatedWithMode0600` |
| G22 — EnableServiceLinks | `pod_builder.go:203` sets false | `security_test.go:490-499` |
| G24 — no seccompProfile | `pod_builder.go:329-331` RuntimeDefault | `security_test.go:505-515` |
| G26 — default passwords | `values.yaml:276-278` auto-generate; datastore NetPols | `chart_test.go:345-470` |
| G27 — login timing | `auth.go:698-701,709` dummy bcrypt on all failure paths | Verified in code |
| G31 — frontend CSP/XFO | `values.yaml:580-585` headers on ingress | Verified in values |

### Open Gaps (pentest should validate exploitability)

| Gap | Severity | Key Evidence |
|-----|----------|-------------|
| G4 — no mTLS to pods | Medium | `proxy.go:610` plain HTTP |
| G6 — no per-endpoint rate limit on secrets | Medium | `router.go:237-256` global 100/min only |
| G9 — opencode/gh no checksum | Medium | `Dockerfile:142-154` TLS only |
| G13 — lockout keyed on email | Medium | `auth.go:686` |
| G21 — password file mode 0644 | Medium | `pod_builder.go:350` `cp` preserves source mode |
| G25 — secret "value" logged unredacted | High | `logging.go:48` missing "value" in SensitiveFields |
| G28 — bind handler no-op | High | First-time secret delivery silently skipped |
| G29 — path-traversal accepted by API | Medium | Materialize-time validation blocks exploit; API accepts without error |
| G30 — egress allows external DNS | Medium | NetPol OR-logic allows port 53 to 8.8.8.8 |

### Accepted Risks

| Gap | Rationale |
|-----|-----------|
| G1 — no noexec on emptyDir | K8s limitation; mitigated by tmpfs + seccomp + cap-drop |
| G3 — env-secret via /proc | Accepted; prefer secret-file type |
| G7 — SSE bypasses injection block | SSE cannot be blocked mid-stream |
| G10 — Redis not encrypted at rest | Operator responsibility |
| G14 — no egress body inspection | Accepted; minimize allowedDomains |
| G23 — PVC lacks nosuid | Documented for operator via StorageClass mountOptions; mitigated by runAsNonRoot + NoNewPrivs |
| G32 — no per-user workspace quota | Intentional for single-tenant; SaaS should add limit |

---

## Red Team Methodology

### Phase 0: Environment Setup & Tooling Validation

| Task | Technique | Deliverable |
|------|-----------|-------------|
| RT-0.1 | Provision pentest cluster | Kind or Talos with Cilium/Calico CNI; documented version/CNI |
| RT-0.2 | Deploy LLMSafeSpace via Helm | Full install (API + Controller + Frontend); record image SHAs |
| RT-0.3 | Verify control fixture | Run kube-hunter against a vanilla pod; confirm expected findings |
| RT-0.4 | Provision test accounts | 3 users: admin, regular-A, regular-B (attacker); record JWTs |
| RT-0.5 | Confirm logging baseline | API audit logs, controller logs, K8s audit logs all flowing |
| RT-0.6 | Snapshot baseline | Cluster YAML dump + `helm get all` for diffing post-test |

**Exit criteria:** A control fixture has confirmed at least one expected vulnerability; logging is verified; rollback plan exists.

**Implementation:** Two reproducible kits:
- [`phase-0/`](./phase-0/) — fresh kind cluster from scratch
- [`phase-0-prod/`](./phase-0-prod/) — existing production cluster

---

### Phase 1: Reconnaissance & Attack Surface Mapping

| Task | Technique | Target |
|------|-----------|--------|
| RT-1.1 | API endpoint enumeration | Swagger docs, route registration in `router.go` |
| RT-1.2 | CRD schema analysis | `pkg/apis/llmsafespace/v1/` — mutable fields, kubebuilder validation |
| RT-1.3 | RBAC privilege mapping | `charts/llmsafespace/templates/rbac.yaml` (both scopes) |
| RT-1.4 | Network topology mapping | Service definitions, NetworkPolicy templates |
| RT-1.5 | Dependency audit + SBOM | `trivy fs --format cyclonedx-json .` and `grype` |
| RT-1.6 | Container image analysis | Dockerfiles — installed packages, SUID binaries, base image vulns via Trivy |
| RT-1.7 | Secret storage mapping | Where credentials exist at rest and in transit |
| RT-1.8 | Frontend asset inventory | Bundle analysis; third-party JS shipped to browser |
| RT-1.9 | Build-time supply chain | Verify image digests match published SHAs; check for unsigned binary downloads |

**Phase 1 mandatory artefacts:**

1. **SBOM** in CycloneDX or SPDX format
2. **CVE report** with >= High severity items promoted to Phase-2
3. **Attack surface inventory** ranked by exposure and authentication gates

---

### Phase 2: Authentication & Authorization Testing

| ID | Test Case | Attack Vector | Expected Finding |
|----|-----------|---------------|------------------|
| RT-2.1 | JWT signature bypass | `alg:none`, HS256→RS256 confusion | Should reject (SigningMethodHMAC enforced) |
| RT-2.2 | JWT claim manipulation | Modify user_id, role, exp claims | Should reject (signature invalid) |
| RT-2.3 | Expired token replay | Use token past exp with valid signature | Should reject |
| RT-2.4 | API key brute force | Enumerate `lsp_` prefixed keys | Rate limiting should block |
| RT-2.5 | Registration abuse | Mass account creation | Rate limiting (1/min) should block |
| RT-2.6 | Account lockout DoS | Send N failed logins for victim email | **Confirmed gap G13** — lockout keyed on email only; verify exploitability from multiple IPs |
| RT-2.7 | ~~First-user-admin race~~ | Two concurrent registrations on fresh DB | **Fixed (G8)** — atomic SQL CTE; validate fix holds under concurrent load |
| RT-2.8 | Auth bypass on skip paths | Craft requests matching `/health`, `/docs/` prefixes with path traversal | Should not bypass |
| RT-2.9 | CORS misconfiguration | Cross-origin requests with credentials | Should reject (explicit allow-list, no wildcard) |
| RT-2.10 | Session fixation | Reuse session_id across users | Should be bound to user |
| RT-2.11 | Password reset without recovery key | Verify secrets are wiped | Should be irrecoverable |
| RT-2.12 | Admin role escalation | Non-admin attempts admin-only operations | Should 403 |
| RT-2.13 | ~~JWT revocation enforcement~~ | Revoke JWT via RevokeToken, immediately reuse | **Fixed (G18)** — dual-key revocation; logout calls RevokeToken. Validate: (a) revocation visible to next request, (b) jti fallback when hash evicted, (c) double-revoke idempotent |
| RT-2.14 | Long-lived JWT after credential rotation | Rotate JWT signing key, use old token | Old tokens must be rejected (no kid rotation — A8 refuted) |
| RT-2.15 | API key reveal in list endpoint | GET /api/v1/auth/api-keys | Confirmed: secrets stripped on list (verified in code) |
| RT-2.16 | ~~Login timing enumeration~~ | Measure response time for valid vs invalid emails | **Fixed (G27)** — dummy bcrypt on all failure paths. Validate timing is constant under load |

---

### Phase 3: Sandbox Isolation & Container Escape

| ID | Test Case | Attack Vector | Expected Finding |
|----|-----------|---------------|------------------|
| RT-3.1 | Read other pod's secrets | From sandbox pod, reach K8s API | **Fixed (G16+G17)** — NetworkPolicy blocks in-cluster CIDRs; no SA token mounted. Validate against live CNI. |
| RT-3.2 | ~~SA token absence~~ | Check /var/run/secrets/kubernetes.io/ in sandbox pod | **Fixed (G17)** — Validate SA token directory is absent |
| RT-3.3 | /proc exploration | Read /proc/1/environ, /proc/*/maps | Env vars readable (G3 accepted) |
| RT-3.4 | Write+exec in /tmp | Write binary to /tmp, chmod +x, execute | tmpfs-backed (G15 fixed) but noexec not enforced (G1 accepted — K8s limitation). Mitigated by seccomp + cap-drop. |
| RT-3.5 | Escape via /workspace | Write SUID binary to PVC, exec after resume | Should fail (runAsNonRoot + NoNewPrivs + cap-drop; nosuid not enforced on PVC — G23 accepted) |
| RT-3.6 | Capability abuse | Attempt operations requiring dropped caps | Should fail (Drop ALL on all containers) |
| RT-3.7 | ~~Seccomp bypass~~ | Attempt blocked syscalls (ptrace, mount) | **Fixed (G24)** — RuntimeDefault seccomp. Validate profile is attached. |
| RT-3.8 | Node metadata access | curl 169.254.169.254 from sandbox | **Fixed (G16)** — blockedEgressCIDRs includes 169.254.0.0/16. Validate on real cloud K8s. |
| RT-3.9 | Cross-pod network access | From sandbox A, connect to sandbox B | **Fixed (G16)** — default-deny ingress. Validate with live CNI. |
| RT-3.10 | DNS exfiltration | Encode data in DNS queries to external domain | External DNS resolvers reachable on port 53 (G30). Audit logging. |
| RT-3.11 | Resource exhaustion | Fork bomb, memory allocation, disk fill | Limits should contain |
| RT-3.12 | PID namespace escape | Signal processes outside pod | Should fail (PID namespace isolation) |
| RT-3.13 | Symlink escape | Symlink in /workspace to /etc/shadow | Should fail (read-only root) |
| RT-3.14 | Device access | Access /dev/kmsg, /dev/mem | Should fail (no device mounts) |
| RT-3.15 | ~~Plaintext secrets on node disk~~ | Dump node filesystem after secret materialization | **Fixed (G15)** — emptyDir volumes are tmpfs-backed. Validate no plaintext on node disk. |
| RT-3.16 | /sandbox-cfg/password mode | Check file permissions in pod | **Gap G21** — `cp` preserves Secret defaultMode 0644. Validate `stat` shows 0644. |
| RT-3.17 | Mise-installed runtime tampering | Modify mise-installed binary on PVC; trigger re-exec | Tampered binary survives suspend/resume. Document and verify. |

---

### Phase 4: Credential & Crypto Testing

| ID | Test Case | Attack Vector | Expected Finding |
|----|-----------|---------------|------------------|
| RT-4.1 | Credential API IDOR | PUT/GET secrets for resource owned by another user | Should 403/404 |
| RT-4.2 | Secret value in logs | Trigger errors with credential operations, check API logs | **Gap G25** — "value" not in SensitiveFields; secrets can appear in logs |
| RT-4.3 | ~~Entrypoint shell injection~~ | Crafted secret payloads including full bypass corpus | **Fixed (G2)** — materialization in Go, not bash. Validate against live cluster. |
| RT-4.4 | Secret file path traversal | Set mount_path to `../../etc/passwd` | Materialize-time validation blocks (secrets.go:277-296). API accepts without error (**G29**). |
| RT-4.5 | DEK extraction from Redis | Connect to Redis from compromised API pod | Redis auth required (auto-generated password); datastore NetworkPolicy restricts ingress. |
| RT-4.6 | Wrapped DEK offline attack | Extract wrapped_dek from DB, attempt offline unwrap | Should require password (HKDF-derived KEK) |
| RT-4.7 | JWT signing key extraction | Check if key is in env var, config file, or hardcoded | Should be in K8s Secret only |
| RT-4.8 | Redaction bypass | Craft credential patterns that evade all 16 regex rules | Document bypass patterns; add new rules. Note: redaction library exists but is not wired into the agent output pipeline. |
| RT-4.9 | Redaction DoS | Send very large input through redact binary | No maxInputBytes cap in `pkg/redact/redact.go`; `cmd/redact/main.go` uses `io.ReadAll` with no size limit. Document. |
| RT-4.10 | ~~Password hash timing attack~~ | Measure response time for valid vs invalid usernames | **Fixed (G27)** — constant-time via dummy bcrypt on all failure paths |
| RT-4.11 | Recovery key brute force | Attempt to brute-force 128-bit recovery key | Infeasible (2^128) |
| RT-4.12 | DEK lifecycle on workspace deletion | Delete workspace; query Redis for DEK afterwards | Verify EvictDEK called by workspace deletion path |
| RT-4.13 | ~~DEK lifecycle on session revocation~~ | Revoke JWT; confirm DEK evicted | **Fixed (G18)** — RevokeToken wired into logout. Validate DEK eviction after revocation. |
| RT-4.14 | Concurrent credential rotation | Two simultaneous rotate-key calls | Verify atomicity |
| RT-4.15 | ~~mise binary tampering at build time~~ | Replace mise tarball URL | **Fixed (G19)** — MISE_GITHUB_ATTESTATIONS=1 enforced |
| RT-4.16 | opencode binary tampering at build time | Same as RT-4.15 for opencode | **Gap G9** — no checksum verification; upstream does not publish checksums |

---

### Phase 5: Proxy & Network Testing

| ID | Test Case | Attack Vector | Expected Finding |
|----|-----------|---------------|------------------|
| RT-5.1 | SSRF via proxy | Craft workspace that resolves to internal IP | Proxy uses CRD-reported pod IP only |
| RT-5.2 | Proxy to arbitrary port | Modify request to target port other than agentd port | Should be hardcoded |
| RT-5.3 | HTTP request smuggling | Chunked encoding / CL-TE mismatch through proxy | Gin should handle correctly |
| RT-5.4 | SSE injection | Inject SSE events into stream from sandbox | Verify stream integrity |
| RT-5.5 | Connection exhaustion | Open maxConnectionsPerWorkspace+1 connections | Should reject excess |
| RT-5.6 | Stale pod IP exploitation | Pod deleted, IP reassigned, proxy connects to wrong pod | Verify retry logic + ownership |
| RT-5.7 | NetworkPolicy DNS bypass | DNS rebinding, DNS tunneling, external DNS resolver abuse | **Gap G30** — port 53 to 8.8.8.8 allowed. Test DNS exfil via external resolver. |
| RT-5.8 | Egress to kube-apiserver | From sandbox, HTTPS to kubernetes.default.svc | **Fixed (G16)** — RFC1918 blocked. Validate unreachable. |
| RT-5.9 | MCP transport injection | Malformed MCP messages via stdio/SSE | Should reject gracefully |
| RT-5.10 | WebSocket upgrade abuse | Attempt WebSocket upgrade on non-WebSocket endpoints | Should reject |
| RT-5.11 | Plain-HTTP proxy MITM | MITM API→sandbox traffic within cluster | **Gap G4** — plain HTTP. Document residual risk; recommend service mesh. |
| RT-5.12 | stripPatchParts JSON parser DoS | Deeply nested / huge JSON response | 32 MB read limit (proxy.go:664). No JSON depth limit. Verify behaviour. |
| RT-5.13 | ~~Proxy header timeout exhaustion~~ | Open many slow-response connections | **Fixed (G12)** — reduced to 60s |
| RT-5.14 | verbose=true filter bypass | Craft response that confuses stripPatchParts | Verify filter fails-safe (returns original on parse error) |

---

### Phase 6: Kubernetes & Infrastructure Testing

| ID | Test Case | Attack Vector | Expected Finding |
|----|-----------|---------------|------------------|
| RT-6.1 | CRD manipulation | Create Workspace CRD with malicious spec | Webhook validation should reject (failurePolicy: Fail) |
| RT-6.2 | Controller SA abuse | If controller SA token leaked | Namespace-scoped by default (G5 fixed); cluster scope opt-in with read-only informer |
| RT-6.3 | API SA abuse | If API SA token leaked | Namespace-scoped Secret + Pod CRUD; no runtimeenvironments/pods/log grants |
| RT-6.4 | Webhook bypass | Delete ValidatingWebhookConfiguration | Requires cluster-admin (accepted) |
| RT-6.5 | Helm values injection | Malicious values in Helm install | Template injection in YAML |
| RT-6.6 | etcd Secret exposure | Verify etcd encryption at rest configured | Operator responsibility (A1 unvalidated); document in NOTES.txt |
| RT-6.7 | PVC cross-mount | Create pod that mounts another workspace's PVC | RWO + controller ownership should prevent |
| RT-6.8 | Namespace escape | From workspace namespace, access system namespace | RBAC should prevent |
| RT-6.9 | Leader election poisoning | Create fake lease to disrupt controller | Requires SA permissions |
| RT-6.10 | Image pull from untrusted registry | Modify RuntimeEnvironment to point to attacker image | Webhook restricts to allowedImageRegistries (values.yaml:505-506) |
| RT-6.11 | ~~PSA enforcement absence~~ | Attempt to schedule privileged pod in workspace namespace | **Fixed (G11)** — namespace has enforce=restricted labels |
| RT-6.12 | NetworkPolicy enforcement | Verify NetworkPolicies render and apply | **Fixed (G16)** — chart ships policies. Validate against live CNI. |
| RT-6.13 | Helm chart preflight | Deploy chart on cluster missing CNI / etcd encryption | No preflight; chart succeeds silently |
| RT-6.14 | ~~TLS default off~~ | Helm install with default values; check ingress TLS | **Fixed** — `values.yaml:565` defaults `tls: true` |
| RT-6.15 | Redis/Postgres exposure check | Verify not LoadBalancer/NodePort | Operator responsibility |
| RT-6.16 | ~~Controller SA cluster scope on-by-default~~ | Verify default values | **Fixed (G5)** — defaults to namespace scope |

---

### Phase 7: Application Logic & Business Logic Testing

| ID | Test Case | Attack Vector | Expected Finding |
|----|-----------|---------------|------------------|
| RT-7.1 | Workspace limit bypass | Create more workspaces than quota allows | G32 accepted — no per-user limit |
| RT-7.2 | Suspend/resume race | Rapidly suspend+resume to corrupt state | Controller should handle idempotently |
| RT-7.3 | Concurrent credential update | Race condition on credential write | Should be atomic |
| RT-7.4 | Session hijacking via workspace transfer | Transfer workspace while session active | Should invalidate sessions |
| RT-7.5 | Injection detection bypass | Craft prompts that evade built-in patterns | Injection detection not yet wired; document bypass patterns for when it is |
| RT-7.6 | Activity tracking manipulation | Forge lastActivityAt to prevent auto-suspend | Should only accept from API server |
| RT-7.7 | Workspace name collision | Create workspace with name matching another user's | Should be per-user scoped |
| RT-7.8 | Delete workspace with active sessions | Delete while SSE streams are open | Should gracefully terminate |
| RT-7.9 | Frontend XSS via crafted markdown | Submit assistant content with XSS bypass corpus | rehype-sanitize default schema; fuzz against known bypasses (cure53, OWASP) |
| RT-7.10 | Frontend code-block injection | Markdown code block with malicious tool input rendering | Verify `<pre>` and `<code>` paths do not parse HTML |
| RT-7.11 | Frontend tool_use input/output rendering | Tool input with crafted JSON containing HTML | React auto-escapes; confirm |
| RT-7.12 | Frontend diff viewer bypass | Tool input with oldString/newString containing HTML | Verify react-diff-viewer-continued escapes content |
| RT-7.13 | ~~CSP / clickjacking absence~~ | Inspect ingress response headers | **Fixed (G31)** — CSP, X-Frame-Options DENY configured in values.yaml |
| RT-7.14 | JWT storage in browser | Inspect frontend code | Document attack model; HttpOnly Secure cookie mitigates |
| RT-7.15 | Workspace deletion DEK cleanup race | Delete workspace mid-credential-write | Verify partial state cleanup |

---

## Pentest Environment Requirements

| Component | Requirement |
|-----------|------------|
| Kubernetes cluster | Kind or Talos with Calico/Cilium CNI; PSA enforcement available |
| LLMSafeSpace deployment | Full Helm install at fixed image SHA |
| Test accounts | 3 users (admin, regular-A, regular-B); separate API keys |
| Network tools | nmap, curl, netcat, mitmproxy, dig, tcpdump |
| Monitoring | kubectl logs, Prometheus metrics, audit logs enabled |
| Tooling | kube-hunter, trivy, grype, cosign, syft |
| Frontend tooling | Headless Chrome via Playwright; XSS payload corpora |
| Recording | All RT-* test cases recorded with timestamps and evidence |

---

## Severity Rating

| Rating | Definition | SLA | Deployment Gate |
|--------|-----------|-----|-----------------|
| **Critical** | Remote code execution, full credential theft, cluster compromise, broken isolation | Must fix before merge; remediation PR + regression test | **HARD BLOCK**: Release Manager sign-off required |
| **High** | Cross-tenant data access, auth bypass, privilege escalation, plaintext secrets at rest | Fix within current sprint; regression test | **SOFT BLOCK**: Release Manager + Security Lead joint sign-off |
| **Medium** | Information disclosure, DoS, defense-in-depth bypass, race conditions | Fix within 2 sprints | Tracked in backlog; ship with documented mitigation |
| **Low** | Minor info leak, hardening gap, theoretical attack | Track and fix opportunistically | No deployment impact |
| **Informational** | Best practice deviation, documentation gap | Note for future improvement | None |

---

## Reporting Template

Each finding must include:

```markdown
### [SEVERITY] Finding Title

**ID:** RT-X.Y
**CWE:** CWE-### (if applicable)
**Component:** API / Controller / Runtime / Frontend / Infra
**Status:** Open / Fixed / Accepted Risk / False Alarm

#### Description
What the vulnerability is.

#### Reproduction Steps
1. Step 1
2. Step 2
3. Observe: [result]

#### Impact
What an attacker gains.

#### Root Cause
Why the vulnerability exists. Cite file:line.

#### Remediation
Specific fix with regression test reference.

#### Evidence
- Screenshots / logs / PoC: `design/stories/epic-17-security-review/artefacts/RT-X.Y/`
- Regression test: `<file:line>`
```

---

## Workflow

```
For each Phase (1-7):
  1. Phase implementation → execute test cases, produce findings
  2. Skeptical Validator → re-run, confirm or refute each finding
  3. Findings Triage → mark Real / False Alarm; document false alarms
  4. Remediation → fix every Real finding with regression test
  5. Re-Validate → loop until validator returns zero real findings
  6. Worklog → record all findings, fixes, evidence
```

---

## Success Criteria

1. All Critical findings have remediation PRs + regression tests + Release Manager sign-off.
2. All High findings have remediation PRs + regression tests + joint sign-off.
3. Threat model updated with any newly discovered attack vectors.
4. All gaps transitioned to: Fixed (with PR ref + regression test) / Accepted Risk (with sign-off) / False Alarm (with rationale).
5. Automated security regression tests in place per finding.
6. Phase 1 SBOM committed to artefacts/.
7. No previously-fixed gap has regressed.

---

## Cross-Reference: Epic 10 Multi-Tenant Trust Invariants

| Epic 10 Invariant | Validating Test Cases | Status |
|-------------------|----------------------|--------|
| Workspaces namespace-isolated by NetworkPolicy | RT-3.1, RT-3.8, RT-3.9, RT-5.7, RT-5.8, RT-6.12 | Fixed (G16) |
| Cross-workspace PVC access impossible | RT-6.7 | Unchanged |
| Controller does not have cluster-wide blast radius | RT-6.2, RT-6.16 | Fixed (G5) |
| API SA cannot escape its namespace | RT-6.3, RT-6.8 | Unchanged |
| Sandbox pod cannot reach K8s API | RT-3.1, RT-3.2, RT-5.8 | Fixed (G16, G17) |
| Per-user secret encryption (DEK isolation) | RT-4.5, RT-4.6, RT-4.12, RT-4.13 | Partial (G28 open) |
| Sandbox pods isolated from shell-injection | RT-4.3 | Fixed (G2) |
| JWT revocation actually revokes | RT-2.13, RT-4.13 | Fixed (G18) |

---

## Related Documents

- [Threat Model](./THREAT-MODEL.md)
- [Architecture](../../0021_2026-05-21_evolution-v2.md)
- [Security Policy V2.1 Draft](../../0027_2026-05-24_security-policy-v21.md) (unimplemented draft)
- [V1 Security Model](../../0005_2025-03-05_security.md)
- [Multi-Tenant Trust (Epic 10)](../epic-10-multi-tenant-trust/README.md)
- [Worklog Index](../../../worklogs/)
