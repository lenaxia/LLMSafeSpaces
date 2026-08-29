# Worklog NNNN — opencode overlay delivery S1, supervisor half (design 0053 §4.2)

**Date:** 2026-08-28
**PR:** (this PR)
**Branch:** feat/opencode-delivery-s1

## What

Implements the agentd/supervisor half of design 0053 S1 "opencodeDelivery":
verify-before-spawn + overlay binary resolution for the opencode child. When
the controller stamps `OPENCODE_IMAGE_VOLUME=1` (digest-pinned image volume
mounted read-only at `/opencode`), the supervisor verifies the opencode
binary's sha256 against the arch pin BEFORE the first spawn and execs the
overlay path directly instead of the PATH lookup. Marker unset/other →
legacy PATH lookup, byte-identical to today (pinned by function-pointer +
argv equality tests). Failure exits with attribution codes so the
controller can distinguish opencode-overlay failures from agentd failures:
83 = verify failed (missing arch pin, hash mismatch, unreadable/unhashable
binary), 84 = overlay missing. Reason to stderr + best-effort
`/dev/termination-log`. The managed process never starts on failure — the
supervisor exit is the signal (no opencode crash-loop).

## Components

- **`cmd/workspace-agentd/opencode_overlay.go`** (new): env contract
  constants, typed failure decision (`opencodeOverlayDecision` — pure,
  table-tested), binary-path resolution (env override →
  `/opencode/usr/local/bin/opencode` default), `resolveOpencodeSpawnFactory`
  (stat + stream-hash + decide), `opencodeSpawnBaseFactory` (the single
  seam; reports and exits 83/84 on failure), `writeTerminationLog`
  (best-effort; never masks the exit code).
- **`managed_process.go`**: `start()`'s lazy factory default now resolves
  through `opencodeSpawnBaseFactory()` (single-container `--supervise`
  mode); `defaultOpencodeCmdFactory` and the new overlay factory share
  `opencodeServeCmd(bin)` so argv/stdio/env construction cannot drift —
  only argv[0] differs.
- **`supervise_opencode.go`**: `newSupervisorProcess` resolves the spawn
  base ONCE and hands the SAME factory to both the process and the
  control-socket adapter — a spawn-env push wraps the verified overlay
  base instead of regressing to PATH lookup (would have been the S1
  integration bug: first spawn from overlay, post-push restarts from PATH).
  Agentd self-verify's termination-log write now uses the shared helper.
- **Test infra**: `buildAgentdBinary` builds once per test process
  (`sync.Once`). 39 call sites each re-linking the same sources cost
  ~140s of wall time; the suite drops 318s → 150s, keeping the 300s gate
  honest (baseline before this change was already 292s).

## Assumptions → validation

1. Contract (env names, default path, 83/84, message shapes) matches the
   parallel controller-side delegation → **verified against their
   in-tree code** (`controller/internal/workspace/opencode_overlay.go`:
   same marker, `opencodeMountPath + "/usr/local/bin/opencode"`, 83/84;
   detection consumes `term.Message` — the termination-log text this code
   writes).
2. `node_arch` vocabulary is `runtime.GOARCH` (`amd64`/`arm64`), matching
   the pin env names — not the uname vocabulary supervise_selfverify uses
   → **stated here**; the controller does not parse the field (uses the
   message verbatim), verified in their detection code.
3. Overlay binary absent check precedes pin check (mirrors the bash
   `verify_and_select_agentd` order) → **locked by table test**
   ("overlay missing precedes pin check").
