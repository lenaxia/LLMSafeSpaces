# Worklog: US-70.1 — spawn-time env pull (bounded wait + last-good delta cache, spawned_rev, cross-uid boot matrix)

**Date:** 2026-08-30
**Session:** Implement Epic 70 story US-70.1 (#1162): flip env-class credential delivery from the structurally-broken boot-time push to a spawn-time pull; terminal `spawned_rev` verification; loud degrade plumbing healthz → CRD; A2 validation + standing R3 cross-uid boot matrix.
**Status:** Complete (in-process scope) — cluster-bound ACs (AC-2 suspend/resume e2e, gVisor runsc, 100-resume scale) blocked on US-70.0's harness, in-process analogs green.

---

## Objective

Fix the live boot bug: in sidecar mode the sidecar pushes the secrets-env delta to the supervisor's control socket (4099) *before its own startup probe can pass* — and the workspace container starts only after that probe — so the dial is `connection refused` at every boot (2026-08-30 fleet audit: 3/6 pods; suspend/resume re-breaks deterministically; manual reload + the relay injector's ~T+7s restart mask it live). Replace the push with a spawn-time pull (design 0057 R2), verify at the point of consumption (I4), degrade loudly (I10/I13), and stand up the cross-uid boot-path matrix (R3) with A2 validated.

---

## Work Completed

### Pull endpoint (sidecar user mux)

- `cmd/workspace-agentd/spawn_env_pull.go` — `GET /v1/spawn-env` handler (`spawnEnvHandler`): serves the parsed secrets-env delta + canonical revision (`spawnDeltaRev`: hex SHA-256 over sorted `k=v` lines). §D1 Basic pair auth (`checkBasicAuthAny`) — either credential accepted, auth checked before method. Absent file → quiet empty delta (law 5); corrupt file → 500 (complete-or-loud). Registered in `buildUserMux` (server.go) — pinned by `TestBuildUserMux_RegistersSpawnEnvEndpoint`.
- Wire shape `spawnEnvResponse{Env, Rev}` + reason-code set (`spawn_env_unavailable`, `spawn_env_unauthorized`, `spawn_env_no_credential`, `spawn_env_bad_response`).

### Supervisor spawn-time pull (R2 mechanics)

- `cmd/workspace-agentd/spawn_env_pull.go` — `spawnEnvPuller.pullBounded`: total bound 2s, per-attempt 500ms, retry gap 150ms; permanent failures (401, no-credential, fully-read malformed 200 body) return immediately; transient (dial/timeout/5xx) retry until the bound; context cancel aborts (shutdown never waits out the bound).
- `cmd/workspace-agentd/managed_process.go` — new `preSpawn` hook invoked at the top of each supervise-loop iteration **outside `p.mu`** (bounded I/O must never block restart/state/metrics readers); nil = no-op (single-container unaffected).
- `cmd/workspace-agentd/supervise_opencode.go` — `managedProcAdapter` rework:
  - `preSpawn()` — the bounded pull; success (including EMPTY delta — revocation is absence, I12) replaces `currentDelta` and clears the degrade; failure keeps the **last-good delta from supervisor memory only** (I7) and records the reason.
  - `composeChild()` — the sole child factory: base env + `parentPlusDelta` (platform wins), records `spawnedRev = spawnDeltaRev(effectiveDelta)` — the keys that actually landed — **at composition time** (I4: never the server-advertised rev, never materialization/fetch). A platform-shadowed key drops out of the effective rev (surfaces as divergence, which is correct).
  - `SetSpawnEnv` (legacy push store, demolition US-70.5): stores the delta; composition at next spawn; a later successful pull supersedes it (I3 pull-only correctness). No production caller remains.
  - `newSupervisorProcess(ctx)` — wires puller (`spawnEnvPullAddr()` — default `127.0.0.1:4097`, env override `LLMSAFESPACES_SPAVENV...`-style test seam `LLMSAFESPACES_SPAWN_ENV_PULL_ADDR`) + credential `OPENCODE_SERVER_PASSWORD` (already controller-wired to the main container).

