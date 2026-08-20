# Worklog: design 0051 US-1/US-2 — supervisor-subprocess tests + executable L3

**Date:** 2026-08-20
**Session:** Implement the two pieces the US-2 integration test plan (#985) left specified-but-not-implemented: an exec-level US-1 supervisor integration test, and the L3 kind-cluster checks as an executable script + CI workflow.
**Status:** Complete (pending review)

---

## Objective

The plan (docs/testing/0051-us2-integration-test-plan.md) shipped with L0–L2 automated but L3 as a manual runbook, and no test anywhere exercised the actual `supervise-opencode` SUBCOMMAND as a process — every suite wired its components in-process. Close both.

---

## Work Completed

### L1.5 — supervisor subprocess tests (`cmd/workspace-agentd/supervisor_subprocess_test.go`, NEW)

Runs the REAL `supervise-opencode` subcommand as a real subprocess (test-binary re-exec helper, same pattern as `managed_process_test.go`) and drives it with the real `controlClient` over real TCP:

- `..._LifecycleAndContract` — hello supervisor identity, status shape, metrics envelope, `spawn_env` → `restart` → the env lands in the next REAL child's `/proc/<pid>/environ` (wholesale replacement — the supervisor's own env must not leak through), pid swap on restart. Documents that `status.restarts` counts CRASH recoveries (operator restarts reset it by design — an assertion against it here was wrong and moved to the crash test).
- `..._ChildCrashRespawn` — SIGKILL the child as in-pod uid-1000 code can; fresh child within backoff, socket stayed up THROUGH the crash window, crash counter incremented.
- `..._ShutdownReapsChild` — SIGTERM → supervisor exits 0, child gone (ESRCH), no orphan.
- `..._BadRequestOverWire` — malformed JSON → `bad_request`, unknown method → `method_unknown`, supervisor still serving.

Fakes: the `opencode` binary only — a single-process `exec sleep 3600` stub. An earlier `trap 'exit 0' + sleep &` variant backgrounded a sleep that survived the trap and held the test's stdout pipe open past exit: tests PASSED but `go test` failed with "WaitDelay expired". Fixed by the single-process stub + `cmd.WaitDelay`.

### Production seam (minimal, test-only reach)

`runSuperviseOpencodeCommand` honors `LLMSAFESPACES_CONTROL_SOCKET_ADDR` (unset → the fixed `127.0.0.1:4099`; the override follows the existing `LLMSAFESPACES_AGENT_CONFIG_PATH` pattern). Needed so the subprocess tests can use an ephemeral port without colliding with anything on the host/CI runner. No behavior change in production.

### L3 — executable kind script (`scripts/us2-kind-integration.sh`, NEW) + workflow (`.github/workflows/us2-kind-integration.yml`, NEW)

Implements K1–K8 from the plan against a real kubelet. Self-contained:

- **Registry wiring done right**: throwaway local registry (canonical kind pattern — bridge network + `certs.d` hosts.toml aliasing `localhost:<port>` to the registry container) because the agentd sidecar REQUIRES a digest-pinned image reference (controller validation) and digest pulls must actually resolve. Digest read from `RepoDigests` after `docker push`. An earlier draft's dead code (docker-save-as-manifest PUT) removed — `docker save` output is not a manifest.
- **Controller-lean install**: `api.enabled=false`, `mcp.enabled=false`, `webhooks.enabled=false` (the chart's webhook gate removes the cert-manager requirement — verified in values.yaml), `rbac.scope=cluster`. The reconciler needs none of the disabled components to build pods; the sidecar split is what is under test.
- **Runtime image** referenced as `localhost:<port>/llmsafespaces/runtime-base:ci` — passes through `resolveRuntimeImage` untouched (image-shaped runtime string; verified in runtime_resolver.go) and pulls from the local registry.
- Node image pinned to the same digest as `local/kind-cluster.yaml` (chart floor 1.35). Pod-status fields: `.status.initContainerStatuses` (the long-standing field — an earlier draft used `.status.initStatuses`, which I could not verify exists; dropped).
- K6/K7 use `crictl stop` on the kind node for container-level restart isolation; degrade to SKIP with a recorded result when crictl is unavailable. K8 asserts ≤30s for pod delete (5s grace + API overhead).
- Workflow: workflow_dispatch + weekly Mondays 05:00 UTC, NOT PR-gating (L0–L2 remain the per-PR gates), failure diagnostics dump pod describe + controller/sidecar/workspace logs.

### Plan doc updated

Level table gains L1.5; §4 replaced by the executable script usage + honesty note (authored on a docker-less sandbox; first CI execution may need iteration — record on #978); §6 updated (L3 already in CI).

---

## Key Decisions

1. **Env-override seam over a build tag or test hook**: the socket address is a deployment constant in production; an env override is the repo's established pattern for exactly this (agent-config path, marker path) and costs three lines. The alternative — making the address a flag — adds interface surface to a frozen subcommand.
2. **Single-process child stub**: `exec sleep` instead of trap+background-sleep. The failure mode (pipe held open past test exit → WaitDelay) is subtle and worth the comment it now carries.
3. **Local registry over `kind load` for the agentd image**: digest-pinned references need a resolvable registry; `kind load` imports by tag and the digest-by-IfNotPresent path was unproven — the registry makes it correct by construction. Controller/runtime images go through the same registry for uniformity.
4. **K1 via pod-status timestamps** (initContainerStatuses lastState.terminated.finishedAt / state.running.startedAt) rather than Event parsing — events are prunable and harder to order; status timestamps are monotonic strings.
5. **Non-gating L3 workflow**: kubelet behavior is stable-surface; per-PR 25-min kind runs would be a bad trade. The plan's decision point (promote to per-PR) stays after US-5.

---

## Blockers

None. (docker/kind unavailable on this sandbox — the script's first real execution is CI's workflow_dispatch; noted in the doc and PR.)

---

## Tests Run

- `go test -race ./cmd/workspace-agentd/ ./controller/internal/workspace/ ./helm/...` — green (subprocess tests included; count=1, 500s budget).
- `go test -run 'TestSupervisorSubprocess_' -count=2` — green twice (stability).
- `bash -n scripts/us2-kind-integration.sh` — syntax OK (shellcheck unavailable locally).
- vet/gofmt/goimports/golangci-lint — clean (0 issues).

---

## Next Steps

1. Merge this PR; then trigger the workflow once (`workflow_dispatch`) and record K1–K8 outcomes on #978.
2. US-3 (credential split) per the plan's §6: extend `..._SecretBackedEnv` to `agentdPassword`, add V4.
3. US-4: extend `..._SpawnEnvHandoff` to drive the real reload handler; K3 mount-relocation assertions.

---

## Files Modified

- `cmd/workspace-agentd/supervisor_subprocess_test.go` (new)
- `cmd/workspace-agentd/supervise_opencode.go` (LLMSAFESPACES_CONTROL_SOCKET_ADDR seam; no default change)
- `scripts/us2-kind-integration.sh` (new, executable)
- `.github/workflows/us2-kind-integration.yml` (new)
- `docs/testing/0051-us2-integration-test-plan.md` (L1.5 row; §4 executable L3; §5/§6 updates)
- `worklogs/NNNN_2026-08-20_0051-us1-supervisor-l3-integration.md` (this file)