4. Unreadable/unhashable binary → empty actual hash → guaranteed
   mismatch → 83 (mirrors supervise_selfverify's fail-closed) → **locked
   by table test** ("unreadable binary hashes empty: fail closed").
5. Hash-to-exec TOCTOU is closed by the read-only mount → **verified** in
   the controller's wiring (`ReadOnly: true` on the volume mount).
6. The overlay volume mount is read-only and digest-pinned, so no
   executable-bit check was added (contract lists exactly:
   missing-pin/mismatch/unhashable → 83, absent → 84) → **assumption
   documented**; a wrong-perm mount would surface as supervisor Start
   backoff, not a wrong binary executing.
7. `LLMSAFESPACES_TERMINATION_LOG_PATH` is a test-only seam (production
   pods always have `/dev/termination-log`); same override pattern as
   `LLMSAFESPACES_CONTROL_SOCKET_ADDR` → **verified** by the
   unwritable-log (EISDIR) subprocess tests.
8. Executable permissions/relative paths are not validated beyond
   existence — controller always sets an absolute path; a relative path
   fails closed via the stat check → **assumption documented**.

## Adversarial review (Phase 1→3)

- **SetSpawnEnv regressing to PATH lookup** (first-spawn-overlay vs
  restart-from-PATH divergence): real finding — the adapter's fallback
  factory was `defaultOpencodeCmdFactory`. Fixed structurally
  (`newSupervisorProcess` shares the resolved base with the adapter);
  pinned in-process (`TestNewSupervisorProcess_SharesOverlayBaseWithAdapter`)
  and e2e (`TestSuperviseOpencode_OpenCodeOverlayMatch_SpawnsOverlayNotPath`
  asserts every spawn's argv[0] — including the post-spawn_env restart —
  is the overlay path, on a PATH with no `opencode`).
- **Typed-nil error trap**: the first implementation returned
  `*opencodeOverlayError`; a literal nil pointer converts to a non-nil
  `error` interface. Disproved-by-red-test during TDD; API reshaped to
  `(code int, err error)` — no trap remains for future callers.
- **`exec.Command` PATH resolution**: cmd.Path for a bare name is
  environment-dependent (LookPath at construction); the byte-identical
  legacy pin asserts the factory function pointer + `Args[0]` instead.
  False alarm on the original `cmd.Path == "opencode"` assertion — test
  fixed, behavior unchanged.
- **Memoization vs t.Setenv**: a `sync.Once` seam would poison later
  legacy tests with an overlay factory (test-order dependence). Rejected;
  "verify once" is structural (resolution at construction; start()'s lazy
  default only when unset).
- **os.Exit inside the seam** skips caller defers (log.Sync): identical to
  the existing selfverify/`readAgentPassword` boot-exit precedents; the
  failure message goes to unbuffered stderr before exiting.
- **Env inheritance in subprocess tests**: this dev workspace itself runs
  under the AGENTD overlay (AGENTD_IMAGE_VOLUME=1 + pins inherited via
  `os.Environ()` would make the agentd self-verify exit 81 first).
  Filtered explicitly (`filteredEnviron`).

## Test evidence

- `go vet ./cmd/workspace-agentd/...` — clean.
- `go test -count=1 -timeout 300s ./cmd/workspace-agentd/...` — ok,
  150.3s (669 PASS/SKIP incl. 26 new tests, 0 FAIL).
- `go test -race` on the new in-process overlay tests — ok.
- `go build ./...` — clean (whole repo, incl. the parallel
  controller/helm delegation's in-tree work).

New tests (opencode_overlay_test.go): decision table (13 rows incl.
legacy/marker-other/match-both-arches/mismatch/missing-pin/unknown-arch/
missing-overlay/ordering/unreadable), exact message shapes, legacy
byte-identical resolution (pointer + argv + stdio + env parity), overlay
direct-exec + env parity, mismatch/missing/pin in-process, adapter base
sharing, single-container start() seam, and six real-subprocess legs
(83/84/no-pin/default-path/unwritable-log × 83+84/match-spawns-overlay
with socket restart) plus three re-exec legs for the `--supervise` path
(match spawns overlay; 83-before-spawn; 84-before-spawn).

## Deferred / notes for the validator

- `design/0053_*.md` was not present in the tree during implementation;
  the contract came from the task brief and was then cross-checked
  against the controller delegation's landed code (match confirmed).
- Constants stay local to cmd/workspace-agentd (mirrors the
  supervise_selfverify precedent; nothing outside the package consumes
  them — the controller keys on numeric codes and the termination-log
  text, both cross-checked).
- The `--supervise` e2e legs drive `startManagedProcess` via test-binary
  re-exec (the exact main() seam) rather than full `main()`, which
  requires `/sandbox-cfg/password` semantics owned by the boot-gate
  suite.