### Push supersession

- `sidecar_mode.go` — the boot push block (`pushInitialSpawnEnv` at mux-boot) **deleted**; `pushInitialSpawnEnv` itself retained for US-70.5 demolition (marked superseded, no production caller, tests pin its behavior).
- `spawn_env_consumer.go` — `socketReloadProc` is restart-only (the pre-restart push removed): the restarted child's spawn pulls the fresh delta. Deferred (#852 session-aware) restarts therefore still hand off the latest env — at fire time, via the pull.

### Terminal verification reporting (I4) + loud degrade (I10/I13)

- `control_socket.go` / `control_client.go` — `status` result extended additively: `spawned_rev`, `spawn_env_degraded`, `spawn_env_reason` (interface +`SpawnEnvState()`; hash + reason code only — A.4 invariant 1 intact: no env values out).
- `cmd/workspace-agentd/supervisor_status.go` — sidecar poller (15s, immediate first poll) mirrors supervisor status into `supervisorStatusStore`; failed polls keep the last snapshot (no flapping); `spawnEnvWarning` renders `degraded:<reason>` (no semicolons — condition-message contract).
- `healthz.go` / `server.go` / `pkg/agentd/types.go` — `HealthzResponse.SpawnEnv *SpawnEnvHealth{SpawnedRev, Degraded, Reason}` + warning; healthz stays process-only (cached snapshot, no socket I/O; US-22.1 preserved); degrade never flips `Healthy` (no liveness→pod-kill cascade).
- CRD: `SecretsDeliveryStatus{SpawnedRev, DegradedReason}` on `WorkspaceStatus` (`pkg/apis/.../workspace_types.go`, deepcopy, `helm/crds/workspace.yaml`).
- Controller `health.go` — mirrors `healthz.SpawnEnv` → `ws.Status.SecretsDelivery` on healthy scrapes; clears to nil on unreachable/undecodable/unhealthy (same doctrine as `UserCredsPresent`); a healthy scrape omitting the field (pre-US-70.1 runtimes, mixed fleet W15) keeps the previous value (no flapping). The `degraded:...` warning rides the existing AgentHealthy condition message path (alert surface; SLO alert rules land with US-70.3 per the epic story map).

### A2 + R3 standing cross-uid boot matrix

- `cmd/workspace-agentd/cross_uid_boot_matrix_test.go` — standing registry (9 rows: writer uid, reader uid, mode, outcome) + executable checks: materializer CROSS_UID file modes (0640/0770 family), `AgentConfigWriteMode` 0640 pin, pull-endpoint credential gate (401, no value leakage), supervisor credential env-readability (A2 runtime half).
- `controller/internal/workspace/cross_uid_boot_matrix_test.go` — pod-spec rows: `OPENCODE_SERVER_PASSWORD` on main (A2 wiring), `AGENTD_CONTROL_PLANE_PASSWORD`/`AGENTD_ADMIN_TOKEN` sidecar-env-only, `agentd-secrets` never mounted in uid-1000 space, `agentd-config` RO, `sandbox-cfg` RO for sidecar.

### R3 matrix findings — real breaches found and fixed

