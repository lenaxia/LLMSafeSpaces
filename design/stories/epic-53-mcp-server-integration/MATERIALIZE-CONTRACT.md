# US-53.1 — opencode External-MCP Config Contract

**Status:** Validated (evidence: the pinned opencode JSON schema shipped in this repo).
**Validates:** Epic 53 assumptions A6, A7, A17.
**Pinned schema source:** `cmd/workspace-agentd/testdata/opencode-config.schema.json` (the schema the
`AgentConfigWriter` schema-test suite validates every emitted `agent-config.json` against).

This is the contract US-53.7 (injection) emits and US-53.8 (materialization) consumes. It replaces
the original "run a live opencode" spike with schema-derived evidence: the schema file is the
authoritative, test-enforced description of what opencode accepts.

---

## A6 — opencode supports a top-level `mcp` section (CONFIRMED)

The pinned schema declares `mcp` as a top-level object whose properties are named servers
(`additionalProperties`), each one of three shapes.

**Location:** `cmd/workspace-agentd/testdata/opencode-config.schema.json:1014-1043`

```jsonc
"mcp": {
  "type": "object",
  "additionalProperties": { /* per-server config: local | remote | disable-form */ },
  "description": "MCP (Model Context Protocol) server configurations"
}
```

The property KEY is the server name (arbitrary string; we use the stored `mcp_servers.name`).

### Per-server shape 1 — `remote` (our transports `http` and `sse`)

**Schema:** `McpRemoteConfig` (`opencode-config.schema.json:623-673`)

```jsonc
"my-server": {
  "type": "remote",          // required, enum ["remote"]
  "url": "https://host/mcp", // required
  "enabled": true,           // optional (omit to leave default)
  "headers": { "Authorization": "Bearer ..." },  // optional map[string]string
  "oauth": false,            // optional: McpOAuthConfig object | false (false disables auto-detect)
  "timeout": 5000            // optional int ms, > 0, default 5000
}
```
Required: `type`, `url`.

### Per-server shape 2 — `local` (our transport `stdio`)

**Schema:** `McpLocalConfig` (`opencode-config.schema.json:550-593`)

```jsonc
"github": {
  "type": "local",                                    // required, enum ["local"]
  "command": ["npx", "-y", "@modelcontextprotocol/server-github"], // required array[string]
  "environment": { "GITHUB_TOKEN": "ghp_..." },       // optional map[string]string
  "cwd": "/workspace",                                // optional
  "enabled": true,                                    // optional
  "timeout": 5000                                     // optional int ms, > 0, default 5000
}
```
Required: `type`, `command`.

### Per-server shape 3 — disable form (we do NOT emit this)

`{ "enabled": false }` turns off a server defined elsewhere. We never merge user-provided
opencode config, so we never need the disable form — omission is equivalent to "not configured".

### Platform transport enum → opencode type mapping

| Platform `transport` | opencode `type` | Required platform fields |
|---|---|---|
| `http`  | `remote` | `url`, (`headers`) |
| `sse`   | `remote` | `url`, (`headers`) |
| `stdio` | `local`  | `command`+`args`, (`env`) |

opencode does not distinguish `http` from `sse` at the config level (it auto-detects the remote
transport). The platform keeps the distinction for fidelity/UX and for future transport-specific
validation; both render to opencode `type:"remote"`.

---

## A7 — `{env:VAR}` interpolation (CONFIRMED — used for secret references)

opencode supports `{env:VAR}` interpolation in provider config (proven by `opencode.json`
`"baseURL": "{env:OPENAI_API_BASE}"`). The `mcp` section is plain JSON consumed by the same
config loader, so interpolation is available in `url`, `headers`, `command`, `environment`,
and `args`.

**Platform decision: support BOTH inline secrets AND `{env:VAR}` references.**

1. **Inline secrets** (default): the user pastes a secret value into the MCP server form.
   The platform encrypts it (admin/org via master KEK, user via session DEK) and injects the
   decrypted value inline at materialization time.

