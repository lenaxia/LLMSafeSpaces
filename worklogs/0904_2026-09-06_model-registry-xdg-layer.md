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

- `cmd/workspace-agentd/xdg_config_layer.go` (new) — symlink install, quarantine, JSONC parser, watcher, ownership normalization
- `cmd/workspace-agentd/xdg_config_layer_test.go` (new) — 8 tests incl. the production `\$schema` artifact
- `cmd/workspace-agentd/bootstrap.go` — bounded first-boot retry
- `cmd/workspace-agentd/bootstrap_retry_test.go` (new) — 3 tests
- `cmd/workspace-agentd/supervise_opencode.go` — boot wiring
- `cmd/workspace-agentd/secrets.go` — typed authStoreEntry
- `local/us-70-secret-delivery-e2e.sh` — AC-1b registry row

## Live-validation record

Pod `8daf4ef8…-ad3695a4`: symlink installed 22:46 → registry `{"opencode":31,"opencode-relay":27,"thekaocloud":16}` at 22:47 → steer on `thekaocloud/glm-5.3` → assistant "OK", finish=stop, tokens input=3578/output=3. Also: jsonc quarantined-by-hand (deleted), pod recovered from restartCount 9.

## Follow-ups (noted, not blocking)

- Sidecar auth-store merge flips ownership back to uid 2000 on every mid-life reload (temp+rename) — the supervisor normalizes at boot; a durable fix is an in-place-write or ownership-preserving merge on the sidecar side.
- Watcher fingerprints agent-config only; auth-only batch changes rely on the same materialize rewriting config (true today).
- The `.16–.18` migration-window note in the Dockerfile stands; the contract-differential pool idea (characterize binary behavior per pin) would have caught this class before prod.
