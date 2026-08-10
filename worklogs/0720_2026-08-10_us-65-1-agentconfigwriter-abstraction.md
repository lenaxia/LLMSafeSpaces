# Worklog: US-65.1 — AgentConfigWriter Abstraction (the deferred half)

**Date:** 2026-08-10
**Session:** Re-introduce the AgentConfigWriter interface that worklog 0700 deferred under Rule 12; user direction overrides the prior deferral.
**Status:** Complete

---

## Objective

US-65.1 was originally specified (design/0049 §5, epic-65 README) as a two-part story:

1. **Containment** — move opencode config-shape knowledge behind `pkg/agent/opencode/`.
2. **Abstraction** — introduce an `AgentConfigWriter` interface with `Apply(AgentConfigInput) (restartRequired bool, err error)` so platform code calls `Apply` and branches on `restartRequired` instead of knowing *why* a restart is needed.

Worklog 0700 shipped part 1 and explicitly deferred part 2 under Rule 12 (no speculative abstraction against a single consumer; no recurring pain in the writer seam itself). That was the right call at the time.

User direction in this session overrides that call. The justification: US-65.3 (opencode adapter) is the next story, and shipping it on top of an unsealed seam would propagate the leakage the design doc exists to fix. The abstraction is now the foundation US-65.3 builds on, not a speculative generalization.

The override is documented here honestly so future readers can see why the prior decision was reversed and judge whether the reversal was correct.

---

## Assumptions (Rule 7) — stated and validated

1. **The interface lives in `pkg/agent/agentconfig.go`** — same package as `AgentRuntime`, no import cycle. **Validated:** `pkg/agent` does not import `pkg/agent/opencode` today (grep verified); the reverse import is fine.
2. **`AgentConfigInput` is a partial-update payload** (pointer fields, `nil` = leave unchanged). **Validated** by tracing every existing call site of the writer:
   - `reloadSecretsHandler` (secrets.go:807) updates providers + MCP servers — model and relay must survive from boot and injection respectively.
   - `relay_injector` (relay_injector.go:347) updates relay only — providers came from init container's `FlushProviders`, model came from `workspace-config.json`.
   - `pre_boot_relay` (pre_boot_relay.go:267) updates relay only — same source-of-truth split.
   
   A literal reading of design/0049 §5 (which lists three sources and implies full-replace) would force every caller to read the writer's existing state to preserve sources it doesn't own. That inverts the writer's role as state-holder and re-introduces exactly the multiple-writer race fragility US-46.10 eliminated. The partial-update shape is the honest design; the design doc was stale on this point.
3. **The interface has two methods: `Apply` and `HasRelay`.** **Validated:** `HasRelay` is read by `server.go` readyz and `relay_injector.go` short-circuit. Removing it would require pushing that signal through `Apply`'s return value, which is wrong — it's a read, not a write.
4. **The interface returns `restartRequired bool`.** For the opencode implementation this is always `true` (no hot reload). **Validated** against the existing `proc.restart()` pattern in callers. The contract is what matters — a future hot-reload adapter returns `false` and the restart machinery no-ops.
5. **The existing exported setters on `*opencode.ConfigWriter` remain** as impl details (not in the interface). **Validated:** test code in `secrets_test.go:589` uses `SetRelay` as a test seam to pre-seed state. Migrating every test to `Apply` would be churn without benefit; the setters stay for backward compat.
6. **Repolint boundary from US-65.6 stays unchanged.** **Validated:** this PR doesn't change import boundaries. Callers in `cmd/workspace-agentd/` still import `pkg/agent/opencode` for construction (allowed) and for `FormatOpenCodeConfig`/`NewClient` (legitimate agentd-scope usage). Repolint still reports 6 known leaks tolerated, no new leaks introduced.

---

## Work Completed

### New: `pkg/agent/agentconfig.go` — the seam

- `AgentConfigWriter` interface — two methods: `Apply(AgentConfigInput) (bool, error)`, `HasRelay() bool`.
- `AgentConfigInput` — partial-update payload with four pointer fields (`Providers`, `Model`, `Relay`, `MCPServers`). Nil = leave source unchanged; non-nil zero-value = clear source.
- `AgentProvidersChange`, `ModelSelection`, `RelayState`, `RelayModel`, `MCPServerChange`, `MCPServerEntry` — the input payload types. Agent-neutral at the type level; the opencode adapter converts.

### New: `pkg/agent/agentconfig_test.go`

- Pin the zero-value contract (nil fields = leave unchanged).
- Pin the partial-update contract (each caller fills in only the source it owns).
- Pin the `RelayState` clear-vs-unchanged distinction (`&RelayState{}` vs `nil`).
- Pin the interface satisfaction via a fake implementation (`var _ AgentConfigWriter = (*fakeConfigWriter)(nil)`).
- Pin that Apply on an empty input still returns restart=true (opencode always restarts).

### Modified: `pkg/agent/opencode/configwriter.go`

