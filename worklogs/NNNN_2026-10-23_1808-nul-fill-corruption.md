# Worklog: #1808 — sandbox filesystem zero-fill corruption (second class after #1793)

**Date:** 2026-10-23
**Session:** lane-1808 (P1 data-integrity), branch `fix/1808-nul-fill-corruption`.
**Status:** Complete — PR iterating review.

---

## Objective

Issue #1808: five managed files in a workspace clone were silently
replaced with NUL bytes (length preserved) mid-session and reached the
remote via a pushed commit. Second sandbox-filesystem corruption class
after #1793 (SSH trailing spaces). Deliver the #1793-analogous belt at
the seams the platform controls, per the design offered in the issue's
read-only analysis (2026-10-21) which the 2026-10-23 triage marked
"fix unclaimed".

## Root-cause position (honest boundary)

The issue's verified analysis holds (re-validated against
main @ ba3bc7bf): no platform writer is in the corrupted files' path
(platform-init enumerated paths; uploads tmp+fsync+rename O_EXCL;
materializer on tmpfs; disk-recover writes nothing; controller never
execs into pods). The zero-fill enters below the file API:
ext4 delayed-allocation loss on an unclean stop (H1) or Longhorn
replica divergence (H2) are the ranked candidates; discriminating
them needs cluster-side evidence (Longhorn volume events, node dmesg)
not reachable from a workspace pod. This lane does NOT claim a single
storage-layer root cause; it closes every platform-controllable gap:

