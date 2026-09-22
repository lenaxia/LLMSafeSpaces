# Epic 16: Agent Input Requests (Questions & Permissions)

**Status:** Planning
**Created:** 2026-05-29
**Priority:** High
**Depends on:** Epic 3 (Proxy/Sessions), Epic 6 (Collapse Sandbox), Epic 15 (Streaming Reconnect), Worklog 0069 (API validation), Worklog 0070 (upgrade analysis), Worklog 0071 (v1.15 upgrade)

## Problem Statement

When the AI agent needs user input during execution, it has no way to ask. Two scenarios exist:

1. **Questions**: The agent calls the `question` tool to ask the user structured questions (e.g., "Which database should I use?" with options). The agent blocks until the user answers. Currently, the frontend has no UI to render these questions or submit answers — the agent hangs indefinitely.

2. **Permissions**: The agent attempts a tool call that requires approval (e.g., writing a file, running a shell command). The permission system blocks until the user approves or denies. Currently, there is no UI for this — the agent hangs or auto-approves silently depending on config.

Both systems emit SSE events that already reach the browser (via `onRawEvent` → `WorkspaceEventBroker` → `opencode.event`), but nothing acts on them.

## Validated Contracts (from Worklogs 0069, 0070)

All contracts below are verified against live opencode v1.2.27 (worklog 0069) and confirmed compatible with v1.15.12 (worklogs 0070, 0071).

### Question API

| Endpoint | Method | Body | Response |
|----------|--------|------|----------|
| `/question` | GET | — | `QuestionRequest[]` |
| `/question/:requestID/reply` | POST | `{"answers": [["label1"], ...]}` | `true` (200, idempotent for unknown IDs) |
| `/question/:requestID/reject` | POST | — | `true` (200, idempotent for unknown IDs) |

### Permission API

| Endpoint | Method | Body | Response |
|----------|--------|------|----------|
| `/permission` | GET | — | `PermissionRequest[]` |
| `/permission/:requestID/reply` | POST | `{"reply": "once"\|"always"\|"reject", "message?": "..."}` | `true` (200, idempotent for unknown IDs) |

### SSE Events (on `/event` stream, forwarded to browser as `opencode.event`)

| Event Type | Payload | Meaning |
|-----------|---------|---------|
| `question.asked` | `QuestionRequest` | Agent is waiting for user to answer |
| `question.replied` | `{sessionID, requestID, answers}` | Question was answered |
| `question.rejected` | `{sessionID, requestID}` | Question was dismissed |
| `permission.asked` | `PermissionRequest` | Agent is waiting for permission approval |
| `permission.replied` | `{sessionID, requestID, reply}` | Permission was resolved |

### Data Schemas (verified from opencode source + live capture)

```typescript
interface QuestionOption {
  label: string;         // Display text (1-5 words)
  description: string;   // Explanation of choice
}

interface QuestionInfo {
  question: string;      // Full question text
  header: string;        // Short label (max 30 chars)
  options: QuestionOption[];
  multiple?: boolean;    // Allow multi-select (default: false)
  // NOTE: "custom" field is ABSENT from the event (defaults to true per opencode docs)
}

interface QuestionRequest {
  id: string;            // Format: "que_<opaque>" (prefix validated)
  sessionID: string;     // Format: "ses_<opaque>"
  questions: QuestionInfo[];  // 1 or more questions
  tool?: {
    messageID: string;
    callID: string;
  };
}

interface PermissionRequest {
  id: string;            // Format: "per_<opaque>" (prefix validated)
  sessionID: string;
  permission: string;    // Tool name: "shell", "edit", "write", etc.
  patterns: string[];    // What's being accessed (file paths, commands)
  metadata: Record<string, unknown>;
  always: string[];      // Patterns that "always" would approve
  tool?: {
    messageID: string;
    callID: string;
  };
}
```

### Behavioral Facts (verified)