- Added `Apply(in agent.AgentConfigInput) (bool, error)` implementing the interface. Holds `w.mu` for the entire source-update + write cycle so concurrent Apply calls serialize atomically.
- Added `rebuildLocked()` extracted from `Rebuild()` (now wraps `rebuildLocked` with lock acquire/release). Apply calls `rebuildLocked` directly to avoid double-locking.
- Added `setProvidersLocked(formattedConfig []byte) error` extracted from `SetProviders`. Same atomicity rationale.
- Compile-time assertion: `var _ agent.AgentConfigWriter = (*ConfigWriter)(nil)`.
- The opencode-specific rendering (`$schema`, `disabled_providers`, `opencode-relay` provider block, `agent.build.prompt` merge, `mode.permissions.external_directory` merge, `mcp` section, `preMarshalHook`) stays entirely in `rebuildLocked` — none of it leaks through the interface.

### New: `pkg/agent/opencode/configwriter_apply_test.go`

- 14 tests covering Apply's contract: nil-input no-op, each source alone, partial-update preserves unchanged sources, clear semantics for each source, atomic write, concurrent serialization, backward compat with existing setters, and the F1 atomicity regression test.
- F1 regression: `TestApply_AtomicAcrossSources` runs 50 goroutines concurrently, half updating providers, half updating relay. Without atomicity, the final file would be inconsistent. With atomicity, it's always valid JSON. This test was red before the F1 fix (Apply used to call Set/Rebuild which each locked independently).

### Modified: `cmd/workspace-agentd/server.go`

- `serverDeps.agentConfigWriter` type: `*opencode.ConfigWriter` → `agent.AgentConfigWriter`.
- Removed now-unused `opencode` import; added `pkg/agent`.

### Modified: `cmd/workspace-agentd/relay_injector.go`

- `relayInjectorConfig.AgentConfigWriter` type: `*opencode.ConfigWriter` → `agent.AgentConfigWriter`.
- Injection call site: `SetRelay + Rebuild` → single `Apply` with `Relay` source. Branches on `restartRequired` to skip the kill+restart if a future adapter reports no restart needed (today always true for opencode; the branch documents the contract).
- Added `relayModelsToAgent` helper — converts the opencode-layer `RelayModel` slice to the agent-layer type used by the seam. Inverse of `relayModelsFromAgent` in `configwriter.go`. Type-distinctness is the point per Rule 12.

### Modified: `cmd/workspace-agentd/pre_boot_relay.go`

- Pre-boot injection call site: `NewConfigWriter + SetRelay + Rebuild` → `NewConfigWriter + Apply` with relay source. The `restartRequired` bool is discarded (opencode hasn't started yet; there's nothing to restart).

### Modified: `cmd/workspace-agentd/secrets.go`

- `reloadSecretsDeps.AgentConfigWriter` type: `*opencode.ConfigWriter` → `agent.AgentConfigWriter`.
- Reload handler write path: `SetProviders + SetMCPServers + Rebuild` → single `Apply` with `Providers` + `MCPServers` sources. Builds the input struct once, fills in only the sources this caller owns. The model and relay sources already on the writer (from boot and injection respectively) are preserved automatically.
- The 500-on-failure error message changed from "agent-config rebuild" to "agent-config apply" to match the new method name.

---

## Key Decisions

1. **Override worklog 0700's Rule 12 deferral.** Justification recorded in the package doc and this worklog: US-65.3 is the next story and depends on this seam. Building the adapter on top of an unsealed seam propagates the leakage the design doc exists to fix. Rule 12's test ("recurring pain in the same seam") is debatable today but unambiguous once the adapter is funded work — which it is.

2. **Partial-update input, not full-replace.** design/0049 §5 literally specifies three sources and implies full-replace. Code-study of the call sites proved every caller updates ONE source per Apply call. Full-replace would force callers to read+preserve state they don't own — inverting the writer's role. Partial-update (nil = leave unchanged) matches actual caller behavior. The design doc was stale on this point.

3. **Two-method interface, not one.** `HasRelay` is a read used by readyz and the relay-injector short-circuit. Removing it would push the signal through `Apply`'s return value, which conflates reads with writes. Keep the read separate.

4. **Keep the existing exported setters on `*opencode.ConfigWriter`.** They're not in the interface but stay as impl methods. Test code uses them as test seams to pre-seed state (e.g., `secrets_test.go:589` `writer.SetRelay(...)` to simulate "relay injector already ran"). Migrating every test to Apply would be churn without benefit.

5. **Atomicity is part of the Apply contract.** Adversarial review (F1) caught that the first implementation called Set/Rebuild which each locked independently — concurrent Apply calls could interleave such that one caller's update was lost. Refactored Apply to hold `w.mu` for the entire merge+write cycle, with internal `rebuildLocked`/`setProvidersLocked` helpers. Regression test `TestApply_AtomicAcrossSources` pins the contract.

---

## Adversarial Self-Review (Rule 11)