1. **Detection** — `pkg/fsaudit` + `POST /v1/fs-audit` (issue Ask #2).
2. **Propagation block** — pre-push template hook (the "before
   `git push`" half of Ask #2): the remote is the last uncorrupted
   copy; the belt keeps it that way.
3. **Flush discipline** — bounded `sync()` on the platform's own
   shutdown paths (found missing): the H1 window is dirty pages at
   unclean stop; the suspend path deletes the pod and never flushed.

## Red-first record (honest)

New-package red = build failure against not-yet-existing seams
(quoted in full in the red commit `dc0d9b0d` message):

```
pkg/fsaudit: undefined: NewEngine, Request, Report, ErrBusy,
  VerdictNulFilled, MaxListEntries, allocatedBytes,
  DefaultMaxFileBytes, DefaultMaxTotalBytes, ChunkSize —
  'FAIL github.com/lenaxia/llmsafespaces/pkg/fsaudit [build failed]'
cmd/workspace-agentd: undefined: fsAuditHandler, socketFsAudit,
  fsAuditControlMethod, fsAudit field, FsAudit client method,
  flushSync/shutdownFlushWait/flushWorkspaceWrites — build failed
```

Real discrimination is carried by the mutation-verify recipes (below),
per the orchestrator's gate: every pin names the mutation that
reddens it.

## What landed

- `pkg/fsaudit` — engine + types. Signature: size>0 AND 100% NUL AND
  non-sparse. Verdicts clean/sparse/nul_filled/suspect/skipped/error.
  Root grounding (Clean("/"+root) — no root can escape the base).
  Budgets: 64MiB/file, 256MiB/total, chunk 4096, lists capped at 1000
  with exact counts. Zero writes. `st_blocks*512` sparseness via
  syscall.Stat_t (posix fallback: assume non-sparse — fail toward
  flagging, never hiding).
- `cmd/workspace-agentd/fs_audit.go` — `POST /v1/fs-audit` user-mux
  route (§D1 carve-out pair, bounded 1KiB body, empty-body-valid),
  `fs_audit` control-socket method (params explicit-over-implicit,
  busy/timeout error-code enum), `FsAudit` control client with typed
  sentinel mapping. Wiring: serverDeps.fsAudit + userMux (server.go),
  in-process engine (main.go), sidecar forwarder (sidecar_mode.go),
  supervisor engine (supervise_opencode.go).
- `cmd/workspace-agentd/shutdown_sync.go` — flushWorkspaceWrites:
  seam-var `flushSync` (syscall.Sync), 10s bounded wait, called from
  runShutdown (single-container) and the supervisor shutdown path.
- `api/internal/handlers/fs_audit.go` — owner-authz facade, Active
  phase gate, typed relay, nil-list hardening, plain-text error
  mapping (the disk-recover r1 F1 lesson). Router + app wiring.
- `sdks/openapi.yaml` — `/workspaces/{id}/fs-audit` + FsAuditRequest/
  FsAuditReport/FsAuditFileReport schemas (router contract test
  green).
- `runtimes/base/git-templates/hooks/pre-push` + Dockerfile COPY into
  git's template dir — the never-worse guard (fail-closed on positive
  all-NUL blob detection, fail-open on scanner error,
  LLMSAFESPACE_PREPUSH_AUDIT=0 opt-out). Pinned in
  `pkg/fsaudit/prepush_parity_test.go` incl. REAL git push behavior
  (blocks a dead-blob push, passes a clean push) and template-dir
  activation.

## Test evidence

- `go test ./pkg/fsaudit/` — 18 pins green (incl. 1001-file list-cap
  fixture in non-short).
- `go test ./cmd/workspace-agentd/` — FULL suite green (361s), incl.
  the fs-audit/control-leg/shutdown pins.
- `go test ./api/internal/handlers/ ./api/internal/server/
  ./api/internal/app/` — green (contract test updated with the
  FsAuditHandler fixture).
- Hook pins run real `git push` against a **hermetic local bare-repo
  fixture** (`pushFixture` in `pkg/fsaudit/prepush_parity_test.go`):
  work tree + bare remote both under `t.TempDir()`, pushes via
  `file://<tmpdir>/remote.git`, hook installed through
  `-c init.templateDir=<tmpdir>/templates` — the image's exact
  activation path. No network, no origin writes, no shared state.

## CI round 1 (e54da67a) — findings + remediation

- `Test (-short, with coverage)` FAILURE: **real race, mine** — the
  abandoned flush goroutine read the `flushSync` package var with no
  happens-before edge against the test-cleanup restore (race detector
  is CI-only; my local run didn't use `-race`). Fix in bfb72750: seam
  snapshotted on the caller's goroutine before spawning; the bound
  test's parked spy goroutine released post-assertion. Verified
  `-race -count=2` clean.
- `pkg/secrets integration (Postgres + Redis)` FAILURE: NOT this
  lane — the failing tests are `pkg/board` TestBoardSuite
  (TestPendingExit_AgentManagerWorkspaceBindingHolds,
  TestRearm_ReplayReturnsOriginalOutcome, TestReviewAuto_ReplayIsInert)
  from today's #1832 board merges (#1868/#1869/#1870/#1872); this
  diff touches no board/secrets code. Cross-lane, flagged to the
  orchestrator.
- All other gates green on e54da67a (lint, kind posture, gitleaks,
  govulncheck, trivy, buf/ABI, builds, docs-only detect).

## Mutation-verify recipes (for the orchestrator's gate)

Each recipe: named pin + the mutation that reddens it.

1. **Signature/non-sparse** — mutate `pkg/fsaudit/engine.go`
   `classify`: drop the `fr.AllocatedBytes >= size` condition (flag
   all all-NUL as nul_filled) → `TestAudit_SparseAllNulFileNotFlagged`
   RED (sparse file flagged).
2. **Signature/100%-NUL** — mutate `classify` to set `allNul = false`
   unconditionally (never flag) → `TestAudit_NulFilledFileFlagged`
   RED.
3. **Early exit** — mutate the read loop to ignore the non-NUL break
   (read to limit always) → `TestAudit_CleanFileEarlyExit` RED
   (BytesRead ≥ 1MiB).
4. **Per-file cap honesty** — mutate the suspect branch to emit
   nul_filled → `TestAudit_PerFileCapYieldsSuspectNotConfirmed` RED.
5. **Total budget truncation** — mutate the budget check to never set
   Truncated → `TestAudit_TotalBudgetTruncates` RED.
6. **Root grounding** — mutate `resolveRoot` to
   `return filepath.Clean(root)` (no grounding) →
   `TestAudit_RootCannotEscapeBase` RED (../../.. escapes the base).
7. **Busy serialization** — delete the TryLock →
   `TestAudit_BusySerialization` RED (no ErrBusy).
8. **Route auth gate** — mutate `fsAuditHandler` to drop
   `checkBasicAuthAny` → `TestFsAuditHandlerAuthGate` RED.
9. **Nil engine 503** — mutate the nil check to fall through →
   `TestFsAuditHandlerNilEngine` RED (nil-panic or 500).
10. **Error-code enum** — mutate `FsAudit` client to return raw err on
    busy → `TestControlClientFsAuditErrorMapping` RED (not ErrBusy).
11. **Sync seam bypass** — delete `flushWorkspaceWritesLogged()` from
    `runShutdown` → `TestRunShutdown_FlushesBeforeExit` RED.
12. **Sync bound** — mutate `flushWorkspaceWrites` to `<-done` only
    (unbounded wait) → `TestFlushWorkspaceWrites_BoundedWait` RED
    (returns after 5s bound fails the <5s assertion... precisely: the
    blocker never completes, test asserts elapsed <= 5s — unbounded
    hangs past it).
13. **Hook fail-closed direction** — mutate the hook's block branch to
    `status=0` (never refuse) → `TestPrePushHook_BlocksAllNulBlobPush`
    RED (push succeeds).
14. **Hook fail-open direction** — mutate `command -v git` line to
    `exit 1` → `TestPrePushHook_FailsOpenWhenGitUnavailable` RED.
15. **Hook kill-switch** — mutate the env default to `0` (guard off by
    default) → `TestPrePushHook_BlocksAllNulBlobPush` RED (guard
    inert).
16. **Template wiring** — delete the Dockerfile COPY →
    `TestPrePushHook_DockerfileTemplateWiring` RED.

## PG-gated / environment disclosures

- No schema migrations (confirmed against the dispatch: none needed).
- No PG-gated pins in this lane; the engine/route/facade suites are
  all hermetic. The agentd full suite (361s) is the heaviest run.

## Follow-ups (not this lane)

- Longhorn/node evidence plan (issue §4 items 4-6) — cluster-side;
  decides H1 vs H2 and where the durable storage-layer fix lives.
- statusz last-audit summary + MCP tool surface for the audit (the
  agent-in-session ergonomics) — small, separate.
- Existing clones in pre-image workspaces have no hook (documented;
  new-clone-forward).
