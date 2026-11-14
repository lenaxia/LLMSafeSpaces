# NNNN — pi validation epic + flip: U-1…U-9 settled, harness=pi e2e legs, three rulings (tsk-bronze-oxbow-slogs)

Board story: tsk-bronze-oxbow-slogs (pi second-harness epic, story S7 of the
scoping report §9; siblings S1 #2019 / S2 #2020 / S3 #2025 / S4 / S5 #2034 /
S6 #2036).

## The gap

Scoping report §8 left nine UNVALIDATED items (read-only spikes) and three
open questions (§1.4 process model, §1.5 permission inbox, §5.3 session
subPath). The e2e proof obligation — launch→ready on stripped base + triple
overlay in kind incl. the gVisor leg, plus the ~59 MB overlay resume-pull
measurement — existed as the S5 script but had never included the second
harness itself as the LIVE harness (S5.2 proves the tri-overlay pod with
opencode as the harness; pi was mounted+verified cargo).

## What this story did

### 1. The U-1…U-9 spikes (read-only, against the pin v1.5.2)

All nine resolved; evidence + citations in `design/evidence/pi-validation/`,
rulings indexed in design 0073 §3:

- **U-1** tarball: 234-entry tree, ELF x86-64 dynamically linked against
  glibc only; tarball SHA = release SHA256SUMS; source-archive SHA = the
  Dockerfile pin. Cross-check: the CI source-build binary (54c7cc79…, kind
  run pins line) is NOT bit-identical to the release-tarball binary
  (6a2ac1c5…) → recorded: CI stamps/verifies what CI builds; release
  checksums are ingress verification.
- **U-2** `switch_session` mid-stream: pi-level abort-and-persist-then-swap
  (`agent-session-runtime.ts` teardownCurrent → session.abort()); only a
  `session_before_switch` extension can veto. Scoping's "busy blocks
  switching" was imprecise — the truth STRENGTHENS Ruling A.
- **U-3** `get_entries since`: in-memory findIndex+slice over the
  once-parsed session; typed error on unknown cursor; RAM-residency is the
  real large-session cost (bounded by per-session children).
- **U-4** `!command`: models.json keys/headers resolve UNCACHED per model
  request (resolveConfigValueOrThrow → resolveConfigValueUncached →
  execSync, inherited cwd+env, 10 s cap, failure names the command);
  auth.json keys are process-cached. Relay-token-via-`!cat` no-restart
  refresh plan validated; sync-exec 10 s event-loop block noted (per-session
  blast radius).
- **U-5** offline: `--offline` → `PI_OFFLINE=1` + `PI_SKIP_VERSION_CHECK=1`
  (main.ts:616); ONE seam — `modelNetworkEnabled = PI_OFFLINE === undefined`
  at ModelRuntime construction; every refreshModels honors
  `context.allowNetwork`; persisted overlay restores offline. Platform
  wiring (--offline per spawn) already correct.
- **U-6** trust: RPC-no-UI "ask" → DENY; bare `.pi/` needs no trust;
  `--approve/--no-approve` is the headless pin → hardened (below).
- **U-7** personas: no registry in settings.md (full key surface read);
  `subagent/` is an extension that spawns processes; prompt-level only.
- **U-8** command history: paged the commits API for both docs; diffed the
  `###`-heading command set across every truth-up/restructure — ZERO RPC
  commands removed or renamed ever; 34-command set stable #9898→v1.5.2; the
  one post-pin commit is doc-only. S6 fixture regime calibrated right.
- **U-9** hang: `isStreaming` stays true through a hang; no keepalive
  anywhere in the agent loop (grep-zero); get_state responsive during async
  hangs; supervisor-owned liveness (S2's wedge ladder) validated as
  necessary, not just sufficient.

### 2. The three decision records (design 0073 §2)

- **A (§1.4)**: process-per-session is THE policy; single stays behind the
  explicit switch. Decisive: U-2 (destructive switch semantics) + design
  0055 R4/D6 per-session busy truth + ~79 MiB/child recorded budget.
- **B (§1.5)**: no in-band ask/permission for pi workspaces; the container
  is the permission boundary (pi security.md's own framing); extension-owned
  ask is a scoped future story behind Capabilities().
- **C (§5.3)**: ratify `/workspace/pi-sessions` on the main PVC, runtime-uid
  supervisor-owned, no new subPath; RO-sidecar unaffected (bridge/control
  socket only).

### 3. e2e: the second harness drives the pod (S7 legs in s5-overlay-validation.sh)

- **S7.1a-c**: helm sidecar-enable upgrade → `spec.harness=pi` workspace →
  Active; PID1 = overlay agentd `supervise-pi`; `PiVerified=True`; sidecar
  init carries `AGENTD_HARNESS=pi`. Chart restored to the S5 topology after.
- **S7.2a-d**: bridge Basic-auth (`agentd.AuthUsername`="opencode" / OPENCODE_SERVER_PASSWORD read
  from PID1 environ): session create → 201 (REAL pinned-binary child from
  /pi); `get_state` round-trip (success + sessionId); PVC session file at
  `/workspace/pi-sessions/`; list + DELETE = unlink. Plus a per-child RSS
  step-summary reading (Ruling A cost evidence, weekly cadence).
- Structural pins added in `local/s5_kind_script_test.go`
  (TestS7SecondHarnessLegs_Pinned: leg inventory, sidecar enable/restore
  pairing, bridge-surface pins).

### 4. Hardening from U-6

`piChildCmd` now passes `--no-approve` on EVERY child spawn (cmd/workspace-agentd/pi_rpc.go):
the PVC cwd is user-controlled; the RPC no-UI fallback already denies, but
the flag makes the posture explicit and drift-proof against a future pi
change to the fallback. Pinned by TestPiChildCmdArgv; docs updated.

## Verification

- `go build ./...`, `go vet` clean; `make fmt-check` clean.
- Suites: `cmd/workspace-agentd` (TestPiChildCmdArgv + pi suites),
  `local` (S5 script + new S7 structural pins) — green.
- bash -n on the e2e script; structural test validates leg inventory.
- Kind e2e (workflow_dispatch on this branch head): see PR description +
  design 0073 §4 for the run URL and the S7 leg results (baseline stack run
  43373000078 @ 95f2c7f2f: all S5 legs green, cold-pull resume 17 s ≤ 120 s
  budget, gVisor leg PASS).

## Files touched

- `cmd/workspace-agentd/pi_rpc.go` — `--no-approve` in the spawn contract
- `cmd/workspace-agentd/pi_rpc_test.go` — argv pin
- `local/s5-overlay-validation.sh` — S7.1/S7.2 legs
- `local/s5_kind_script_test.go` — TestS7SecondHarnessLegs_Pinned
- `design/0073_2026-11-14_pi-validation-rulings.md` — new
- `design/evidence/pi-validation/` — new (9 evidence files + tarball listing + release checksums)
- `docs/operator/pi-supervisor.md` — spawn line + status + U-6 note
