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

Two fixture PAIRS pin the opencode event surfaces (1.18.10 + 1.18.15).
They exist because wire-shape drift was the root cause of issue #739
(context usage silently NULL for weeks):

| Fixture | Surface | Type names | How captured |
|---|---|---|---|
| `sse_events_1_18_10_live.jsonl` | `/event` SSE stream (what the API tracker + agentd consume) | **unsuffixed** (`message.part.updated`) | verbatim `curl -N /event` capture through a full LLM turn on a live pod; IDs redacted to synthetic sequences, >120-char strings trimmed, `message.part.delta` subsampled 1-in-50 (homogeneous streaming fragments) |
| `event_store_1_18_10.jsonl` | persisted `event` table in `opencode.db` | **version-suffixed** (`message.part.updated.1`) | rows read from a live pod's `opencode.db`, reconstructed into the `/event` envelope shape (cross-checked against the live capture) |
| `sse_events_1_18_15_live.jsonl` | same `/event` surface, opencode 1.18.15 | **unsuffixed** | local `opencode serve` (1.18.15) + OpenAI-compatible mock provider (tool-call round + terminal usage chunk); same redaction, deltas NOT subsampled (only 2 — the subsample rule is volume-driven) |
| `event_store_1_18_15.jsonl` | same store surface, opencode 1.18.15 | **version-suffixed** | rows read from the local serve's `opencode.db` `event` table for the captured session |

**1.18.15 fixture limitations (deliberate, documented):** captured
against the ai-sdk provider runtime (openai-compatible mock), which on
1.18.15 emits `step-start`/`step-finish` PART types but produced no
`tool` parts in this environment. The 1.18.10 pod capture REMAINS the
tool-part lifecycle pin until a staging-pod re-capture on 1.18.15+;
both pairs stay in the tree and every fixture test runs against both.
New in the 1.18.15 live capture: `session.created` on the live stream
(already in the taxonomy), `step-finish` parts with terminal-usage
tokens.

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

---

# opencode upgrade runbook (gated)

Every runtime-image opencode bump (`OPENCODE_VERSION` in
`runtimes/base/Dockerfile`) changes the wire every parser, taxonomy
table, and metering decode depends on. The bump → re-capture pairing is
**enforced** by gates (pre-commit + CI fail a bump without fixture
changes) — this section is the procedure the gates point at.

## Rollout coupling (release checklist)

The event-system env flag lives in the supervisor's spawn seam
(`opencodeChildEnv`, `cmd/workspace-agentd/managed_process.go` — moved
off the deleted entrypoint in design 0053 S3, keeping the #942
containment) — so it now ships with the **agentd delivery artifact**,
not the runtime image. Order: build + push the agentd artifact (and
bump the `agentdDelivery` digest), then roll the controller. A stale
agentd artifact leaves NEW pods with the
flag still in the pod spec (fine), and REBUILT pods on the new image
with it in the entrypoint (fine) — but a controller roll of the
flag-REMOVAL while workspaces still run a pre-#942 image build would
produce pods with the flag in neither place: event-blind, the worklog
0263 regression. Deploy images and controller together.

## Pre-merge (same PR as the bump)

1. **Stage a capture pod** on the new version (staging workspace or a
   scratch `opencode serve` with the new binary) and drive one full LLM
   turn: prompt → tool calls → response → idle.
2. **Re-capture both fixtures** (live `/event` verbatim + store
   `opencode.db` rows; procedures at the top of this file) and replace
   the testdata files.
3. **Extend, don't loosen**: `wire.IsKnownEventType`'s taxonomy and
   `pkg/repolint/event_literal.go`'s literal table must gain any new
   event types — the fixture-coverage test enforces this for wire.
4. **Watch the pinned counts**: `TestGoldenFixture_EventStore_SessionUpdatedAllDecode`
   pins 81 — update consciously, never delete the pin.
5. Full `make test` — the golden taxonomy tests fail loudly on any
   shape drift the tolerant parsers must be taught.

## Post-deploy (within the first hour)

- **Drift counters**: `llmsafespaces_agent_events_total` — every known
  type should be non-zero and growing on active workspaces; the
  `unknown` label must stay flat at zero. A known series flatlining +
  `unknown` growing = taxonomy drift: check the warn log for
  `unrecognized agent event type '<name>'` (one per distinct type).
- **Usage sanity (#739 DoD)**: `session_index.context_used` must be
  non-NULL for sessions that ran a real LLM turn (spot-check via the
  sessions list API or `SELECT count(*) FROM session_index WHERE
  context_used IS NULL AND updated_at > <deploy-time>` on active rows).
- **Billing sanity**: `llmsafespaces_inference_requests_total` and the
  `llm_tokens` usage events must be non-zero — a taxonomy rename that
  removes `session.updated` zeroes these silently (the counters are the
  catch).

## Drift alert (recommended)

Alert on `increase(llmsafespaces_agent_events_total{event_type="unknown"}[15m]) > 0`
sustained 30m — that is the machine-readable form of "upstream renamed
something; run this runbook."

Alert semantics and edge cases:

- The per-type warn log caps at 64 DISTINCT unknown types per API
  process. Beyond the cap, counting continues but warns stop — a full
  taxonomy rewrite (65+ new names) shows up as a large `unknown` rate
  with only the first 64 names in the log. Triage by rate first, then
  capture.
- Events are classified by TYPE before metering decodes properties. A
  rename that only changes a payload's SHAPE (type unchanged) still
  counts as known and surfaces through the metering drift warn
  ("usage-bearing event failed to decode"), not the unknown counter —
  watch both signals.
- Events whose ENVELOPE is undecodable (no type in either shape) also
  bucket under `unknown`, with a single class-level "malformed agent
  event" warn. Envelope-shape drift is therefore as visible as
  type-name drift.

## ABI translation goldens (Epic 69 US-69.3)

`golden/sse_events_*_live_abi.want.jsonl` lock the output of the sole
opencode→contract translation point (`translate_abi.go`,
`TestTranslateABI_GoldenFixtures`). Regenerate after refreshing the live
fixtures:

```bash
REGEN_GOLDEN=1 go test -run TestTranslateABI_GoldenFixtures ./pkg/agent/opencode/
```

Review the diff deliberately: ID fields, event-type mappings, and cost
shapes are contract surfaces (I12 stitch / Epic 33 billing). A diff you
cannot explain means opencode changed shape — update the translation table
in the same commit as the refreshed goldens.
