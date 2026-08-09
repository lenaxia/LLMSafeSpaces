# Worklog: US-65.1 — AgentConfigWriter Containment (move config rendering behind pkg/agent/opencode/)

**Date:** 2026-08-09
**Session:** US-65.1 containment extraction — relocate opencode config rendering from agentd's package main into pkg/agent/opencode/, the existing agent-integration seam.
**Status:** Complete

---

## Objective

Move the opencode config-shape knowledge (the AgentConfigWriter, 593 lines, all 8 sources: providers/model/relay/mcpServers/adminPrompt/agentRaw/modeRaw/allowedDirs) from `cmd/workspace-agentd/agent_config_writer.go` (package main) into `pkg/agent/opencode/` so it is importable, independently testable, and behind the existing boundary per Epic 65 / Rule 12 (Containment Before Abstraction).

Discovery during study: US-46.10 already shipped the single-writer consolidation that the original US-65.1 design doc proposed. The "multiple writers racing" fragility (#1 in the design doc's C1 list) is resolved. What remained was the *containment* gap — the writer lived in package main, scattering opencode schema knowledge outside the pkg/agent/opencode/ seam. This PR closes that gap. No new interface was introduced (Rule 12: one consumer, no recurring pain in the writer seam itself).

---

## Work Completed

### Relocated the writer to pkg/agent/opencode/

- New `pkg/agent/opencode/configwriter.go` (~580 lines): the `ConfigWriter` type (renamed from `AgentConfigWriter`), exported setters (`SetProviders`, `SetModel`, `SetRelay`, `SetMCPServers`, `Rebuild`, `HasRelay`), exported data types (`RelayModel`, `MCPServerEntry`), unexported helpers (`relaySource`, `parseRelayFromExisting`, `buildRelayProviderEntry`, `atomicRenameWrite`, `loadExisting`, `loadAdminPrompt`, `loadAllowedDirs`).
- Constructor changed from `newAgentConfigWriter(path)` (hardcoded `agentd.AdminPromptPath`/`agentd.AllowedDirsPath`) to `NewConfigWriter(path, opts...)` with three options: `WithAdminPromptPath`, `WithAllowedDirsPath`, `WithPreMarshalHook`. The opencode package no longer imports `pkg/agentd` for paths — paths are caller-supplied.
- The `injectAgentdMCPServer(cfg)` call (which injects the agentd built-in admin MCP server referencing `agentd.AgentdAdminPort`) became a `preMarshalHook` callback. agentd registers its `injectAgentdMCPServer` function via `WithPreMarshalHook` at construction; the opencode package knows nothing about agentd's admin port. Clean separation of concerns.

### Moved tests + testdata

- New `pkg/agent/opencode/configwriter_test.go` (~700 lines, `package opencode`): merged three former agentd test files (`agent_config_writer_test.go`, `agent_config_writer_schema_test.go`, `allowed_dirs_regression_test.go`) + the three `TestLoadExisting_*` tests from `relay_injector_test.go` (these access the unexported `w.relay` field, so they must live in the opencode package). All method calls updated to exported names; `relayModel` → `RelayModel`; `mcpServerEntry` → `MCPServerEntry`.
- Moved `testdata/opencode-config.schema.json` + `testdata/REFRESH.md` from `cmd/workspace-agentd/testdata/` to `pkg/agent/opencode/testdata/`. The authoritative LLMSafeSpaces#486 schema-validation harness now travels with the code it validates.
- New `cmd/workspace-agentd/schema_helper_test.go`: a local `assertMatchesOpencodeSchema` helper for agentd integration tests (e.g. `mcp_e2e_test.go`'s `applyMCPServersToConfig` path) that validates against the shared pinned schema via relative path. Avoids duplicating the 226KB schema file and avoids making jsonschema a production dependency of the opencode package.

### Updated all agentd callers

- `main.go`: `opencode.NewConfigWriter(path, WithAdminPromptPath(...), WithAllowedDirsPath(...), WithPreMarshalHook(injectAgentdMCPServer))`.
- `pre_boot_relay.go`: `opencode.NewConfigWriter(path)` (no options — materialize already rendered adminPrompt/allowedDirs; loadExisting preserves them).
- `secrets.go`: `*opencode.ConfigWriter` type, `SetProviders`, `Rebuild`, `opencode.MCPServerEntry`.
- `server.go`: `*opencode.ConfigWriter`, `HasRelay`.
- `relay_injector.go`: deleted the local `relayModel` struct (now `opencode.RelayModel`), `*opencode.ConfigWriter`, `SetRelay`, `Rebuild`, `HasRelay`.
- Test files (`relay_injector_test.go`, `reload_credentials_e2e_test.go`, `secrets_test.go`): constructor + method renames + `opencode.RelayModel` + imports.

### Deleted

- `cmd/workspace-agentd/agent_config_writer.go`
- `cmd/workspace-agentd/agent_config_writer_test.go`
- `cmd/workspace-agentd/agent_config_writer_schema_test.go`
- `cmd/workspace-agentd/allowed_dirs_regression_test.go`
- `cmd/workspace-agentd/testdata/` (moved, not deleted)

---

## Key Decisions

1. **Move the whole writer, not just rendering.** Considered extracting only the opencode config rendering as a pure function (`RenderConfig(sources) []byte`) and keeping the orchestration (mutex, file I/O, source management) in agentd. Rejected: the rendering IS the opencode knowledge, and it's tangled with the source management (each source has its own merge branch in Rebuild). Moving the whole writer puts all of it behind the seam in one move and preserves the caller pattern (setX + Rebuild) that 3 callers already use.

2. **preMarshalHook for the agentd admin MCP server.** `injectAgentdMCPServer` references `agentd.AgentdAdminPort` — that's LLMSafeSpaces-platform-specific, not opencode knowledge. A hook callback keeps it in agentd while letting the opencode package call it during Rebuild. Alternative considered: making the builtin MCP server a regular `MCPServerEntry` source — rejected because it's a single special entry injected unconditionally, not a managed source list.

3. **No new interface.** The original US-65.1 design proposed an `AgentConfigWriter interface` with `Apply(input) → restartRequired`. The code study proved US-46.10 already delivered the single-writer consolidation. Building an interface now (Rule 12: one consumer, no recurring pain in the writer seam) would be the speculative-abstraction tax. This PR is pure containment — moving concrete code behind a boundary, no abstraction.

4. **Schema helper stays test-only.** Exporting `AssertMatchesOpencodeSchema` from the opencode package would make `github.com/santhosh-tekuri/jsonschema/v6` a production dependency. Kept the helper in test files; agentd's integration tests get their own thin helper that references the shared testdata via relative path.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — clean (exit 0)
- `go vet ./cmd/workspace-agentd/... ./pkg/agent/opencode/...` — clean (exit 0)
- `go test -timeout 60s ./pkg/agent/opencode/...` — PASS (10.9s)
- `go test -timeout 90s -short ./cmd/workspace-agentd/...` — PASS (63.2s)
- `go test -timeout 60s -run "ConfigWriter|AllowedDirs|LoadExisting|MCP|Relay|ReloadSecrets" ./pkg/agent/opencode/... ./cmd/workspace-agentd/...` — PASS (opencode 0.15s, agentd 5.8s)

---

## Next Steps

US-65.1 is complete. The opencode config-shape knowledge is now fully contained behind `pkg/agent/opencode/`. Per Epic 65 sequencing:

1. **US-65.2 (pkg/session contract types)** — the platform-owned session/message/event contract. ~3 days. Independent of opencode adapter work.
2. **Epic 63 (V2 session API adoption)** — ~2.5 weeks (revised down from 3 after F15 correction). Gates US-65.3.
3. **US-65.3 (opencode adapter)** — gated on Epic 63; wraps the V2 client 63 builds.

US-65.6 (repolint import-rule enforcement) can now add a rule: `pkg/agent/opencode/` may only be imported by `api/internal/app/`, `cmd/workspace-agentd/`, and `pkg/agent/`. Worth doing soon to lock in the boundary this PR established.

---

## Files Modified

**Created:**
- `pkg/agent/opencode/configwriter.go` (moved + adapted from agent_config_writer.go)
- `pkg/agent/opencode/configwriter_test.go` (merged from 3 agentd test files + 3 LoadExisting tests)
- `cmd/workspace-agentd/schema_helper_test.go` (local schema validator for agentd integration tests)

**Moved:**
- `cmd/workspace-agentd/testdata/opencode-config.schema.json` → `pkg/agent/opencode/testdata/`
- `cmd/workspace-agentd/testdata/REFRESH.md` → `pkg/agent/opencode/testdata/`

**Modified (agentd callers):**
- `cmd/workspace-agentd/main.go` (constructor + import)
- `cmd/workspace-agentd/server.go` (type + HasRelay + import)
- `cmd/workspace-agentd/secrets.go` (type + SetProviders + MCPServerEntry + Rebuild)
- `cmd/workspace-agentd/relay_injector.go` (deleted relayModel struct, type + SetRelay + Rebuild + HasRelay + import)
- `cmd/workspace-agentd/pre_boot_relay.go` (constructor + RelayModel + SetRelay + Rebuild + import)
- `cmd/workspace-agentd/relay_injector_test.go` (constructor + HasRelay + import; removed 3 moved tests)
- `cmd/workspace-agentd/secrets_test.go` (constructor + SetRelay + RelayModel + import)
- `cmd/workspace-agentd/reload_credentials_e2e_test.go` (constructor + import)

**Deleted:**
- `cmd/workspace-agentd/agent_config_writer.go`
- `cmd/workspace-agentd/agent_config_writer_test.go`
- `cmd/workspace-agentd/agent_config_writer_schema_test.go`
- `cmd/workspace-agentd/allowed_dirs_regression_test.go`

**Docs (prior turns, for context):**
- `design/0049_2026-08-09_agent-session-contract.md`
- `design/stories/epic-65-agent-session-contract/README.md`
- `design/stories/epic-63-inboard-session-queue/README.md` (F15 correction)
- `design/stories/README.md` (epic 65 added)
- `README-LLM.md` (version 1.24, contract section, Rule 8 table)
