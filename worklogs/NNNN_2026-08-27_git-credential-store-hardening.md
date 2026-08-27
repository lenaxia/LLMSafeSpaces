# Worklog: Git credential store hardening (#1087)

**Date:** 2026-08-27
**Session:** Platform-side fix for issue #1087 — GitHub auth (GH_TOKEN env) lost on workspace suspend/resume; make git consume the materialized `git-credentials` file directly and gate the durable path with regression tests.
**Status:** Complete

---

## Objective

Issue #1087 documented a dev-session incident: GitHub auth provisioned solely as a `GH_TOKEN` env var evaporated across a workspace suspend→resume (env injection is not replayed), killing both `gh` and `git push` mid-conversation. The platform's durable path (bound `git-credential` secret → per-boot materialization to `/sandbox-runtime/rt/git-credentials` → `$HOME/.git-credentials` symlink) existed and worked, but (a) nothing made git *consume* the file without a `gh` shim + env token, and (b) no regression gate asserted the full boot sequence.

Platform-side scope (the session-spawner/harness env injection lives outside this repo — verified: zero `GH_TOKEN` references in production code):

1. Ship the credential-store helper in the runtime image so git reads the materialized file directly.
2. Regression gates: cold-boot/suspend-resume sequence test + image-build smoke checks.
3. Rule-5 cleanup of pre-existing breakage found in the touched area.

---

## Work Completed

### Runtime image: two-layer git credential consumption

`runtimes/base/Dockerfile`:
- `/etc/gitconfig` (system level): `[credential] helper = store --file=/home/sandbox/.git-credentials` (printf inline, following the `/etc/mise/config.toml` precedent).
- `ENV GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0="store --file=/home/sandbox/.git-credentials"`.

Why two layers (the critical finding of this session): **a system-level helper alone is defeated by `gh auth setup-git`** — that command writes an empty `helper =` reset plus the gh shim into the PVC-persisted `~/.gitconfig` (global level), and git processes helper entries in read order (system → global → local → command/env), so the reset clears the system entry for github.com. Validated empirically against git 2.39.5 (the bookworm series in the image): system-only → shim fires, no credential, prompt fails (the exact incident). Adding the env layer (`GIT_CONFIG_KEY_*`, applied *after* user global config) → shim fires and fails, git skips it, the store helper supplies the materialized token. Defense in depth: env can be scrubbed/clobbered by user tooling (then `/etc/gitconfig` serves); `/etc/gitconfig` is defeated by the setup-git reset (then env serves).

Placement: both layers precede the build-time smoke-test RUN so the gate exercises them. `ENV` reaches every process in the workspace container in both modes: legacy (agentd PID 1 spawns opencode with parent env) and sidecar (`spawn_env_consumer.go` composes parent `os.Environ()` + delta with parent-wins semantics; the sidecar agentd image is a separate container and unaffected).

### Regression gates

