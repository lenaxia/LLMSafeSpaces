# NODE-EXECUTE-CONTRACT.md — Epic 64 Spike (US-64.1)

**Date:** 2026-08-05
**opencode version:** 1.15.12
**Spike environment:** live workspace pod, probed via `curl` + Go tests

---

## Assumption Validation Summary

| ID | Assumption | Result | Evidence |
|---|---|---|---|
| A1 | `/sessions/:id/prompt` returns synchronous structured response | ✅ **VALIDATED** (path differs) | `POST /session/:id/message` returns synchronous JSON |
| A2 | opencode enforces JSON Schema output | ✅ **VALIDATED (negative)** | No schema enforcement in 1.15.12; we must validate+retry |
| A3 | Script wrapper function-call contract works | ✅ **VALIDATED** (Python + Node) | 8 tests pass; **Go deferred to v2** |
| A4 | agentd → opencode local call without new auth | ✅ **VALIDATED** | `OpenCodeClient` already does this |
| A5 | `freemodels/refresher.go` pattern for reconciler | ✅ **KNOWN** | Documented in codebase |
| A6 | expr-lang type-checks at validate time | ✅ **VALIDATED** | 7 tests pass; requires `reflect.StructOf` |
| A7 | Workspace activation ~22s | ⏸ **DEFERRED** | Needs live cluster |
| A8 | Named-agent-not-found error | ✅ **VALIDATED (differs)** | opencode silently falls back; use `GET /agent` to pre-validate |
| A9 | `/sandbox-runtime/secrets-env` format | ✅ **VALIDATED** | `KEY=VALUE` lines; file absent when no env-secrets bound |

---

## A1 — opencode session/message API (VALIDATED)

### Correct endpoint: `POST /session/:id/message`

The design doc assumed `/sessions/:id/prompt`. The actual synchronous API is **`POST /session/:id/message`** (singular `session`, path segment `message`).

Note: `POST /session/:id/prompt` exists but returns the SPA HTML, not an API response. `POST /session/:id/prompt_async` is the queued async variant the proxy uses. For workflow `agent` nodes, **`POST /session/:id/message`** is the correct synchronous endpoint.

### Request format

```json
POST /session/{id}/message
Authorization: Basic base64(opencode:{OPENCODE_SERVER_PASSWORD})
Content-Type: application/json

{
  "parts": [
    {"type": "text", "text": "your prompt here"}
  ]
}
```

Optional fields:
- `"agentID": "build"` — select a named agent (silently falls back if invalid — see A8)
- `"model": null` — null = use session default; object = override

### Response format (captured from live run)

```json
{
  "info": {
    "id": "msg_...",
    "sessionID": "ses_...",
    "role": "assistant",
    "agent": "build",
    "mode": "build",
    "modelID": "glm-5.2",
    "providerID": "thekaocloud",
    "cost": 0,
    "tokens": {"total": 7742, "input": 57, "output": 5, "reasoning": 0, "cache": {"read": 7680, "write": 0}},
    "time": {"created": 1785964803729, "completed": 1785964805631},
    "finish": "stop",
    "path": {"cwd": "/workspace", "root": "/"}
  },
  "parts": [
    {"type": "step-start", "id": "prt_...", ...},
    {"type": "text", "text": "hello from spike", "time": {"start": ..., "end": ...}, "id": "prt_...", ...},
    {"type": "step-finish", "reason": "stop", "tokens": {...}, "cost": 0, ...}
  ]
}
```

**Key contract points for the `agent` node:**
1. Response is **synchronous JSON** — not SSE. The HTTP response body contains the full assistant reply.
2. Text output is in `parts[]` where `type == "text"` — extract and concatenate.
3. Token usage is in `info.tokens` — useful for metering/cost tracking.
4. `info.finish` indicates completion reason (`"stop"` = normal, `"length"` = token limit, etc.).
5. Tool-call parts (`type == "tool"` or `type == "tool-call"`) may appear for agents that use tools.

### Auth: HTTP Basic Auth

- Username: `opencode` (constant: `pkg/agentd/types.go:57 AuthUsername`)
- Password: read from `/sandbox-cfg/password` at startup → `OPENCODE_SERVER_PASSWORD` env var

### Session creation: `POST /session`

```json
POST /session
Authorization: Basic ...
Content-Type: application/json
{}

→ 200 {"id": "ses_...", "slug": "...", "projectID": "global", "directory": "...", ...}
```

Session deletion: `DELETE /session/:id` — needed for `ephemeral` session lifecycle.

---

## A2 — Structured Output (VALIDATED: opencode does NOT enforce)

opencode 1.15.12 does **not** support JSON Schema output enforcement. Checked:
- The opencode config schema (`opencode-config.schema.json`) has no `outputSchema`, `response_format`, `json_schema`, or `structured` field.
- The agent config has no schema-related properties.
- The message API has no schema parameter.

