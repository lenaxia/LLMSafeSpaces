# Agent Session Contract — Decoupling the Platform from opencode (2026-08-09)

> **Status:** Design — Authoritative for agent integration architecture
> **Epic:** [Epic 65](stories/epic-65-agent-session-contract/README.md)
> **Supersedes:** The coupling status quo documented in Rule 12 and the "Relay Config Subsystem" fragility list. Does NOT supersede `design/0021_evolution-v2.md` (system architecture); this doc refines the agent-integration layer of §7.

---

## 1. Motivation

LLMSafeSpaces runs `opencode serve` inside every workspace pod and reverse-proxies to it. The platform's value is the orchestration, isolation, multi-tenancy, and billing layer — **not** the agent loop. Today, opencode's implementation details leak across 70+ files in `api/`, `controller/`, `cmd/workspace-agentd/`, and `pkg/`. Every accommodation of opencode's config-merge semantics, response shapes, and process model is a hack that makes the next change a jury-rig and an eventual agent swap a rewrite.

This document specifies the path off the hacks: a **platform-owned session contract** behind a single adapter seam, validated against the capability surfaces of six open-source coding agents, deliberately minimal where abstraction is premature (Rule 12), and deliberately rich where it must be (mobile + desktop + API as first-class surfaces).

### 1.1 What an investigation of six agents revealed

A comparative study of opencode, pi, aider, strands, eino, and langchaingo (full clones, deep architecture dives) established three findings that drive every decision below:

1. **The agent loop and preset tools are commoditized.** Every serious agent has a ReAct/tool-calling loop with read/write/edit/bash/grep. Aider's loop, opencode's loop, pi's loop — all competent, none individually novel. **LLMSafeSpaces must not compete here.** Building a homegrown agent inverts where the platform's value is.

2. **Defensible value lives in layers the platform should NOT own.** Aider's moat is its SEARCH/REPLACE robustness + PageRank repo map + benchmark-driven per-model tuning (357-entry `model-settings.yml`). opencode's is the SystemContext algebra + CodeMode. pi's is its extension API surface. These took years. Replicating any one is a multi-year investment with no platform payoff.

3. **Multi-agent abstraction is viable ONLY if interactive is agent-agnostic.** Every agent's response shape, part types, question/permission model, and event stream are different. The only universal interactive surface is the TUI (every agent has one). For structured web/mobile chat, each agent needs a translation adapter — the question is how thin that adapter can stay.

### 1.2 The mobile constraint rules out terminal-only

The terminal pass-through (xterm.js over PTY, the Coder/Gitpod model) would make interactive agent-agnostic for free — every coding agent ships a good TUI. But mobile is a first-class requirement with **full primary sessions** (users start and run real coding sessions from phones, including reviewing diffs). A coding-agent TUI on a phone via xterm.js is Termius, not a product. Therefore: structured web/mobile chat on both surfaces is required, and the terminal becomes an optional power-user desktop mode, not the architecture.

Resource-based billing (cgroup CPU/memory/disk — no token-based metering) removes the strongest objection to opaque sessions: the platform loses nothing economically by not parsing agent output. Token data appears only for cost display, never billing.

---

## 2. Coupling Diagnosis

opencode knowledge leaks out of `pkg/agent/opencode/` (the existing seam) in five shapes, ranked by pain:

### C1 — Config-merge / `agent-config.json` write orchestration (deepest)

`cmd/workspace-agentd/agent_config_writer.go` (591 lines), `secrets.go` (1,116 lines), `relay_injector.go`, `pre_boot_relay.go`, plus the controller's `freemodels/` and `pod_builder.go` encode opencode-specific behaviours:

- Last-writer-wins deep-merge; `OPENCODE_CONFIG` always wins; **no hot reload** (forces the `proc.restart()` dance)
- `disabled_providers` + `opencode-relay` provider-block injection
- One-shot relay injector + 20s stale window + the defense-in-depth `annotateModels` remap guard (`models.go:152`)
- `OPENCODE_EXPERIMENTAL_EVENT_SYSTEM=true` env (`pod_builder.go:89`)

This is the documented canonical example of leakage (Rule 12). Recurring pain in the same seam — the valid abstraction signal.

### C2 — HTTP API contract (broad)

