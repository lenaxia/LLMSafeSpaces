# 0051 — agentd uid separation: in-workspace privilege tiers (v6, sidecar architecture)

**Status:** Phase 1 SHIPPED (#933 D5.1 + #934 D5.2/D5.3, merged 2026-08-19); Phase 2 (uid tiers) under review (holds; never auto-merges)
**Date:** 2026-08-18 (v6 consolidation 2026-08-19)
**Issue:** #887
**Supersedes:** the unmerged `design/0050_2026-08-16_agentd-uid-separation` draft (bot run #31919066574; its branch push failed, and the 0050 number was subsequently reused by the starvation-truthfulness design in #892/#898). v6 **adopts 0050's architecture wholesale** — its code pass was correct where v1–v5 of this doc were wrong (see §4a). Supersedes nothing else; refines the residual-risk record of PRs #883/#884/#886.
**Author:** agent session 2026-08-18; v6 rewrite after five review rounds

---

## 1. Problem statement

Every agentd user-mux endpoint requires Basic auth (#762/#847/#848). That closed the *unauthenticated*
in-pod surface. What remains open — #887's scope — is the **same-uid** path: workspace code (uid 1000)
can read every credential that exists in uid-1000 space and reach every loopback port. The goal: make
agentd's credentials and platform-acting surfaces unreachable by uid-1000 code, to the extent
structurally possible — and be precise about what is not possible.

## 2. Verified current state (post-#933/#934, main @ 77e47ec7)

| Fact | Evidence |
|---|---|
| Whole workspace container runs uid 1000; agentd is PID 1 via `exec --supervise` | `runtimes/base/Dockerfile:323-329`; `entrypoint-opencode.sh` |
| Container hardening: `RunAsNonRoot`, `AllowPrivilegeEscalation: false`, `Capabilities: drop ALL` | `pod_builder.go:176-178` — **no setuid path exists; none can be granted without weakening this** |
| **The workspace password exists in FOUR uid-1000-readable forms**: the file (`/sandbox-cfg/password` 0600), `OPENCODE_SERVER_PASSWORD` env (opencode's own auth — upstream env-only), **#884's `injectAgentdMCPServer` Basic header embedded in `agent-config.json`** (opencode must read that file), and (pre-#933) the admin-token env — now split & file-only | `pod_builder.go` init script; `mcp_server.go` (Basic-header injection); §Phase 1 for the closed fourth copy |
| `reload-secrets` materializes a batch decoded from the request body — a readable Basic secret means in-pod code can **inject or wipe credential material**, an integrity attack, not just disclosure | `secrets.go` reload handler (0050 finding 3, re-verified) |
| Admin mux `/metrics` unauthenticated; token now distinct + file-only (D5.1 shipped) | `server.go` requireBearerToken wrap; #933 |
| opencode reads `agent-config.json` once at boot, no hot reload | README-LLM §Relay Config (stable across pinned versions) |

## 3. Threat-model frame (unchanged since v2)

Same-uid in-pod code is the workspace owner's own code. The defensible boundary is **platform-acting
surfaces** (workflow dispatch, reload-secrets, agentd integrity) and **platform-materialized provider
keys** — plus now, from 0050's finding 3, the **integrity** of credential materialization. The user's
own secrets/history are theirs by design and out of scope.

## 4. What does NOT work (v1–v5 errors, now closed)

- **In-container uid split (v1–v5 D1): impossible.** A non-root process with an empty capability set
  cannot `setuid` to another uid; `SysProcAttr.Credential` from uid 65532 → 1000 needs CAP_SETUID we
  deliberately do not have (0050 finding 2; reviewer F1, five rounds). Granting CAP_SETUID/SetUID
  binaries to buy separation would be a larger attack surface than the one removed.
- **Cross-uid signaling (v1–v5 D1/V8): `kill(2)` across uids requires matching ruid/euid or
  CAP_KILL — parentage is irrelevant** (reviewer F2). agentd→opencode SIGTERM/SIGKILL
  (`managed_process.go:348-405`, `relay_injector.go:469`) would be EPERM under any split.
- The only viable topology is **two containers in one pod** (shared netns keeps all
  `127.0.0.1:{4096,4097,4098}` URLs unchanged) — exactly 0050's architecture.

### 4a. The 0050 draft (adopted)

agentd becomes a **native sidecar** (init-container with `restartPolicy: Always`; chart floor 1.35 ✓)
at **uid 2000 / gid 1000**, running the existing #872 digest-pinned image with `--sidecar`. A new
`supervise-opencode` subcommand becomes **PID 1 of the workspace container** — extracted 1:1 from
`managed_process.go` so `wait()`, OOM classification, and SIGTERM supervision stay same-uid where they
must be. Policy (session-aware deferral, healthz watchdog, SSE tracking) stays in the sidecar; the
supervisor exposes an **unauthenticated `127.0.0.1:4099` control socket** (restart/status/metrics),
whose safety rests on capability equivalence: *restart is not stronger than SIGKILL, which same-uid
code already holds*. Native-sidecar start order preserves the #857 stamp-before-opencode-reads
guarantee. Cgroup metrics are sourced via the supervisor (a sidecar reads its *own* cgroup). gVisor
(`runsc`) behavior for native sidecars and nested RO+RW subPath mounts is the big unvalidated
assumption, with a plain-sidecar fallback documented if it fails.

## 5. Design (Phase 2)

**D1 — Native-sidecar split (0050 architecture, replaces v1–v5 D1/D3/D4).** As §4a. Consequences:

- **Credential split**: a NEW `agentdPassword` Secret key becomes the user-mux Basic secret, delivered
  env-only to the sidecar — **agentd's secret never exists in uid-1000 space at all** (no file to
  protect, no mode dance, no rebuild-window). This supersedes v5's D2 (0400 password file) and D3
  (post-boot chmod of `agent-config.json`): the file and its embedded MCP Basic header remain
  uid-1000-readable **by necessity** (opencode is their reader; the MCP entry's only caller is
  opencode itself on loopback) — recorded as an honest residual, not papered over.
- **User-mux becomes two-credential (per-endpoint table; 0050's carve-out made explicit):**

  | Endpoint group | Basic secret | Callers |
  |---|---|---|
  | Control plane: `reload-secrets`, `workflow/*`, `agent/reload` | `agentdPassword` (sidecar env only) | API server |
  | `/v1/mcp` | **workspace password (carve-out)** — its only caller is opencode on loopback, which lives in uid-1000 space and cannot hold `agentdPassword` by design | opencode |
  | Dev-preview (`/v1/dev-preview/`) | workspace password | API server |

  Consequence for US-3 (implementer note): the shared `checkBasicAuth` gate gains a per-mux
  credential parameter — the sidecar's user mux accepts either `agentdPassword` OR the workspace
  password per route registration; existing routes keep `deps.password`, control-plane routes switch.
  The sidecar retains the workspace password as a CLIENT credential (healthz, MCP proxy → opencode
  `:4096`, workflow agent-node session calls) — it lives in sidecar env (uid-2000 space) and leaks
  nothing into uid-1000 that opencode's own config doesn't already hold.
- **File ownership**: `secrets-env`, `rt/secrets/*`, `admin-prompt.md`, reload cache → sidecar-owned
  under its own mount; `agent-config.json` + `rt/auth.json` → uid-1000 space (opencode's).
  `agent-config.json` gains **integrity** (not confidentiality): the sidecar writes it via a mount the
  workspace container sees read-only (RO root + RW `rt/` subPath per 0050 — a plain RW mount would
  allow rename-over).
- **Integrity of reload-secrets** closes with reachability: the control-plane surfaces
  (`:4097/:4098`) hold credentials uid-1000 code can no longer obtain.
- **Supervisor scope invariant**: `supervise-*` is plumbing — spawn, reap, signal, status, metrics
  forward, nothing else. It runs as uid 1000 *inside* the snoopable space; anything it holds or
  decides is reachable by the threat actor. New capability ideas (env assembly, exec hooks, config
  rendering) are wrong-sided by definition — they belong in the sidecar behind the control socket.
  Reviewers should reject supervisor PRs that grow it.

**D5 — Phase 1 (SHIPPED, unchanged).** D5.1 distinct admin token/file delivery/env scrub (#933);
D5.2 required-token boot + D5.3 empty-password reject (#934); D5.4 `/metrics` ruling (no change —
per-pod scrape secrets are structurally unavailable to PodMonitor; labels audited workspaceID+counts).

**D6 — Rollout.** Chart-gated `agentdSidecar.enabled` (default false = today's single-container mode,
unchanged); canary on TEST; the V-matrix gates any default flip. The supervisor extraction is
behavior-identical by construction (1:1 move), pinned by the existing managed-process test suite.

**D6.1 — Rollback (mixed-generation fleet).** The migration is stateful: Secrets gain keys, mounts
relocate, opencode restarts under a new parent. Un-flipping `agentdSidecar.enabled` must therefore
converge, not assume uniformity — same pattern as #933's admin-token rollout:

1. Flag off → controller builds single-container pods again; existing multi-`Data` Secrets are
   simply ignored by the old code path (extra keys are inert — the #933 upsert already established
   that extra Secret keys don't break legacy readers).
2. Running sidecar-generation pods are drained by the normal `spec.restartGeneration` bump, not
   force-recreated; pod-level convergence is the reconciliation loop's job.
3. The user-mux two-credential table (§D1) means BOTH credentials remain valid during the window —
   the sidecar accepts either per route, so API-server dispatch works against pods of any generation.
4. File relocations are one-directional (US-4 writes sidecar-owned files; the single-container mode
   writes the same paths uid-1000-owned) — a rolled-back pod re-chowns by rewriting, which
   `reload-secrets` already does via `reset()` + re-materialize on every boot.

Rollback is exercised as part of US-5's canary (flip on → validate → flip off → validate legacy
pods healthy → flip on again), so D6.1 is tested before any default flip, not just asserted.

## 6. What this design does NOT do

- **No capability grants** — CAP_SETUID/CAP_KILL buy-backs are rejected outright (§4).
- **No protection of the user's own secrets/history from same-uid code** — not a boundary (§3).
- **No closing of the `OPENCODE_SERVER_PASSWORD` env leak or the `agent-config.json` MCP Basic
  header** — both are opencode-upstream constraints (env-only password; opencode must read the file
  it's configured from). Upstream asks tracked; both are residuals by necessity, now named.
- **Does not revisit #863 D1** ("the image is the workspace") — that decision governs *delivery
  provenance* (agentd ships via the digest-pinned image volume, #872); the sidecar runs the SAME
  digest-pinned artifact, so single-artifact provenance is preserved. v5's §6 sidecar rejection was
  premised on the impossible in-container split and is withdrawn.

## 7. Validation matrix (gate for leaving canary)

| # | Check | PASS criterion |
|---|---|---|
| V1 | `printenv AGENTD_ADMIN_TOKEN` in a bash tool (**runnable now — Phase 1 shipped**) | empty |
| V2 | uid-1000 shell vs sidecar-owned files (`secrets-env`, `rt/secrets`, admin-prompt, reload cache) and `agentdPassword` | EACCES / absent from uid-1000 space entirely |
| V3 | `agent-config.json` readable (expected — opencode's) **but not writable** (RO mount); `rt/auth.json` RW (expected) | integrity holds: hash unchanged across a session |
| V4 | `:4097`/`:4098` auth with workspace password / old admin token | 401 — credentials unknown to uid-1000 code |
| V5 | opencode boot, Active, one agent turn; suspend→resume budget; cold boot | No regression vs baseline |
| V6 | Watchdog restart, session-aware deferral, relay-injector restart — now via control socket → supervisor (same-uid signals) | Restart paths work; **no cross-uid signaling exists anywhere** |
| V7 | Zombie/orphan reaping (#904/#908) — supervisor remains subreaper in the workspace container's PID ns | Unchanged |
| V8 | Dev-preview tunnel + MCP proxy loopback | Unaffected (shared netns) |
| V9 | gVisor leg: runsc × {native sidecar, plain sidecar fallback}, incl. nested RO+RW subPath mounts | Documented accept/reject + fallback decision |
| V10 | (SHIPPED — pinned by #934's integration tests) fail-closed boots | Green in CI |

## 8. Phasing

- **Phase 1 — SHIPPED** (#933, #934; merged 2026-08-19): distinct admin token (file-only + env scrub
  + mixed-fleet bearer fallback; the round-2 dash-bashism lesson is why exec-level init-script tests
  now exist), fail-closed boots, D5.4 ruling. GO-2026-6173 cleared en route.
- **Phase 2 — sidecar migration** (0050's US-1..US-5 shape, plus US-0):

  **US-0 (precondition — no implementation before these are specified and reviewed):**
  1. *Control-socket protocol.* The `127.0.0.1:4099` channel is the new trust boundary and is
     currently one sentence. Spec must fix: message shapes (request/response), a version field from
     day one, behavior on unknown version/method (reject, don't guess), restart-idempotency (restart
     during a restart), and the observation that it is DELIBERATELY unauthenticated per the
     capability-equivalence rule — state the rule in the spec so nobody "fixes" it into something
     that holds secrets.
  2. *`secrets-env` crossing.* Decide the one mechanism by which the child env crosses the uid
     boundary: (a) sidecar hands the composed env to the supervisor over the control socket at
     spawn (crossing stays in IPC; supervisor memory transiently holds it — acceptable, it's uid-1000
     data by destination anyway), or (b) a uid-1000-readable copy under the existing mount (simple,
     but re-creates a readable `secrets-env` residual — must be ledgered if chosen). Prefer (a)
     unless measurement says otherwise.
  3. *Rollback sketch (D6.1 below) reviewed.*

  US-1 extract `supervise-opencode` (1:1) + control socket (per US-0.1 spec); US-2 sidecar container
  + flag split; US-3 credential split (`agentdPassword` key + Secret upsert; per-endpoint mux table,
  §D1); US-4 file/mount relocations (D1 ownership + integrity mounts; implements US-0.2's decision);
  US-5 chart gate, canary, V-matrix, gVisor leg. Each US is a reviewable PR; security-sensitive ones
  carry the `/security` pass.
- **Upstream asks (blockers for the two named residuals)**: opencode file-based server password (or
  child-env scrubbing); nothing else gates Phase 2.

## 9. Open questions

1. **Q1** — Native sidecar vs plain sidecar under runsc (V9): decide by measurement on the TEST
   gVisor node; fallback documented in 0050 §12.
2. **Q2** — `supervise-opencode` control-socket protocol surface (restart/status/metrics): keep it
   minimal-and-versioned from day one; extensibility is a future trap.
3. **Q3** — Secret key migration order: `agentdPassword` upsert must precede any sidecar-enabled pod
   build (same convergence pattern as #933's admin-token upsert, including never-rotate-in-place).
4. **Q4** — Local-dev (`docker run`) story: single-container mode remains the default off-Flink; the
   sidecar path is chart-gated, so local dev is unaffected by default.

## 10. Relationship to existing controls

1. NetworkPolicy (primary), 2. Basic auth on all agentd muxes (#883/#884/#886), 3. **uid tiers (this
design — Phase 2)**, 4. gVisor RuntimeClass (opt-in; also the V9 leg). The password env residual
(§6) is **not** affected by gVisor — plain envp inheritance is not a ptrace phenomenon — and closes
only via the upstream ask.
