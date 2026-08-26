# Worklog: sidecar migration step 1 — platform boot logic leaves the runtime image

**Date:** 2026-08-25
**PR:** TBD
**Incident:** 2026-08-25 — factory-built runtime bases (`ws:s-…-0.8.0`) baked a pre-#871 agentd; `credential-setup` ran it from the runtime image and crash-looped `Init:Error` on contract-shape MCP metadata (`cannot unmarshal array … Secret.metadata`). Chat workspaces `7bde7762…` and `80683260…` affected.

## Root cause (architecture level)

Platform boot logic (the credential-setup heredoc + workspace-dirs bash) executed from the runtime image — a user-plane artifact on its own release cadence. The agentd overlay (#863) mounted `/agentd` only on the MAIN container; the init container ran the baked binary. The image factory layers onto an operator-pinned base (`bookworm@0.8.0`, drifted from the CRD's `base@0.20.1`) — refresh, not removal, of the dependency.

## Delivered (TDD — every component test-first, red confirmed before implementation)

1. **`init-fs` subcommand** (`cmd/workspace-agentd/init_fs.go`): uid-1000 PVC prep — subPath roots (absorbs workspace-dirs), US-35.7 symlink farm **hardened** (lstat semantics: a pre-planted link is replaced without following; attack-corpus tests incl. victim-survival), G21 password install (0600, temp file created with final mode), #887 admin-token (0400), free-models copy. Exit 1 on operational failure (G46 class: missing password is loud).
2. **Sidecar-absorbed bootstrap/materialize** (`sidecar_boot.go`): runs first in `runSidecarCommand`, before `ensureBootAgentConfig`/muxes — the #857 stamp-before-read guarantee now rides the startup probe. Bootstrap output relocates to `/sandbox-runtime/rt/secrets.json` (sidecar's `/sandbox-cfg` is RO). **Idempotency guard**: non-empty secrets.json ⇒ no API re-hit on sidecar restart (token expired, API may be down); materialize re-runs (idempotent). API-down still boots (empty batch, reload path recovers) — never-block-boot preserved. Materialize failure propagates (fail fast).
3. **Pod builder** (`platform_init.go`): overlay mode replaces BOTH bash inits with `[binary, subcommand]` containers from the pinned agentd image — no shell anywhere (delivery image may be shell-less). `platform-init` (always) / `platform-bootstrap` + `platform-materialize` (legacy mode; sidecar mode defers to the sidecar). Sidecar gains the bootstrap-token mount + `LLMSAFESPACE_API_URL` + `LLMSAFESPACES_ENRICHER_CACHE_DIR` (off-PVC: the sidecar does not mount `/home/sandbox`). Legacy-no-overlay keeps the bash path (step-5 deletion candidate; rollback surface).
4. **Visibility** (`boot_failure.go`): platform-container boot failures (init-fs/bootstrap/materialize/sidecar boot) map to `BootReady=False` (`ReasonPlatformBootFailed`) + one-shot event + `workspace_platform_boot_failures_total` — the incident's eternal-reasonless-Creating class now self-reports. Wired in `handleCreating` beside the #863 verify-failure detection (same skip-recovery semantics: pod deletion cannot fix a platform bug).
5. **Docs**: design 0051 D7 amendment (ordering, path relocation, restart semantics, uid/mode matrix, rollback residual).

## Assumptions stated

1. Entrypoint audit (F4, carried): in sidecar mode the baked `entrypoint-opencode.sh` stays the main container's entrypoint for now — it performs mise activation + secrets-env sourcing + OPENCODE_* exports that the supervisor child env depends on (`buildEnvFrom` already replicates secrets-env in Go). Migration step 2 (Command: = supervisor agentd) requires moving that env work into pod-spec env / supervisor; NOT done here.
2. `rt/*` credential outputs written by the uid-2000 sidecar remain readable by uid-1000 opencode via shared gid 1000 + the secrets package's 0051-adapted modes (T2 exception 0640 for agent-config.json) — covered by the existing materializer suites; L3 kind integration is the flip gate.
3. The sidecar's projected SA token mount is a documented residual widening (kubelet-refreshed, audience-scoped, platform-owned container) — recorded in D7 context; threat-model note stands from US-2.

## Verification

- TDD corpus: `init_fs_test.go` (8 tests incl. symlink attack corpus), `sidecar_boot_test.go` (5: fresh boot, restart guard w/ no-API-hit pin, API-down degradation, failure propagation, cross-uid write profile), `platform_init_test.go` (5), `boot_failure_test.go` (4).
- Full suites: `./cmd/workspace-agentd/` (284s, subprocess corpus incl. exit-code pins), `./controller/...`, `./pkg/apis/...` — green. `go vet` + envtest-tagged compile clean.
- Updated `TestAgentdSidecar_Enabled_OrderingAfterCredentialSetup` to the new invariant (sidecar after platform-init; internal ordering pinned agentd-side).

## Integration-test round (2026-08-25, second session)

**Integration bug found and fixed by writing the tests first**: in sidecar mode the boot-phase materializer runs as uid 2000, but `secrets-env`/`rt/secrets/*`/`rt/ssh/*`/`git-credentials` are consumed by uid 1000 (supervisor's `buildEnvFrom`, entrypoint source, ssh, git). 0600 uid-2000 files EACCES there and `buildEnvFrom` degrades SILENTLY — env-secrets would have vanished in sidecar mode with zero signal. Fix: `secrets.Paths.CredentialWriteMode` profile (`CredentialModeCrossUID`=0640 via the pod's shared gid 1000 — the documented T2 exception extended from agent-config.json to the full cross-uid boot set); selected by `LLMSAFESPACES_SECRETS_CROSS_UID=1`, which the controller sets on the sidecar. Legacy zero-value keeps 0600 byte-identical. Pinned at L0 (`cross_uid_write_test.go`), L0.5 (sidecar boot profile test), L2 (pod-spec env pin).

**L3 kind plan revised to the step-1 world** (the old K1 pinned `credential-setup → agentd → main`, which step 1 invalidated):
- K1 → platform-init ordering; K3 + secrets-env {0640, absent}.
- **K9** (new): platform-init is `[binary, "init-fs"]` from the pinned image, legacy bash inits absent, symlink farm live on the real PVC (A1+E2).
- **K10** (new): planted legacy PVC state (real `.secrets` dir, real-file auth.json) → pod delete → recreated pod Ready with farm links replacing planted paths (A2, the force-upgrade path).
- **K11** (new): after K6's sidecar restart, current sidecar instance logs zero `bootstrap:` lines while the previous shows the first-boot fetch (B1 restart guard, observable because the lean topology runs API-off).
- Script meta-tests extended (`TestUS2KindScript_Step1MigrationChecks`) so the checks can't silently rot; workflow job renamed K1–K11; testing-plan doc updated.

**L2 envtest added** (`platform_init_envtest_test.go`): platform-init spec admission for both modes (legacy chain + sidecar) against a real API server, and BootReady=False persisting through the real status subresource (A4).

## Step 2 (same session): supervisor Command bypass

The F4 entrypoint audit, executed. `entrypoint-opencode.sh` did five things; each disposition:

| Entrypoint step | Disposition |
|---|---|
| verify_and_select_agentd (sha256, exit 81/82) | **Moved into the supervisor** (`supervise_selfverify.go`): self-hashes `/proc/self/exe` against the pod-spec pins, exit 81 + `expected=/got=` message shape — `detectAgentdVerificationFailure` works unchanged. Legacy-no-volume skips (baked path unchanged). |
| `eval "$(mise activate bash)"` | Dropped: image `ENV PATH` already carries the shims dir + `MISE_DATA_DIR`; a non-shell PID 1 needs no shell activation. |
| `source /sandbox-runtime/secrets-env` | Already replicated in Go by `buildEnvFrom` (secrets.go) at child spawn. |
| `OPENCODE_CONFIG`/`XDG_DATA_HOME`/`OPENCODE_EXPERIMENTAL_EVENT_SYSTEM` exports | Relocated to pod-spec env on the main container. |
| `OPENCODE_SERVER_PASSWORD=$(cat /sandbox-cfg/password)` | Pod-spec `secretKeyRef` — fail-closed (missing Secret = create error, not a silent gap). |
| sidecar/legacy branch (`exec AGENTD_BIN …`) | Bypassed: sidecar-mode main `Command:` = overlay binary, `Args: ["supervise-opencode"]`. Legacy mode keeps the entrypoint (step-5 deletion target). The entrypoint's sidecar branch is now dead-but-retained as the D6.1 rollback surface. |

Tests: `supervise_selfverify_test.go` (decision matrix, message shape, real-subprocess exit-81), `TestPlatformInit_Step2_*` pod-spec pins (bypass env + legacy-unchanged), **K12** in the kind script + meta-test pin + testing-plan row.

## Step 4: compatibility gate + incident-class regression coverage

- **Factory base floor** (`api/internal/imagefactory`): `MinBaseVersion = "0.15.7"` (the #871 platform release) enforced in `RenderDockerfile` — the factory now REFUSES to stage onto bases whose baked agentd predates the secrets contract. This is the structural closure of the 2026-08-25 vector (catalog row was 5 weeks stale; nothing reconciled it). Ships in the API release train, so bumps are reviewed release actions (`TestBaseFloor_IsTheIncidentFixVersion` pins the value to a conscious decision). ~30 handler/imagefactory fixtures bumped 0.6.0→0.20.1 (floor correctly rejected them; `HashSelection` excludes version, so no hash churn).
- **K13** (kind): builds a DEGRADED runtime base (baked agentd + entrypoints deleted) and boots a second workspace on it — must go Ready. The incident-class regression at L3: proves platform code is base-independent post steps 1–2, end-to-end.
- **e2e-nightly flipped to sidecar mode**: local-registry wiring (same pattern as the us2 script) + digest-pinned agentd delivery + `agentdSidecar.enabled=true`. The full stack (API on postgres → REAL pod-bootstrap fetch → sidecar boot → materialize) now runs nightly in sidecar mode — covering B2/F-layer journeys the lean us2 topology cannot.
- **Runbook** `docs/runbooks/sidecar-flip.md`: flip preconditions (release + K1–K13 green), canary→batch rolling deletion (normal SIGTERM deletes only; `platform_boot_failures_total` is a stop signal), suspended-workspaces-ride-free, D6.1 rollback, and step-5 deletion criteria.
- `values.yaml` agentdSidecar comment updated to the real migration state (stale US-3/US-5 gating text removed).

## Migration state after this worklog

| Step | State |
|---|---|
| 1 platform boot → agentd artifact | **Done** (this session) |
| 2 supervisor Command bypass + self-verify | **Done** (this session) |
| 3 flip + rolling delete | **Runbook-ready** — operator executes at next release (flip = one values default; not flipped in-repo before L3 runs once on released artifacts — that would assume the contract) |
| 4 compatibility gate | **Done** (base floor + K13 + nightly coverage) |
| 5 delete baked agentd + bash path | Post-soak (runbook criteria; legacy path retained as the D6.1 rollback surface until then) |

## Follow-ups (migration steps 2-5)

- Step 2: supervisor-Command main container (needs the F4 env audit executed).
- Step 3: flip + rolling deletion (canary one workspace first).
- Step 4: chart compatibility gate (base floor + single-coordinate pin) — closes the D6.1 helm-rollback residual.
- Step 5: delete baked agentd + bash init path from the base build after soak.
- #860 remains open: e2e that boots a FACTORY-built image (this incident's regression class end-to-end).
