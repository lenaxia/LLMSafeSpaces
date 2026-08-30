# Worklog: redact subcommand fold (design 0053 S2)

**Date:** 2026-08-29
**Session:** Implement S2 of design 0053 — fold `cmd/redact` into `workspace-agentd` as a subcommand, install the supervisor PATH wrapper, delete the standalone binary. Continues from the S2 work left uncommitted on local main (subcommand half) after S1 (#1126) merged.
**Status:** Complete

---

## Objective

Design 0053 S2: absorb the standalone `redact` binary into the agentd binary (one-file-one-hash artifact contract), preserve the documented `some-command | redact` UX via supervisor-written PATH wrapper, and remove the `cmd/redact` build from the platform.

---

## Work Completed

### `redact` subcommand (fold)

- `cmd/redact/main.go` deleted (was left deleted in the working tree from the prior session; committed here).
- `cmd/workspace-agentd/main.go`: `redact` dispatch + `runRedactCommand` — stdin → `pkg/redact` → stdout, `-config` flag defaulting to `/sandbox-cfg/redact-patterns.json`, exit 1 on config/stdin/redact/write failure. Behavior parity with the deleted binary (same flag, same default, same fail modes); errors now go through the zap logger (stderr — stdout stays clean for piping).
- 6 subcommand tests (`redact_subcommand_test.go`): built-in rules without config (missing file degrades to built-ins — verified `NewRedactorFromFile` semantics), custom pattern via config, malformed config → exit 1, invalid regex → exit 1, clean-text byte-identical passthrough, default config path leg.

### Supervisor PATH wrapper

- New `cmd/workspace-agentd/redact_wrapper.go`:
  - `redactWrapperPath()` — `/sandbox-runtime/bin/redact` default, `LLMSAFESPACES_REDACT_WRAPPER_PATH` override (same pattern as the pkg/agentd store-coordinate overrides).
  - `writeRedactWrapper(dir, agentdPath)` — atomic (temp + rename) 0755 `/bin/sh` script exec-ing the agentd binary with the `redact` subcommand; single-quote shell escaping (`shellSingleQuote`) so a hostile binary path cannot split argv or interpolate (`$`, spaces, embedded quotes covered by test).
  - `ensureRedactWrapper(logger)` — resolves `os.Executable()` (correct in both baked and #863 overlay modes — resolves to whatever binary is actually running), installs the wrapper, best-effort: Warn + boot continues (`ensureMiseShims` precedent, design 0051 D1).
  - `prependPathEnv(env, dir)` — non-mutating PATH prepend, no-op when already first.
- Wiring: `opencodeServeCmd` (the S1 single construction point both spawn modes share) prepends `/sandbox-runtime/bin` to the child PATH; `ensureRedactWrapper` called at supervisor boot in both modes — main.go (after subcommand dispatch → sidecar mode unreachable by construction) and `runSuperviseOpencodeCommand` (next to `ensureMiseShims`).
- 10 wrapper tests (`redact_wrapper_test.go`): content + exec bit + shebang, hostile-path quoting, idempotent rewrite with no temp residue, nested dir creation, best-effort on unwritable path (no panic/exit), env override, PATH prepend/add/already-first, factory PATH inclusion.

### Base image

- `runtimes/base/Dockerfile`: `redact-builder` stage deleted; `/usr/local/bin/redact` is now a 4-line wrapper script (`runtimes/base/tools/redact-wrapper.sh`) exec-ing `workspace-agentd redact`. Zero bytes of a second platform executable; smoke-test `which redact` and the documented UX keep working. This baked wrapper is the pre-S3 bridge and dies with the base strip.

### Docs

- `docs/reference/cli.md` redact section: subcommand + two wrapper paths, no standalone binary; fail-mode wording updated.
- `docs/operator/runtime-environments.md`: redact row (subcommand + wrappers), stale entrypoint description fixed (it execs the agentd supervisor, it does not pipe through redact or exec opencode itself — pre-existing inaccuracy in the artifact's doc surface), custom-image requirement + supply-chain table wording.

---

## Key Decisions

- **Wrapper execs `os.Executable()`, not a hardcoded overlay path.** Design 0053 §4.1 sketches `exec /agentd/usr/local/bin/workspace-agentd redact`; resolving the running binary at install time is correct in both delivery modes (baked pre-S3, overlay after) with no new configuration. Faithful to intent (this binary, redact subcommand).
- **Best-effort wrapper install (Warn, never blocks boot).** The wrapper preserves UX; it is not pod-contract load-bearing. Mirrors the `ensureMiseShims` precedent; pre-S3 the baked `/usr/local/bin/redact` wrapper is the fallback, and PATH ordering makes the supervisor's fresh wrapper win when both exist.
- **Baked `/usr/local/bin/redact` wrapper kept in base until S3** rather than dropping the path now — smoke-test (`which redact`) and every existing PATH consumer keep working through the transition; S3 deletes it with the entrypoints.
- **PATH prepend lives in `opencodeServeCmd`** (S1's shared seam), so legacy PATH-lookup spawn and overlay direct-exec spawn cannot drift.

## Assumptions validated

- "No live runtime consumer pipes through the standalone redact binary" — grep of entrypoints/agentd/supervise code: entrypoint-opencode.sh execs the agentd supervisor; no `redact` invocation anywhere in runtimes/ shell code; consumers are the Dockerfile build, smoke-test `which redact`, and docs only. (Design 0053's table said the same; re-verified against the merged tree.)
- "Both spawn modes share one env-construction point" — verified post-S1: `opencodeServeCmd(bin)` in `managed_process.go` is used by `defaultOpencodeCmdFactory` and `opencodeSpawnBaseFactory`.
- "Missing config file is not an error" — `pkg/redact/redact.go:93-97` degrades to built-in rules on `os.ErrNotExist`; subcommand parity confirmed by test.

---

## Blockers

None. (golangci-lint not installed in this sandbox — `make lint` unavailable locally; `go vet` + `gofmt` clean, CI lint gate covers the PR.)

---

## Tests Run

- `go test -timeout 300s -count=1 -run 'TestWriteRedactWrapper|TestEnsureRedact|TestRedactWrapperPath|TestPrependPathEnv|TestDefaultOpencodeCmdFactory_PathIncludes|TestRunRedactCommand' ./cmd/workspace-agentd/` — 16/16 PASS (verified failing-first: `undefined: writeRedactWrapper` before implementation).
- `go test -timeout 600s -race -count=1 ./cmd/workspace-agentd/` — ok (178.8s, full package, race on).
- `go build ./...` — clean.
- `go vet ./cmd/workspace-agentd/` — clean.
- `bash -n runtimes/base/tools/redact-wrapper.sh` — clean.

---

## Next Steps

- Land this PR, then S3 (base strip + pod env): delete entrypoints, baked wrapper, ENV block; controller injects env incl. PATH composition; pins become mandatory. S4 (factory: drop ENTRYPOINT, delete MinBaseVersion, catalog reseed) must land with S3.
- S3 should also split smoke tests per design §4.3 (platform-contract assertions — incl. the redact pipe — move to the agentd artifact's own CI; consider an exec-level `workspace-agentd redact` golden test there).
- S1 remainder: standalone opencode artifact image + CI per-arch stamping (was blocked on #1118, now merged).

---

## Files Modified

- `cmd/redact/main.go` — deleted
- `cmd/workspace-agentd/main.go` — redact dispatch + subcommand + boot wiring
- `cmd/workspace-agentd/redact_wrapper.go` — new (wrapper + PATH helpers)
- `cmd/workspace-agentd/redact_wrapper_test.go` — new (10 tests)
- `cmd/workspace-agentd/redact_subcommand_test.go` — new (6 tests)
- `cmd/workspace-agentd/managed_process.go` — PATH prepend in `opencodeServeCmd` + import
- `cmd/workspace-agentd/supervise_opencode.go` — `ensureRedactWrapper` call
- `runtimes/base/Dockerfile` — redact-builder stage removed; wrapper COPY
- `runtimes/base/tools/redact-wrapper.sh` — new (baked interim wrapper)
- `docs/reference/cli.md` — redact section
- `docs/operator/runtime-environments.md` — redact/entrypoint/custom-image/supply-chain wording
