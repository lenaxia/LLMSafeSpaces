# Worklog: agentd boot-time MCP/prompt injection — unconditional normalize

**Date:** 2026-08-15
**PR:** TBD

---

## Context

Direct continuation of the 2026-08-15 agentd-MCP-unavailable incident.
After patching the stale-image half (worklog 0751), the recreated pods
ran v0.15.5 — and verification showed the ORIGINAL symptom still
present, in a new form: `/sandbox-runtime/agent-config.json` contained
no `mcp` section at all (nor `agent`/`mode` blocks).

### Root cause

The `llmsafespaces` built-in MCP entry, the admin system prompt, and
the allowed external directories are rendered exclusively by
`ConfigWriter`'s marshal path (`WithPreMarshalHook` +
`WithAdminPromptPath` + `WithAllowedDirsPath` merges). At boot,
`materialize` writes only `{$schema, provider, model, mcp?}` (staged
user MCP servers), and every subsequent write trigger is conditional:

| Write path | Fires only when |
|---|---|
| Pre-boot relay (`applyRelayConfigPreBoot`) | free-models catalog file present AND no personal key |
| Relay injector (~T+7s) | free-models fetch succeeds |
| Credential reload | user action |

On this workspace all three skipped (no catalog; injector fetch failed
— `decode /provider: unexpected EOF`, also seen on the old pod; no user
reload yet). Result: opencode booted with no platform MCP server, no
platform system prompt, and no `/tmp/*` external-dir auto-approval —
until the first credential reload happened to repair it. v0.13.0
masked this class by injecting the MCP entry at materialize time
unconditionally (with the wrong port, 4098 — fixed by #725, which also
moved injection behind the writer's hook and introduced this gap for
skip-path boots).

## Fix

`cmd/workspace-agentd/boot_config.go` — `ensureBootAgentConfig(path…)`:
construct the standard writer and immediately `Apply(AgentConfigInput{})`
(empty input = re-marshal current sources; nil fields preserve
providers/model/relay/mcp loaded from disk). Called in `main.go`
BEFORE `startManagedProcess`, so opencode's first read sees the
completed config and no restart is needed. One unconditional write
closes every skip path; conditional writers need no changes.

Degradation policy: on failure, warn and continue boot — no agentd at
all is strictly worse than a config the next credential reload repairs.

## Assumptions stated and validated

1. `Apply` with all-nil fields preserves loaded sources — validated by
   reading `configwriter.go` Apply semantics and pinned by
   `TestConfigWriter_ApplyEmpty_StampsMissingBlocks` (provider + model
   survive).
2. Empty-input Apply is idempotent (boot after pre-boot-relay wrote
   everything rewrites the same config) — pinned by
   `TestConfigWriter_ApplyEmpty_Idempotent` and
   `TestEnsureBootAgentConfig_Idempotent`.
3. Missing bootstrap files (admin-prompt.md / allowed-dirs) degrade to
   MCP-only injection, not error — pinned by
   `TestConfigWriter_ApplyEmpty_MissingPromptAndDirs_StillWrites` and
   `TestEnsureBootAgentConfig_MissingBootstrapFiles`.
4. Writing before `startManagedProcess` is race-free w.r.t. opencode's
   config read (opencode starts strictly after the write completes).
5. The 4097 port in the injected URL is correct (user mux hosts
   `mcpHandler`) — pinned by
   `TestEnsureBootAgentConfig_StampsPlatformBlocks` asserting
   `:4097/v1/mcp`.

## Verification

- New tests: 3 × `pkg/agent/opencode/configwriter_bootnormalize_test.go`,
  3 × `cmd/workspace-agentd/boot_config_test.go` — all pass.
- Full `./cmd/workspace-agentd/` suite (322s, includes e2e) — pass.
- Full `./pkg/agent/...` suite — pass.
- `go build ./...` clean; `gofmt` clean.
- Live-cluster reproduction confirmed the bug shape (recreated v0.15.5
  pod: `mcp url: MISSING`, `:4097/v1/mcp` → 200); image with this fix
  ships via the next release.

## Adversarial review

- **Double-write with pre-boot relay:** idempotent rewrite, byte-equal
  JSON — validated, not assumed.
- **Boot cost:** one small atomic file write before process start;
  negligible.
- **Rollback:** revert restores the conditional-only behavior; no
  schema/data changes.
- **Why not fix materialize to stamp these?** materialize already has a
  writer path (pre-boot relay) that does exactly this — but only on its
  happy path. Duplicating prompt/dirs/MCP knowledge in the materialize
  unconditional path would fork the rendering logic; the boot normalize
  in agentd reuses the single writer seam (Rule 12).
