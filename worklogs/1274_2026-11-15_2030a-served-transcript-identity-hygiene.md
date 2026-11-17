# Worklog: #2030a — served-transcript identity-stamp hygiene (replay hygiene, the model's view)

- **Story**: tsk-windy-windmill-hovers (`#2030a: strip identity stamps from the SERVED transcript`)
- **Branch/worktree**: `fix/2030a-served-transcript-hygiene` at `/tmp/opencode/2030a-serve-hygiene` (off `origin/main` 71cb54f80)
- **Date**: 2026-11-15

## Context

Issue #2030 comment 'Live instance 2026-11-15' (the injection-metadata bleed subclass) + the #1374/#1307 transcript-repair precedent. Adjudicated mechanism: the MCP boundary stamps every platform tool call with `lsp_injected_session` (opencode: the first-party origin plugin, `runtimes/opencode/plugins/llmsafespaces-origin.js`, inside the pinned harness; the boundary reads it back at `mcp_server.go` for attribution); stamped args persist into session history; every later turn replays the stamp into model context; the model pattern-continues it — benign extra-field most times, value-replacement into a semantic param under the bleed condition (live-verified on `board_import_plan` `projectId`).

**Charter update mid-flight** (harness-agnostic requirement, owner ruling 2026-11-15): NOT an opencode-only patch — the hygiene contract goes through the harness-provider surface (the S5 registry-built Adapter seam), implemented for BOTH in-tree harnesses (opencode AND pi), with a loud startup/failure signal for a non-compliant adapter (no silent per-harness gaps).

## What landed

M1 — serve-side strip, storage byte-intact (the #1374/#1307 discipline: repair the model's VIEW, keep the record):

1. **The contract** (`pkg/agent/transcripthygiene.go`, NEW): `IdentityStampKeys` (`lsp_injected_session`, `sessionId`, `session_id` — lockstep with the accept-side `boardWireBody` / `boardIdentityInlineKeys`), `StripIdentityStampKeys` (pure map strip), the `TranscriptHygiene` interface (`StripIdentityStamps(served []byte) []byte` over the harness's own wire shape), and `TranscriptHygieneFor` (loud type-assert helper naming the concrete type).
2. **Registry enforcement** (`pkg/agent/agent.go` `NewAdapterForType`): every registry-built adapter must provide the contract — a non-compliant factory product fails construction loudly (startup-fatal for a default harness, per-call error for a lazy one). Third-harness compliance = implement the method.
3. **opencode** (`pkg/agent/opencode/transcripthygiene.go` NEW + two choke points): `stripIdentityStampsJSON` wired into `AcpToolCallFromNative` (`tc.RawInput` — covers BOTH wire generations, flat 1.18.x and legacy nested, since both decode into `ocTool.Input`) and `translateV2Tool` (`tp.Input`); package-level `StripIdentityStamps(body)` walks the raw V1 message list (shape-aware: only tool parts' `state.input`/`tool.input`) and is wired into `mcpSessionRead` (`cmd/workspace-agentd/mcp_server.go`) beside the #1307 image strip and #1756 phrase rewrite — never-break-the-payload rule, byte-stable when clean.
4. **pi** (`pkg/agent/pi/transcripthygiene.go` NEW + one choke point): `stripIdentityStampsJSON` wired into `translateAssistantMessage` (the served ToolPart's `Input`, composed from `/workspace/pi-sessions` entries via get_entries — the driver-owned path); the Adapter method strips the raw entries shape (entry → message.content blocks of type `toolCall` → arguments). Session files untouched.

Heal-forward: no migration, no storage rewrite — the next serve of any existing session is stamp-free. Accept-side stamping untouched.

## Verification (all on the worktree head)

- **Born-RED pins, mutation-verified** (each arm neutered → its pin RED, restored → green):
  - M1 opencode raw serve → `TestMCPSessionRead_StripsIdentityStamps` (cmd/workspace-agentd) RED.
  - M2 opencode translation tier → `TestAcpToolCallFromNative_StripsStamps` (pkg/agent/opencode) RED.
  - M3 pi translation tier → `TestTranslateAssistantMessage_StripsStamps` (pkg/agent/pi) RED.
  - M4 registry enforcement → `TestNewAdapterForType_NonHygienicFactoryProductFailsLoud` (pkg/agent) RED.
- **Pin (a)** zero identity keys in served tool args, semantic params (`projectId`, `destination`, `message`) survive; **pin (b)** stored fixtures keep stamps (pure serve-side strip); byte-stability pins (clean payloads byte-identical); never-break pins (non-array/garbage/non-object passthrough); composition-with-#1307 pin (stamps+image strip in one pass).
- **Suites green**: pkg/agent, pkg/agent/pi, pkg/agent/opencode full (12.3s — includes the #1374 transcript-repair and #1307 vision-gate families), api/internal/app (router wiring under enforcement), cmd/workspace-agentd full 537s EXCEPT `TestSupervisePiSubprocess_GracefulShutdown`/`_RepeatSignalExitsAtOnce` — **pre-existing environmental flakes: re-verified FAILING IDENTICALLY on stashed pristine origin/main** (real-subprocess 60s timeouts, unrelated to this change).
- `go build ./...` clean; `go vet` clean; `golangci-lint run` 0 issues; `make fmt-check` clean; gofmt clean.

## Notes / deviations

- The charter's "opencode (its transcript service path)" resolved to the loopback seam's raw serve (`mcpSessionRead`) + the adapter's ToolPart translation — the two platform-owned serves of opencode transcripts. The harness binary's own request composition is upstream-pinned and not platform-rewritable; the relay-router aged-arg path (the #1949 class) remains the request-side seam for relay-fronted traffic and is explicitly OUT of this story's scope (M1 is the transcript-service mirror of the accept-side strip).
- pi has no stamping plugin today (its MCP client surface would stamp via the same boundary contract when one lands); the pi implementation makes the DRIVER-owned serve path hygienic regardless — the contract holds before the first pi-side stamp exists.
- The shared test fake in `pkg/agent/adapter_test.go` gained a contract method (compliance); the non-compliant shape is a dedicated fake (`nonHygienicAdapter`, wrong-signature shadow) for pin (e).

## Files

- `pkg/agent/{transcripthygiene.go (NEW),transcripthygiene_test.go (NEW),agent.go,adapter_test.go}`
- `pkg/agent/opencode/{transcripthygiene.go (NEW),transcripthygiene_test.go (NEW),acpvocab.go,translate.go}`
- `pkg/agent/pi/{transcripthygiene.go (NEW),transcripthygiene_test.go (NEW),translate.go}`
- `cmd/workspace-agentd/{mcp_server.go,transcript_hygiene_2030a_test.go (NEW)}`
- `CHANGELOG.md`, `worklogs/1274_2026-11-15_2030a-served-transcript-identity-hygiene.md`, `COORDINATE.md`