- `cmd/workspace-agentd/git_creds_boot_test.go` (Go, wired into `make test`): drives the REAL subcommands (`init-fs` then `materialize`, the pod's boot pair) against a PVC/tmpfs-shaped temp tree. Cold boot with a bound `git-credential` secret → file materializes, `$HOME/.git-credentials` resolves; simulated pod death (wipe tmpfs) → dangling symlink (the incident state); resume (same boot pair, same bound secret) → resolves again, PVC side still a symlink (US-35.7). Second test pins the degraded path: no git-credential bound → boot exits 0, ssh-key materializes, git symlink dangles (never a boot failure). Red-check performed: mutating the resume boot to drop the secret makes the gate fail at exactly the "must resolve — no dangling symlink" assertion, then reverted.
- `runtimes/base/tools/smoke-test.sh`: two build-time checks — `git config --file /etc/gitconfig --get-all credential.helper` contains the store entry (system layer) and `git config --show-origin --get-all credential.helper` contains a `command line:`-origin store entry (env layer; git labels `GIT_CONFIG_KEY_*` entries `command line:` — grepping the origin is what makes the check env-layer-specific, since the plain effective query would pass via `/etc/gitconfig` alone). Verified locally (no docker in the dev pod) that each check passes with its layer present and fails with it dropped.
- Trivy DS-0031 (PR round 1): the security-scan config job flagged `GIT_CONFIG_KEY_0` as a possible secret env (name-pattern rule). False positive — the value is the literal public git config key name — but a global `.trivyignore` suppress would disable DS-0031 for every Dockerfile repo-wide. Resolved with a surgical inline `# trivy:ignore:DS-0031` marker directly above the ENV instruction (trivy only honors the marker on the line immediately preceding the instruction — validated empirically with trivy 0.70.0, the CI-pinned version, using minimal Dockerfile fixtures plus the full CI-equivalent repo scan: clean, exit 0).
- `local/test-entrypoint.sh`: repaired (see below) and its full-secrets test now asserts the git-credential → `$HOME/.git-credentials` round trip through the symlink — the shell variant of the gate.

### Rule-5 cleanup (pre-existing errors in the touched area)

- `local/test-entrypoint.sh` was 6/9 failing: it still sed-patched the pre-Epic-17 **bash** materializer (stale `ENV_FILE=` seds, `/tmp/agent-config.json` assertions, `api_key` llm-provider fixture, `grep claude`), scoped `HOME` to a single command so post-checks read the outer home, and probed `/v1/healthz` on port 4097 (health endpoints moved to the admin port 4098 in US-22.8). Rewritten to drive the real `workspace-agentd materialize` subcommand via the documented `LLMSAFESPACES_*` env overrides against a PVC/tmpfs temp tree with an init-fs-style symlink farm; hermetic (`unset INFERENCE_RELAY_BASEURL`); always-fresh binary build; guarded `kill`. Now 7/7 green.
- Bare `workspace-agentd` (server-only, no `--supervise`) panicked with a nil deref at `main.go:182` — `startManagedProcess(false, …)` returns nil **by design** and every other consumer nil-guards (`server.go` typed-nil guards, `maybeStartRelayInjector`); the admin-token assignment was the one unconditional deref. Guarded it; the repaired shell test 3 (bare invocation → HTTP server responds) is its regression gate. Verified: panic gone, clean bind-conflict shutdown observed in the dev pod, 200 from healthz.
- `pkg/agentd/secrets/symlink_lifecycle_test.go` llm-provider fixture used `api_key`; the schema requires `apiKey` (`pkg/secrets/types.go:242`), so the entry silently *skipped* (skips aren't failures — the test passed without exercising the provider). Fixture corrected; provider now materializes.

---

## Key Decisions

1. **Env-layer (`GIT_CONFIG_COUNT/KEY_0`) in addition to `/etc/gitconfig`** — not the issue's literal one-liner, because the literal suggestion (system gitconfig only) provably does not achieve the issue's stated goal ("user-run `gh auth setup-git` can't couple git auth to the env"): the setup-git reset clears system-level helpers for github.com. The env layer is git's documented mechanism for config that lands after user global config. Both layers kept as defense in depth per repo doctrine (single mechanisms fail — see the relay remap guard rationale).
2. **Absolute `--file` path** (not bare `store`, which reads `~/.git-credentials`) — immune to `$HOME` drift across tooling.
3. **Gate at Go level + build level, not only the local shell script** — the shell harness is not CI-wired; `make test` and the image build are.
4. **No controller/API changes** — image-level wiring only; the controller's env composition (parent-wins) already preserves platform ENV into spawned children.
5. **Harness-side fix (bind the `git-credential` secret at dev-workspace creation) is out of repo** — documented in README-LLM.md as the recommended provisioning path instead.

---

## Assumptions (stated and validated per Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | git resolves credential helpers in config-read order; an empty `helper =` in a URL-specific global subsection clears prior (system) entries for matching URLs | Empirical, git 2.39.5: system store + global setup-git reset/shim → no credential (CASE A) |
| A2 | `GIT_CONFIG_KEY_*` env config applies after user global config and survives the reset | Empirical: same setup + env layer → token supplied (CASE B); `git config --show-origin` shows `command line:` |
| A3 | A dangling `~/.git-credentials` symlink is fail-open for `git credential fill` | Empirical: degrades to prompting (CASE C2), no crash |
| A4 | Bookworm git (image) supports `GIT_CONFIG_COUNT` (needs ≥2.31) | Bookworm ships 2.39.x; local validation on 2.39.5 |
| A5 | Dockerfile ENV reaches git in both pod modes | `spawn_env_consumer.go:208` parent+delta composition, parent wins; legacy mode spawns from agentd's own environ (README boot sequence) |
| A6 | `GH_TOKEN` injection lives outside this repo (issue claim) | `grep -rn GH_TOKEN` — only test fixtures |
| A7 | The materialize/init-fs boot pair restores a bound git-credential on a fresh pod (pre-existing behavior to gate, not new code) | New gate test passes unmodified against `main` + incident forensics in the issue |
| A8 | `/v1/healthz` lives on the admin port 4098 | `server.go:392` + `pkg/agentd/types.go:79` |

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 400s -count=1 ./cmd/workspace-agentd/` — ok (303s; includes the two new gate tests).
- `go test -timeout 400s -count=1 ./pkg/agentd/secrets/` — ok.
- `bash local/test-entrypoint.sh` — 7/7 passed, exit 0 (was 3/9, exit 1).
- Gate red-check: resume-without-secret mutation → gate fails at the intended assertion; reverted → green.
- Smoke-check red/green simulations: system-layer check fails when `/etc/gitconfig` drops the entry; env-layer check fails when `GIT_CONFIG_COUNT` is unset; both pass with the image wiring.
- Credential-resolution e2e (git 2.39.5): system config + gh-shim global + env store + symlinked credential file → `git credential fill` returns `oauth2/<token>` for github.com despite the failing shim.
- Docker build NOT run (no docker daemon in this dev workspace) — the Dockerfile changes are exercised by the in-image smoke gate at build time in CI; the smoke commands themselves were validated standalone.

---

## Next Steps

1. Harness (outer repo): bind a `git-credential` secret (host `github.com`, protocol `https`) to dev workspaces at creation instead of (or in addition to) env-injecting `GH_TOKEN` — that is what makes acceptance criterion 1 (`gh auth status` post-resume) fully hold; the platform side now consumes it durably.
2. Optionally surface a healthz/statusz hint when `$HOME/.git-credentials` dangles (unbound git-credential) so agents self-diagnose the incident class.
3. After PR merge, confirm the image-build smoke gate runs green in CI (first build with the new checks).

---

## Files Modified

- `runtimes/base/Dockerfile` — `/etc/gitconfig` + `GIT_CONFIG_*` ENV layers (#1087 comment block).
- `runtimes/base/tools/smoke-test.sh` — two credential-helper build gates.
- `cmd/workspace-agentd/git_creds_boot_test.go` — new: cold-boot/suspend-resume regression gate.
- `cmd/workspace-agentd/main.go` — nil guard on the server-only admin-token assignment.
- `local/test-entrypoint.sh` — repaired to drive real subcommands; git-credential round-trip assertion.
- `pkg/agentd/secrets/symlink_lifecycle_test.go` — llm-provider fixture `apiKey` correction.
- `README-LLM.md` — "Git credential consumption (#1087)" paragraph in the relay-config subsystem section.
- `CHANGELOG.md` — Unreleased entries.
- `worklogs/NNNN_2026-08-27_git-credential-store-hardening.md` — this worklog.