| # | Finding | Class | Resolution |
|---|---|---|---|
| F1 | Apply was not atomic across Set+Rebuild — concurrent calls could interleave, last writer wins, caller's view of what got persisted was wrong | **Real bug** | **Fixed.** Refactored Apply to hold `w.mu` for the entire merge+write cycle. Added `rebuildLocked`/`setProvidersLocked` internal helpers. Regression test `TestApply_AtomicAcrossSources` (50 goroutines, mixed providers/relay updates) verifies the file is always valid JSON. |
| F2 | `clearProviders`/`clearRelay` acquired lock independently (same root cause as F1) | Real (same fix) | **Fixed with F1** — the public `clearX` methods were deleted; Apply mutates `w.providerRaw`/`w.relay` directly under the held lock. |
| F3 | `relayModelsToAgent` (relay_injector.go) duplicates the inverse of `relayModelsFromAgent` (configwriter.go) | Acceptable (Rule 12 tradeoff) | No fix. Type-distinctness between `agent.RelayModel` and `opencode.RelayModel` is the point — the seam shouldn't export the agent-specific type. Both converters are ~10 LoC; if a field is added on one side, the converter needs updating in both places. |
| F4 | `ModelSelection("")` clears the model source | Correct as documented | No fix. Empty string is the documented clear semantic; matches how pointer-to-zero-value clears the other sources. |
| F5 | `applyWorkspaceConfig` in `secrets.go` writes the model directly to disk, bypassing the writer | Correct (separate process) | No fix. The materialize subcommand runs as a separate process before agentd starts; it owns the file exclusively. The agentd-process writer (`main.go`) is constructed after materialize completes, and `loadExisting` reads the post-materialize state. |
| F6 | `HasRelay` short-circuit in `relay_injector.go:269` reads state set by `loadExisting` | Correct | No fix. When pre-boot relay succeeds, `loadExisting` populates `w.relay` from the existing file. The check correctly skips redundant in-pod injection. |
| F7 | `restartRequired` contract applied correctly in `relay_injector.go` | Correct | No fix. Branches on the bool; for opencode today always true so kill+restart always fires. For a future hot-reload adapter, the kill is correctly skipped. |
| F8 | `restartRequired` correctly discarded in `reloadSecretsHandler` | Correct | No fix. The reload path's restart decision is independent of the config write — it's gated on `shouldRestart(batch)` (env secrets changed) downstream of the Apply call. |
| F9 | Repolint boundary from US-65.6 stays intact | Validated | No fix. Migration changed types from `*opencode.ConfigWriter` to `agent.AgentConfigWriter` in 3 deps structs, but those files still import `pkg/agent/opencode` for legitimate construction/usage. Repolint reports 6 known leaks tolerated, same as before this PR. |
| F10 | Comments are long | Acceptable | No fix. The Rule 12 override rationale (package doc on `agentconfig.go`), the partial-update-vs-full-replace decision (Apply doc), and the F1 atomicity contract (Apply + rebuildLocked docs) are load-bearing context future maintainers need. Trimming them would hide WHY the design diverges from design/0049 §5's literal spec. |

**Phase 2 result:** zero unresolved real findings in US-65.1 scope.

---

## Blockers

None.

---

## Tests Run

- `go build ./...` — clean (exit 0)
- `go vet ./...` — clean (exit 0)
- `gofmt -l pkg/agent/ cmd/workspace-agentd/` — clean (no diffs)
- `go test -timeout 30s -count=1 ./pkg/agent/` — PASS
- `go test -timeout 60s -count=1 -race ./pkg/agent/opencode/` — PASS (13.3s with race detector)
- `go test -timeout 90s -short -count=1 ./cmd/workspace-agentd/` — PASS (62.6s)
- `go test -timeout 240s -short -count=1 -race -run "Relay|Reload|Materialize|PreBoot|Apply" ./cmd/workspace-agentd/` — PASS (7.2s with race detector)
- `go test -timeout 240s -short -count=1 ./...` — PASS (every package)
- `go run ./cmd/repolint` — `ok agent-import boundary (6 known leak(s) tolerated pending US-65.4)`; all checks passed

---

## Next Steps

US-65.1 is complete (containment from worklog 0700 + abstraction from this worklog). Per Epic 65 sequencing:

1. **US-65.3 (opencode adapter)** — the next story. Implements `pkg/agent.Adapter` (~18 methods) against the contract types landed in US-65.2 and the seam landed in this PR. Starts with the FileChange production-path prototype (the design-doc §10 open risk).
2. **US-65.6-followup** — small (~1 day). Retires 3 of the 6 knownLeaks by centralizing `ErrNoRunningPod` and explicit boot wiring. Optional before US-65.3 but worth doing to tighten the boundary.

---

## Files Modified

**Created:**
- `pkg/agent/agentconfig.go`
- `pkg/agent/agentconfig_test.go`
- `pkg/agent/opencode/configwriter_apply_test.go`
- `worklogs/0720_2026-08-10_us-65-1-agentconfigwriter-abstraction.md`

**Modified:**
- `pkg/agent/opencode/configwriter.go` (Apply method + rebuildLocked/setProvidersLocked extraction)
- `cmd/workspace-agentd/server.go` (type change + import swap)
- `cmd/workspace-agentd/relay_injector.go` (type change + Apply migration + relayModelsToAgent helper)
- `cmd/workspace-agentd/secrets.go` (type change + Apply migration)
- `cmd/workspace-agentd/pre_boot_relay.go` (Apply migration)