1. **Session stays `busy` while waiting** — no `session.status: idle` fires until the question/permission is resolved
2. **Reply/reject are idempotent** — unknown IDs return `200 true` (no 404)
3. **Permission reject cascades** — rejecting one permission rejects ALL pending permissions for that session
4. **`custom` defaults to `true`** — "Type your own answer" is always available unless explicitly disabled
5. **Permissions require explicit config** — default opencode config auto-approves everything; permissions only fire when the workspace has the TOP-LEVEL `permission` config with `"ask"` rules *(corrected 2026-09-22: the epic-era `mode.permissions` shape was proven inert on the pinned opencode — the tier-ruling wire finding; the platform now renders `permission.external_directory` on every boot)*
6. **Questions work out of the box** — the LLM calls the `question` tool when it needs user input; no config required

## Assumptions (Epic-Level)

| # | Assumption | Status | Validation |
|---|-----------|--------|------------|
| EA1 | opencode v1.15.12 is deployed | ✅ | Worklog 0071 |
| EA2 | SSE events for `question.asked` and `permission.asked` already reach the browser as `opencode.event` | ✅ | Verified: `onRawEvent` in proxy.go publishes all events to broker; browser receives them |
| EA3 | The proxy can forward requests to `/question/:id/reply` using the same `proxyToWorkspace` pattern | ✅ | Verified: `proxyToWorkspace` is path-agnostic; only needs workspace ID + target path |
| EA4 | Proxy routes have auth middleware but no ownership middleware | ✅ | Verified: `workspaceGroup.Use(services.GetAuth().AuthMiddleware())` only; pre-existing pattern |
| EA5 | Epic 15 will be completed before the frontend stories in this epic | ✅ | Decision: backend work parallelizes; frontend depends on Epic 15 |
| EA6 | MCP `SendMessage` SSE parsing is BROKEN — looks for `"session.idle"` event type but real wire format is `{"type":"session.status","status":"idle"}` | ❌ BUG | Verified: `client.go:248` checks `event.Type == "session.idle"` but broker emits `WorkspaceSSEEvent{Type:"session.status", Status:"idle"}`. Tests pass only because mocks emit fake event type. Must fix in US-16.0. |
| EA7 | `validID` regex in MCP client rejects opencode IDs — underscores not matched | ❌ BUG | `validID = ^[a-zA-Z0-9][a-zA-Z0-9.\-]{0,252}$` excludes `_`. Real IDs: `ses_18b28260affeoxXrX1iwPH8wFg`. Must fix in US-16.0. |
| EA8 | Permissions only fire when workspace has explicit permission config with `"ask"` rules | ✅ | Verified live (worklog 0069): default config auto-approves everything. Permission prompts require explicit configuration. *(2026-09-22: the live shape is the top-level `permission` key — `mode.permissions` is inert on the pinned opencode.)* |
| EA9 | A second agent (claude-code or similar) will be added soon | ✅ | Decision from user: keep Dialect interface for extensibility |

## Design Decisions

| Decision | Rationale |
|----------|-----------|
| **Agent Dialect interface** for route mapping + event classification | Single point of change when swapping agents. Extensible to all future agent APIs. |
| **Normalized events** (`agent.question`, `agent.permission`) emitted alongside raw `opencode.event` | Frontend depends on stable contract; raw events continue for streaming/tools |
| **Render ALL questions at once** (not tabbed) | User sees full context; simpler UX than opencode's TUI tabs |
| **Headless auto-approve for permissions** | Programmatic callers shouldn't block on permission prompts. Controlled by workspace-level `autoApprovePermissions` setting (not subscriber count heuristic). |
| **MCP surfaces questions as tool results** | MCP callers need to see the question and respond with a separate tool call |
| **Pending state recovery on SSE connect** | Page refresh while question pending must show the prompt immediately |
| **Clear prompts on session idle/error** | Pod restart or agent crash must not leave stale prompts |
| **Permission persistence acceptable** | opencode stores "always" rules in SQLite on PVC; survives pod restarts |

## Architecture

### Data Flow: Question (Interactive — Browser)

