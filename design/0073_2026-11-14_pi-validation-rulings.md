# 0073 — pi second harness: validation rulings (§1.4 process model, §1.5 permission inbox, §5.3 session storage) + the U-1…U-9 resolution

**Status:** Accepted — closes the pi scoping report's §8 UNVALIDATED list and records the three open-question rulings. Ruling-documentation + evidence index, not a proposal.
**Date:** 2026-11-14
**Scope:** board story tsk-bronze-oxbow-slogs (scoping §9 story S7 — "validation epic + flip"), delivered on the pi stack (S1–S6 branches, PRs #2019/#2020/#2025/#2034/#2036 + the S4 adapter PR).
**Completes:** `/tmp/opencode/pi-scoping/report.md` §8 (9 UNVALIDATED items + 3 open questions); amends-in-place: `docs/operator/pi-supervisor.md` (§1.4 long form — this doc ratifies it with the U-2 evidence sharpened).

---

## 1. One-page summary

| Item | Verdict | One line |
|---|---|---|
| U-1 artifact | **Resolved** | 234-entry directory tree, ELF x86-64 dynamic glibc (libc/ld/pthread/dl/m only); tarball+source SHAs match the release `SHA256SUMS`; the platform's source-build is NOT byte-reproducible → CI pins what CI builds (already the shipped posture) |
| U-2 switch mid-stream | **Resolved** | pi-level `switch_session` mid-turn = abort-and-persist-then-swap (`teardownCurrent` → `session.abort()`); only an extension can veto — strengthens the per-session default (the destructive path is unreachable through the supervisor) |
| U-3 `get_entries since` | **Resolved** | O(n) in-memory `findIndex` + tail `slice` over the once-parsed session; typed error on unknown cursor; compaction-safe; the real cost is RAM-resident sessions (bounded by per-session children) |
| U-4 `!command` context | **Resolved** | models.json keys/headers resolve UNCACHED per model request (`execSync`, inherited cwd+env, 10 s cap, stdout-only; failure names the command); `auth.json` keys are process-lifetime cached — relay-token-via-`!cat` plan validated |
| U-5 offline gating | **Resolved** | `--offline` normalizes to `PI_OFFLINE=1` (+`PI_SKIP_VERSION_CHECK=1`); one seam (`modelNetworkEnabled = PI_OFFLINE === undefined`) gates every catalog fetch; persisted overlays still restore; platform wires `--offline` per spawn |
| U-6 project trust | **Resolved + hardened** | RPC-no-UI "ask" resolves DENY; bare `.pi/` needs no trust; `--approve/--no-approve` pins headlessly → S7 adds explicit `--no-approve` to every child spawn (defense against a future fallback flip) |
| U-7 persona registry | **Resolved** | No persona/agent registry exists in settings; `subagent/` is an extension that spawns processes; prompt-level (`APPEND_SYSTEM.md` / `--append-system-prompt`) is the only mechanism — board personas render as prompt content |
| U-8 command history | **Resolved** | The documented command set has never lost or renamed an RPC command (34 commands, stable since the 2026-09-22 docs split through v1.5.2); removals in history are extension/TUI surfaces; wire evolution is additive |
| U-9 hang behavior | **Resolved** | `isStreaming` stays true through a hang; NO stdout keepalive; `get_state` answers during async hangs (sync blocks bounded at 10 s by `!command`); supervisor-owned timeout logic is required — exactly what S2 shipped |
| §1.4 process model | **RULING A** | process-per-session is THE policy (default `per-session`); single stays behind the policy switch; U-2 evidence + ~79 MiB/child recorded + e2e S7.2 RSS reading |
| §1.5 permission inbox | **RULING B** | No in-band ask/permission for pi workspaces; the sandbox/container boundary is the permission system; extension-owned ask is a scoped future story, not a launch dependency |
| §5.3 session storage | **RULING C** | `/workspace/pi-sessions` on the main PVC subtree, owned by the main container's runtime uid; no new subPath carve; the RO-sidecar is unaffected (bridge/control-socket only) |
| e2e flip gate | **Green** | Tri-overlay launch→ready + verify-failure surfacing + pins-outage fallback + cold-pull resume + gVisor all PASS on the stack (run 43373000078 @ 95f2c7f2f, 2026-11-13); S7 adds the `harness=pi` launch→ready + bridge round-trip legs on this head |

Evidence index: `design/evidence/pi-validation/` (one file per U-item, sources cited at tag v1.5.2).

---

## 2. The three decision records

### 2.1 RULING A (scoping §1.4) — process-per-session is the platform policy

**Question.** pi RPC mode is a single-conversation process. Map platform sessions onto (a) one child per session, or (b) one child + `switch_session`?

**Ruling.** **(a) Process-per-session**, exactly as `pi_policy.go` executes today (`LLMSAFESPACES_PI_POLICY`, default `per-session`; `single` remains implemented behind the same interface as the cost-bounded alternative — selectable per workspace by explicit operator action, never by accident and never as a silent fallback).

**Evidence.**
- U-2 (decisive): pi-level `switch_session` does not refuse while busy — it **aborts the active turn** (persisted, then disposed) and swaps (`agent-session-runtime.ts` `teardownCurrent`; the only veto is a `session_before_switch` extension). A single-process multi-session pod would let session B's prompt kill session A's in-flight turn. The platform's `singleProcessPolicy` refuses while busy (`errPiBusySwitch`) — correct, but under (b) "busy" then blocks every other session's TURN START, which contradicts design 0055 R4 (session-state authority: per-session busy truth) and the D6 escalation inputs (`OldestBusySeconds` per session). Per-session children make both impossible states unreachable.
- Cost (the §8 R2 concern, recorded in `docs/operator/pi-supervisor.md`): idle RPC child VmRSS ≈ **78.8 MiB** (HWM ≈ 89.5, 8 threads; `/proc` probe against the release binary). Children exist only while attached; unattached sessions render idle from the session-dir scan at zero process cost. On the 2-CPU quota profile (`pod_builder.go:171` starvation notes): children are I/O-bound (JSONL + an idle 5 s health poll); CPU pressure is set by model-call concurrency, which the platform already bounds per session — N processes do not multiply model traffic. The e2e S7.2 leg now reads a live child's RSS on every run (step summary) so drift is visible weekly.
- U-3 (supporting): `get_entries since` is cheap per call, but the whole session is RAM-resident per child — under (b) ONE process accumulates every session's memory with no isolation; under (a) memory is per-active-session and releasable on detach.
- U-9 (supporting): no pi-native keepalive means the supervisor owns liveness per child — per-session children give per-session wedge evidence (stdin-drain) rather than one process's aggregate.

**Consequences.** statusz aggregates across children (S2/S5 shipped). Wedge restart is per child (generation-scoped). RSS budget: ~79 MiB × concurrently-attached sessions — the same order the opencode overlay precedent already accepts (design 0053 §8 "~10× agentd"), and pods that cannot afford it run the `single` policy explicitly.

### 2.2 RULING B (scoping §1.5) — no in-band ask/permission for pi workspaces

**Question.** The platform's inbox/ask surface (ListPending/AnswerQuestion/ReplyPermission/RejectInput) rides opencode's permission machinery. pi has none (README; security.md: "Pi does not include a built-in permission system"). Extension-owned re-materialization, or drop?

**Ruling.** **Drop for launch.** A pi workspace runs with **no in-band ask/permission flow**: the container is the boundary — pi's own security.md frames "entirely inside a container, virtual machine, or sandbox" as "usually the strongest practical option", which is precisely the platform's pod model (uid separation, RO overlays, egress policy, PVC-scoped FS). The adapter (S4) advertises capabilities without ask/permission affordances; the frontend is capability-driven (S5 item 5), so pi workspaces honestly render no ask UX rather than a dead one.

**Evidence and shape of the future path (not a launch dependency).**
- pi's extension pipeline is the sanctioned interception point: "Every MCP call passes through Pi's tool pipeline. Extension `tool_call` and `tool_result` handlers, including permission gates, therefore apply" (`mcp.md` Permissions), and RPC carries extension UI records (dialogs/notifications, `rpc.md`) — a platform-authored extension could re-materialize the ask flow over the bridge. That is new platform-owned code version-coupled to pi's extension API — a scoped story if product demands in-band ask for pi, costed when demanded, not before.
- U-7 reinforces the boundary: with no registry, personas/materialization are prompt-level; there is no pi-side agent identity to attach permissions to — another reason tier concepts do not translate.
- `AllowedDirs` (an opencode permission concept) is DROPPED for pi (S3 ruling, unchanged here).

**Consequences.** Permission tiers are opencode-harness features; product copy must not promise ask/permission on pi workspaces. If the extension story is ever built, it lands behind `Adapter.Capabilities()` so the UX flips on, not around, the platform.

### 2.3 RULING C (scoping §5.3) — session storage is `/workspace/pi-sessions` on the main PVC

**Question.** Sessions on PVC: new subPath carve (design 0051 mount-topology) vs a subtree of the existing workspace volume; who owns the files under uid separation?

**Ruling.** **Ratify the as-built**: `LLMSAFESPACES_PI_SESSION_DIR` → **`/workspace/pi-sessions`**, a subtree of the main PVC volume, owned by the main container's runtime uid (the supervisor writes; pi children inherit). **No new subPath.**

**Evidence / why this is sound under design 0051.**
- The sidecar never reads session files: every session-file consumer (adapter List/Open/Remove, VerifyDelivery file-scan, statusz list rendering) reaches them through the supervisor's bridge/control socket — the sidecar's `/workspace` stays RO, unaffected.
- The supervisor (main container, same uid as the children) is the sole writer — no cross-uid write window, no 0051 topology change.
- Subtree-on-one-volume matches the opencode precedent (`opencode.db` inside the workspace tree, `pod_builder.go:211`); a dedicated subPath would add a volume + quota surface for zero isolation gain (the threat model is the pod, not the PVC layout).
- pi-side grouping is cwd-derived (`--<path>--` segments); the fixed `/workspace` cwd makes it stable (scoping §5.2 caveat — non-issue).
- Delete = unlink (pi-documented); S4's gone-classification and the S7.2 e2e DELETE leg rely on it. CountMessages is a file scan (`sessionfile.go`) — cheaper than opencode's walk.

**Consequences.** Backup/quotas see pi sessions as workspace files (same treatment as `opencode.db`). A future dedicated-volume need (session-size quotas per harness) would be an explicit CRD change, not a silent move.

---

## 3. U-1…U-9 resolutions (index; full evidence in `design/evidence/pi-validation/`)

| # | Resolution (one paragraph each — see evidence file) |
|---|---|
| U-1 | `u1-artifact-inspection.md` — tree artifact verified; glibc-dynamic against the base userland; release checksums authenticate BOTH the tarball (811c6f2c…) and the source archive the overlay builds from (da14b72e… = Dockerfile pin). CI-built binary ≠ tarball binary → the pin contract covers what the platform builds (correct; recorded). |
| U-2 | `u2-switch-session.md` — abort-and-swap semantics; platform busy-refusal makes them unreachable; feeds Ruling A. |
| U-3 | `u3-get-entries.md` — in-memory cursor, typed stale-cursor error, compaction-safe; RAM-residency is the real large-session cost, bounded by Ruling A. |
| U-4 | `u4-command-credentials.md` — per-model-request UNCACHED resolution (validated the no-restart relay-token refresh); sync-exec 10 s cap noted; `auth.json` contrast documented. |
| U-5 | `u5-offline-catalog.md` — single seam, fully suppressing startup/periodic catalog fetches; platform wires `--offline`; online pods keep etag-revalidated 4 h refresh. |
| U-6 | `u6-project-trust.md` — RPC default DENY; S7 hardens every spawn with explicit `--no-approve` (`pi_rpc.go`, pinned by `TestPiChildCmdArgv`). |
| U-7 | `u7-persona-registry.md` — no registry exists; prompt-level only; board personas → APPEND_SYSTEM.md content (S3 writer). |
| U-8 | `u8-rpc-command-history.md` + `u8-commit-history.txt` — zero RPC command removals/renames ever; 34-command set stable through the pin; the S6 fixture regime is the right drift response. |
| U-9 | `u9-hang-behavior.md` — `isStreaming` truthful through hangs, no keepalive, `get_state` responsive; supervisor-owned liveness (S2) validated as necessary. |

---

## 4. e2e flip gate — evidence

**Baseline (stack head before S7):** kind run
[`43373000078`](https://github.com/lenaxia/LLMSafeSpaces/actions/runs/43373000078)
@ `95f2c7f2f` (2026-11-13), all legs PASS:

- S5.1/1b/1c missing-pin render refusals (all three overlays)
- S5.2a-e launch→ready (PID1 = overlay agentd; opencode serving; redact
  wrapper). NOTE — that run predates S5's per-harness selection
  (`harness_delivery.go` is absent at `95f2c7f2f`): its S5.2d/e asserted the
  S1-era tri-overlay shape (pi mounted + verified on EVERY pod) and passed
  as such. The current script's S5.2d/e assert the S5 semantics (opencode
  pod mounts NO `/pi`, no `PiVerified` condition) — first proven green on
  this head's run below. Likewise its S5.4b (exit 85 via an opencode-harness
  pod) is S7.3 now (the exit-85 path requires a `harness=pi` workspace
  under S5 semantics)