**Impact on `agent` node design:** We MUST validate the response ourselves:
1. Send the prompt with instructions to produce JSON matching the schema (in the prompt text).
2. Parse the text parts from the response.
3. `json.Unmarshal` into a Go value.
4. Validate against the JSON Schema (using `github.com/xeipuuv/gojsonschema` or similar).
5. On mismatch: retry within `maxAttempts` with a repair hint appended.
6. On exhaustion: node fails with `error_code: schema_mismatch`.

This matches the design doc's plan. The finding confirms it's necessary, not optional.

---

## A3 — Script Wrapper Contract (VALIDATED)

Package: `pkg/workflows/scriptwrap/` — 6 tests, all pass with `-race`.

### Contract

The handler defines a function: `def handler(input) -> dict`. agentd:
1. Writes the handler source to a temp file (`handler.py` / `handler.js`)
2. Generates a thin per-language wrapper (`_wrapper.py` / `_wrapper.js`)
3. Execs the wrapper via the runtime (`python3` / `node`)
4. Feeds JSON input via stdin
5. Captures stdout (the JSON-serialized return value)

### Python wrapper

```python
import json, sys, os
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from handler import handler
_input = json.loads(sys.stdin.read())
_result = handler(_input)
sys.stdout.write(json.dumps(_result))
```

### Node wrapper

```javascript
const h = require('./handler');
const _input = JSON.parse(require('fs').readFileSync(0, 'utf8'));
const _result = h.handler(_input);
process.stdout.write(JSON.stringify(_result));
```

### Error handling

- **Exception / non-zero exit** → `error_code: script_failed`, stderr captured
- **Non-dict / non-serializable return** → `error_code: script_output_invalid` (JSON parse failure on wrapper stdout)
- **Context cancellation / timeout** → process killed, error returned

### Validated capabilities (Python + Node)

- Python round-trip (dict in, dict out) ✓
- Node round-trip ✓
- Exception handling (stderr captured, exit code reported) ✓
- Non-dict return detection at the caller layer (scriptwrap returns `json.RawMessage`; US-64.7 enforces dict shape) ✓
- Python builtin import (json, sys, os) ✓
- Context cancellation (timeout kills the process within ~500ms) ✓
- Unsupported-language rejection ✓
- Input-marshal-failure handling ✓

### Go — deferred to v2

The original design doc listed `language: python|node|go`. The spike validated Python and Node only. Go handlers are deferred to v2 because: (1) Go is compiled, not interpreted — wrapping `def handler(input) -> dict` requires building a plugin or a generated main package, materially different from the Python/Node source-import pattern; (2) no concrete workflow has demanded Go; (3) the workspace's existing `runtimes/go` image already supports `go run` for ad-hoc scripts, covering the "I need Go" case without workflow integration. Adding Go later is additive (new Language constant + new wrapper).

---

## A4 — agentd → opencode Call Path (VALIDATED)

No new auth surface needed. The `OpenCodeClient` (`cmd/workspace-agentd/client.go:20`) already:
- Connects to `getAgentAddr()` (default `127.0.0.1:4096`)
- Uses `req.SetBasicAuth(agentd.AuthUsername, c.password)` for every request
- Calls opencode API paths directly (`/global/health`, `/provider`, etc.)

For the `agent` node, agentd adds one new method to this client: `PostMessage(ctx, sessionID, body)` → POST `/session/:id/message`. Same auth, same base URL, same HTTP client. No new token, no new port, no new middleware.

---

## A6 — expr-lang Type-Checking (VALIDATED with constraint)

Package: `pkg/workflows/exprlang/` — 7 tests, all pass with `-race`.

### Key constraint: maps do NOT type-check

expr-lang treats `map[string]any` as dynamically keyed — any field access returns nil (no compile error). To get strict type-checking, the env must be a **Go struct** (or `reflect.StructOf`-generated type).

### Working pattern

```
JSON Schema → deriveType() → reflect.StructOf → map[string]any{"input": structInstance}
                                                              ↑
                                          expr.Compile("input.FieldName == true", expr.Env(env))
```

- JSON Schema properties → struct fields (snake_case → CamelCase: `error_code` → `ErrorCode`)
- Condition expressions reference fields by **CamelCase** name: `input.Skipped`, `input.ErrorCode`
- Missing fields → compile error: `type ... has no field Nonexistent`
- Wrong types → compile error: `type int has no method Contains`
- Nested objects → nested structs, same strict checking

### Impact on condition node spec

The design doc showed `input.skipped` (snake_case). The validated contract is `input.Skipped` (CamelCase). This is a minor syntax change — the `capitalize()` function in `validate.go` handles the conversion from JSON Schema property names.