```
1. User sends prompt → agent starts processing (session: busy)
2. Agent calls question tool → opencode blocks, emits question.asked on /event SSE
3. SSETracker receives event → onRawEvent fires
4. onRawEvent:
   a. Publishes raw: WorkspaceSSEEvent{Type:"opencode.event", EventType:"question.asked", Data:...}
   b. Dialect detects question → parses → publishes normalized: WorkspaceSSEEvent{Type:"agent.question", Data: QuestionRequest}
5. Browser SSE receives "agent.question" → renders QuestionPrompt component
6. User selects answer → frontend POSTs /api/v1/workspaces/:id/question/:requestID/reply
7. Proxy forwards to pod: POST /question/:requestID/reply with {"answers":[["Go"]]}
8. opencode unblocks → agent continues → eventually session goes idle
9. SSETracker receives session.status idle → publishes session.status event
10. Frontend clears QuestionPrompt (if still showing), fetches final history
```

### Data Flow: Question (MCP — Programmatic)

```
1. MCP caller invokes session_message tool
2. MCP server calls POST /prompt_async, subscribes to SSE
3. SSE delivers question.asked event
4. MCP server detects question (via Dialect) → returns tool result:
   {"type":"question","request":{"id":"que_...","questions":[...]}}
5. MCP caller reads the question, invokes session_question_reply tool
6. MCP server calls POST /question/:id/reply with answers
7. Agent unblocks, continues processing
8. MCP server (still subscribed to SSE) receives session.status idle → returns final response
```

### Data Flow: Permission (Headless Auto-Approve)

```
1. Agent calls tool requiring permission → opencode blocks, emits permission.asked
2. SSETracker receives event → onRawEvent fires
3. onRawEvent: Dialect detects permission
4. Check: are there browser SSE subscribers for this workspace?
   - YES → publish normalized event, let browser handle it
   - NO → auto-approve: POST /permission/:id/reply {"reply":"always"} to pod
5. opencode unblocks → agent continues
```

### Component Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│ pkg/agent/                                                               │
│                                                                          │
│  dialect.go          ← Interface: Dialect                                │
│  types.go            ← QuestionRequest, PermissionRequest (normalized)   │
│  opencode/dialect.go ← OpenCodeDialect implementation                    │
└─────────────────────────────────────────────────────────────────────────┘
         │ used by
         ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ api/internal/handlers/                                                    │
│                                                                          │
│  proxy.go            ← New handlers: QuestionList, QuestionReply,        │
│                        QuestionReject, PermissionList, PermissionReply    │
│                      ← Modified: onRawEvent (detect + normalize events)  │
│                      ← New: emitPendingInputRequests (on SSE connect)    │
│                      ← New: autoApprovePermission (headless mode)        │
│                                                                          │
│  event_broker.go     ← No changes needed                                 │
│                                                                          │
│  session_tracker.go  ← No changes (already forwards all events)          │
└─────────────────────────────────────────────────────────────────────────┘
         │ used by
         ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ api/internal/server/router.go                                            │
