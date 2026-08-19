# Pinned opencode fixtures — refresh procedures

(Originally the config-schema refresh doc; the SSE event-fixture
procedures were appended below in #938. Contents: opencode config
schema, then SSE event fixtures — live wire + persisted store.)

`opencode-config.schema.json` is a pinned copy of opencode's official config schema (source: <https://opencode.ai/config.json>). The chart-side agent-config writer (`pkg/agent/opencode/configwriter.go`, behind the AgentConfigWriter seam) MUST produce output that validates against this schema — enforced by `TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema` and the generic `assertMatchesOpencodeSchema` helper called from every `rebuild()` test.

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
    -o pkg/agent/opencode/testdata/opencode-config.schema.json
go test ./cmd/workspace-agentd/... -run 'TestAgentConfigWriter_Rebuild_MatchesOpencodeSchema'
```

If tests still pass → commit the schema update. If they fail → the writer needs to catch up to the new schema; fix the writer, re-run.

## Note on external `$ref`s

Opencode's schema has four `$ref` targets pointing at `https://models.dev/model-schema.json` — a 226 KB enum of every model on `models.dev`. These are resolved by the loader (`loadOpencodeSchema` in `configwriter_test.go`) by **replacing each `$ref` with `{"type": "string"}`** before compilation. Rationale:

- The writer emits arbitrary provider/model strings from user config; we do not gate on "must be a known models.dev model."
- The models.dev enum changes weekly and would add a huge, unstable dependency for zero contract-testing value.
- If opencode later adds a schema constraint that materially affects our writer's output shape (not the model-name enum), we'll notice via the compilation-time diff.

---

# SSE event fixtures — refresh procedure

Two fixtures pin the opencode 1.18.10 event surfaces. They exist because wire-shape drift was the root cause of issue #739 (context usage silently NULL for weeks):

| Fixture | Surface | Type names | How captured |
|---|---|---|---|
| `sse_events_1_18_10_live.jsonl` | `/event` SSE stream (what the API tracker + agentd consume) | **unsuffixed** (`message.part.updated`) | verbatim `curl -N /event` capture through a full LLM turn; IDs redacted to synthetic sequences, >120-char strings trimmed, `message.part.delta` subsampled 1-in-50 (homogeneous streaming fragments) |
| `event_store_1_18_10.jsonl` | persisted `event` table in `opencode.db` | **version-suffixed** (`message.part.updated.1`) | rows read from a live pod's `opencode.db`, reconstructed into the `/event` envelope shape (cross-checked against the live capture) |

The same logical event type carries different names on the two surfaces — that is why `wire.IsPartUpdated` is suffix-tolerant, not as hedging. `session.status` events exist ONLY on the live SSE stream (never persisted) — agentd's busy/idle tracker depends on that.

## Refresh when

1. A runtime-image opencode bump lands (gated runbook step — see issue #942), or
2. `TestGoldenFixtureTaxonomy_*` fails on main without a seam change — upstream drifted; re-capture before touching the parser.

## How to capture (live fixture)

```bash
PW=$(cat /sandbox-cfg/password)
# from a scratch session, while any LLM turn runs on the pod:
curl -sN -u "opencode:$PW" http://127.0.0.1:4096/event > capture.txt
# then redact: IDs → synthetic, trim >120-char strings, subsample deltas
```

Every fixture event must remain byte-shape-faithful apart from redaction/trimming — parsers are pinned against these bytes.
