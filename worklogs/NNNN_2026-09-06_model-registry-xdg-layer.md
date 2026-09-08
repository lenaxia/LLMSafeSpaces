# Worklog: #1300 root cause — the registry layer that /config/providers lied about

**Date:** 2026-09-06 (session 2)
**Session:** Continued from worklog 0903's handoff (issue #1300). Full root-cause isolation under a "no unproven assumptions" discipline: every hypothesis was tested to destruction before the next. Fix validated live on the production pod (turn on `thekaocloud/glm-5.3` completed end-to-end).
**Status:** Complete

---

## Objective

Prove the root cause of fleet-wide `SessionRunnerModel.ModelUnavailableError` on fresh workspace pods, fix it at the right level, and close the testing gap that let it ship.

## Assumptions at session start — and what happened to each

| # | Assumption inherited from handoff | Verdict |
|---|---|---|
| 1 | "Old working workspace had ZERO credential bindings" | **WRONG** — DB shows identical 3 bindings at birth for both workspaces (created_at within ms) |
| 2 | "Version is not the variable" (for the registry issue) | **Proven** — both workspaces ran image_tag 2026.08.0 / agent_version 1.18.15 (workspaces table) |
| 3 | kind:"opencode" config blocks poison the registry (top hypothesis) | **WRONG** — the pod's exact 4-provider config, run locally against the extracted 1.18.15 binary, admits everything incl. free-tier/free-user |
| 4 | auth entries with type:"api" are rejected by key resolution | **WRONG** — one `{"type":"api"}` entry admits the provider locally (predicate A: apiKey-in-request.body OR connections OR no-integration) |
| 5 | The stale jsonc experiment file was wiped on pod recreation | **WRONG** — /home/sandbox is the SAME Longhorn PVC as /workspace; it persisted (and was malformed → crashloop) |
| 6 | "Workspace pods are offline" (implicit in earlier reasoning) | **WRONG** — the pod has full egress (models.opencode.ai 200) |
| 7 | /app/restart doesn't rebuild the registry; only pod recreation does | **Misleading** — restart DOES rebuild; the configs it rebuilt FROM were the problem |

## Root cause (validated, full chain)

**opencode 1.18.15's V2 model registry (`CatalogV2` → `model.available()`, the source `SessionRunnerModel.resolve` searches) only ingests provider blocks from the XDG config layer (`~/.config/opencode/*.json`). A config supplied via `OPENCODE_CONFIG` — ours, at `/agentd-config/agent-config.json` — is loaded by the config service but NEVER by the catalog.**

Evidence chain:
1. `/config/providers` (ProviderHttpApi.list — raw config-service view) shows all 4 providers, keys resolved. The lying endpoint.
2. `/api/model` (model.available()) shows zen-only, for hours, on a clean process (post jsonc-cleanup) with the 4-provider config on disk.
3. Local, same binary (sha c1971d3d…): OPENCODE_CONFIG outside XDG → registry excludes config providers; same file reachable via XDG symlink → registry admits them in ~15s. Deterministic both ways.
4. Binary source (extracted): the config→catalog normalize (Et/Ut) runs in the XDG-layer pipeline; env-config feeds the legacy view only.

