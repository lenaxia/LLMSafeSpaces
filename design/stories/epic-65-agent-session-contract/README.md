# Epic 65: Agent Session Contract — Decouple the Platform from opencode

**Status:** Definition (not yet in implementation)
**Created:** 2026-08-09
**Priority:** High — eliminates the single largest source of hacks and jury-rigs in the codebase; unblocks mobile-first-class UX and multi-agent viability
**Depends On:** The existing `pkg/agent/opencode/` seam (`AgentRuntime` + `Dialect`, folded into `Adapter` by US-65.3), Epic 30 (Unified Credential Model — `FormatProviderConfig` stays).

> **Note on Epic 29:** Epic 29's interface-extraction goal (US-29.1 `AgentClient`) is superseded by this epic — `Adapter` (US-65.3, design 0049 §4.6) is a strict superset that folds `AgentRuntime` + `Dialect` + `AgentClient` into one seam. The seam Epic 65 depends on already exists today (`pkg/agent/agent.go:31`, `pkg/agent/dialect.go:10`, `pkg/agent/opencode/`); Epic 29 need not ship first. Only Epic 29's handler-decomposition stories (US-29.2–29.8) remain, and they are orthogonal cleanup, not a prerequisite.
**Authoritative for:** How the platform integrates any coding agent. The contract that web, mobile, SDK, and MCP all consume; the adapter seam that contains all agent-specific knowledge.

**Design document:** [`design/0049_2026-08-09_agent-session-contract.md`](../../0049_2026-08-09_agent-session-contract.md)

---

## Problem Statement

### Current State

The platform runs `opencode serve` in every workspace pod. opencode's implementation details leak across 70+ files outside the existing `pkg/agent/opencode/` seam. The five shapes of coupling (C1–C5) are documented in `design/0049 §2`; the summary:

- **C1 (config-merge):** `agent-config.json` write orchestration encodes opencode's last-writer-wins deep-merge, no-hot-reload (forces `proc.restart()`), `disabled_providers` relay injection, one-shot injector + 20s stale window. This is the documented canonical example of leakage (Rule 12). Recurring pain in the same seam.
- **C2 (HTTP contract):** The proxy parses opencode's response shapes — patch-part stripping (`?verbose`), history pagination/cap, question/permission event translation — in platform code. The existing `Dialect` covers paths/events but not response-shape translation.
- **C3–C5:** Process supervision, storage paths, credential format. Lower pain; C5 is already well-contained.

Every new opencode quirk becomes a platform hack. Every eventual agent swap becomes a rewrite. The frontend renders opencode's shapes directly, which blocks mobile-first-class UX (a structured web/mobile chat on both surfaces is required; terminal pass-through is insufficient for mobile's full-primary-session bar).

### Desired State

1. **A platform-owned session contract** (`pkg/session/`) — the single surface web, mobile, SDK, and MCP consume. Shaped by what a mobile coding session needs to render, NOT by what opencode emits.
2. **A single adapter seam** (`pkg/agent.Adapter`) — the only place agent-specific knowledge lives. The opencode adapter translates opencode's shapes to/from the contract. Platform code imports the interface, never an implementation.
3. **The AgentConfigWriter seam (C1)** — opencode's config-merge quirks move behind `Apply(input) → restartRequired`. Platform reacts to the signal; it doesn't encode the reason.
4. **Mobile + desktop + API as first-class peers** — all three consume the same contract. Terminal pass-through stays as an optional desktop power-user mode.
5. **Repolint enforcement** — a build-time invariant that `pkg/agent/opencode/` is never imported outside its allowed consumers.

### What this epic does NOT do

