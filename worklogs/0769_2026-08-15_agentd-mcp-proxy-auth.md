# Worklog: agentd /v1/mcp Basic-auth gate + credential in injected opencode MCP entry (#847)

**Date:** 2026-08-15
**Session:** Fix issue #847 — POST /v1/mcp on workspace-agentd (4097) is a JSON-RPC MCP proxy exposing session_list/session_read with no authentication. Any in-pod process could read the workspace's entire conversation history. Add the Basic-auth gate and make the legitimate caller (opencode itself, via the injected platform MCP entry) carry the credential.
**Status:** Complete

---

## Objective

Close #847. The proxy's only legitimate consumer is opencode, which connects to `http://127.0.0.1:4097/v1/mcp` through the `llmsafespaces` remote MCP entry stamped into `agent-config.json` by the boot-time pre-marshal hook. Enforcing auth therefore requires both sides: the gate on agentd, and the credential in the injected entry.

---

## Work Completed

- `mcp_server.go`:
  - `mcpHandler` gates at entry via the shared `checkBasicAuth`/`rejectUnauthorized` (from the #762 fix) — 401 + `WWW-Authenticate` before any JSON-RPC processing.
  - `injectAgentdMCPServer` is now a hook **constructor** taking the workspace password and stamping `headers: {"Authorization": "Basic ..."}` on the injected entry. Empty password → entry without headers (agentd main fails fatal on an unreadable password per G46, so this only affects tests).
- `boot_config.go` / `ensureBootAgentConfig` + `main.go`: password threaded to the hook construction.
- `pre_boot_relay.go` / `applyRelayConfigPreBoot`: password param added; `secrets.go` materialize subcommand reads it via `readAgentPasswordFromPath` (non-fatal on failure — logs to stderr, entry stamped without headers; the same unreadable password kills agentd main per G46, so no live window where opencode talks to a gated proxy without credentials other than the fatal-boot case).

## Assumption validated (Rule 7)

**opencode v1.18.10 (the pinned runtime version) applies remote-entry `headers` to every JSON-RPC request, including `initialize`.** Verified against the tagged source (`packages/opencode/src/mcp/index.ts`): config `mcp.headers` flows into `requestInit` for both `StreamableHTTPClientTransport` and `SSEClientTransport` — transport-level init applied to all requests, not per-call. `MATERIALIZE-CONTRACT.md` §remote shape documents `headers` as schema-supported (`McpRemoteConfig`, pinned schema). If a future opencode version regresses this, the MCP tools fail visibly (`initialize` 401) rather than silently exposing data — fail-closed direction.

---

## Key Decisions

1. **Headers in the injected entry** (issue fix-option 1 + the transport-level half of option 3) rather than dropping `session_read`/`session_list` (option 2) — the tools are the documented value of the platform MCP server; removing them changes agent behavior for all workspaces.
2. **Stacked on the #762 branch** — the gate reuses its shared auth helpers; PR is retargeted to main after #762 merges.
3. **Non-fatal password read in materialize** mirrors the existing materialize error semantics (log + continue, exit code reserved for credential I/O per its comment); the fatal enforcement lives in agentd main (G46).

---

## Blockers

None.

---

## Tests Run

- New: `TestMCPHandler_RequiresAuth` (401 + challenge on unauthenticated JSON-RPC), `TestMCPHandler_WrongPassword`, `TestInjectAgentdMCPServer_EmptyConfig` extended to assert the `Authorization` header value, `TestEnsureBootAgentConfig_*` asserts headers on the stamped entry.
- Updated: all existing `/v1/mcp` handler tests send the Basic credential.
- `go test -run 'TestMCP|TestInjectAgentdMCPServer|TestCallMCPTool|TestEnsureBootAgentConfig'` — ok (0.26s)
- `go test -run 'TestMaterialize|TestPreBootRelay|TestRunMaterialize|TestSecrets|TestReload'` — ok (49s)
- Full `./cmd/workspace-agentd/` suite — ok (see CI)
- `golangci-lint run --new-from-merge-base=origin/main` — 0 issues

---

## Next Steps

- PR 3 (#848): gate `/v1/reload-secrets` + `/v1/agent/reload`; wire `agentpush.Service.Push` and both `AgentReloadHandler` dispatch sites.
- Follow-up: true in-pod mitigation (uid separation) — same follow-up issue as #762.

---

## Files Modified

- `cmd/workspace-agentd/mcp_server.go`
- `cmd/workspace-agentd/mcp_server_test.go`
- `cmd/workspace-agentd/boot_config.go`
- `cmd/workspace-agentd/boot_config_test.go`
- `cmd/workspace-agentd/pre_boot_relay.go`
- `cmd/workspace-agentd/pre_boot_relay_test.go`
- `cmd/workspace-agentd/secrets.go`
- `cmd/workspace-agentd/main.go`
<<<<<<<< Updated upstream:worklogs/0769_2026-08-15_agentd-mcp-proxy-auth.md
- `worklogs/0769_2026-08-15_agentd-mcp-proxy-auth.md` (this file)
========
- `worklogs/0768_2026-08-15_agentd-mcp-proxy-auth.md` (this file)
>>>>>>>> Stashed changes:worklogs/0768_2026-08-15_agentd-mcp-proxy-auth.md
