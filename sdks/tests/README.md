# SDK Contract Tests

Language-agnostic contract tests defined as [Hurl](https://hurl.dev/) files. These verify that all SDKs produce correct HTTP requests and handle responses identically.

## Running

```bash
# Against a running API server
make -C .. contract-test BASE_URL=http://localhost:8080 TOKEN=lsp_xxx

# Against Prism mock server (validates requests match OpenAPI spec; no live API needed)
make -C .. contract-test-mock

# Or invoke Hurl directly
hurl --variable base_url=http://localhost:8080 --variable token=<jwt> contract/*.hurl
```

CI runs `make contract-test-mock` on every PR via the `sdk-contract` job
(`.github/workflows/ci.yml`). It installs Hurl + Prism, spins up a mock from
`sdks/openapi.yaml`, and runs every file below.

## Test Files

| File | Coverage |
|------|----------|
| `auth.hurl` | Register, login, API key CRUD, auth header format |
| `workspaces.hurl` | Create, list, get, rename, suspend, activate, delete |
| `sessions.hurl` | Ensure, list, get, abort, active-session pointer |
| `sessions_contract.hurl` | Contract Session on getSession, prompt receipt bodies, bodyless 204s on abort/delete (#1304) |
| `sessions_delete_seen.hurl` | Mark-seen (204), delete (204) |
| `sessions_queue.hurl` | Enqueue receipt, queue list, dismiss |
| `pagination.hurl` | limit/offset honored, pagination envelope shape, audit log |
| `errors.hurl` | 400, 401, 404 error format consistency |
| `inputs.hurl` | InputRequest lists, reply vocabulary, late-answer 202 body, dismiss exit (#1302) |
| `mcp-servers.hurl` | MCP server CRUD (Epic 53) |
| `orgs.hurl` | Org CRUD, invitations, policies |
| `passkeys.hurl` | WebAuthn ceremony shapes (Epic 59) |
| `workflows.hurl` | Workflow CRUD + runs (Epic 64) |

## Contract Guarantees

All SDKs must produce HTTP traffic equivalent to these Hurl files:
1. Correct URL paths and HTTP methods
2. Correct request body JSON structure
3. Correct Authorization header format (`Bearer <token>`)
4. Correct handling of 2xx, 4xx response codes
5. Correct parsing of response JSON into typed objects
