# 0051 — agentd uid separation: in-workspace privilege tiers

**Status:** Draft — under review (holds; never auto-merges)
**Date:** 2026-08-18
**Issue:** #887
**Supersedes:** none. Refines the residual risk documented in PRs #883/#884/#886 (Epic 67).
**Author:** agent session 2026-08-18

---

## 1. Problem statement

Every agentd user-mux endpoint now requires Basic auth (#762/#847/#848, merged). That closed the
*unauthenticated* in-pod surface. What remains open — and what #887 tracks — is the **same-uid** path:
code running inside the workspace container as uid 1000 (malicious pip package, InitScript, compromised
tool output) can read `/sandbox-cfg/password` and authenticate to agentd exactly like the API server does.

The naive reading of #887 is "run agentd as a different uid." This doc's job is to establish what uid
separation actually buys once every credential channel is accounted for — because the analysis below shows
the env channel, not the file, is the pivot — and to ship the pieces that survive scrutiny.

## 2. Verified current state (main @ 728f56f0)

| Fact | Evidence |
|---|---|
| Whole container runs as uid 1000 (`USER sandbox`); agentd is PID 1 via `exec --supervise` | `runtimes/base/Dockerfile:323,326,329`; `entrypoint-opencode.sh:21-24` |
| The entrypoint reads the password file and exports it into agentd's environment | `entrypoint-opencode.sh:17-19` |
| agentd spawns opencode with `buildEnvFrom(secrets-env)`, which **starts from `os.Environ()`** — opencode inherits `OPENCODE_SERVER_PASSWORD` in its environment | `managed_process.go:474`; `secrets.go:1203-1205` |
| User code (bash tool etc.) is a child of opencode, same uid 1000, no credential drop anywhere | `managed_process.go:471-475` (no `SysProcAttr.Credential`) |
| Password file is mode 0600, uid 1000 (init container `install -m 0600`, pod `runAsUser: 1000`) | `pod_builder.go:555-560`, `:425-438` |
| `/sandbox-runtime` (agent-config.json with provider keys, secrets-env, rt/* symlinks) is RW tmpfs owned by uid 1000 | `pod_builder.go:166-190`; `pkg/agentd/types.go:22-38` |
| opencode reads `agent-config.json` **once at boot, no hot reload** | validated in README-LLM.md §Relay Config Subsystem (opencode 1.15.12 behavior, stable) |
| Admin mux: bearer gate is a **no-op when `AGENTD_ADMIN_TOKEN` is unset**; `/metrics` on 4098 is unauthenticated | `server.go:163-176` (early return), `:213` |
| Basic-auth gate accepts a readable-but-**empty** password file (`Basic b3BlbmNvZGU6` is computable) — controller-generated secrets are 32 random chars, but operator-created `workspace-pw-*` Secrets are adopted as-is | `auth.go:33`; flagged in #886 review rounds |

## 3. Threat-model reframe (the important part)

**Same-uid in-pod code is, by product definition, the workspace owner's own code.** It runs the user's
builds, their git commits, their tests. Three sub-cases matter, with different answers:

| Asset | Same-uid read | Verdict |
|---|---|---|
| User's own secrets (SSH keys, git creds, env secrets) | **Intended.** They are delivered to the workspace to be used | Not a boundary. Out of scope |
| Session history (opencode `:4096`, MCP proxy `:4097`) | The user's own conversation in their own workspace | #847 closed the *unauthenticated* read. Same-uid read of your own history is not a tenant-boundary violation. Out of scope |
| **Platform-acting surfaces**: workflow dispatch (spends platform resources), reload-secrets (config tamper + restart), agentd's own integrity, **provider keys materialized by the platform** | Unauthenticated access was the #762/#848 bug (fixed). Authenticated-with-stolen-credential access remains | **This is #887's actual scope** |

So the goal is narrow: **make agentd's credential and the platform-materialized provider keys unreachable
by uid-1000 code, to the extent structurally possible.** Everything else same-uid code can read is either
yours by design or already yours through opencode.

## 4. The env-channel pivot — corrected after source verification (v2 of this doc)

> **v1 of this section framed the env channel as a `/proc/<pid>/environ` read gated by Yama
> `ptrace_scope`. That was wrong.** Verified against the pinned opencode v1.18.10 source: the actual
> channel is **spawn-time inheritance** — opencode's shell tool spawns tool processes with
> `extendEnv: true` (`session/prompt.ts:559-562`), passing its **full parent environment** to every
> bash/child process. No ptrace, no Yama, no conditional. Two secrets ride this chain today:

| Secret | How it enters opencode's env | In every tool's env today? | Evidence |
|---|---|---|---|
| `OPENCODE_SERVER_PASSWORD` | entrypoint export → agentd → `buildEnvFrom(os.Environ())` | **Yes** | `entrypoint-opencode.sh:17-19`; `secrets.go:1203-1205`; `prompt.ts:559` (`extendEnv: true`) |
| `AGENTD_ADMIN_TOKEN` | **pod-spec env on the main container** | **Yes** | `pod_builder.go:78-85` (SecretKeyRef → env); same inheritance chain |

**Implication A — the admin token is the worst-kept secret in the pod.** It authenticates the admin mux
(`:4098` — statusz session lists, provider introspection, `/metrics`), the mux is loopback-reachable from
in-pod code, and the token sits in every tool's env. This is a **live finding**, worse than #887's
theoretical premise, and — critically — fixable **without upstream**: opencode does not need this token.

**Implication B — the password cannot be file-injected by us alone.** opencode reads the server password
**env-only** (`cli/cmd/serve.ts:15`, `server/auth.ts:18` — no file option exists) and hands its whole env
to children. So: no uid split, file mode, or ptrace policy protects the password from same-uid code under
the current opencode. Basic auth on `:4097` is honestly **network-boundary-only** defense until opencode
either supports a file-based password or scrubs its child env. Both are upstream asks.

**What Yama still governs** (demoted from linchpin to footnote): cross-process `/proc/<pid>/environ` reads
of *non-descendants* (e.g. one tool reading another agent session's process). Relevant only if opencode
ever scrubs the two platform secrets — then it becomes the residual gate. No longer a design dependency.

**User env-secrets are explicitly out of scope of any "remove from env" work**: they are the product
feature (bound for the user's builds/tools). File-vs-env for them is UX, not a security boundary.

## 5. Design

### D1 — In-container uid split, not a sidecar

agentd stays PID 1 (supervision, single-artifact digest provenance — reaffirms #863 decision #1) but the
container's `runAsUser` becomes **uid 65532** (agentd tier, non-root, unused); agentd spawns opencode via
`setpriv --reuid=1000 --regid=1000 --clear-groups` (`SysProcAttr.Credential` in
`defaultOpencodeCmdFactory`). Supervision is unchanged — agentd remains the parent, signals and exit
reaping work identically.

- agentd tier (65532): reads `/sandbox-cfg/password` (becomes `0400`, owned 65532), writes
  `/sandbox-runtime/*`.
- workspace tier (1000): opencode, all tool processes, `/workspace`, `/home/sandbox` — as today.
- The init containers (credential-setup, materialize) move to uid 65532: they write credential state
  agentd consumes; nothing in them needs uid 1000 (they already run before the main container starts and
  touch only tmpfs/PVC-symlink setup, which needs uid 1000 for `$HOME` symlinks — those few `ln -s`/`chown`
  steps run via a bounded `setpriv --reuid=1000` wrapper inside the script, or stay in a second short init
  container at uid 1000 ordered before credential-setup).

### D2 — Password file: 0400, agentd tier only

`install -m 0400 -o 65532` in the init script. Main-container (uid 1000) never sees the file. The env
channel (§4) is the residual; its status depends on the ptrace validation.

### D3 — `agent-config.json` post-boot lockdown (protects provider keys)

Provider keys are the highest-value platform-materialized secret. opencode reads the file **once at
boot** (verified, no hot reload). Sequence:

1. materialize/boot writes `agent-config.json` mode `0644` (both tiers must read during boot).
2. agentd, immediately after opencode reports ready (existing readiness path), `chmod 0400` (owned 65532).
3. Every ConfigWriter rebuild re-writes atomically at 0400 — the ~ms window per reload between rename and
   ready-re-lock is accepted and logged (reload cadence is user-driven, not adversarial-timed).

Residual: keys are plaintext in tmpfs during boot and for the rename window on each reload. Accepted —
strictly narrower than today (always-readable).

### D4 — `rt/` credential tree splits by consumer

- `rt/auth.json` (opencode relay identity): stays uid-1000-writable — opencode writes it at runtime.
  Residual: readable by same-uid code. Accepted (low value: free-tier relay token, user-scope).
- `rt/secrets/*`, `secrets-env`: owned 65532; opencode receives env at spawn (already the mechanism).
  `secrets-env` mode 0400-65532. `secrets-env` content already reaches user code only via deliberate
  env-secret binding — unchanged semantics.

### D5 — Fail-closed fixes that ship regardless of the uid work (Phase 1)

Independent of §4's outcome; each closes a hole found during the #886 review rounds **or this doc's v2
env-channel verification**:

1. **`AGENTD_ADMIN_TOKEN` becomes a DISTINCT secret and leaves the environment entirely** (highest
   priority — live leak). *Correction from this doc's own v2: token==password today (both sourced from
   the same Secret key), so a mere scrub/file move would be theater — `OPENCODE_SERVER_PASSWORD` (same
   value) rides the same chain. The fix only delivers value if the admin token is regenerated as a
   separate 32-char value:*
   - `ensurePasswordSecret` upserts a distinct `admin-token` key (generated once; **never rotated in
     place** — running pods hold the accepted value in agentd memory while rebuilt probe specs read the
     Secret; in-place rotation desyncs them).
   - Controller delivery: init installs `/sandbox-cfg/admin-token` mode 0400 (runtime-guarded on key
     presence); main container gets `AGENTD_ADMIN_TOKEN_FILE` only — **no `AGENTD_ADMIN_TOKEN` env in
     file mode**. Legacy Secrets (pre-upsert) keep the env path for the transition; no NEW pod is built
     in legacy mode once upsert converges.
   - agentd reads the token file-first (`AGENTD_ADMIN_TOKEN_FILE`), env fallback for legacy pods.
   - **agentd scrubs `AGENTD_ADMIN_TOKEN`/`AGENTD_ADMIN_TOKEN_FILE` from the env it passes opencode**
     (`buildEnvFrom`) — applied post-merge so a user-staged env-secret cannot smuggle one back.
   - Bearer consumers of `:4098` (kubelet probes, controller deep-status, API relayChecker + statusz
     sites) try the distinct admin token first and fall back to the password on 401 — self-healing
     across the mixed fleet while pods rebuild.
2. **`AGENTD_ADMIN_TOKEN` required**: agentd refuses to start when unset/unreadable (env-driven wiring
   gap today is silent pass-through). Dev/kind escape hatch: explicit `AGENTD_ALLOW_NO_ADMIN_TOKEN=1`.
3. **Empty-password boot reject**: `readAgentPassword` treats a readable-but-empty file as fatal (G46
   path) instead of arming a guessable credential.
4. **`/metrics` off the admin mux's unauthenticated path**: bind to the token-gated handler (or
   loopback + kube-rbac-proxy per the existing values.yaml guidance).

The password's env leak (`OPENCODE_SERVER_PASSWORD` → tools) is **not** in Phase 1 — it is
upstream-gated (§4 Implication B): opencode must add file-based server auth or child-env scrubbing.
Tracked as the upstream dependency below; until it lands, docs and issue threads must describe `:4097`
Basic auth as network-boundary defense only.

### D6 — Rollout: chart-gated, canary-first, reversible

`controller.agentdUidSplit.enabled` (default false → container stays uid 1000 as today; the split is
purely additive wiring: runAsUser, file modes, setpriv spawn). Enable on TEST for a full validation pass
(§7) before any default flip. Reverting = unset the flag + recreate pods.

## 6. What this design does NOT do (and why)

- **Sidecar agentd** — rejected (reaffirms #863 D1): breaks PID-1 supervision of opencode, splits
  single-artifact digest provenance into an N×M version matrix, and buys nothing over the in-container
  split because the pod network namespace is shared either way (`:4097` reachable from the main container
  regardless).
- **Protecting the user's own secrets/history from same-uid code** — not a boundary (§3). Chasing it means
  fighting opencode's process model for no tenant-security gain.
- **Closing the env channel ourselves** — not in our control: `OPENCODE_SERVER_PASSWORD` is opencode's
  supported mechanism. If opencode grows file-based server auth (upstream ask, to file), D2's residual
  closes completely. Tracked as an upstream dependency note, not a blocker.

## 7. Validation matrix (gate for leaving canary)

| # | Check | PASS criterion |
|---|---|---|
| V1 | **(Phase 1)** After D5: `printenv AGENTD_ADMIN_TOKEN` in a bash tool | empty — token gone from tool env |
| V2 | uid-1000 shell cannot read `/sandbox-cfg/password`, `agent-config.json` (post-ready), `secrets-env`, `/sandbox-cfg/admin-token` | EACCES on all four |
| V3 | `printenv OPENCODE_SERVER_PASSWORD` in a bash tool | **Expected non-empty under current opencode** (upstream-gated residual — record, don't fail) |
| V4 | opencode boots, agent reaches Active, one full agent turn (config read at boot unaffected) | End-to-end works |
| V5 | Relay injector + reload-secrets path: config rebuild → opencode session-aware restart cycle | Works; post-restart re-lock observed |
| V6 | Dev-preview tunnel + MCP proxy loopback (`127.0.0.1:4097`) | Unaffected (network namespace unchanged) |
| V7 | Suspend → resume (~22s budget) and cold boot | No regression vs. baseline |
| V8 | Watchdog SIGTERM path (agentd 65532 signaling opencode 1000) | Signal delivery works (parent→child unaffected by uids) |
| V9 | Zombie reaping (#908 path) under uid split | Unchanged (agentd is reaper as PID 1) |
| V10 | Phase-1 items: no-token boot fails; empty-password boot fails; `/metrics` gated | Fail-closed observed |
| V11 | Yama footnote (informational): `cat /proc/sys/kernel/yama/ptrace_scope` per node class | Document values; only security-relevant if/when the upstream password fix lands |

## 8. Phasing

- **Phase 1 (D5.1 first) — small, independent PRs, no uid changes, no rollout risk**:
  1. agentd env-scrub + admin-token file read (controller + agentd; closes the tool-env leak of the
     admin-mux bearer token **today**)
  2. the remaining fail-closed trio (required token, empty-password reject, gated metrics)
- **Phase 2 (D1–D4, D6) — chart-gated pilot**: controller wiring, entrypoint changes, ConfigWriter lock,
  runtime image entrypoint rework. TEST validation matrix → canary workspaces → default flip decision.
  Note: with D5.1 shipped, Phase 2's uid split protects the admin token, provider keys (D3), and the
  secret files — but NOT the workspace password (§4 Implication B).
- **Upstream asks (blockers for closing the password residual)**: (a) opencode file-based server
  password, or (b) opencode child-env scrubbing of designated secrets; also per-tool uid drop (would
  enable a third tier where tool code runs below even the workspace uid — out of scope until it exists).

## 9. Open questions (resolved before Phase 2 implementation)

1. **Q1 — Init-container split mechanics**: does the `$HOME` symlink farm move to a separate uid-1000 init
   container (cleaner) or a bounded `setpriv` wrapper inside the existing script (fewer moving parts)?
   Lean: separate init container, ordered first — init containers are cheap and the ordering is already
   explicit in the pod spec.
2. **Q3 — Windows-service-style uid**: is 65532 free across all runtime images (base/python/nodejs/go)?
   Verified for base in this doc's drafting; the others need the same one-line check during Phase 2.
3. **Q4 — `docker run` local-dev story**: the split must not break non-K8s local runs of the runtime
   image (where uid 1000 may be the only human-mapped uid). Escape hatch: env `AGENTD_NO_UID_SPLIT=1`
   for local dev only, loudly logged.

## 10. Relationship to existing controls

This is layer 3 of the defense-in-depth stack; it changes nothing about layers 1–2:

1. NetworkPolicy (primary — workspace↔platform, workspace↔tenant edges)
2. Basic auth on all agentd muxes (#883/#884/#886 — network-reachable attackers, misconfig coverage)
3. **uid tiers (this doc) — in-pod, authenticated-with-stolen-credential attackers**
4. gVisor RuntimeClass (opt-in, Epic 51 — kernel-exploitation containment; also potentially closes V3)
