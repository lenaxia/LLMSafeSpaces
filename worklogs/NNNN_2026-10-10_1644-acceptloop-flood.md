# Worklog NNNN — #1644 accept-loop warn-flood fix (PR1)

Branch: `fix/1644-acceptloop-flood` (from main @ f9a74a19)

## Incident class

CI job killer (#1644): `controlSocketServer.serve()` retried `Accept()`
forever with one WARN per iteration whenever the listener was closed by
any path other than `close()` (the only path setting the `closed` flag).
Jobs died exit-1 with ~1M-line `use of closed network connection` logs
and zero test failures (runs 37733161509, 37989366061; third signature
in #1637 run-3 tail). A prior automated attempt announced a fix
(`feat/issue-1644-control-socket-accept-errclosed`, commit 99003858) but
its push failed (`fatal: could not read Username`) — no remote branch,
no PR; this lane re-does the work from main.

## Red-first pin run (measured, unfixed tree)

`go test ./cmd/workspace-agentd/ -run 'TestControlSocket_AcceptLoop' -v -count=1`

- `ExitsOnExternalListenerClose` — **FAIL at 2.00s**: "accept loop still
  running 2s after the listener was closed — the #1644 warn-flood shape"
  (the loop spins forever on old code; cleanup's `close()` stops the
  leak so the red run terminates cleanly).
- `ExitsOnDeliberateCloseUnderDialTraffic` — PASS (0.05s): expected on
  old code; this is the regression guard for the orderly path, not the
  killer pin.
- `SurvivesTransientErrorsAndExitsOnClose` — **FAIL at 2.01s** on the
  exit half (warn+survive halves passed on old code).

3 tests ran, 2 red / 1 guard-green — pattern verified by name, not
vacuously green.

## Fix

`control_socket.go serve()`: `errors.Is(err, net.ErrClosed)` → one
debug-level line → return. Non-ErrClosed errors keep the warn+retry
path (EMFILE-class transient failures can self-heal; killing the loop
on them would break recovery — pinned by the scope guard). Debug (not
info/warn) so teardown of the suite's many socket servers stays silent
at default level; the ≤1-warning assertions pin it either way.

## Green run (measured, fixed tree)

Same command: 3/3 PASS — 0.00s / 0.05s / 0.01s (killer pin 2.00s →
0.00s; the exit is now the loop's next iteration).

Full CI shape: `go test ./cmd/workspace-agentd/ -race -count=1` →
**ok, 381.488s**, no flood, no flakes. `go vet` + `golangci-lint run
cmd/workspace-agentd/...` → 0 issues.

## Notes

- The `closed`-flag check remains as defense for non-ErrClosed errors
  after deliberate close; ErrClosed is checked first because stdlib
  semantics make it terminal for a TCP listener (post-Close Accept
  returns ErrClosed permanently).
- PR2 (the #1532 flake deadline restructures) lands separately on its
  own branch.