`/session/{id}/message`, `/session/{id}/prompt_async`, `/event`, basic-auth (`opencode` username), SSE event types, the `?verbose=true` patch-part filtering, the request buffer that parks POSTs during opencode restarts, history pagination/cap logic. The existing `Dialect` interface covers route paths and event classification but NOT response-shape translation (patch-part stripping, message-body parsing for display).

### C3 — Process-supervision contract

agentd supervises opencode as a child `exec.Cmd` (SIGTERM→5s→SIGKILL, port 4096 liveness, in-place restart). Generic supervision pattern, opencode-specific constants.

### C4 — Storage/path contract

`XDG_DATA_HOME=/workspace/.local`, `auth.json` symlink, `opencode.db`, `/workspace/.local/opencode/storage/`. A path convention, not logic. Low pain.

### C5 — Credential format contract

`FormatOpenCodeConfig` produces opencode's exact provider JSON. **Best-contained** — already behind `AgentRuntime.FormatProviderConfig`.

### What is NOT coupling (the 60% noise)

Most of the 70-file `grep -ri opencode` hits are incidental: comments ("the opencode child process"), log messages, test fixtures. Documentation, not architecture. Ignore these.

---

## 3. The Decision: Split Surfaces + Platform-Owned Contract

```
                    pkg/session (the contract)
                          │
        ┌─────────────────┼─────────────────┐
        ▼                 ▼                 ▼
   Interactive        Programmatic       Control-plane
   (web + mobile,     (API/SDK/MCP —     (workspace/cred/
    structured chat    agent-as-caller)   workflow lifecycle,
    on both surfaces)                     terminal PTY)
```

Three first-class surfaces, one contract:

| Surface | Primary caller | Latency model | State model |
|---|---|---|---|
| **Interactive** | Human at web/mobile | Streaming, real-time | Live session, status, input prompts |
| **Programmatic** | External agent via MCP/SDK | Request-response, pollable | Async sends, retrievable results |
| **Control-plane** | Admin/operator/CI | Synchronous CRUD | Workspace, credential, workflow |

All three consume the same `pkg/session` contract. Per-agent translation lives **entirely** in `pkg/agent/<name>/`. Platform code imports `pkg/agent` (the interface) and `pkg/session` (the types); it never imports an agent implementation package.

### 3.1 What was considered and rejected

| Alternative | Why rejected |
|---|---|
| **Build a homegrown agent** | Inverts the moat. Agent loops are commoditized; edit formats + model tuning take years. The platform's value is orchestration, not cognition. |
| **Full agent-provider abstraction now** | Violates Rule 12. One consumer. The interface would encode opencode's shape as universal. |
| **Terminal pass-through as primary architecture** | Mobile requires full primary sessions with diff review. xterm.js on a phone is not a product surface. Terminal stays as an optional desktop power-user mode. |
| **Universal structured protocol (ACP/A2A-style)** | Huge investment that only pays off with many agents. The terminal-optional + thin-adapter model means this isn't needed. |
| **Switching off opencode (e.g. to pi)** | Orthogonal. The coupling is in the platform, not the agent. A new agent has its own quirks. Do the seam work first; then a swap is bounded to one package. |

---

## 4. The Contract

### 4.1 Design rules (load-bearing)

1. **No agent identifier leaks.** opencode's `ses_`/`msg_` IDs, `patch` part, `session.next.step.ended` event, `opencode` vs `opencode-relay` naming — none appear in the contract. They live in `pkg/agent/opencode/`.
2. **Closed part union.** Parts are a discriminated struct with a fixed `Type` set. Adding a part type is a contract change, not a config tweak. **The union is capped at 5 forever** (see 4.3).
3. **All tools are `ToolPart`, discriminated by `Name`, forever.** Todos, subagents, plan mode, shell, LSP diagnostics — all tool calls. Resist `PartTodo`, `PartEdit`, `PartSearch`. This is the single most important discipline: tools are extensible, part types are not.
4. **Diffs are unified-diff text (`Patch string`), not hunk structs.** Every renderer (GitHub, monaco-diff, codemirror, terminal) consumes unified-diff text. Hunks would reinvent diff parsing in every client.
5. **Usage is for cost display only.** Billing is cgroup-based. Tokens appear only when the agent reports them.
6. **The contract is richer than any single agent fills.** Optional fields mean the opencode adapter omits what it cannot produce. A future adapter populates what it can.

### 4.2 Capability-gated pass-through

Agent-specific operations (rewind, fork, stash) are **pass-through**, not contract types. The platform forwards the call to the adapter without modeling the result. The client discovers capabilities and renders/hides UI accordingly. When a second agent needs rewind and the shapes diverge, *then* abstract (Rule 12). For now, contain.