│                                                                          │
│  registerProxyRoutes ← 5 new routes added                                │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ pkg/mcp/                                                                 │
│                                                                          │
│  server.go           ← 3 new tools: session_question_reply,              │
│                        session_question_reject, session_permission_reply  │
│  client.go           ← 3 new methods + SendMessage modified to detect    │
│                        question.asked and return early with question data │
└─────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────┐
│ frontend/src/                                                            │
│                                                                          │
│  api/types.ts        ← QuestionRequest, PermissionRequest,               │
│                        AgentQuestionEvent, AgentPermissionEvent types     │
│  api/input.ts        ← NEW: questionReply, questionReject,               │
│                        permissionReply, listQuestions, listPermissions    │
│  components/chat/QuestionPrompt.tsx  ← NEW: renders all questions        │
│  components/chat/PermissionPrompt.tsx ← NEW: renders permission request  │
│  pages/ChatPage.tsx  ← Handle agent.question/agent.permission events,    │
│                        render prompts, clear on idle/resolved             │
└─────────────────────────────────────────────────────────────────────────┘
```

## Stories

| Story | Title | Layer | Priority | Depends On |
|-------|-------|-------|----------|------------|
| US-16.0 | Fix MCP client: SSE parsing + validID regex | Backend | Critical | — |
| US-16.1 | Agent Dialect interface + OpenCode implementation | Backend | Critical | — |
| US-16.2a | Question/Permission proxy routes (new `proxy_input.go`) | Backend | Critical | US-16.1 |
| US-16.2b | Migrate session routes to dialect + split proxy.go | Backend | Medium | US-16.2a |
| US-16.3 | Normalized event emission (agent.question, agent.permission) | Backend | Critical | US-16.1 |
| US-16.4 | Pending state recovery on SSE connect | Backend | High | US-16.2a, US-16.3 |
| US-16.5 | Headless permission auto-approve | Backend | High | US-16.2a, US-16.3 |
| US-16.6 | MCP tools for question/permission reply | Backend | High | US-16.0, US-16.2a |
| US-16.7 | MCP SendMessage question detection | Backend | High | US-16.0, US-16.6 |
| US-16.8 | Frontend types + API client | Frontend | Critical | US-16.2a (API exists) |
| US-16.9 | QuestionPrompt component | Frontend | Critical | US-16.8, Epic 15 |
| US-16.10 | PermissionPrompt component | Frontend | High | US-16.8, Epic 15 |
| US-16.11 | ChatPage integration (event handling, prompt lifecycle) | Frontend | Critical | US-16.9, US-16.10, Epic 15 |
| US-16.12 | Clear prompts on session idle/error | Frontend | High | US-16.11 |
| US-16.13 | E2E integration tests | Both | High | US-16.0–16.12 |

## Non-Goals

- **Permission rule configuration UI** — Permissions only fire when the workspace has explicit permission config (the top-level `permission` key — `mode.permissions` is inert on the pinned opencode, corrected 2026-09-22). Configuring these rules is out of scope (can be done via workspace settings in Epic 9/13).
- **Question tool invocation** — We don't control when the LLM calls the question tool. We only render and respond.
- **Multi-tab coordination for questions** — If two tabs are open, both show the prompt. First to answer wins; second gets 200 (idempotent no-op) and the prompt dismisses via the `question.replied` SSE event.
- **Custom question tool registration** — We use opencode's built-in question tool as-is.
- **Permission audit logging** — Logging which permissions were approved/denied is deferred to Epic 10 (audit system).

## Known Limitations

1. **Pod restart while question pending**: If the pod restarts after `question.asked` but before the user answers, the question is lost (opencode's in-memory deferred is gone). The session stays busy until opencode's internal tool timeout fires (typically 5 minutes), then goes idle with an error. The frontend clears the stale prompt on idle. Recovery is slow but automatic.

2. **No ownership check on proxy routes**: Pre-existing — any authenticated user can proxy to any workspace by ID. This epic inherits the same pattern. Multi-tenant isolation depends on workspace IDs being unguessable (UUIDs).

3. **Permission prompts require explicit config**: Default opencode config auto-approves all tool calls. The permission UI will only be exercised when workspaces are configured with permission rules containing `"ask"` actions *(2026-09-22: in the top-level `permission` key — `mode.permissions` is inert on the pinned opencode)*.

## Success Criteria

1. Agent calls question tool → user sees prompt within 1s → selects answer → agent continues
2. Page refresh while question pending → prompt re-appears immediately on reload
3. Agent finishes (session idle) → any stale prompts are cleared
4. MCP caller sends message → question surfaces as tool result → caller replies → agent continues
5. Headless workspace (no browser) → permissions auto-approve → agent never blocks
6. Multiple questions in one request → all render simultaneously → user answers all → single submit
7. "Type your own answer" works for every question (custom=true default)
8. Permission prompt shows tool name + patterns → user approves/denies → agent continues or errors
9. No regressions: existing send → stream → idle flow works identically