---

## A8 — Named Agent Validation (VALIDATED: opencode silently falls back)

opencode 1.15.12 does **not** return an error for a non-existent `agentID`. It silently falls back to the default agent (`build`). A message with `{"agentID": "nonexistent-agent-12345"}` was processed normally with `"agent": "build"`.

### Solution: pre-validate via `GET /agent`

```json
GET /agent
Authorization: Basic ...

→ 200 [
  {"name": "build", "description": "...", "mode": "primary", "native": true, ...},
  {"name": "compaction", ...},
  {"name": "explore", ...},
  ...
]
```

agentd must call `GET /agent` before dispatching an `agent` node and reject unknown names with `error_code: agent_not_found`. The agent list can be cached per-pod (agents don't change at runtime).

---

## A9 — Secret Resolution Path (VALIDATED)

### Correct path: `/sandbox-runtime/secrets-env`

Confirmed in source: `pkg/agentd/secrets/secrets.go:174`:
```go
SecretsEnvPath  string // env-file (/sandbox-runtime/secrets-env)
```

### Format: `KEY=VALUE` lines

Env-secrets are materialized by `applyEnvSecret()` (`secrets.go:580`):
```go
func (m *Materializer) applyEnvSecret(s Secret) error {
    varName := s.Metadata["var_name"]
    line := FormatEnvLine(varName, s.Plaintext)
    return appendFile(m.FS, m.Paths.SecretsEnvPath, []byte(line), 0o600)
}
```

Each line: `VAR_NAME=value\n`

### Key constraint: file only exists when env-secrets OR api-keys are bound

In a workspace with no `env-secret` AND no `api-key` credentials (only llm-providers), the file does NOT exist. The `http` node's secret resolution must:
1. Read `/sandbox-runtime/secrets-env` (if it exists)
2. Parse `KEY=VALUE` lines into a map
3. If file doesn't exist → empty map → any `{{secrets.NAME}}` reference → `error_code: secret_not_found`
4. If NAME not in map → `error_code: secret_not_found`

### Which secret types are reachable

| Type | Written to `secrets-env`? | Format |
|---|---|---|
| `env-secret` | ✅ Yes (`applyEnvSecret`, secrets.go:580) | `VAR_NAME=value` (var_name from metadata) |
| `api-key` | ✅ Yes (`applyAPIKey`, secrets.go:485) | `API_KEY_<NAME>=value` (NAME sanitized from secret name) |
| `llm-provider` | ❌ No (in `agent-config.json`) | — |
| `mcp-server` | ❌ No (in `agent-config.json mcp` section) | — |
| `ssh-key`, `git-credential`, `secret-file` | ❌ No (in `/sandbox-runtime/rt/*`) | — |

Both `env-secret` and `api-key` are reachable via `{{secrets.NAME}}`. The http node resolver must read the whole file and look up either the raw var name (for env-secrets) or `API_KEY_<NAME>` (for api-keys). Downstream design implication: the `{{secrets.NAME}}` syntax alone is ambiguous between env-secret and api-key — recommend either `{{secrets.env.NAME}}` + `{{secrets.apikey.NAME}}` qualifiers, or a unified lookup that tries both.

*Correction note: an earlier version of this contract claimed only `env-secret` was in `secrets-env`. That was wrong — `applyAPIKey` at secrets.go:485-497 also writes here. Caught in PR #655 review.*

---

## Cancel Mechanism

### script nodes
`exec.CommandContext(ctx, ...)` — cancelling the context sends SIGKILL to the process. Validated: a 30-second sleep was killed within ~500ms of context cancellation.

### agent nodes
Two approaches (validated via source reading, not live test):
1. **Preferred**: `DELETE /session/:id` — destroys the session, opencode stops generating.
2. **Alternative**: `POST /session/:id/abort` — aborts the current generation without deleting the session.

For `ephemeral` sessions, `DELETE` is correct (session is torn down anyway). For `existing`/`new` sessions, `POST /session/:id/abort` is correct (preserves the session for later use).

### http nodes
`context.WithCancel` on the `http.Request` — standard Go HTTP client cancellation.

---

## Impact on Design Doc

| Section | Change needed |
|---|---|
| `agent` node spec | Correct endpoint to `POST /session/:id/message` (not `/prompt`) |
| Condition expressions | Use CamelCase field names: `input.Skipped` (not `input.skipped`) |
| `agent` node `enforceStructuredOutput` | Confirmed: opencode doesn't enforce; our validate+retry is mandatory |
| `agent` node `agent_not_found` | Pre-validate via `GET /agent`, not rely on opencode error |
| `http` node secret resolution | Confirmed correct path; add "file may not exist" handling |

These are minor corrections to the design doc, not architectural changes. The overall design is sound.