### 4.3 Part union — validated against six agents

Five part types, validated against opencode, pi, oh-my-pi, claude-code, aider, and strands (all six fit):

| Part type | What it carries | Coverage |
|---|---|---|
| `Text` | Prose, system messages, compaction notices | All 6 |
| `Reasoning` | Model thinking (rendered collapsed) | 5/6 (aider folds into text — optional) |
| `Tool` | Every tool call (bash, edit, read, grep, todowrite, task, plan_enter, webfetch) with state machine (pending→running→completed\|error) | All 6 |
| `FileChange` | Structured unified-diff for a file | All 6 |
| `Custom` | Pressure-relief valve for extension-defined semantics (required `Kind` discriminator) | pi's `custom` message type proves the need |

The `Custom` type is the anti-over-engineering move: one explicit unknown bucket prevents part-type creep the moment an agent extension does something interesting. Without it, pressure builds to add `PartDoom`, `PartBranchSummary`, etc.

**Deliberately NOT part types:** todos (tool output), subagent spawns (tool + `Session.ParentID`), plan mode (tool), shell commands (tool or `MessageShell`), multimodal images (always tool output — screenshot tool → `Tool` part).

### 4.4 Steering — first-class delivery mode

Steering (injecting a redirect while the agent is mid-turn, without killing in-flight tools) is a real primitive in opencode (steer vs queue inbox) and pi (steering messages). It is NOT abort+send — abort kills the current tool batch; steer folds the redirect into the next model call at a safe boundary. Modeled as a delivery mode on `SendOpts`, not a separate method.

```go
type Admission string
const (
    AdmissionSteer Admission = "steer" // inject at next safe boundary; tools keep running
    AdmissionQueue Admission = "queue" // promote when agent would otherwise go idle
)
```

Capability flags (`CapSteer`, `CapQueue`) let the UX render the right interaction. Adapters without native steer fall back to abort+send (best-effort).

### 4.5 Full type definitions

The authoritative Go types live in `pkg/session/` (created by US-65.2). The shapes are:

- **`Session`** — ID, WorkspaceID, ParentID, Title, AgentID, Model, Status, Cost (display), TimeRange, Summary, Archived. No `Revert`/`Stashed`/`Tags` (pass-through).
- **`Message`** — discriminated by `Type`: user, assistant, shell, agent_switch, model_switch, compaction, system. Agent/model switches are transcript entries (not side-band config) so the timeline is coherent after a switch — matches opencode's schema.
- **`Part`** — the 5-type union (4.3).
- **`Event`** — 9 streaming event types: session.status, session.updated, message.start, message.end, part.start, part.delta, part.end, input.request, input.resolved, error.
- **`InputRequest`** — unified question + permission (both are "agent needs a human").
- **`SendOpts`** — Model override + Admission mode.
- **`Cost`** / **`Usage`** — display-only token/cost fields.
- **`ModelRef`** / **`ModelInfo`** — identity + context/output limits (limits needed for "context: 45% used" display).

### 4.6 The Adapter interface (~18 methods)

The `pkg/agent.Adapter` interface folds the existing `Dialect` + `AgentRuntime` into one seam:

- **Sessions (5):** Create, Get, List, Rename, Delete
- **Messaging (4):** Send, SendAsync, Abort, GetHistory
- **Streaming/Input (3):** Stream, ListPending, Resolve
- **Config/Credentials (3):** ApplyConfig (returns `restartRequired bool`), FormatProviderConfig, ValidateCredentials
- **Models (2):** ListModels, SetModel
- **Capabilities + pass-through (3):** Capabilities, Rewind, Fork

Every method returns **platform-shaped** data. The existing `Dialect` path/classification methods (`SessionMessagePath`, `IsSessionIdle`, `ParseQuestionRequest`) become private helpers inside `pkg/agent/opencode/adapter.go`.

---

## 5. The AgentConfigWriter Seam (C1 — abstract now)

Rule 12's own test is met for C1: recurring pain in the same seam is a valid abstraction signal even before a second consumer. The seam:

```go
type AgentConfigInput struct {
    Providers []LLMProviderData
    Model     ModelSelection
    Relay     *RelayState
}

type AgentConfigWriter interface {
    Apply(input AgentConfigInput) (restartRequired bool, err error)
}
```