1. **`AGENTD_ADMIN_TOKEN`/`AGENTD_ADMIN_TOKEN_FILE` residue in the uid-1000 main container (sidecar mode).** The base pod build wires the admin bearer for single-container mode; `applyAgentdSidecar` never stripped it — contradicting design 0051 D1 and the supervisor's own `skipHealthProbe` rationale ("the supervisor must never hold that token"). Fixed: `stripEnvVars` in `applyAgentdSidecar`. Pinned by `TestCrossUIDBootMatrix_MainContainerCredentialSet` + `TestCrossUIDBootMatrix_SidecarAdminTokenEnvOnly`.
2. **Distinct-token file install guard**: the bash heredoc's `install -m 0400 admin-token` is now gated `!= sidecar mode` — defense-in-depth (structurally unreachable today because sidecar ⇒ overlay ⇒ no bash cred init; kept for the same reason as the annotateModels guard, with rationale in the script comment).
3. **Pre-existing (flagged, not fixed — tooling)**: `make deepcopy` (`hack/update-deepcopy.sh`) is a silent no-op for `pkg/apis/...` — it greps for `+k8s:deepcopy-gen=` markers, but the package uses kubebuilder `object:generate` and the committed `zz_generated.deepcopy.go` is controller-gen output. I hand-extended the generated file in its exact style (2 blocks). **Follow-up needed:** wire controller-gen into the Makefile `deepcopy` target.

### Test-isolation fix (pre-existing, latent)

`opencode_overlay_test.go`'s `filteredEnviron` passed ambient env through — inside this repo's own workspace pods the ambient `OPENCODE_SERVER_PASSWORD` + a live :4097 mux made the supervisor pull a REAL old sidecar, eat the 2s bound per spawn, and blow the 2s control-client restart timeout. Fixed by filtering `OPENCODE_SERVER_PASSWORD` + `LLMSAFESPACES_SPAWN_ENV_PULL_ADDR` in `overlayEnvKeys()` (pull behavior has dedicated tests).

### Exec-level integration (in-process analogs of the ACs)

`cmd/workspace-agentd/spawn_env_pull_exec_test.go` — REAL subcommand + REAL handler over HTTP:
- first-spawn env via `/proc/<pid>/environ` + `spawned_rev == rev(delta)` (AC-1 analog), never-block + loud `spawn_env_unavailable` on dead mux, last-good survival across a failed pull + recovery + degrade clearing, revocation-is-absence, `spawn_env_unauthorized` on a wrong credential.

### Docs

