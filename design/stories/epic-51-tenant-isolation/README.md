# Epic 51: Tenant Isolation — gVisor + Resource Quotas

**Status:** Planning
**Depends On:** Epic 30 (Unified Credential Model — delivered), Epic 43 org policies (delivered)
**Does NOT depend on:** Epic 18 hot migration, Capsule, vcluster, or any namespace work
**Target scale:** 1,000+ tenants, shared namespace

---

## Origin

This epic consolidates and supersedes:

- **Epic 10 US-10.6 (Virtual Namespace Tenant Isolation)** — superseded. The original proposed per-tenant virtual namespaces as the isolation boundary, which was the wrong control for the actual threat (see "Why not namespaces" below).
- **Epic 18 S18.7 (gVisor RuntimeClass)** — moved here. Promoted from Epic 18 Phase C (production hardening) to a first-class multi-tenancy prerequisite. No dependency on hot migration, RWX, or Karpenter.

Epic 18 S18.8 is reduced to **EFS storage isolation only** (access points, `efs.csi.aws.com/rootDirectory` annotations) — the proxy namespace refactor and NetworkPolicy/cascade items it originally carried are either satisfied today or dropped (no namespaces).

---

## Why this is a rewrite of US-10.6

The original US-10.6 (`epic-10-multi-tenant-trust/README.md:361`) was authored against an architecture that no longer exists:

- "Controller has cluster-wide Secret read access" — **false since Epic 17**: `rbac.scope=namespace` is the default (`charts/llmsafespaces/values.yaml:644`), secrets CRUD is namespace-scoped (`charts/llmsafespaces/templates/rbac.yaml:49-101`).
- "Pod in tenant A can reach tenant B" — **already prevented**: chart-level default-deny ingress (`workspace-network-policy.yaml:18-65`) + RFC1918/CGNAT-filtered egress (`controller/internal/workspace/network_policy.go:94-150`) blocks pod-to-pod traffic even in a shared namespace.
- "vcluster vs Loft virtual namespaces" — **obsolete framing**: Loft's virtual-namespace prototype never shipped as a product; vcluster costs ~256MB/tenant (256GB at 1,000 tenants — a non-starter at target scale).
- "RBAC scoped to tenant label" — **invalid concept**: Kubernetes RBAC is namespace-scoped, not label-scoped.
- "Per-user isolation" — **superseded by Design 0031 D4**: org members' workspaces are org-attributed; the tenant unit is org *or* user, not user-only.

## Why not namespaces

Per-tenant namespaces (Capsule, vcluster, or plain) do not solve the primary threat and don't scale to the target.

**Not a security boundary for this threat model.** The defining risk for a platform where tenants run arbitrary code is container escape via kernel exploitation. Once on the host node, namespace boundaries are irrelevant — the attacker reads every pod's volumes on that node regardless of namespace. The control that stops container escape is a sandboxed container runtime (gVisor), not namespace topology.

**Doesn't scale to 1,000+ tenants.** At 1,000+ namespaces:
- Controller informer cache memory grows ~linearly with namespace count (requires a cluster-wide informer rewrite).
- API server memory and RBAC evaluation latency grow.
- Aggregate etcd object count (Workspaces + Pods + Secrets + NetPols + RoleBindings across 1,000 namespaces) hits watch latency and compaction.
- Helm and kubectl degrade well before etcd does.

The earlier Epic 18 S18.8 Capsule decision (original text preserved in a `<details>` block in `epic-18-hot-migration/README.md` S18.8) reasoned: *"vCluster = ~256MB/tenant = 256GB at 1000 tenants. Capsule = ~0 overhead"* — but that comparison only measured RAM. It didn't address namespace-count viability, and S18.8's own scale test was scoped at **100 tenants × 10 workspaces** — an order of magnitude below target. That decision was made against the wrong scale envelope.

**Most of US-10.6 is already solved without namespaces.** Network isolation between tenants is shipped (chart-level default-deny ingress + RFC1918/CGNAT-filtered egress). Secret scoping is shipped (namespace-scoped `Role`). Tenant identity is on the CRD (`WorkspaceOwner`). Org/user deletion flows exist. The only thing namespaces would buy that isn't already done is per-tenant resource quotas — and that's a narrow problem solvable with an admission webhook keyed on a pod label.

---

## Threat model & layered controls

| Threat | Control | Status |
|---|---|---|
| Tenant container escape → host kernel | **gVisor RuntimeClass** (primary) | **This epic** |
| Tenant pod → tenant pod (network) | Chart default-deny ingress + egress filtering | ✅ Shipped |
| Tenant → kube-apiserver / cloud metadata | Egress filtering + `AutomountServiceAccountToken: false` | ✅ Shipped (`controller/internal/workspace/pod_builder.go:214`) |
| Tenant reads another tenant's K8s Secret | No tenant API credentials; namespace-scoped controller `Role` | ✅ Shipped |
| Tenant exhausts node resources (noisy neighbor) | **Per-tenant ResourceQuota via admission webhook** | **This epic** |
| Side-channel (CPU/cache/Rowhammer) | Accepted risk for soft-to-medium multi-tenancy | Out of scope |

