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
- **File ownership** (amended US-4b, 2026-08-21 — owner ruling: stores split by CONSUMER, no new
  PVCs, Memory-medium emptyDirs only): `secrets-env`, `admin-prompt.md`, reload cache → a
  sidecar-ONLY volume (`agentd-secrets`), never mounted in the workspace container — absent from
  uid-1000 space BY MOUNT TOPOLOGY (US-0.2(a): env crosses only via spawn_env). `agent-config.json`
  + `allowed-dirs.json` → `agentd-config` volume: RW sidecar, **RO workspace container** — the
  integrity property (rename-over impossible) is a mount fact, not a mode dance. `rt/secrets/*`,
  `rt/ssh`, `rt/git-credentials`, `rt/auth.json` stay in `sandbox-runtime` as uid-1000
  **tool-consumed** paths (US-35.7 class C — the original §D1 wording listing `rt/secrets/*` as
  sidecar-owned was an imprecision that would have broken every `secret-file` bind). The restart
  marker stays shared at `/sandbox-runtime/last-restart-reason.json` (reason strings, not secret).
  Single-container mode is byte-identical: all relocations are sidecar-mode env overrides.
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

**D7 — Boot-executor relocation (2026-08-25 amendment; step-1 of the fast-track to sidecar mode).**
Motivated by the 2026-08-25 incident: the credential-setup bash heredoc executed `/bin/sh` and
`workspace-agentd` from the RUNTIME image — a user-plane artifact on its own release cadence. A
factory-built base (`ws:s-…-0.8.0`, built that day) carried a pre-#871 agentd and crash-looped
`Init:Error` on contract-shape MCP metadata. Consequence adopted here: **platform boot logic ships in
the platform artifact.**

- `init-fs` subcommand (uid 1000, digest-pinned agentd image): PVC subPath roots (absorbs
  workspace-dirs), the US-35.7 symlink farm hardened against pre-planted symlinks (lstat semantics —
  the link inode is replaced, never followed), G21 password install (0600, never briefly wider),
  #887 admin-token install, free-models copy.
- Bootstrap+materialize: legacy single-container mode keeps them as `platform-bootstrap` /
  `platform-materialize` init containers from the agentd image; **sidecar mode absorbs them into the
  sidecar's boot phase** (`sidecar_boot.go`), running before `ensureBootAgentConfig` and the muxes.
