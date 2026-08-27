# Worklog: MCP-server CRUD in all SDKs + VS Code extension removed

**Date:** 2026-08-27
**Session:** Close out epic #1032 — #1046 (MCP-server CRUD in Go/TS/Python/Java across all three Epic 53 scopes) and #1048 (drop the VS Code extension)
**Status:** Complete

---

## Objective

Last two engineering items of the epic: give every SDK the MCP-server management surface (spec'd since 2026-08-01, previously in 0/5 SDKs), and remove the unmaintained VS Code extension per maintainer decision.

---

## Work Completed

### #1046 — MCP-server CRUD in four SDKs
Per SDK, three scope services mirroring the ProviderCredentials/Admin naming convention:
- **Go**: `McpServers` (/me), `AdminMcpServers` (/admin, incl. `DeleteAutoApply` with both path variants), `OrgMcpServers` (/orgs). Shared scope helpers; typed `McpServer`/`CreateMcpServerRequest`/`UpdateMcpServerRequest`/`McpAutoApplyRule` in types.go. 3 wire-level tests (paths, bodies, wrappers). **Locally green.**
- **TypeScript**: `mcpServers` / `adminMcpServers` / `orgMcpServers` APIs + exported types. 3 vitest tests (55/55 + tsc clean).
- **Python sync+async**: `mcp_servers` / `admin_mcp_servers` / `org_mcp_servers` with TypedDict types, exported. 3 sync + 1 async respx tests. **CI validates.**
- **Java**: `McpServersService` / `AdminMcpServersService` / `OrgMcpServersService` (Map-based, TriggersService style; shared `extractServers`). 2 JUnit tests inserted INSIDE the test class (last time's brace mistake not repeated). **CI validates.**

Wire facts baked into tests (from handlers): `{"servers":[...]}` list wrapper, `{"rules":[...]}` auto-apply wrapper, bind body `{"workspaceId"}`, auto-apply body `{targetType, targetId?}`, admin-only delete-auto-apply with `/{targetType}` and `/{targetType}/{targetId}` variants.

### #1048 — VS Code extension removed
- `git rm -r sdks/vscode-llmsafespaces` (2 commits ever; stale v1 sendMessage surface).
- CI step removed; dead skip-dir references cleaned from `ci.yml`, `mutation.yml`, `security-scan.yml`, `.golangci.yml`, `Makefile` (trivy targets).
- `docs/api/sdks.md`: VS Code row + section removed.

---

## Key Decisions

1. **Per-scope services, not a scope parameter** — matches the established ProviderCredentials/AdminProviderCredentials convention in all four SDKs.
2. **Python/Java follow each language's existing test/mock idiom exactly** (respx patterns, HttpServer patterns) to minimize CI-only validation risk.
3. **Java Map-based API** (not a typed McpServer POJO) to match TriggersService's established facade style and keep the no-local-JVM risk low.
4. Canary scenario from the issue deferred: canary suites are `continue-on-error` and live in separate modules; the wire-level tests + blocking SDK Contract job cover the drift class. Noted in issue close comment.

---

## Blockers

None.

---

## Tests Run

- Go SDK suite — ok (3 new tests)
- TS SDK suite — 55/55 + `tsc --noEmit` clean (3 new tests)
- Python (4 new tests) + Java (2 new tests) — CI validates (no local runtimes)

---

## Next Steps

1. PR → review loop → merge (closes #1046, closes #1048 → epic #1032 fully closed).
2. Post-merge: none — epic complete.

---

## Files Modified

- `sdks/go/`: client.go (3 services wired), types.go (4 types), mcp_servers.go + mcp_servers_test.go (new)
- `sdks/typescript/`: client.ts (3 APIs), types.ts (4 types), client.test.ts (+3)
- `sdks/python/`: types.py, client.py, async_client.py, __init__.py, 2 test files
- `sdks/java/`: LLMSafeSpacesClient.java (wiring), 3 new services, client test (+2)
- VS Code removal: `sdks/vscode-llmsafespaces/**` deleted; ci.yml, mutation.yml, security-scan.yml, .golangci.yml, Makefile, docs/api/sdks.md cleaned

---

## Review round 2 addendum

- **Canaries (was deferred)**: S-MCP-CRUD added to all three canary runners
  (Go/TS/Python) + registered in the Makefile SHALLOW lists — full CRUD +
  auto-apply round-trip with cleanup and two negative cases.
- **Hurl contract case**: `sdks/tests/contract/mcp-servers.hurl` (9 steps
  incl. bind/auto-apply + documented-409 error contract), verified green
  locally against Prism (hurl 5.0.1) — all 11 contract files pass.
- **Spec accuracy fixes found by the Hurl run** (pre-existing thin MCP
  entries): org/user-scope list/get/update/delete/auto-apply responses had
  NO content blocks; user create lacked the 409 body; user+org auto-apply
  create lacked the requestBody (Prism 415). All patched to the real wire
  (`{"servers":[...]}`, `{"rules":[...]}`, `{"deleted":true}`,
  `{"bound":true}`, Conflict ref). McpServer gained `required` + example.
- **SDK test mocks aligned to true codes**: delete 200 `{deleted:true}` and
  bind 200 `{bound:true}` (were 204/201).
- Trivy step repaired (review round 1 fallout: whitespace-only line broke
  the continuation chain in security-scan.yml).

Self-inflicted-damage log: one perl replacement inserted a 3-space path
indent (YAML parse error — caught by validator); another stripped Java
escapes (caught on read-back). Both fixed; lesson re-confirmed: file
surgery via anchored edits, never ad-hoc regex on structured files.