**Posture:** Soft-to-medium multi-tenancy. Paying customers running dev tooling, not adversarial nation-states. Matches the threat model of comparable SaaS dev platforms (Google Cloud Run, App Engine) that use gVisor or a similar kernel-interposition layer.

### Why gVisor

gVisor (`runsc`) is a userspace kernel that intercepts syscalls from the container and services them itself, instead of passing them to the host kernel. A container escape that would normally call `openat("/proc/1/root/...")` or exploit a kernel CVE hits gVisor's sandboxed implementation, not the host. The host kernel never sees the untrusted syscall.

It is the standard answer for multi-tenant arbitrary-code platforms. Its limits:

- **Not complete isolation.** Side-channel attacks (CPU/cache timing, Rowhammer) work on shared hardware regardless of gVisor. State-actor adversaries require node-per-tenant or VM-per-workspace.
- **Performance overhead.** 5–30% on syscall-heavy workloads (file I/O, networking). CPU-bound compute is unaffected. For an LLM-coding-sandbox workload (mostly waiting on LLM API calls), the overhead is usually negligible — must be measured (acceptance criterion 8).
- **Compatibility.** Some syscalls and kernel features aren't implemented. Most dev tooling works; things like ptrace-based debuggers, certain seccomp filters, and some low-level networking tools may break. Per-workspace opt-out is a necessary escape hatch.
- **x86_64 and arm64 only.** Fine for the Graviton target.

### Alternatives considered

| Option | Isolation strength | Cost | Fit |
|---|---|---|---|
| **gVisor** (chosen) | Strong (kernel-interposed) | 5–30% perf overhead | **Right answer for soft-to-medium multi-tenancy** |
| Kata Containers | Strong (nested VM per pod) | Higher overhead (~50–100ms cold start, memory per pod) | Stronger than gVisor, heavier |
| VM-per-workspace (Firecracker, like Codespaces) | Maximum (separate kernel) | Highest overhead, most complex orchestration | For hard multi-tenancy / regulated workloads |
| Node-per-tenant-tier | Strong (physical isolation) | Expensive at small tenant sizes | For enterprise tier, not 1,000+ tenants |
| Per-tenant namespaces (Capsule/vcluster) | Weak (not a kernel boundary) | Doesn't scale (namespace count) | Rejected — see "Why not namespaces" |

---

## Scope

### S51.1 — gVisor RuntimeClass (primary isolation)

- Ship a `RuntimeClass` named `gvisor` in the Helm chart, gated on `gvisor.enabled` (default `false` for dev/single-tenant; `true` for production multi-tenant).
- Workspace pod builder sets `RuntimeClassName` when `gvisor.enabled` (add to the PodSpec hardening block in `controller/internal/workspace/pod_builder.go` `buildPod`, alongside `AutomountServiceAccountToken` and `SecurityContext`).
- Node setup: AMI/userData installs `runsc` and configures the container runtime (Epic 18 S18.9 Karpenter `EC2NodeClass` plans for gVisor in userData — coordinate but don't block on Karpenter).
- Per-workspace opt-out: add a `spec.runtimeClass` field or annotation to allow `runc` fallback for workloads incompatible with gVisor (ptrace debuggers, certain seccomp filters). Opt-out is admin-gated, not tenant-selectable, to prevent weakening the default.

### S51.2 — Per-tenant resource quotas via admission webhook

- New validating/mutating admission webhook on `Pod` create, keyed on the `llmsafespaces.dev/tenant` pod label (S51.3).
- Webhook rejects pod creation when the tenant's aggregate running resource usage would exceed their quota. Quota source: org plan (extend Epic 43's existing `org_policies` table — `max_workspaces_per_member` is already enforced; add `max_cpu_cores`, `max_memory_gb`, `max_total_workspaces`).
- For personal (non-org) tenants: instance-level defaults from Helm values.
- Implementation notes:
  - Webhook queries the internal API for current tenant usage (the `usage` metering service, Epic 12, already tracks per-owner usage — reuse it).
  - No `ResourceQuota` K8s objects needed (which would require namespaces); quota enforced at admission time in-process.
  - Race window between webhook check and pod scheduling is acceptable — workspace pods are long-lived; the check is a guard against gross overage, not precise concurrency control.

### S51.3 — Pod tenant label

- Add `llmsafespaces.dev/tenant=<tenant_id>` to workspace pod labels (`controller/internal/workspace/pod_builder.go:43-48`).
- `tenant_id` resolution: `WorkspaceOwner.OrgID` if set (Design 0031 D4 — org members are org-attributed), else `WorkspaceOwner.UserID`.
- Enables: webhook quota enforcement (S51.2), audit attribution, future admission policies, tenant-scoped metrics.
- Also propagate to the Workspace CR's existing label set (`api/internal/services/workspace/workspace_service.go:851-854`) for consistency.

### S51.4 — In-pod hardening verification under gVisor (defense-in-depth)