- **Ordering (supersedes the init-exit form of the #857 guarantee):** the main container is gated on
  the sidecar's startup probe, and the sidecar serves `/v1/healthz` only after boot completes —
  opencode's first config read observes completed credential state by construction.
- **Path relocation:** bootstrap output moves to `/sandbox-runtime/rt/secrets.json` (the sidecar's
  `/sandbox-cfg` mount is ReadOnly; the tmpfs has identical pod-scoped lifetime semantics).
- **Restart semantics (a native sidecar RESTARTS; an init container does not):** a non-empty
  secrets.json means bootstrap already ran for this pod — the API is not re-hit (it may be down; the
  600s projected token is expired); materialize re-runs (idempotent by design). Materialize failures
  propagate non-zero → CrashLoopBackOff, surfaced by the controller as `BootReady=False`
  (`ReasonPlatformBootFailed`) + event + metric — never an eternal, reason-less Creating.
- uid/mode matrix for the boot phase (gid 1000 is the pod-wide read bridge):

  | Path | Writer (uid) | Readers (uid) | Mode |
  |---|---|---|---|
  | PVC subPath roots + symlink farm | init-fs (1000) | opencode (1000) | 0755 dirs, links |
  | `rt/ssh`, `rt/secrets` | init-fs (1000) | materializer (2000 sidecar / 1000 legacy) | 0700 |
  | `/sandbox-cfg/password` | init-fs (1000) | main-container entrypoint/agentd (1000); sidecar uses env | 0600 |
  | `/sandbox-cfg/admin-token` | init-fs (1000) | legacy main agentd (1000); sidecar uses env | 0400 |
  | `/sandbox-cfg/free-models.json` | init-fs (1000) | materialize (2000/1000) | 0644 |
  | `rt/secrets.json` (bootstrap out) | bootstrap (2000 sidecar / 1000 legacy) | materialize | 0600 |
  | `rt/*` credential outputs, `agent-config.json` | materialize (2000/1000) | opencode (1000) | 0600 / 0640 (T2 exception) |

- Legacy-no-overlay pods (no `agentdDelivery.image`) keep the bash init containers unchanged; that
  path is deleted in migration step 5 together with the baked binary. Helm rollback (D6.1) that
  lands on a no-overlay chart against a base without the baked binary is the residual D6.1 risk —
  the chart compatibility gate (base floor + single-coordinate agentd pin) is migration step 4.

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
| V2 | uid-1000 shell vs sidecar-owned files (`secrets-env`, admin-prompt, reload cache — on the sidecar-only `agentd-secrets` volume) and `agentdPassword` | absent from uid-1000 space entirely (never mounted in the workspace container; `rt/*` tool-consumed paths are deliberately shared — US-35.7 class C) |
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

  **US-0 (precondition — SPECIFIED in Appendix A + decision below; this PR is the review):**
  1. *Control-socket protocol* → **Appendix A** (this PR): wire format with `v`+`id`, closed v1
     method set (`hello`/`status`/`restart`/`spawn_env`/`metrics`), restart idempotency
     (in-progress-wins), unknown-version/method rejection, and the capability-equivalence rule
     stated IN SPEC (A.4) with its two enforced invariants (no env values out; no arbitrary argv in)
     plus the TDD targets US-1 must hit (A.6).
  2. *`secrets-env` crossing* → **DECIDED: option (a), IPC handoff** — the `spawn_env` method.
     Rationale: option (b) re-creates the readable-`secrets-env` residual the split exists to close;
     (a)'s costs (supervisor memory holds uid-1000-destined data — its destination is uid-1000
     anyway; one extra method) are strictly smaller. Memory-only, write-only, per A.2/A.4.
  3. *Rollback sketch* → D6.1 (already in place, reviewed with v6+amendments).

  US-1 extract `supervise-opencode` (1:1) + control socket (per US-0.1 spec); US-2 sidecar container
  + flag split; US-3 credential split (`agentdPassword` key + Secret upsert; per-endpoint mux table,
  §D1); US-4 file/mount relocations (D1 ownership + integrity mounts; implements US-0.2's decision);
  US-5 chart gate, canary, V-matrix, gVisor leg. Each US is a reviewable PR; security-sensitive ones
  carry the `/security` pass.
- **Upstream asks (blockers for the two named residuals)**: opencode file-based server password (or
  child-env scrubbing); nothing else gates Phase 2.

## 8a. Appendix A — Control-socket protocol (US-0.1 spec)

`supervised` sidecar agentd ⇄ `supervise-*` (PID 1 of the workspace container), `127.0.0.1:4099`.
Single TCP listener on the supervisor; the sidecar is the only intended client. Request/response,
one JSON object per connection, connection per request (no framing, no sessions, no streaming) —
deliberately minimal; anything richer belongs to a future versioned extension, not v1.

### A.1 Wire format

```jsonc
// Request
{ "v": 1, "id": 42, "method": "restart", "params": { ... } }
// Response — exactly one of result | error
{ "v": 1, "id": 42, "result": { ... } }
{ "v": 1, "id": 42, "error": { "code": "method_unknown", "message": "..." } }
```

- `v` (int): protocol version, MUST be `1`. Any other value → error `version_unsupported`, connection
  closed. No negotiation in v1.
- `id` (int, required): echoed verbatim in the response. Lets the sidecar correlate despite the
  one-connection-per-request model (and costs nothing).
- `method` (string), `params` (object, optional; unknown keys within a known method are IGNORED —
  forward compatibility for additive fields).
- Unknown `method` → error `method_unknown`. Never guess, never dispatch on prefix.
- Malformed JSON / wrong top-level types → error `bad_request` where parseable, else connection
  close with no response.

### A.2 Methods (v1 — the complete set)

| Method | Params | Result | Notes |
|---|---|---|---|
| `hello` | `{}` | `{"supervisor":"supervise-opencode","pid":1,"child_pid":123,"child_state":"running"\|"stopped"}` | Liveness + state probe. `hello` with an incompatible `v` is how a version mismatch is DETECTED, not negotiated. |
| `status` | `{}` | `{"child_pid":123,"child_state":"running"\|"stopped","restarts":7,"last_restart_at":"RFC3339"}` | Feeds sidecar statusz/healthz surfaces. |
| `restart` | `{"reason":"health_watchdog"\|"relay_injector"\|"session_aware"\|"credential_reload"\|"manual","grace_seconds":30}` | `{"restarted":true}` or, if already restarting, `{"restarted":false,"in_progress":true}` | Idempotent: a restart arriving mid-restart does NOT queue or fail — it reports `in_progress` and the FIRST restart's parameters win. See A.3. |
| `spawn_env` | `{"env":{"KEY":"val",...}}` | `{"stored":true}` | **US-0.2 decision (a)**: the sidecar pushes the composed child env (secrets-env + parent) over IPC; the supervisor holds it in memory only and uses it for the NEXT spawn. The uid-1000-readable `secrets-env` file is NOT created in sidecar mode. Memory-only: never logged, never dumped, dropped on supervisor exit (pod death wipes the container anyway). Sent once per credential reload before the `restart` that applies it. |
| `metrics` | `{}` | `{"cgroup":{...mem/cpu fields...}}` | The supervisor reads the WORKSPACE container's cgroup (its own) and forwards; the sidecar's own cgroup is the wrong one (0050 finding). Field set fixed in US-1 tests. |

### A.3 Semantics & error model

- **Idempotency**: `restart` is idempotent-by-in-progress (above). `hello`/`status`/`metrics` are
  pure reads. `spawn_env` last-write-wins (a reload replaces the whole env, matching `reset()` +
  re-materialize semantics).
- **Error codes** (closed set v1): `version_unsupported`, `method_unknown`, `bad_request`,
  `child_gone` (supervisor alive, no child and cannot spawn — e.g. missing argv config),
  `internal`. Messages are diagnostics, never secrets.
- **Ordering** (amended in US-2, superseding US-1's implementation note):
  **handler-per-connection**. Each accepted connection is served on its own
  goroutine; reads (`hello`/`status`/`metrics`) are lock-free; `restart` is
  serialized by a dedicated `restartMu` with `TryLock` — a restart arriving
  mid-restart reports `in_progress` instead of queueing — and `spawn_env`
  stores are mutex-protected assignments (last-write-wins; no lock-free
  claims). The original v1 wording ("single-threaded request
  handling — no concurrency needed at v1 volumes") was wrong on its own
  terms: one slow `restart` (seconds of child teardown) head-of-line-blocks
  every status/hello poll under single-threaded accept→handle ordering,
  contradicting the idempotency requirement the same section states. Proven
  by US-1's blocked-restart test (`TestControlSocket_RestartIdempotency`)
  and pinned by US-2's concurrency tests
  (`TestControlSocket_ReadsNotBlockedBySlowRestart`,
  `TestControlSocket_ConcurrentSpawnEnvLastWriteWins`), which stage a slow
  restart and verify reads and spawn_env stores are served meanwhile.
- **Transport honesty**: plain TCP on loopback. Not TLS (loopback in-pod; same-netns spoofing is
  equivalent to uid-1000 code — see A.4), not unix-socket (file path would be a uid-boundary
  artifact).

### A.4 Security posture — the capability-equivalence rule, in spec

The socket is **deliberately unauthenticated**. The rule, stated so nobody "fixes" it later:

> Any 127.0.0.1:4099 caller can cause: opencode to restart, its env to be replaced at next spawn,
> or status to be read. A uid-1000 process in this pod can ALREADY: SIGKILL opencode, SIGSTOP it
> forever, ptrace-inject env, and read /proc. Therefore the socket grants NO capability the threat
> actor lacks. The moment a method would grant something uid-1000 code cannot do natively (e.g.
> "read the stored spawn_env back", "exec arbitrary argv", "fetch a secret"), the socket has
> crossed the line and the design is wrong — such methods are prohibited in ALL future versions
> unless separately designed and reviewed.

Two invariants follow, enforced in review + tests: (1) the supervisor never returns env VALUES
(`spawn_env` is write-only; `status`/`hello` expose pids/state only); (2) `restart` takes a closed
reason enum, never an arbitrary command.

### A.5 Versioning

Bumps add methods/fields, never change existing semantics. Unknown-field tolerance (A.1) is the
forward-compat mechanism. A v2 proposal must re-derive A.4 before review — the rule is versioned
with the protocol.

### A.6 Test plan (US-1's TDD targets)

1. Golden wire tests: every method's request/response shape pinned byte-level (version, id echo).
2. `version_unsupported` on `v:2`; `method_unknown` on `"method":"exec"`; `bad_request` on malformed.
3. Restart idempotency: two concurrent `restart`s → exactly one restart, second gets `in_progress`.
4. `spawn_env` memory-only: no file written anywhere under any tmpfs path the tests can observe.
5. Capability-equivalence test (negative): assert NO v1 method returns env values or accepts argv.

---

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