- S5.3/S5.4/S5.4b verify-failure surfacing (exit 81 / 83 / **85** distinct
  — the baseline's exit-85 leg ran under the tri-overlay semantics; the
  S5-semantics proof is S7.3 on this head)
- S5.4c pi pins ConfigMap survives registry outage + controller restart
- **S5.5 cold-pull resume 17 s ≤ 120 s budget** (agentd+opencode+pi evicted; the ~59 MB pi overlay pulls inside the same resume budget the S5.5 precedent set — the size concern from scoping §2.3 item 5 is closed with margin)
- S5.6 gVisor (runsc): workspace Active, both image volumes serve under gVisor

**S7 additions (this story, `local/s5-overlay-validation.sh`):**

- **S7.1a-d** — `spec.harness=pi` workspace on the sidecar topology →
  Active; PID1 = overlay agentd `supervise-pi`; `PiVerified=True`; sidecar
  init `AGENTD_HARNESS=pi`; the pi tree mounted at `/pi` (the positive
  counterpart of S5.2d's per-harness negative).
- **S7.2a-d** — bridge Basic-auth (`agentd.AuthUsername`/"opencode" +
  the workspace password): session create → 201 (REAL pinned-binary
  child); `get_state` round-trip answering `sessionId` + the PVC
  `sessionFile` path; the zero-message deferral asserted honestly (pi
  creates the session file at the first conversation entry —
  `session-manager.ts` `_hasConversation` — so a fresh session is
  correctly NOT on disk yet) with the list/read/unlink surface proven
  via a materialized session file; DELETE unlinks (gone-class). Plus
  the per-child RSS step-summary reading.
- **S7.3** — the pi verify-failure leg (the S5.4b relocation): wrong
  pi pin on a `harness=pi` workspace → exit **85** → `PiVerified=False`
  (codes disjoint from 81/82/83/84); needs the sidecar chart, hence its
  home in S7.

**This head's run (all 22 legs PASS):**
[`43657434802`](https://github.com/lenaxia/LLMSafeSpaces/actions/runs/43657434802)
@ `5074b6ca3` (2026-11-15) — S5.1-S5.6 baseline legs green (cold-pull
resume **16 s** ≤ 120 s; gVisor PASS; per-harness negatives) AND S7.1a-d +
S7.2a-d + S7.3 green: the `harness=pi` workspace reached Active with
`supervise-pi` as PID1 and `PiVerified=True`, the bridge spawned the real
pinned-binary child and answered `get_state` through it, the PVC session-dir
contract held, the gone-class DELETE unlinked, and the wrong-pi tamper leg
exited **85** with `PiVerified=False`. The per-child RSS reading lands in
the run's step summary (weekly cadence re-proves all of it).

---

## 5. Follow-ups recorded (no action required now)

1. **U-8 drift watch**: one post-pin commit (ede360e4d) is doc-only; the S6
   bump-gate owns the next pin refresh's diff.
2. **U-4 sync-exec**: if the relay ever moves to `!command` chains heavier
   than a file read, prefer `${ENV}` interpolation (no shell, no sync block).
3. **Ruling B extension story**: opened only on product demand; the
   capability seam is the switch.
4. **Reproducibility**: if upstream ever publishes reproducible-build
   metadata, the tarball-binary sha becomes a second cross-check — not a
   replacement for the CI-stamped pin.