- Does not build a homegrown agent (the platform's moat is orchestration, not cognition — `design/0049 §1.1`).
- Does not build a second adapter (Rule 12 — one consumer does not validate interface shape).
- Does not design a universal agent protocol (ACP/A2A-style — the contract is LLMSafeSpaces's own).
- Does not make terminal pass-through the primary UX (mobile requires full structured sessions).

---

## Scope

### In scope

- **`pkg/session/` contract package** — `Session`, `Message`, `Part` (5-type union), `Event` (10 types), `InputRequest`, `SendOpts` (with `Admission` mode for steering), `Cost`, `ModelRef`/`ModelInfo`, `Capability` flags. ~150 lines of types. Validated against opencode, pi, oh-my-pi, claude-code, aider, strands (all 6 fit).
- **`pkg/agent.Adapter` interface** — folds the existing `Dialect` + `AgentRuntime` into one seam (~18 methods). Existing `Dialect` path/classification methods become private to the opencode adapter.
- **`AgentConfigWriter` seam (C1)** — `Apply(AgentConfigInput) (restartRequired bool, err error)`. The opencode implementation owns deep-merge, `OPENCODE_CONFIG`, `disabled_providers`, and the always-restart return value.
- **opencode adapter** — implements `Adapter` against the existing opencode HTTP API. Translates opencode parts → platform parts, opencode events → platform events, opencode questions/permissions → unified `InputRequest`.
- **Proxy handler migration** — `proxy.go`/`proxy_handlers.go`/`proxy_events.go`/`proxy_input.go`/`proxy_permissions.go` call `Adapter` instead of translating opencode shapes inline.
- **Hack deletion** — `proxy_filter*.go` (patch-part stripping, `?verbose`), opencode-shape history parsing, inline question/permission translation.
- **MCP tool surface** — `session_question_reply`/`reject` + `session_permission_reply` collapse into one `run_resolve` tool matching the unified `InputRequest`. `session_message` documents the sync-wrapper semantics.
- **Repolint rule** — forbids `pkg/agent/opencode` imports outside allowed consumers; flags new `PartType` constants for review.

### Out of scope (with rationale)

| Item | Reason |
|---|---|
| Second adapter (claude-code, codex, aider) | Rule 12 — not funded. One adapter validates weak spots, not universality. |
| `Run` abstraction (async job primitive) | Over-engineering. The assistant `Message` already has ID/status/result; `SendAsync` + poll covers programmatic callers. |
| `AutoResolvePolicy` (per-run permission policy) | Duplicates config. Permission policy is a workspace config concern handled by `ApplyConfig`. |
| `FooterState` event | Client-computed from events already received (session.status, message timing, cost). Pushing a UI concern into the contract forces the adapter to synthesize it. |
| Dedicated part types for todos/subagents/plan/shell | All are `ToolPart` with different `Name`. Tools are extensible; part types are not. |
| `Revert`/`Stashed`/`Tags` as contract types | Pass-through capabilities, not universal semantics. Modeled via capability flags + generic adapter methods. |
| Typed rewind/fork results | `json.RawMessage` until a second adapter validates a typed shape. |
| Terminal pass-through as primary UX | Mobile requires structured sessions. Terminal stays optional desktop mode. |
| Inbound multimodal as top-level part | Always tool output (screenshot tool → `Tool` part). No agent emits inline images as first-class content today. |

---

## Alternatives Considered

| Alternative | Assessment | Verdict |
|---|---|---|
| **Build a homegrown agent** | Inverts the moat. Agent loops are commoditized; edit formats + model tuning take years (aider's 357-entry `model-settings.yml`). | **Reject** — platform value is orchestration, not cognition. |
| **Full agent-provider abstraction now** | Violates Rule 12. One consumer. The interface would encode opencode's shape as universal (the relay-config leakage frozen into a contract). | **Reject** — contain first; abstract when a second adapter is funded or a forcing rewrite occurs. |
| **Terminal pass-through (Coder/Gitpod model)** | Makes interactive agent-agnostic for free. But mobile requires full primary sessions with diff review — xterm.js on a phone is Termius, not a product. | **Reject as primary; keep as optional desktop mode.** |
| **Switch off opencode (e.g. to pi)** | Orthogonal. The coupling is in the platform, not the agent. A new agent has its own quirks (pi's JSONL-tree sessions, claude-code's settings.json). | **Defer** — do the seam work first; a swap is then bounded to one package. |
| **Universal structured protocol (ACP/A2A)** | Huge investment; only pays off with many agents. The adapter model means this isn't needed. | **Reject** — the contract is LLMSafeSpaces's own. |
| **Run abstraction for programmatic callers** | The assistant `Message` already has ID + status (streaming→complete→error) + result. A `Run` duplicates this with its own lifecycle/store/events. | **Reject** — `SendAsync` + poll is the robust pattern; configurable timeout fixes the long-task case. |

---

## Discipline Rules (load-bearing — enforced by review + repolint)

1. **5 part types forever** (`Text`, `Reasoning`, `Tool`, `FileChange`, `Custom`). Adding a part type is a contract change requiring design-doc update.
2. **All tools are `ToolPart`** discriminated by `Name`, forever. No `PartTodo`, `PartEdit`, `PartSearch`.
3. **Agent/model switches are transcript events**, not side-band config.
4. **Diff text is authoritative** (`Patch string`), not hunk structs.
5. **Cost is optional everywhere and never billing.** Billing is cgroup-based.
6. **Agent-specific operations are pass-through** (`json.RawMessage` results) until a second adapter validates a typed shape.

---

## Stories

### US-65.1: AgentConfigWriter seam (C1)

**Goal:** Move opencode's config-merge quirks behind a single seam so platform code reacts to `restartRequired` without knowing why.

**Files:**
- New: `pkg/agent/agentconfig.go` — `AgentConfigWriter` interface, `AgentConfigInput`, `ModelSelection`, `RelayState`.
- Modified: `pkg/agent/opencode/agentconfig.go` — implementation that owns deep-merge, `OPENCODE_CONFIG` path, `disabled_providers`, always-restart return.
- Modified: `cmd/workspace-agentd/agent_config_writer.go`, `relay_injector.go`, `secrets.go` — call `Apply` instead of inline merge/restart logic.

**Done when:**
- `AgentConfigWriter.Apply` is the only write path to `agent-config.json` within agentd.
- The 20s stale window, one-shot injector, and `annotateModels` guard are inside the opencode implementation, not platform code.
- Platform code calls `Apply` and branches on `restartRequired`, not on opencode-specific reasons.

---

### US-65.2: `pkg/session/` contract types

**Goal:** The platform-owned session/message/event contract. ~150 lines of types.

**Files:**
- New: `pkg/session/session.go` — `Session`, `Status`, `ModelRef`, `Cost`, `TimeRange`, `Capability`.
- New: `pkg/session/message.go` — `Message`, `MessageType`, `UserMessage`, `AssistantMessage`, `ShellMessage`, `AgentSwitchMessage`, `ModelSwitchMessage`, `CompactionMessage`, `SystemMessage`.
- New: `pkg/session/part.go` — `Part`, `PartType` (5 types), `ToolPart`, `ToolState`, `ToolStatus`, `FileDiff` (payload of the `FileChange` part), `CustomPart`. (`Text`/`Reasoning` are inlined string fields on `Part`; no single-field wrapper structs. `ErrorPart` is deliberately excluded — errors are transient events, not content blocks; see design 0049 §4.1 rule 1.)
- New: `pkg/session/event.go` — `Event`, `EventType` (10 types), `InputRequest`, `InputKind`, `InputOption`, `SendOpts`, `Admission`, `Error` (payload of the `error` Event and optional turn-level error on `AssistantMessage`).
- New: `pkg/session/session_test.go` — JSON round-trip tests for every type; verify optional fields omit cleanly.

**Done when:**
- All types compile, round-trip through JSON, and contain zero agent-specific identifiers.
- `PartType` has exactly 5 constants (`Text`, `Reasoning`, `Tool`, `FileChange`, `Custom`); a test enforces the count. No `ErrorPart` — errors flow via the `error` Event payload (`Error{Code, Message}`) and optionally as a turn-level `Error` on `AssistantMessage`; `ToolState.Error` carries tool-call errors (tool state, not a part type).
- `EventType` has exactly 10 constants; a test enforces the count.
- `Text`/`Reasoning` are inlined string fields on `Part` (no single-field wrapper structs). The file-change payload type is `FileDiff`; the part field is named `FileChange` (type `*FileDiff`) — describes what the part *is*, not the data shape inside it.
- Type↔payload consistency validation is deferred to the adapter (`Part.Validate()`/`Message.Validate()` in US-65.3), not enforced in this types package.

---

### US-65.3: opencode adapter implementing the contract

**Goal:** Translate opencode's HTTP API and event stream to/from `pkg/session` types.

**Files:**
- Modified: `pkg/agent/opencode/adapter.go` (new, replaces `agent_client.go` + folds `dialect.go`) — implements `pkg/agent.Adapter`.
- Modified: `pkg/agent/opencode/format.go` — stays (behind `FormatProviderConfig`).
- The existing `Dialect` methods (`SessionMessagePath`, `IsSessionIdle`, `ParseQuestionRequest`, etc.) become private helpers.

**Key translation work:**
- opencode `AssistantContent` (text/reasoning/tool) → platform `Part` (text/reasoning/tool).
- opencode `patch` part → dropped (snapshot.files); `FileChange` parts produced from `session.diff` event or `git diff` on PVC at message completion.
- opencode `question.asked`/`permission.asked` → unified `InputRequest`.
- opencode `session.status` busy/idle → platform `Status`.
- opencode message IDs (`msg_`/`ses_`) → platform IDs (adapter keeps the mapping).

**Implementation risk (flagged in design doc §10):** `FileChange` hunks require verifying whether opencode's `session.diff` event carries enough data, or whether the adapter must `git diff` on the PVC. Prototype this before committing the adapter to hunk-bearing `FileChange`. Requires workspace to be a git repo (init on creation if not already).

**Done when:**
- `Adapter` interface fully implemented for opencode.
- A real opencode session (send message → get response → get history → stream events) round-trips through the contract with zero opencode identifiers in the output.
- `FileChange` production path is verified (event-based or git-diff-based).

---

### US-65.4: Migrate proxy handlers to Adapter

**Goal:** Platform proxy code calls `Adapter`, not opencode shapes.

**Files:**
- Modified: `api/internal/handlers/proxy.go`, `proxy_handlers.go`, `proxy_events.go`, `proxy_input.go`, `proxy_permissions.go`.
- Modified: `api/internal/app/app.go` — construct `Adapter` (opencode impl) and inject into `ProxyHandler`.

**Done when:**
- `ProxyHandler` holds an `agent.Adapter`, not an `agent.Dialect` + raw HTTP client.
- `proxy_handlers.go` history fetch returns `[]session.Message`, not opencode-shaped JSON.
- `proxy_input.go`/`proxy_permissions.go` call `adapter.ListPending`/`Resolve`, not inline translation.
- The 3 current `Dialect` consumers (`proxy.go`, `proxy_events.go`, `proxy_input.go`) call `Adapter` methods.

---

### US-65.5: Delete deprecated proxy hacks

**Goal:** Remove the hacks the contract makes redundant.

**Files:**
- Deleted: `api/internal/handlers/proxy_filter*.go` (patch-part stripping, `?verbose` flag).
- Deleted: opencode-shape history parsing in `proxy_handlers.go:205-483`.
- Modified: `pkg/types/session.go:60` — genericize the `SessionListItem.ParentID` comment (currently names the opencode `task` tool).

**Done when:**
- `?verbose` flag no longer exists (contract has no `patch` part to strip).
- History endpoint returns contract-shaped `[]Message`.
- No opencode-specific identifiers in `pkg/types/session.go` comments.

---

### US-65.6: Repolint import-rule enforcement

**Goal:** Build-time invariant that agent-specific knowledge stays behind the seam.

**Files:**
- Modified: `pkg/repolint/` — new rule: `pkg/agent/opencode/` may only be imported by `api/internal/app/` (construction) and `cmd/workspace-agentd/` (config writer). Everything else imports `pkg/agent` (interface) + `pkg/session` (types).
- Modified: `pkg/repolint/` — new rule: new `PartType` constants in `pkg/session/part.go` require a linked design-doc update (flags the diff for review).

**Done when:**
- A test import of `pkg/agent/opencode/` from an unauthorized package fails repolint.
- Repolint runs in CI and pre-commit.

---

### US-65.7: MCP tool surface migration

**Goal:** MCP tools consume the contract; question/permission tools collapse.

**Files:**
- Modified: `pkg/mcp/server.go` — `session_question_reply`/`reject` + `session_permission_reply` → one `run_resolve` tool taking `Resolution`. `session_message` documents sync-wrapper semantics (calls `Send` + waits with configurable timeout; `Send` + `GetHistory` is the robust pattern for long tasks).

**Done when:**
- MCP tools return contract-shaped results.
- `run_resolve` handles both question and permission via `InputKind`.
- `session_message` tool description documents the timeout caveat.

---

### US-65.8: Frontend migration to contract (can lag)

**Goal:** Web and mobile clients consume `pkg/session` types via OpenAPI-generated SDK.

**Files:**
- Modified: `sdks/openapi.yaml` — regenerate from contract types.
- Modified: `sdks/typescript/`, `sdks/python/` — regenerate.
- Modified: `frontend/` — replace opencode-shaped rendering with contract-shaped rendering.

**Done when:**
- Frontend renders `Part` types (text, reasoning, tool with state, file change, custom) from the contract.
- Mobile renders the same shapes.
- No opencode-specific parsing in any client.

---

## Story Dependency Graph

```
US-65.1 (AgentConfigWriter) ──────────────────────────────────────┐
                                                                    │
US-65.2 (pkg/session types) ──┬── US-65.3 (opencode adapter) ──┐  │
                              │                                  ├──┤
                              └── US-65.4 (proxy migration) ────┤  │
                                                                 │  │
                                  US-65.5 (delete hacks) ────────┤  │
                                                                 │  │
                                  US-65.6 (repolint) ────────────┤  │
                                                                 │  │
                                  US-65.7 (MCP surface) ─────────┤  │
                                                                 │  │
                                  US-65.8 (frontend) ────────────┘  │
                                                                      │
                                                       (independent)──┘
```

US-65.1 (AgentConfigWriter) is independent and can start immediately. US-65.2 is the foundation for the rest. US-65.8 (frontend) can lag the backend and proceed in parallel once the OpenAPI spec is regenerated.
