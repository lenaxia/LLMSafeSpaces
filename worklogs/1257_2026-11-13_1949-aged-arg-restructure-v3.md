# Worklog: #1949 v3 — aged-arg history restructure (markdown bullets), verified cure

## Context

The glm-5.3 duplicate-key/tiling runaway class (60/60 events across three
workspaces, always inside the first tool call of a generation) is seeded by
prior tool-call STRUCTURE in history — the model pattern-continues its own
prior `{"key":...}` emission blocks. Experiment log (all exact-context
replays via litellm): names ×7, schemas ×6, value formats, batching, echo,
descriptions, sampling knobs — all ruled out. The only levers that ever
produced clean emissions: (a) neutralize prior arg VALUES (dotting), (b)
remove the tool-call STRUCTURE (this work).

## What shipped

v3 in cmd/relay-router/agedarg.go: prior tool usage (ALL families — the
validated config) renders as plain markdown inside the assistant message
that made the calls:

    - Called `tool_name`(**param**: value, ...)
      - Result: <result content verbatim>

replacing the tool_calls entry + paired tool messages. LOSSLESS: params and
outputs stay fully visible. Determinism contract pinned (pure function of
message bytes; own-key-order token decode; json.Number; no HTML escaping;
no timestamps) — one prefix-cache bust per message at enablement, byte-
stable forever after.

## Verification

- Harness (python mirror): myriad 3/3 CLEAN (baseline 1/1 runaway),
  quick-panda 3/3 CLEAN (baseline 3/4 runaway) — model still emits correct
  new tool calls.
- Shipped Go transform bytes replayed end-to-end: myriad 2/2 CLEAN,
  quick-panda 2/2 CLEAN. 10/10 total across both testbeds.
- Tests: agedarg suite rewritten for v3 semantics (restructure correctness,
  determinism byte-pin, idempotence, passthrough, window protection,
  kill switch, synthetic replay cure, executed Go↔python render parity).

## Post-mortems honored (do not regress)

v1 prose-note stub: FAILED in production (word-bearing stub = fuel; leaked
into 4 delivered agent messages). v2 value-dotting: clean but lossy. A
NotContains pin forbids the v1 sentence in the harness; the header carries
both post-mortems.