2. **Secret references** (via the secret store): the user's existing `env-secret` entries are
   already injected into the pod's shell environment via `/sandbox-runtime/secrets-env` (sourced
   by `entrypoint-opencode.sh` before opencode starts). The MCP server config stores the value
   as the literal string `{env:VAR_NAME}` (e.g. `{env:GITHUB_TOKEN}`). This string is NOT a
   secret — it's a variable name — so it passes through encryption/materialization as a plain
   string, and opencode resolves it at runtime from the environment the entrypoint already
   populated.

   Chain: `env-secret` → injection pipeline → `/sandbox-runtime/secrets-env` → `source` by
   entrypoint-opencode.sh → inherited by opencode subprocess → `{env:VAR}` resolved by opencode.

   Frontend UX: the MCP server form offers a "Ref" button next to env/header value fields that
   lets the user pick from their existing `env-secret` entries, auto-populating the field with
   `{env:VAR_NAME}`.

---

## A17 — redaction of MCP tokens (assessment: low risk, recommendation below)

MCP server secrets are delivered inline in `agent-config.json` (`/sandbox-runtime`, tmpfs — wiped on
pod death, never on PVC). The exfiltration vectors are:

1. The agent prints/echoes a header/env value into a tool result or chat. The existing `redact`
   pipeline (16-rule) covers well-known token shapes (`ghp_…`, `sk-…`, `AKIA…`, bearer tokens) but
   cannot cover arbitrary custom MCP tokens.
2. opencode itself logs config at debug level.

**Recommendation (implemented in US-53.11 observability, not a blocker):**
- Secrets are never written to `secrets-env` (the sourced env file) — they live only inside
  `agent-config.json`'s `mcp.*.headers`/`mcp.*.environment`. This narrows the leak surface vs. env
  vars.
- No new `redact` rules are added in this epic; custom MCP token formats are unbounded. Operators
  who register a server whose token matches a known shape benefit from existing rules; others rely
  on opencode's output handling. Documented as an operator advisory in the README-LLM section.
- The kill-switch (disable/delete → reload) is the primary remediation if a token is compromised.

---

## Injection payload contract (US-53.7 emits, US-53.8 parses)

Each bound MCP server travels as one entry in `secrets.json` (the existing bootstrap tmpfs file).
The entry shape mirrors the existing `InjectedSecret` but carries MCP-specific metadata.

```jsonc
{
  "type": "mcp-server",
  "name": "github-tools",                 // mcp_servers.name — becomes the opencode mcp key
  "metadata": {
    "transport": "stdio",                 // "http" | "sse" | "stdio"
    "url": "",                            // set for http/sse
    "command": "",                        // set for stdio
    "args": ["-y", "@modelcontextprotocol/server-github"],
    "timeoutMs": 5000                     // optional; 0 = omit (opencode default 5000)
  },
  "plaintext": "{\"env\":{\"GITHUB_TOKEN\":\"ghp_xxx\"},\"headers\":{}}"
}
```

- `metadata` carries the **non-secret** server config (transport, endpoint, command/args, timeout).
- `plaintext` is the JSON encoding of the **secret** payload — `{"env":{...},"headers":{...}}` —
  decrypted by the injection pipeline (admin/org via master KEK; user via session DEK). For a server
  with no secrets, `plaintext` is `{"env":{},"headers":{}}`.

### Rendered opencode config (the materializer's output)

Given the payload above, `AgentConfigWriter.rebuild()` emits into `agent-config.json`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "github-tools": {
      "type": "local",
      "command": ["npx", "-y", "@modelcontextprotocol/server-github"],
      "environment": { "GITHUB_TOKEN": "ghp_xxx" },
      "enabled": true
    }
  }
}
```

For a remote server (`transport: "http"`, `url: "https://host/mcp"`):

```jsonc
{
  "mcp": {
    "wiki": {
      "type": "remote",
      "url": "https://host/mcp",
      "headers": { "Authorization": "Bearer xyz" },
      "enabled": true
    }
  }
}
```

Multiple bound servers are additive — every entry appears under its own key. Disabled servers are
omitted entirely at injection time (not emitted as the disable form).

---

## Acceptance

- A6, A7, A17 carry "confirmed" status with evidence from the pinned schema.
- US-53.7 and US-53.8 implement against the payload + rendered-config shapes above.
- The `AgentConfigWriter` schema test (`cmd/workspace-agentd/agent_config_writer_schema_test.go`)
  is extended in US-53.8 to assert the emitted `mcp` section validates against the pinned schema.
