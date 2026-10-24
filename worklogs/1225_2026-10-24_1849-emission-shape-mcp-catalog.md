# Worklog: #1849 emission-shape hardening — exact-context replay RCA + agentd MCP catalog fix

## Objective

Reproduce and root-cause the recurring duplicate-key tool-arg runaway
(`"force":true` ×2618, `epicId` ×601, #1832's `dependsOn` ×1583) on
glm-5.3, then fix it at the source in the agentd MCP catalog — no
tolerance hacks, no fail-open.

## Evidence (exact replay harness, litellm → glm-5.3)

Reconstructed the exact failing request from the live pod's opencode.db
(history + pinned-binary system prompt + live tools/list): 6/6 runs
corrupt; 1 full runaway to the 16k cap (`"force":false` ×2601 — the
live incident was ×2618). Zero gateway markers (`integrity=0`,
no `<arg_key>`) — native model emission, not the proxy.

The decisive factorial (3 runs/arm, everything else identical):

| arm | prior call args | results | tools | verdict |
|---|---|---|---|---|
| base + 5 variants | full | full | ours | RUNAWAY 12/12 |
| neutral history | — | — | ours | CLEAN 3/3 |
| generic `delete_epic` | full | full | swap | CLEAN 3/3 |
| prior args stubbed | stubbed | full | ours | CLEAN 3/3 |
| results stubbed | full | stubbed | ours | RUNAWAY 3/3 |

**Cause:** the model pattern-continues its OWN prior
`llmsafespaces_*` call arguments when those tools are callable; our
bulk fields (repeated-skeleton arrays, long freeform strings) were
emitted FIRST by alphabetical Go-map marshaling — the
resume-after-string / repeated-skeleton seam where glm emission
latches into duplicate-key loops. Ruled out by ablation: ids, schema
prose, booleans, results, reasoning, payload size.

**Why platform tools never corrupt (4853 calls, 0):** bash/read/edit/
write emit single freeform keys with the long string LAST — no keys
owed after the string, no repeated skeleton. `send_message`
corrupted 5/5 with the long `message` first and `session_id` owed
after (Unterminated string / Expected '}').

## Changes (PR #1929)

- 10 ordered `json.RawMessage` schemas (discrete ids/flags first,
  bulk arrays mid, longest freeform string LAST): board create/update
  epic/task, add/remove dependency, import_plan, send_message,
  create_session, call_with_model
- `send_message`: `session_id` → `destination` (alphabetically safe:
  destination < message — survives even a regression to map
  marshaling); `from_session_id` REMOVED — origin injected-only;
  degraded pods refuse with a report-the-platform error
- one camelCase catalog (sessionId/includeArchived/allSessions/
  descriptionFile/outcomeFile) with one-release snake_case legacy
  fallbacks at dispatch (`argStr`/`argBool`, presence semantics)
- MCP annotations (destructive/readOnly/idempotent/openWorld) and
  structuredContent twin (`send_message` outputSchema)

## Regression pins

`TestMCPCatalogEmissionShape_BulkLastEverywhere` (recursive, raw
wire bytes — reviewer mutation-verified red without the fix),
`TestMCPCatalogNoSnakeCaseProperties`, `TestMCPCatalogAnnotations`
(complete: idempotent pair + openWorldHint:false),
`TestMCPCatalogAdvertisesNoAttestationFields`,
`TestMCPSendMessage_SchemaInjectedOnly_MessageLast`,
`..._StructuredContentTwin`, `..._NoOriginAtAllRefusesDelivery`,
`..._LegacySessionIdAliasDelivers_HandlerLevel`,
board-file-params: legacy-spelling consumed (path+null),
both-spellings refused, bare `_file` refused.

## Routed elsewhere (not this PR)

- relay-router aged-arg compaction (kills the precedent class
  fleet-wide, incl. send_message prose) — follow-up PR
- atomic-create adjudication items 1+6 (placeholder-class rejection
  at board_create_task) — #1837's tracking home
- synthetic replay harness + native-pathway canary as repo pins —
  follow-up; the harness evidence above is external (litellm)