Aggravators (both observed live):
- **Sidecar bootstrap degrade-to-empty**: first-boot fetch failed transiently ("no route to host" to the API ClusterIP — CNI warming) → never-block-boot doctrine → relay-only config → opencode froze a degraded registry. Resync healed the FILE mid-life (log pass-2) but the running process never re-ingested.
- **Malformed leftover jsonc** (previous session's shell-escaping artifact `\$schema`, on the PVC) crashlooped opencode (exit 1 "not valid JSON(C)") across pod recreations — a second, independent wedge.
- **Auth-store ownership**: sidecar (uid 2000) atomic-writes leave auth.json foreign-owned; opencode (uid 1000) chmods on its own writes → `EPERM` on every PUT /auth (loud, non-fatal degrade).

## The fix (4 layers, all our side — no upstream changes)

1. **XDG registry-layer symlink** (`ensureOpencodeRegistryConfig`): supervisor installs `~/.config/opencode/opencode.json → /agentd-config/agent-config.json` before first spawn. PVC holds only the link (US-35.7 intact — same pattern as the #1296 auth symlink). Validated live: registry admitted thekaocloud:16 + relay:27 in 20s; steer turn completed ("OK", finish=stop).
2. **Bootstrap bounded retry**: first-boot fetch failures (no prior batch) retry 3× with linear backoff (2s,4s) before degrading — absorbs transient CNI warming; 401 never retried; last-good batch skips retry entirely (resync heals).
3. **Post-spawn config watcher**: supervisor polls the agent-config hash (5s; inotify is exactly what we don't trust under gVisor) and restarts opencode once per change (60s cooldown) with the existing `credential_reload` reason marker — heals late-batch delivery into a frozen registry.
4. **Malformed-config quarantine + auth ownership normalization**: unparseable XDG configs are renamed `.invalid-<ts>` at boot (kills the crashloop class); foreign-owned auth store is rewritten as the consuming uid (fixes the chmod EPERM).

## Why no test caught it

The pool pinned env/file delivery (SD_FIRST, ~/.ssh) and the #1296 auth-store mode — never the admission contract (credential bound → model ACTUALLY in model.available()). /config/providers was implicitly trusted as the registry's truth.

**New pin (AC-1b)**: create user provider credential → bind before Active → assert XDG symlink → assert `ac1b-stub/stub-model-1` present in GET /api/model (the registry), with an unreachable stub baseURL so the allowlist render is the model source. Fails on the lying view by construction.

## Contract audit (the second ask)

- `translate_abi.go`: already strongly typed post-0.27.2 (sessionInfo/messageInfo/nextStreamIDs/ocContentItem; lenient cost unmarshal) — no action.
- `FormatOpenCodeConfig`: typed structs, schema-pinned — no action.
- **Auth-store entries were the weak boundary**: untyped `map[string]any` with the schema pinned only by a test comment. Promoted to `authStoreEntry`/`authStoreMetadata` types (byte-identical wire, compile-time contract).
- The opencode admission semantics (which endpoint is truthful) are now executable documentation: AC-1b + the code comments in xdg_config_layer.go.

## Files

- `cmd/workspace-agentd/xdg_config_layer.go` (new) — symlink install (env-precedence target resolution), quarantine, JSONC parser, watcher (topology-split restart semantics), ownership normalization (coupled decision extract)
- `cmd/workspace-agentd/xdg_config_layer_test.go` (new) — symlink contract, quarantine, JSONC table, watcher discipline, wiring + reachability pins, env-precedence + child-env pair pin, ownership decision
- `cmd/workspace-agentd/bootstrap.go` — bounded first-boot retry
- `cmd/workspace-agentd/bootstrap_retry_test.go` (new) — transient/persistent/401/last-good
- `cmd/workspace-agentd/main.go` + `cmd/workspace-agentd/supervise_opencode.go` — boot-layer + watcher wiring in BOTH supervisor topologies (unconditional; r3 moved the single-container start out of the relay-gated path)
- `cmd/workspace-agentd/secrets.go` — typed authStoreEntry; resolveModelWithProvider allowlist strictness (2(b))
- `cmd/workspace-agentd/reload_credentials_e2e_test.go` — fixtures updated for the 2(a) render split
- `pkg/agent/opencode/format.go` — 2(a): zen-kind (kind:"opencode") no-model credentials render no config block; first-party allowlist-less keys still render (+ test)
- `local/lib/us70-common.sh` — create_stub_credential (optional baseURL), registry_admits helpers
- `local/us-70-secret-delivery-e2e.sh` — AC-1b (registry admission), AC-1c (mid-life bind heal), AC-1d (V2 TURN against a mock upstream — fix-design 4)
- `local/us-70-faults-e2e.sh` — F6 faulted-boot → registry convergence (loud skip when the seam is inert)
- `.github/workflows/us-70-delivery-pool.yml` — FAULT_COUNT 8→16 (retries burn 3× per faulted first boot)

## Review round r3 (all findings fixed)

- **Watcher reachability (the big one)**: the single-container watcher start sat inside maybeStartRelayInjector AFTER its relay-off early return — dead code in the chart-default posture (relay off + sidecar off): a mid-life llm-provider reload rewrote config and PUT auth with NO restart, the exact #1300 class at mid-life. Moved to an unconditional block in main(); negative source-pin asserts the watcher is not inside the injector.
- `needsOwnershipNormalization` was a test-only copy — now called by normalizeAuthStoreOwnership.
- Fix-design 2(a): zen-kind no-model credentials no longer render config blocks (narrowed from "all models-less" after realizing first-party allowlist-less keys NEED their block for catalog key-merge); reload e2e fixtures updated to match the split.
- Fix-design 2(b): qualified defaults against ALLOWLISTED providers must verify the model in the models map (stale re-pin guard); allowlist-less first-party providers keep existence-only (catalog-sourced, unverifiable config-side). Both pinned.
- Fix-design 3(a)/(b) disclosures: with the XDG layer live, V2 turns now see the admin prompt/MCP from the delivered config (V1 saw them, V2 did not — behavior change, surfaced here); `disabled_providers:["opencode"]` is V1-only — the V2 catalog keeps the zen provider enabled, which is harmless and load-bearing (zen stays reachable for zen-kind credentials).
- Fix-design 4: AC-1d drives a session-model-pinned V2 TURN against an in-cluster mock OpenAI-compatible upstream and asserts the assistant reply — the row that would have caught #1292b and #1300 as user-visible failures.
- AC-1c: mid-life bind → reconcile → materialize → watcher restart → registry admission (the 3(c) heal contract, both topologies).
- Child-env PAIR pin: effectiveAgentConfigPath must equal the child's OPENCODE_CONFIG across every topology's env combination (would have caught round 1's bug pre-pool).

## Live-validation record

Pod `8daf4ef8…-ad3695a4`: symlink installed 22:46 → registry `{"opencode":31,"opencode-relay":27,"thekaocloud":16}` at 22:47 → steer on `thekaocloud/glm-5.3` → assistant "OK", finish=stop, tokens input=3578/output=3. Also: jsonc quarantined-by-hand (deleted), pod recovered from restartCount 9.

## Follow-ups (noted, not blocking)

- Sidecar auth-store merge flips ownership back to uid 2000 on every mid-life reload (temp+rename) — the supervisor normalizes at boot; a durable fix is an in-place-write or ownership-preserving merge on the sidecar side.
- Watcher fingerprints agent-config only; auth-only batch changes rely on the same materialize rewriting config (true today).
- The `.16–.18` migration-window note in the Dockerfile stands; the contract-differential pool idea (characterize binary behavior per pin) would have caught this class before prod.

## Review round r1 (auto-reviewer, REQUEST_CHANGES — all five findings fixed)

1. **authStoreEntry emitted `"metadata":{}`** — struct fields ignore omitempty (encoding/json never omits non-pointer structs). Fixed: `*authStoreMetadata` pointer, set only when BaseURL != "". New pins: `TestAuthStoreEntry_MarshalMatchesLivePutShape`, `TestWriteStagedProvidersToAuthStore_NoBaseURLOmitsMetadataKey` (byte-parity with the live PUT shape both ways).
2. **Watcher defeated session-aware restarts** — now topology-split: single-container composes `relayKillFunc` (makeSessionAwareRestartDecision — in-flight turns defer, same as the relay injector's kill switch); supervise-opencode keeps a grace restart, matching that topology's incumbent socket-restart semantics for credential changes (spawn_env_consumer.restart → cc.Restart is unconditional there). Cooldown coalesces any same-window double-restart.
3. **Single-container topology gap** — boot layers + watcher now wired in `main()` too via shared `ensureOpencodeBootLayers`; pinned by `TestOpencodeBootLayersWiring` (source-scan across both entry points).
4. **Tests** — added: wiring pin, `needsOwnershipNormalization` decision extract + test (chown-to-other-uid is unprivileged-impossible; the decision is the testable unit), last-good-skip-retry (`…_LastGoodBatchSkipsRetry`: exactly 1 call, byte-preserved batch). The `/api/model` `.data[]` shape is validated by the session-2 live-pod probes; the pool rows (AC-1b/1c/1d, F6) re-prove it on execution — round 1's run is the only execution so far and it failed at the (then-buggy) symlink target, before reaching the registry assertions.
5. **Worklog numbering** — renamed to the `NNNN_` sentinel (the post-merge renumber bot assigns the real number; manual picks race concurrent PRs).

## Pool validation round 1 (run 34066476127 — AC-1b's first live execution)

**Failed at the symlink target — correctly.** The row caught a real bug in the fix: in sidecar mode the controller sets `OPENCODE_CONFIG=/agentd-config/agent-config.json` directly on the workspace container but NOT `LLMSAFESPACES_AGENT_CONFIG_PATH`, so `ensureOpencodeRegistryConfig` resolved the `/sandbox-runtime` default and linked at the wrong target while the child read the controller's path. The watcher had the same divergence (would have watched a file that never changes — a dead heal path).

Fix: `effectiveAgentConfigPath` mirrors the child-env resolution (`OPENCODE_CONFIG` env first, `LLMSAFESPACES_AGENT_CONFIG_PATH` default second; unit-pinned). AC-1b's assertion is now the true topology-independent contract: the XDG link target must equal the live child's `OPENCODE_CONFIG` (read from `/proc/<pid>/environ`), and the provider-block grep uses that same path.

Meta: this is the regression row doing exactly what it was built for — the live-pod manual validation had the right symlink BY HAND, which masked the code's wrong resolution.

## Validation status at r5 (honest record)

- **AC-1b PASS, AC-1c PASS in CI** (run 34083965519, head ec11fc9e — production code byte-identical to eb04c8b2 per the r5 review's diff verification). The #1300 root-cause row and the mid-life heal are proven end-to-end.
- **AC-1d (turn-level, fix-design 4) and F6 (faulted boot) are unexecuted.** Every pool dispatch since the AC-1d rework died on runner-host infrastructure before reaching the rows: two runs on DiskPressure evictions (kind-in-dind full builds exhausting node disk), one on a mid-build runner loss, and two fast-mode runs on resource-starved builds breaking the kind image import (worker CPU overcommitted; the runtime-base layer downloads alone ran 10-12 min). The runner blob was right-sized twice tonight (3→2500m→2300m CPU, talos-ops-prod #2435/#2436) just to become schedulable at all — the cluster lost cp-01 (physical, #2434) and worker-04's memory to the monitoring relocation.
- The merge gate (one green pool run at head executing AC-1b/1c/1d/F6) therefore waits on infra recovery (cp-01 power-on or a dedicated runner host), not on code. AC-1d's row mechanics are source-corroborated (adapter_path_test.go pins the V1 first-turn route; #755 rules out V2 queue; the registry triple-gate precedes the turn).

## Validation final (runs 34217857014, 34231075177)

- **Delivery suite ALL GREEN in one run** (34231075177): AC-1/1b/1c/1d/2/3/5/6/8/11/13 + chaos — the #1300 contract proven end-to-end in CI.
- AC-13 scale: 20-way PASS p95=153s (34217857014); 10-way in the green run. The 40-way number is nightly-owned (worker-04-class host); this 8-core runner host cannot sustain it plus the remaining suites.
- Root cause of the AC-13 "no space left on device": worker-00's re-image reset `user.max_user_namespaces` to Talos default 0 (the #2389 regression); runtime value restored via sysadmin-profile debug pod; **a talhelper config re-apply is needed for persistence across reboots**.
- Remaining suites (revisions/Epic 69/faults-F6) blocked by host throughput (Postgres auth 503 fail-closed under accumulated load), not by branch code.
