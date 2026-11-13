# Worklog: pi S2 — the supervisor driver (`supervise-pi`)

**Date:** 2026-11-13
**Story:** tsk-arctic-brochure-shines (pi second harness, story split S2)
**Branch:** `pi-supervisor-s2` (off `origin/pi-overlay-s1`, which carries S1's
unmerged PR #2019; origin/main is an ancestor — merged before push)
**Status:** complete, review requested

---

## Objective

`supervise-pi`: the in-pod supervisor driver per the scoping report §1.2-§1.4
and §2.3 item 4 — spawn/verify the pinned binary in RPC mode, own the JSONL
pipes, derive agentd's statusz parity state from the event stream, honor the
rpc.md shutdown rules, and implement the §1.4 process-model ruling behind an
internal interface with the chosen policy's per-session memory cost measured.

## What landed

Five new files in `cmd/workspace-agentd/` (plus the dispatch hook in main.go):

1. **`pi_rpc.go`** — the id-correlated JSONL command client for one pi child:
   strict-LF framing via `bufio.Reader.ReadString` (NEVER `bufio.Scanner` — a
   real `agent_end` record carries the full message array and blows past the
   64 KiB cap; pinned by fixture test), four record families routed per
   rpc.md (commands on stdin; id-correlated `response`; session events;
   extension UI records), stdout read continuously (pi honors stdout
   backpressure — a stalled reader stalls pi), stderr drained as diagnostics
   only, stdin writes serialized with the OS pipe AS the backpressure
   mechanism, and a write that cannot drain within `piStdinWriteTimeout`
   (15s) classifies the child WEDGED (later calls fail fast). Shutdown is
   the rpc.md ladder: stdin EOF (waits for pending work) → SIGTERM → SIGKILL;
   the ladder succeeds when the process is gone (signal-death wait errors are
   expected outcomes, never returned).
2. **`pi_state.go`** — the event pump: per-session cells with THE SETTLE
   CONTRACT (`agent_start` marks busy; `agent_end` counts runs and NEVER
   settles — the pinned binary emitted three `agent_end` records under
   auto-retry with no settle while `get_state` kept `isStreaming:true`;
   `agent_settled` is the ONLY idle transition), #1685 message-level progress
   stamps (message_end user/assistant only), `get_state` poll reconciliation
   in BOTH directions, `get_session_stats` token/context folding, and
   `Snapshot()` rendering agentd's own parity shapes (`[]SessionInfo{ID,
   Title,Status,Tokens,Model,ContextUsed}` + `OldestBusySeconds`/`BusyAges`/
   `LastProgressSeconds`). All mutations under the aggregator mutex (a bare
   cell() hand-out raced Snapshot — caught by -race).
3. **`pi_policy.go`** — the §1.4 ruling made executable: `piProcessPolicy`
   (Attach/Release/Snapshot/ReconcileDir/HealthPoll/Shutdown) with exactly
   two production implementations — `processPerSessionPolicy` (THE ruling:
   one persisted child per session, spawned `--session-id <id>`, crash →
   managedProcess backoff curve → respawn-with-resume, `GenerationStart`
   voids the dead generation's busy episode per #1632) and
   `singleProcessPolicy` (the cost-bounded alternative: `--no-session`
   singleton + `switch_session`, typed `piErrBusySwitch` refusal while
   busy — the recorded limitation stated honestly). Session-dir scan (no
   list-sessions RPC exists): ids are everything after the FIRST `_`
   (timestamps never contain `_`, ids may); newest file wins.
4. **`pi_supervisor.go`** — the `supervise-pi` subcommand: self-verify (81)
   → pi overlay verify (85/86) → binary presence (missing = loud config
   error; pi has no PATH-lookup legacy), subreaper + orphan-reap loop, the
   health pass (get_state/get_session_stats corroboration each poll; wedge →
   restart without backoff penalty; busy-age NEVER auto-killed — surfaced
   for upstream D6 escalation only), and the signal contract: FIRST
   SIGTERM/SIGINT runs the graceful ladder with the configured grace;
   SECOND signal collapses every ladder to immediate SIGKILL ("a second
   signal exits at once"). #1808 writeback flush at exit.
5. **`docs/operator/pi-supervisor.md`** — the §1.4 ruling RECORD (S1's PR
   body explicitly recorded no ruling; this doc is it): process-per-session
   chosen because design-0055 session-state authority + the D6 inputs
   require per-session busy truth that single+switch_session structurally
   cannot observe; the measured cost (~78.8 MiB VmRSS / 89.5 VmHWM per
   idle child, real pinned v1.5.2 binary) is the accepted budget; the
   spawn shapes, settle contract, wedge semantics, and shutdown ladder are
   all written down with their upstream-doc provenance.

Tests (`pi_fake_test.go`, `pi_rpc_test.go`, `pi_state_test.go`,
`pi_policy_test.go`, `pi_supervisor_test.go`): a scripted RPC FIXTURE
subprocess (the TestHelperProcess re-exec pattern this package already
uses) whose wire shapes are the ones validated against the real pinned
binary — file-driven behavior knobs so one fake serves every scenario with
per-session variation. Pins: settle contract (unit + e2e), full statusz
parity field-by-field, >64 KiB records, stdin wedge classification +
fail-fast + health-poll restart, crash → respawn with same session-id +
resume (persisted transcript + argv across generations) + no-episode-
inheritance, per-session vs single spawn shapes, switch_session busy
refusal + newest-path resolution, session-dir scan (underscore ids, newest
wins, missing dir), shutdown ladder all three rungs (fixture subprocess),
and exec-level subcommand tests (real agentd binary: graceful exit 0,
repeat-signal exits inside the 30s grace, missing-binary loudness, env
parsing, policy validation). Everything race-clean.

## Decisions recorded

- **§1.4 ruling: process-per-session** (default; `LLMSAFESPACES_PI_POLICY`
  switch selects single-process). Rationale + measured cost in the operator
  doc. Design-0063 §6 ACP-spike criteria applied verbatim: the stdio-pipe
  driver is the bounded, fixture-gated change the spike assessed.
- **`--no-session` is NOT for platform sessions** (cli.md: in-memory,
  not persisted) — the charter's spawn line `--no-session --session-dir`
  was contradictory for persisted sessions; per-session children spawn
  WITHOUT `--no-session` and WITH `--session-id`, preserving the §5
  resume-on-restart contract (validated: kill -9 + respawn with the same
  id resumes with messageCount and entries intact).
- **Wedge = mechanical evidence only** (stdin not draining). Busy-age is
  an escalation input, never a kill trigger — mirrors the agentz contract
  where verdicts need evidence, and the D6 rule "escalates, never executes".
- **Restart-reason markers / crash attribution NOT wired** — the marker
  path is opencode-mode env-shaped; pi attribution rides S5's plumbing.

## Validated against the real binary (previous claimant's groundwork,
relied upon with provenance in the operator doc)

- Idle RPC child RSS ≈ 78.8 MiB (the ruling's number).
- Retry storm: three `agent_end` + `auto_retry_start`, NO `agent_settled`,
  `get_state.isStreaming` stayed true — the settle contract is real.
- Session events carry payload at the envelope TOP level; only responses
  nest under `data`.
- Graceful shutdown: stdin EOF → clean exit; rpc.md #11752/#11812 semantics.

## Verification

- `go test ./cmd/workspace-agentd/ -count=1` — full package green
  (392s), including all new pi suites.
- `go test -race` over every pi test — clean.
- `gofmt` clean; golangci-lint run recorded below.

## Review trail

- (r1 pending — moved to in_review with the PR.)
