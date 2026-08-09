# Pinned opencode config schema — refresh procedure

`opencode-config.schema.json` is a pinned copy of opencode's official config schema (source: <https://opencode.ai/config.json>). The chart-side agent-config writer (`cmd/workspace-agentd/agent_config_writer.go`) MUST produce output that validates against this schema — enforced by `TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema` and the generic `assertMatchesOpencodeSchema` helper called from every `rebuild()` test.

## Why pinned in-tree

Hermetic tests. Fetching the schema at test time couples CI to opencode's website availability (and gives us undetectable drift when opencode updates the schema mid-run). Pinning gives:
- Repeatable failures across CI/dev/reviewer laptops.
- A clean git diff every time the schema changes upstream — reviewers see exactly what opencode added/removed and can update the writer accordingly.
- Bisectability: a schema drift is a git-log finding, not an oracle mismatch.

## Refresh cadence

Refresh when either:
1. **Chart release** — every `ts-*` build tag bump against a new upstream opencode version.
2. **Test failure** — if `TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema` starts failing on main without a writer change, that's the signal that opencode changed the schema and we haven't refreshed.

## How to refresh

```bash
curl -sSL https://opencode.ai/config.json \
    -o cmd/workspace-agentd/testdata/opencode-config.schema.json
go test ./cmd/workspace-agentd/... -run 'TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema'
```

If tests still pass → commit the schema update. If they fail → the writer needs to catch up to the new schema; fix the writer, re-run.

## Note on external `$ref`s

Opencode's schema has four `$ref` targets pointing at `https://models.dev/model-schema.json` — a 226 KB enum of every model on `models.dev`. These are resolved by the loader (`loadOpencodeSchema` in `agent_config_writer_test.go`) by **replacing each `$ref` with `{"type": "string"}`** before compilation. Rationale:

- The writer emits arbitrary provider/model strings from user config; we do not gate on "must be a known models.dev model."
- The models.dev enum changes weekly and would add a huge, unstable dependency for zero contract-testing value.
- If opencode later adds a schema constraint that materially affects our writer's output shape (not the model-name enum), we'll notice via the compilation-time diff.
