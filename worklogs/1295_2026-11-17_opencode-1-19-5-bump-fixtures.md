# opencode 1.18.15 → 1.19.5 bump: golden-fixture refresh

**Date:** 2026-11-17
**PR:** #2083 (chore/opencode-bump-1.19.5)
**Scope:** fixture/testdata refresh accompanying the automated pin bump
(`runtimes/opencode/Dockerfile` ARG + `local/bootstrap.sh` default, already
on the branch). No parser, taxonomy, or writer code changes.

## Why this worklog

The bump PR's original push used a bot token that carries no workflow
events — CI never ran until an empty retrigger commit. The Lint job's
fixture-freshness gate (#942) then failed as designed: an `OPENCODE_VERSION`
bump without a `pkg/agent/opencode/testdata/` refresh. This is the runbook
execution (`pkg/agent/opencode/testdata/REFRESH.md`) the gate points at.

## Capture (local serve, 1.19.5 binary)

Method mirrors worklog 0851 / the 1.18.15 pair: `opencode serve`
(v1.19.5 release tarball) booted the proven way (integration_harness.go
recipe: synthetic XDG/HOME, `OPENCODE_CONFIG`, detach, log-to-file),
OpenAI-compatible mock provider (`@ai-sdk/openai-compatible`, fixed low
port, Connection-close per response), Basic auth, one full turn driven
through V1 `POST /session/:id/message`.

Turn design: title-gen answered "fixture capture"; the agentic round 1
emitted one **bash tool call** (`echo fixture-capture-ok`), round 2 plain
text + terminal usage chunk. `/event` subscribed verbatim via `curl -N`
post-health (pre-health subscriptions never yield frames on this binary);
store rows read from `opencode.db` `event` table ordered by insertion.

Redaction per REFRESH.md: one shared ID map across live-then-store
(evt/ses/msg/prt/call → prefix + 22-digit counters, the 1.18.15 pair's
continuation convention), >120-char strings → 60 chars + `…[trimmed]`
(the 70-char convention measured on the 1.18.10 fixtures), the single
`message.part.delta` kept (volume rule).

## Results

- `sse_events_1_19_5_live.jsonl` (90 events) + `event_store_1_19_5.jsonl`
  (29 rows) added; prior pairs stay. **The tool-part lifecycle** (pending →
  running → completed, callID, state.input) is now pinned on the current
  version — resolving the documented 1.18.15 limitation (its ai-sdk runtime
  produced no tool parts).
- **No new event types** on either surface — `wire.IsKnownEventType` and
  `pkg/repolint/event_literal.go` unchanged. `session.created`,
  `step-finish` w/ tokens carry over from 1.18.15.
- Test registrations + conscious re-pins: completion-anchor classes
  live {4,4,2} / store {4,2,4}; store `session.updated` decode pin **6**;
  taxonomy/session-updated/adapter step-finish lists extended; ABI golden
  `golden/sse_events_1_19_5_live_abi.want.jsonl` regenerated
  (`REGEN_GOLDEN=1`, reviewed: bash tool part status 1→2→3, session
  summaries carry model/provider attribution — old goldens byte-identical).
- `opencode-config.schema.json` refreshed from
  <https://opencode.ai/config.json>: upstream restructured the document
  (flattened `$defs/Config` → top-level `properties`, new `shell`,
  `default_agent`, `update` keys; 3 external models.dev `$ref`s remain).
  Writer tests pass against the new pin unchanged
  (REFRESH.md rule: "if tests still pass → commit the schema update").

## Empirical validations on the 1.19.5 binary (bump-PR checklist)

1. **V1 surface**: sync send, session create, `/event` SSE, statuses —
   exercised by the capture itself and by the full integration battery
   below.
2. **V2 drain (#755 class)**: behind a 4s-held running turn, two
   `delivery:"queue"` prompts admitted (200, admittedSeq 9 then 11) and
   promoted FIFO, each answered with the exact expected text — the
   queue-drain fixes stay fixed on 1.19.5.
3. **Origin-plugin freeze pin**: `TestOriginPlugin`,
   `TestPermissionTier|…SyntheticDataHome`, `TestOriginE2E`,
   `TestIntegration1722` (all `-tags=integration`, `OPENCODE_BINARY`
   pointed at the 1.19.5 tarball) and `scripts/1530-arg-mutation-probe.mjs`
   — all green locally; the `tool.execute.before` hook contract is intact.
4. **Known-bad window**: 1.19.5 sits outside v1.18.16–.18; nothing new to
   add to the skip list.

## Known-bad / hazards

None found. The 1.18.15-limitation note in REFRESH.md updated (tool parts
now pinned current).

## CI follow-ups on the same PR (same session)

- **Trivy**: CVE-2026-112366 (picomatch) landed in the Trivy DB mid-flight
  (main and every open branch went red 2026-11-17 19:15–19:51Z). Accepted
  in `.trivyignore` per convention: transitive dev-dep of build tooling
  (vite/vitest), never shipped (frontend runtime copies only dist/), no
  fixed release yet.
- **Review gate**: the bump bot authors PRs as `github-actions[bot]`, which
  sits in ai-workflows' default bot-skip list — bump PRs were structurally
  reviewless (a `synchronize` event is not enough). Extended
  `canonicalTrustedAppBots` (#1710 single-source) with `github-actions[bot]`
  across all four copies (gha_callers_test.go, ai-comment.yml arm + input,
  pr-review.yml input). An intermediate commit accidentally introduced a
  duplicate `trusted_app_bots` key (invalid workflow, jobless run failure)
  — fixed by merging into the single canonical entry; drift tests green.