The opencode implementation owns: deep-merge semantics, `OPENCODE_CONFIG` path, `disabled_providers` relay block, and the fact that it returns `restartRequired=true` every time. Platform code calls `Apply` and reacts to `restartRequired` — it no longer knows *why* a restart is needed. If a future agent hot-reloads, its implementation returns `false` and the restart machinery no-ops.

The 20s stale window, the one-shot injector, the `annotateModels` guard — all move behind this seam.

---

## 6. What Dies When This Lands

| Hack | Fate |
|---|---|
| `api/internal/handlers/proxy_filter*.go` (patch-part stripping, `?verbose`) | **Deleted** — `FileChange` is structured; snapshot.files dropped |
| `proxy_input.go`, `proxy_permissions.go` translation logic | **Folded into adapter** `ListPending`/`Resolve` |
| `proxy_handlers.go:205-483` (history fetch + paginate + opencode-shape parse) | **Replaced by `adapter.GetHistory`** returning `[]Message` |
| `pkg/agent/dialect.go` path/classification methods | **Private to opencode adapter** |
| opencode-specific doc comments on `SessionListItem.ParentID` / `ContextUsed` (`pkg/types/session.go:60`) | **Genericized** |
| 3 consumers of `Dialect` outside `pkg/agent` (`proxy.go`, `proxy_events.go`, `proxy_input.go`) | **Call `Adapter` instead** |

---

## 7. Discipline — The Social Contract

The contract is right-sized, but the discipline is what keeps it that way:

1. **5 part types forever.** Adding `PartTodo` or `PartSearchReplace` "just this once" rebuilds the coupling in a new file. A repolint rule flags new `PartType` constants for review.
2. **No agent-specific part types.** Plan mode, edits, search — all `ToolPart` with different `Name`.
3. **Agent/model switches are transcript events, not side-band config.** Keep them in the message stream.
4. **Diff text is authoritative.** Not hunks.
5. **Cost is optional everywhere and never billing.**
6. **Agent-specific operations are pass-through.** Rewind/stash/fork results are `json.RawMessage` until a second adapter validates a typed shape.

---

## 8. Sequencing

```
US-65.1  AgentConfigWriter seam (C1)           ~1 week
US-65.2  pkg/session contract types             ~3 days
US-65.3  opencode adapter implementing contract ~2 weeks
US-65.4  Migrate proxy handlers to Adapter      ~1 week
US-65.5  Delete deprecated proxy hacks          ~3 days
US-65.6  Repolint import-rule enforcement       ~1 day
US-65.7  MCP tool surface (unified resolution)  ~3 days
US-65.8  Frontend migration to contract         ~2 weeks (parallel, can lag)
```

Five weeks of focused work to get off the hacks. No second adapter until funded. No operations types until a UX needs them.

---

## 9. Non-Goals

- **A universal agent protocol** (ACP/A2A). The contract is LLMSafeSpaces's own, shaped by its surfaces.
- **A second adapter before one is funded.** Rule 12. One excellent opencode adapter validates the contract's weak spots but not its universality.
- **Competing with agent loops.** The platform does not reason, edit, or tune models.
- **Terminal as primary UX.** Optional desktop power-user mode only.

---

## 10. Open Questions

| Question | Resolution path |
|---|---|
| Does the opencode adapter produce `FileChange` hunks from `git diff` on the PVC at message completion? | Prototype before committing (US-65.3). If opencode's `session.diff` event carries enough, use it; else fallback to `git diff`. Requires workspace to be a git repo (init on creation). |
| Does `session_message` (sync MCP tool) become a wrapper over `Send`+poll, or stay as-is? | US-65.7 — keep as convenience wrapper; document `Send`+`GetHistory` as the robust pattern for long tasks. |
| Where do workflow-run results map into the contract? | They don't — workflows compose sessions; workflow-run results live in the workflow layer (Epic 64). The contract is per-session. |

---

## References

- `design/0021_evolution-v2.md` §7 — original agent architecture (this doc refines the integration layer)
- Rule 12 (`README-LLM.md:252`) — Containment Before Abstraction (the rule this design applies)
- "Relay Config Subsystem" (`README-LLM.md:453`) — the C1 fragility list this design resolves
- `pkg/agent/` — existing `AgentRuntime` + `Dialect` seam (folded into `Adapter`)
- `pkg/agent/opencode/format.go` — existing credential format (stays, behind `FormatProviderConfig`)