- `design/0057_2026-08-30_secret-delivery-v2.md` (in-repo design pointer; normative content remains #1158) + `design/stories/epic-70-secret-delivery-v2/README.md` (story map + US-70.1 notes).
- README-LLM volume-table row updated (push → pull).

---

## Assumptions (Rule 7 — stated and validated)

| # | Assumption | Validation |
|---|---|---|
| A-1 | At supervisor first spawn the sidecar user mux (4097) is already serving (native-sidecar startup gating; materialize writes secrets-env BEFORE muxes serve in `runSidecarCommand`) | Code-verified: `sidecar_mode.go` boot order (boot-secrets → #857 stamp → `wireHTTPServers`); kubelet half pinned at L3 (kind K1). Exec tests model the mux-already-up case |
| A-2 | The pull credential is the §D1 carve-out workspace password (`OPENCODE_SERVER_PASSWORD`), NOT `agentdPassword` — recorded deviation from A2's literal wording (design 0051 D1 forbids the control-plane credential in uid-1000 space; A2 predates the credential choice) | Pod-spec pin (`TestCrossUIDBootMatrix_PodSpec` — env wired, secretKeyRef `password` key) + exec tests (credential accepted by the mux) + handler auth matrix |
| A-3 | `OPENCODE_SERVER_PASSWORD` is present on every sidecar-mode main container | `agentd_sidecar.go:320-325` pre-existing wiring (fail-closed secretKeyRef); pinned by the matrix test |
| A-4 | 2s bound never blocks spawn materially; per-spawn pull cost is single-digit ms when healthy | `TestSpawnEnvPuller_UnreachableExpiresBounded` (bound honored) + exec never-block test; worst-case +2s documented |
| A-5 | 404/5xx retry-until-bound is production-safe because sidecar + supervisor share one digest-pinned image (endpoint skew inside a pod is impossible) | Single-artifact pin (`TestAgentdSidecar_Enabled_NativeSidecarContainer` — same digest) |
| A-6 | Serving the delta to workspace-password holders leaks nothing beyond the delta's destiny (the child env) — same threat class as the /v1/mcp carve-out | Reasoned against design 0051 §D1 table + A.4; documented in `spawn_env_pull.go` header |
| A-7 | The old 2s control-client timeout can be exceeded by a degraded-pull restart (bound+grace); production callers use 30–60s | `socketRestarter` 30s / `socketReloadProc` 60s verified; exec test mirrors the production budget |

---

## Key Decisions

1. **Pull executes outside `managedProcess.mu`** via a `preSpawn` loop hook — the factory (`composeChild`) is I/O-free; bounded waits can never block restart/state/metrics readers.
2. **`spawned_rev` is self-computed over the effective delta** (post parent-shadow filter), ignoring the server's rev field — terminal by construction; a shadowed secret legitimately reads as divergence for US-70.3's reconcile.
3. **Successful pull of an empty delta supersedes last-good** (I12); only *failures* fall back.
4. **bad_response is permanent** (retrying identical fully-read malformed bytes wastes spawn latency); 5xx/404/network retry until bound.
5. **Degrade never flips healthz `Healthy`** — a secrets problem must not become a liveness/pod-kill problem (never-block extended to never-cascade).
6. **`SecretsDelivery` cleared on scrape failure, kept when a healthy scrape omits the field** — mirrors `UserCredsPresent` doctrine + mixed-fleet (W15) tolerance.
7. **A2 interpretation** recorded (A-2 above): D1 outranks the literal wording; the assumption's substance is validated.

## Adversarial review (Rule 11)

Independent skeptical-validator delegation returned **PASS** (zero real findings). Minor findings and their dispositions:
- F4 worst-case pull latency is bound+attempt = 2.5s (not 2s) — doc comments corrected.
- F5 mid-body transport truncation was classified permanent — refined: read-error → `spawn_env_unavailable` (retry), fully-read malformed → `spawn_env_bad_response` (permanent).
- F11 test gaps — added: oversized-body cap test (`TestSpawnEnvPuller_OversizedBodyIsBounded`), concurrent push/pull/compose hammer (`TestAdapterConcurrentPushPullCompose`, `-race`).
- F3 (stop during backoff eats one pull window, aborted early by ctx cancel in production) and F12 (retained-for-US-70.5 `pushInitialSpawnEnv`; served-but-unconsumed `Rev` field) — documented residuals, no action.

## Iteration (review on PR #1164, 2026-08-31)

The automated reviewer returned `REQUEST CHANGES` on #1164: cluster-bound e2e
missing for AC-2 (suspend/resume ≤90s), AC-13 (gVisor runsc + 100 concurrent
resumes + p95), AC-17 (rapid binds), AC-1 latency bound, and chaos-kill.

**Delivered:** `local/us-70-secret-delivery-e2e.sh` — a cluster-bound e2e
following the `us-68`/`us-63` kind harness conventions, wired into
`.github/workflows/e2e-nightly.yml` (step after the attachments rows). Rows:

- **AC-1** — cold-create a workspace with an env-secret bound before first
  Active; assert `/proc/<pid>/environ` of `opencode serve` carries the var and
  `status.secretsDelivery.spawnedRev` is present + `degradedReason` empty.
- **AC-2** — bind → suspend → resume → var present in child env and
  `secretsDelivery` converged ≤90s, owner offline, no manual reload.
  `SUSPEND_SECONDS` defaults to 5 (nightly); the ≥1h #1087 gate is set by the
  pool run (`SUSPEND_SECONDS=3600`).
- **AC-13/17** — `RESUME_SCALE` (default 100) concurrent resumes → sorted p95
  ≤ `P95_BUDGET_MS` (default 30000) and byte-identical `spawned_rev` across the
  batch (single-writer/one-truth). The **gVisor (runsc) leg is feature-detected
  against the cluster's RuntimeClasses** and SKIPs loudly when absent (kind
  can't run runsc) — the runsc leg remains a US-70.0 staged-pool artifact (epic
  W7), exactly as the PR body documented.
- **AC-17 (`SD_B1..B5`)** — rapid sequential env binds on a live workspace → the
  reload path converges to a healthy `spawned_rev` with no stuck degrade and no
  lost env.
- **Chaos** — kill `opencode serve` mid-run → agentd re-spawn re-pulls, env
  survives, `secretsDelivery` re-converges.

The first-spawn and pull-semantics properties remain deterministically pinned
by the in-process exec suite (`spawn_env_pull_exec_test.go`); these rows close
the end-to-end wiring + CRD-mirror + latency gaps the review flagged.

## Blockers

- Cluster-bound ACs (AC-2 #1087 gate with 1h suspend, gVisor runsc, 100-concurrent resumes, chaos kill) need US-70.0's harness (not yet started — no issue/file exists). In-process analogs are green; the exec tests exercise the real subcommand + real handler. **Update:** the cluster rows now EXIST (`us-70-secret-delivery-e2e.sh`, nightly-wired); the ≥1h suspend and runsc legs still require the US-70.0 staged pool to PROVISION those resources (runtime + gVisor RuntimeClass), which the script gates and SKIPs loudly when absent.
- `make deepcopy` is a silent no-op for `pkg/apis` (marker mismatch vs controller-gen output) — hand-extended this session; needs a controller-gen Makefile wiring (flagged for follow-up; flagged in epic README).

## Tests Run

- `go test -timeout 900s ./cmd/workspace-agentd/` — ok (175s; includes the exec-level suite)
- `go test -timeout 900s ./controller/... ./pkg/agentd/... ./pkg/apis/...` — ok
- `go test -race` over all new/changed test families in cmd/workspace-agentd — ok (independently reproduced by the validator)
- `golangci-lint run` (v2.5.0) over touched packages — 0 issues
- `go build ./...` — ok

## Next Steps

1. US-70.0 (delivery test harness) — unblocks the cluster-bound ACs of this story (suspend/resume ≥1h e2e, runsc load profile, chaos-kill), tracked in the epic README.
2. Wire controller-gen into the Makefile `deepcopy` target (replace the marker-mismatched `update-deepcopy.sh` path for `pkg/apis`).
3. US-70.2 (one builder + two-tier revisions) will replace `spawnDeltaRev` with the manifest/batch revision model — the supervisor's self-computed-terminal-rev contract (I4) stays; only the rev derivation changes.
4. PR review loop for this branch (feat/epic-70-us701-spawn-env-pull).

## Files Modified

- cmd/workspace-agentd: spawn_env_pull.go (new), spawn_env_pull_test.go (new), spawn_env_pull_adapter_test.go (new), spawn_env_pull_exec_test.go (new), supervisor_status.go (new), supervisor_status_test.go (new), cross_uid_boot_matrix_test.go (new), supervise_opencode.go, managed_process.go, control_socket.go, control_client.go, control_socket_helpers_test.go, spawn_env_consumer.go, sidecar_mode.go, sidecar_mode_test.go, server.go, healthz.go, has_user_creds_test.go, healthz_test.go, spawn_env_consumer_test.go, supervisor_subprocess_test.go, supervise_opencode_test.go, sidecar_integration_test.go, us2_regression_gaps_test.go, opencode_overlay_test.go, opencode_overlay_test.go (env filtering)
- pkg/agentd/types.go (SpawnEnvHealth + healthz field)
- pkg/apis/llmsafespaces/v1: workspace_types.go (SecretsDeliveryStatus), zz_generated.deepcopy.go
- controller/internal/workspace: health.go (mirror), health_spawn_env_test.go (new), cross_uid_boot_matrix_test.go (new), agentd_sidecar.go (stripEnvVars), pod_builder.go (install guard)
- helm/crds/workspace.yaml (secretsDelivery schema)
- README-LLM.md (volume-table row), design/0057_2026-08-30_secret-delivery-v2.md (new), design/stories/epic-70-secret-delivery-v2/README.md (new)