- gVisor is the primary boundary; existing pod hardening remains as defense-in-depth. The hardening set (non-root, drop-all-caps, read-only rootfs, seccomp, `AutomountServiceAccountToken: false`) is **already regression-tested** today in `controller/internal/workspace/security_test.go` (`TestG17_SandboxPodDoesNotAutomountSAToken`, `TestSandboxPod_SecurityContextHardening`, `TestG22_PodHasEnableServiceLinksFalse`, `TestG24_PodHasRuntimeDefaultSeccompProfile`).
- S51.4's value-add: extend these tests to assert the hardening set **survives under a gVisor `RuntimeClass`** — i.e., that setting `RuntimeClassName: gvisor` does not regress or conflict with the existing security context. This specific combination (gVisor + hardening) is not tested today.

---

## Already satisfied (no work — documented for completeness)

| Original US-10.6 requirement | Current state | Evidence |
|---|---|---|
| Cross-tenant network isolation | Chart NetPols (default-deny ingress + RFC1918/CGNAT-filtered egress) | `charts/llmsafespaces/templates/workspace-network-policy.yaml`; `controller/internal/workspace/network_policy.go:94-150` |
| Controller secret scoping | Namespace-scoped `Role`; `rbac.scope=namespace` default | `charts/llmsafespaces/templates/rbac.yaml:49-101`; `charts/llmsafespaces/values.yaml:644` |
| Secret isolation at rest | Ephemeral secrets deleted after init | `controller/internal/workspace/phase_creating.go:120-122` |
| Tenant identity model | `WorkspaceOwner{UserID, OrgID}` on CRD | `pkg/apis/llmsafespaces/v1/workspace_types.go:13-16` |
| App-layer workspace count quotas | Epic 43 org policies (`max_workspaces_per_member`, `max_active_workspaces_per_member`) | `api/internal/services/database/pg_org_store.go` |
| Org suspension → workload freeze | Controller polls org status and suspends workspaces | `controller/internal/workspace/org_suspend.go` |
| Account deletion → cleanup | Existing user/org deletion flows | — |

---

## Acceptance criteria

1. `RuntimeClass: gvisor` deploys via Helm (`gvisor.enabled`); workspace pods use it when enabled
2. Per-workspace `runc` opt-out works (admin-gated); documented compatibility matrix
3. gVisor-isolated pod cannot reach another tenant's pod (network test — confirms S51.1 doesn't regress network isolation)
4. Workspace pod carries `llmsafespaces.dev/tenant=<tenant_id>` label
5. Admission webhook rejects a workspace pod when tenant CPU/memory/workspace-count quota is exceeded
6. Quota values come from org plan (org members) or instance defaults (personal users)
7. Pod-hardening tests pass under gVisor RuntimeClass (extend existing `security_test.go` tests to assert hardening survives when `RuntimeClassName: gvisor` is set)
8. Performance: gVisor overhead on a representative workload (LLM-coding session) measured and documented — accept/reject gVisor based on <30% overhead target. The runnable harness and methodology are at `helm/scripts/gvisor-benchmark.sh` and `docs/operator/gvisor-benchmark.md`. The default in the chart (`gvisor.enabled: false`) stays off until at least one operator reports a passing measurement; flipping the default unmeasured violates this criterion.
9. Integration test: tenant A's quota does not affect tenant B; opt-out workspace runs under `runc` as expected

---

## Out of scope (owned elsewhere)

| Item | Owner |
|---|---|
| EFS access points / storage root isolation | Epic 18 S18.8 (reduced — storage only) |
| Node-per-tier for enterprise hard multi-tenancy | Future (VM-per-workspace or node isolation) |
| Side-channel mitigation | Out of scope (accepted risk for soft multi-tenancy) |
| Billing tier → quota mapping automation | Epic 43 (manual config until tier enforcement lands) |
| Per-user S3 shared folder | Epic 31 (US-10.7) |

---

## Sequencing

```
Now (no infra deps):
  S51.3 (Pod tenant label)         ← quick win, zero infra dependency
  S51.1 (gVisor RuntimeClass)      ← primary isolation control, blocked on nothing

After S51.1 + S51.3:
  S51.2 (Admission webhook quotas) ← depends on tenant label (S51.3)
  S51.4 (Hardening regression test)
```

S51.3 (pod label) is the quickest win with zero infrastructure dependency — it can land first and makes the eventual quota webhook mechanical. S51.1 (gVisor) is the primary security control and is blocked on nothing.

---

## Relationship to Epics 10 & 18

**Epic 10:** Replaces original US-10.6. `epic-10-multi-tenant-trust/README.md:361-379` is updated to reference this epic; original text marked as superseded. Network-isolation and RBAC-scoping goals were satisfied by Epics 17/30.

**Epic 18 S18.7 (gVisor):** Moves here. Removed from Epic 18 Phase C. No dependency on hot migration, RWX, or Karpenter.

**Epic 18 S18.8:** Reduced to **EFS storage isolation only** (access points, `efs.csi.aws.com/rootDirectory` annotations). Drops proxy namespace refactor (no namespaces) and NetworkPolicy/cascade items (already satisfied). Lands when RWX does.

**Net effect:** Tenant isolation is unblocked — it no longer waits behind ~40 points of hot-migration infrastructure. Hot migration's S18.8 becomes a focused storage-isolation story that lands when RWX does.
