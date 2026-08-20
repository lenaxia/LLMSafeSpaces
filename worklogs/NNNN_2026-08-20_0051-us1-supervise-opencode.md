# Worklog: design 0051 US-1 — supervise-opencode + Appendix-A control socket

**Date:** 2026-08-20
**Session:** Phase 2 begins. US-0 spec merged (#968); US-1 delivers the supervisor role against the existing image + the 4099 control socket, TDD per Appendix A.6.
**Status:** Complete

---

## Objective

US-1 per the phasing: extract `supervise-opencode` (1:1 from managed_process.go) + the control socket per the freshly approved spec. Same image, new mode — the container split is US-2.

## Work Completed

- **`control_socket.go`** (new): Appendix A v1 wire implementation — `{v,id,method,params}` / result|error, one-JSON-per-connection; closed method set (hello/status/restart/spawn_env/metrics); `version_unsupported` / `method_unknown` / `bad_request` / closed restart-reason enum; **A.4 invariants enforced in code**: no method returns env values (spawn_env write-only), restart rejects argv and unknown reasons as bad_request (not ignored-fields — they are capabilities, not additive fields).
- **A.3 amendment discovered by the tests (design note lands with this PR)**: the spec's "single-threaded request handling" head-of-line-blocks — a slow restart (seconds of child teardown) would freeze every status/hello poll, contradicting the same section's idempotency requirement. Implementation: handler-per-connection goroutines; restarts serialized by `restartMu` with `TryLock` giving the in_progress response. The blocked-restart test proves the second connection answers while the first is mid-teardown.
- **`supervise_opencode.go`** (new): the subcommand — subreaper duties (#904/#908 unchanged), managedProcess 1:1, control socket on 127.0.0.1:4099, SIGTERM shutdown. **`managedProcAdapter`**: socket vocabulary → existing semantics (reason-logged restart, state readout, memory-only spawn env installed as the next factory). Supervisor scope invariant in the file header.
- **Tests (TDD — wire tests written first, red on missing server)**: 9 control-socket tests = the A.6 targets verbatim (golden shapes, version/method/malformed rejections, unknown-field tolerance, blocked-restart idempotency with a hang-open fake, spawn-env memory-only + last-write-wins, negative capability: no env out / no argv in / closed reason enum); 4 adapter tests (state, factory env handoff, last-write-wins, no-block-on-unstarted-restart).

## Key Decisions

1. **Adapter, not fork**: managedProcess is reused 1:1; the adapter owns only the A.2 spawn-env memory store. No supervision logic duplicated.
2. **grace_seconds carried but not yet mapped** — restart()'s SIGTERM→SIGKILL window is a hardcoded 5s; honoring longer graces becomes a parameter when US-2 wires it. Socket contract already speaks it (forward-compat by design).
3. **metrics returns the reserved envelope in v1** — the cgroup read is the supervisor's, but the field set is fixed by US-1's integration tests per A.2; filling it rides with US-2's real-container wiring.

## Tests Run

`go test ./cmd/workspace-agentd/ -run 'TestControlSocket|TestManagedProcAdapter'` — 13 pass. Full suite (267s, incl. script/e2e) — green. `golangci-lint --new-from-merge-base` — 0 issues.

## Next Steps

- US-2: sidecar container + flag split (`--sidecar`); grace_seconds wiring; metrics field set; native-sidecar start-order (the #857 stamp-before-read guarantee).
- A.3 amendment text folded into the design doc (concurrency model) — landed with this PR's description; formal doc edit rides US-2.

## Files Modified

- `cmd/workspace-agentd/control_socket.go` (new)
- `cmd/workspace-agentd/control_socket_test.go` (new), `control_socket_helpers_test.go` (new)
- `cmd/workspace-agentd/supervise_opencode.go` (new), `supervise_opencode_test.go` (new)
- `cmd/workspace-agentd/main.go` (subcommand dispatch)
- `worklogs/NNNN_2026-08-20_0051-us1-supervise-opencode.md` (this file)
