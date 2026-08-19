# Worklog: agentd fail-closed boot — admin token required (D5.2), empty-password reject (D5.3)

**Date:** 2026-08-19
**Session:** Implement design 0051 Phase-1 remainder (D5.2/D5.3) + the review-mandated integration boot tests, on top of the distinct-token delivery (#933).
**Status:** Complete

---

## Objective

Two fail-open paths in agentd boot: (1) an unset/empty admin-mux token silently DISABLED the `:4098` bearer gate (requireBearerToken early-return) — a wiring gap invisible to every probe; (2) a readable-but-EMPTY `/sandbox-cfg/password` armed the guessable Basic credential `b3BlbmNvZGU6` on every gated user-mux endpoint (the #886-review-flagged case; controller-generated secrets are 32 random chars but operator-created ones are adopted as-is).

---

## Work Completed

- **D5.2** `resolveAdminTokenForBoot()` (admin_token_file.go): resolves the token file→env→none; no token → boot error naming both delivery modes and the escape hatch. `AGENTD_ALLOW_NO_ADMIN_TOKEN=1` is the explicit dev/kind escape (yields `""` deliberately — the mux then serves tokenless, matching the documented dev posture, now by choice rather than accident). Wired in `main()` after the password read (G46 ordering preserved: missing password still fails with the G46 fatal first).
- **TOCTOU closure (#934 review note)**: the resolver runs ONCE at boot; `serverDeps.resolvedAdminToken` (mux wrap) and `managedProcess.adminToken` (health probe) read the boot value — no file/env re-read can diverge from what the gate verified.
- **D5.3** `readAgentPasswordFromPath`: whitespace-only password → error (same fatal class as G46).
- **Integration boot tests** (boot_gate_integration_test.go — run the REAL binary via the existing `buildAgentdBinary` helper; skip cleanly where the hardcoded `/sandbox-cfg` is unwritable, e.g. this live workspace pod; CI runners exercise them):
  - `NoAdminToken_FatalAfterPassword` — exit 1 + "admin token required", and NOT the G46 message (pins gate ordering)
  - `MissingPassword_G46FiresFirst` — pins the password gate precedes the token gate
  - `EmptyPassword_FatalEvenWithEscapeHatch` — the escape hatch must not bypass password gates
  - `FileToken_PassesGate` / `EscapeHatch_BootsTokenless` — process survives past the gate window (narrow-hatch pin: absence fails, credential/hatch proceed)
- Unit tests: `TestResolveAdminTokenForBoot_{MissingIsFatal, EnvTokenOK, FileTokenOK, EmptyFileIsFatal, ExplicitEscapeHatch}` (now also asserting the returned token — the threading contract), `TestReadAgentPasswordFromPath_EmptyFileIsError`.
- `local/test-entrypoint.sh`: exports a token for the harness boot check (bare tokenless boot is dead after D5.2).

## Key Decisions

1. **Gate order = password first, then token.** G46 (password) is the older, harder invariant; its fatal must keep firing even when both are misconfigured, so ordering is pinned by an integration test.
2. **Escape hatch yields "" deliberately** rather than erroring at the mux: kind/dev clusters genuinely run tokenless agentd; the hatch makes that an explicit operator choice, logged by its absence of token, instead of an accidental fail-open.
3. **Subprocess tests skip, not fake.** The password path is a hardcoded production constant; where it can't be staged (read-only /sandbox-cfg) the tests skip with the reason — CI's writable emptyDir is the real target.

---

## Blockers

None.

---

## Tests Run

- `go test ./cmd/workspace-agentd/ -run 'TestResolveAdminTokenForBoot|TestReadAgentPasswordFromPath|TestBuildEnvFrom|TestResolveAdminToken|TestCredentialScript'` — pass (integration boot tests SKIP locally with reasons; CI executes)
- Full `./cmd/workspace-agentd/` suite green on the base branch (#933 rebase).

---

## Next Steps

- Design 0051 Phase 2 (uid tiers) once #932 merges and #863's legs are validated.
- Live validation on TEST: rebuilt pod boots with file token; a token-stripped pod CrashLoops with the fatal log.

---

## Files Modified

- `cmd/workspace-agentd/admin_token_file.go` (resolver + D5.2)
- `cmd/workspace-agentd/admin_token_file_test.go` (resolver unit tests, threading assertions)
- `cmd/workspace-agentd/main.go` (D5.2 gate wiring, D5.3 empty-password fatal)
- `cmd/workspace-agentd/main_test.go` (D5.3 unit test)
- `cmd/workspace-agentd/boot_gate_integration_test.go` (D5.2/D5.3/escape-hatch integration tests)
- `local/test-entrypoint.sh`
- `worklogs/0802_2026-08-19_agentd-fail-closed-boot.md` (this file)
