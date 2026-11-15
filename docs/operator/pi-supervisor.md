# The pi supervisor (`supervise-pi`)

Status: S2 delivered (pi second-harness story split — scoping report
§1.2-§1.4, §2.3 item 4). This page records the **§1.4 process-model
ruling**, the spawn/verify/shutdown contracts the supervisor owns, and
the health-derivation semantics that replace opencode's HTTP+SSE
surface. The delivery trust ruling (hash-the-binary + digest-the-tree,
exit 85/86) lives in [pi-overlay.md](pi-overlay.md); harness SELECTION
between `supervise-opencode` and `supervise-pi` is S5's plumbing.

## What runs

`workspace-agentd supervise-pi` — PID 1 of the workspace container for
a pi workspace (same topology as supervise-opencode: subreaper, orphan
reap loop, #1808 writeback flush at shutdown). It spawns and owns the
JSONL pipes of one or more pi RPC-mode children:

```
pi --mode rpc [--offline] --session-dir <PVC> [--session-id <id>]
```

- `PI_CODING_AGENT_DIR` points at the writable agent-state tree
  (S3's config writer populates it); the session dir lives on the PVC
  (`LLMSAFESPACES_PI_SESSION_DIR`, default `/workspace/pi-sessions`) so
  sessions survive container restarts.
- **Per-session children are PERSISTED**: they spawn WITHOUT
  `--no-session` and WITH `--session-id <id>` — a crashed child
  respawns with the same id and RESUMES the persisted session
  (validated against the pinned binary: `messageCount` and
  `get_entries` content survive `kill -9` + respawn). `--no-session`
  (an in-MEMORY session, cli.md) is only ever used by the
  single-process policy's initial state — never for platform sessions.
- `--offline` is wired from `PI_OFFLINE=1` explicitly (scoping §8 R4:
  egress-restricted pods must not do startup catalog refreshes).
- Boot order: agentd self-verify (exit 81 contract) → pi overlay
  verify (exit 85/86, `pi_overlay.go`) → binary presence (a missing
  binary with no marker is a loud config error, exit 1 — pi has no
  PATH-lookup legacy, pins are mandatory from day one).

Environment knobs:

| Env | Default | Meaning |
|---|---|---|
| `LLMSAFESPACES_PI_SESSION_DIR` | `/workspace/pi-sessions` | persisted session dir (PVC) |
| `LLMSAFESPACES_PI_AGENT_DIR` | `/workspace/.local/pi` | `PI_CODING_AGENT_DIR` target |
| `LLMSAFESPACES_PI_POLICY` | `per-session` | §1.4 policy switch (below) |
| `LLMSAFESPACES_PI_POLL_INTERVAL_SECONDS` | `5` | health-pass period |
| `LLMSAFESPACES_PI_SHUTDOWN_GRACE_SECONDS` | `15` | pending-work grace before SIGTERM |
| `LLMSAFESPACES_PI_ATTACH_ON_BOOT` | — | bring-up/test knob: comma-separated session ids to attach at boot |

The S2 status surface is `Snapshot()` (in-process parity snapshot;
tests consume it). The HTTP statusz wiring is opencode-shaped and lands
with S5 — nothing else grows in the supervisor (the 0051 D1 scope
invariant, applied verbatim).

## The §1.4 ruling: process-per-session

pi RPC mode is a **single-conversation process**: `get_state`,
`prompt`, and the event stream all address THE active session, and a
busy session blocks `switch_session`. There is no list-sessions RPC at
all (sessions are found by scanning the session dir — the file name
shape is `<timestamp>_<session-id>.jsonl`, timestamps never contain
`_`, ids may).

Scoping §1.4 left two options; S2 records the ruling:

**RULING: process-per-session** (`LLMSAFESPACES_PI_POLICY=per-session`,
the default). One persisted child per platform session, spawned with
`--session-id`; statusz aggregates across children.

Why: the platform's session-state authority (design 0055 R4) and the
D6 escalation inputs (`OldestBusySeconds`/`BusyAges`/
`LastProgressSeconds`, #998/#1685) require per-session busy truth that
the alternative structurally cannot observe — under a single process,
"a busy session blocks switching and busy-state for 'other' sessions
is unobservable" (scoping §1.4). A per-session busy episode would be
invisible exactly when it matters.

The alternative — **single-process + `switch_session`** — stays
implemented behind the same internal interface
(`piProcessPolicy`: Attach/Release/Snapshot/ReconcileDir/
HealthPoll/Shutdown) as the cost-bounded fallback, selected per
workspace by S5's plumbing, never by accident. Its recorded limitation
is stated honestly in code: attaching a second session while the
active one is busy returns a typed refusal (`piErrBusySwitch`).

### The measured cost (the ruling's number)

Per-session means one pi process per session. Measured against the
REAL pinned binary (v1.5.2, linux-x64, sha256 `811c6f2c…` verified
against the release `SHA256SUMS`), an idle RPC-mode child:

- **VmRSS ≈ 78.8 MiB**, VmHWM ≈ 89.5 MiB, 8 threads
  (probe: `/proc/<pid>/status` after boot + settle)

That is the accepted budget: ~79 MiB per ACTIVE session (children
spawn on Attach and stop on Release; persisted-but-unattached sessions
render idle from the session-dir scan with zero process cost). For
scale, the opencode overlay precedent already accepts "~10× agentd"
(design 0053 §8); pi-per-session at ~79 MiB each is the same order.
The 2-CPU quota caveat (scoping §8 R2, `pod_builder.go:171`) is S7's
validation-epic measurement item.

## The settle contract (busy/idle derivation)

The event pump folds every child's stdout records into per-session
cells. The contract, validated against the pinned binary (which
emitted three `agent_end` records under auto-retry with NO settle
while `get_state` kept reporting `isStreaming:true`):

- `agent_start` marks **busy**.
- `agent_end` closes ONE low-level run and **explicitly does NOT mean
  idle** — automatic retry, compaction recovery, steering, or queued
  follow-up work can follow. It counts runs (diagnostic), never
  clears busy.
- **`agent_settled` is the ONLY idle transition** ("Pi will not
  continue automatically through retries, compaction recovery, or
  queued messages" — json.md).
- `get_state` polls corroborate and RECONCILE both directions (a
  restart gap can swallow `agent_settled`; a missed `agent_start`
  leaves the pump idle while pi streams) — `isStreaming` is
  authoritative when the two disagree.
- `message_end` (role user or assistant) stamps the #1685
  message-level progress clock; stream chunks, tool events, and
  `agent_end` do not. `LastProgressSeconds` is the age of that stamp
  on the longest-quiet BUSY session; 0 when unknown (fails toward
  silence — the D6 gate reads 0 as "no evidence", never "alert").

Parity targets are agentd's own shapes: `[]SessionInfo{ID, Title,
Status, Tokens, Model, ContextUsed}` and
`OldestBusySeconds`/`BusyAges`/`LastProgressSeconds`
(`pkg/agentd/types.go`).

## Wedge, kill, restart (#1632 agentz semantics, moved in-pod)

- **Wedge** is mechanical evidence only: a stdin write that cannot
  drain within its window (`piStdinWriteTimeout`, 15s) means the child
  stopped reading commands — the wedge classification. The health pass
  sees `Wedged()` and restarts the child WITHOUT backoff penalty (the
  restart is the remedy, not a crash). Subsequent commands fail fast
  ("pi child wedged") instead of piling onto a full pipe.
- **Busy-age is never auto-killed.** `OldestBusySeconds` et al. are
  escalation INPUTS for the upstream D6 flow ("the API escalates to
  the owner, never executes") — the supervisor surfaces, the platform
  decides.
- **Crash** → managedProcess's backoff curve (2s doubling to 30s,
  reset after 60s healthy) → respawn with the SAME `--session-id` →
  RESUME from the persisted session.
- **Generation contract**: every respawn runs `GenerationStart` — the
  busy episode dies with the dead child (`OldestBusySeconds` must not
  keep growing across a restart), while persisted facts (tokens,
  model, title, context) and the #1685 history stamp survive.

## Shutdown (rpc.md, commits #11752/#11812)

Per child, the ladder is: **stdin EOF** (pi disposes the runtime and
WAITS FOR PENDING WORK — an in-flight turn — before exiting; the
supervisor grants the configured grace) → **SIGTERM** → **SIGKILL**.

For the supervisor: the FIRST SIGTERM/SIGINT runs the graceful ladder
across all children; **a second signal exits at once** — ladders
collapse to immediate SIGKILL, pending work abandoned ("a stdin EOF or
a single signal does not interrupt it; a second signal exits at once"
— rpc.md). Shutdown waits are bounded; the #1808 writeback flush runs
last.

## Fixture provenance

The test suite (`pi_fake_test.go`) pins all of the above against a
scripted RPC fixture subprocess — the wire shapes it emits are the
ones validated against the real pinned binary: responses nest payload
under `data`; session events carry payload at the envelope TOP level;
records may exceed 64 KiB (a real `agent_end` carries the full message
array — the reader is `bufio.Reader`-based, never `bufio.Scanner`);
retry storms emit `agent_end` with no `agent_settled`.

## Out of scope (successor stories)

- S3: `AgentConfigWriter` for pi (models.json / mcp.json /
  SYSTEM.md rendering into the agent dir).
- S4: the API-server Adapter — prompt/steer/abort translation,
  `get_entries since` history, and the queued-input-survives abort
  flow (`clear_queue` before `abort`; rpc-commands.md).
- S5: harness selection (CRD field, controller delivery map, HTTP
  statusz wiring, supervisor subcommand dispatch from the pod spec).
- S6: golden JSONL fixtures + version-bump gate (the REFRESH.md
  regime).
