# #1530 — send_message arg-mutation race: investigation, falsification, and the emission-duplication recovery

Date: 2026-09-24
Branch: `fix/1530-arg-mutation-race`
Status: PR open (iterate to APPROVED, no merge)

## The assignment

The orchestrator's hypothesis: the origin plugin's in-place arg mutation
(`output.args.lsp_injected_session = input.sessionID`,
runtimes/opencode/plugins/llmsafespaces-origin.js:49) races the MCP
serializer, duplicating id fields when calls carry both origin and
target params — the root cause behind 13 delivery misfires (the
orchestrator's 11, this worker's 2, both while REPORTING the #1525
fix). Mandate: reproduce with a probe, inspect for object aliasing,
compare shapes against the observed reports; fix at the plugin if
confirmed, honest negative if not.

## Finding 1 — the race is FALSIFIED (probe A, committed)

`scripts/1530-arg-mutation-probe.mjs` replicates the pinned opencode
1.18.15 dispatch exactly — verified against upstream sources, not
assumed:

- `plugin/index.ts` trigger(): hooks run SEQUENTIALLY, AWAITED, over
  one shared `output` object per call site.
- `session/tools.ts:106-111`: `trigger("tool.execute.before",
  {tool, sessionID, callID}, {args})` then `item.execute(args)` — the
  SAME args reference flows hook → execute → wire → after-hook.
- The wire is JSON.stringify inside the MCP client's HTTP body
  (remote HTTP transport to agentd :4097, per agent-config.json).

Four legs, 11,501 dual-param wire bodies: sequential (10k, injection
correctly OVERWRITES a stale model-supplied lsp_injected_session),
concurrent per-call args (500), ALIASED shared args across two
interleaved dispatches (501 — the aliasing worst case; snapshot
ambiguity between origins is real, torn JSON is not), and literal
mutation-DURING-stringify via hostile Proxy (1000). Zero corrupt
bodies. Structural conclusion: V8's synchronous JSON.stringify cannot
emit duplicated keys or unbalanced quotes from ANY object state. The
aliasing upstream is real but benign.

## Finding 2 — the actual root cause: MODEL EMISSION (probe B, committed)

The corrupt values across all 13 instances contain ESCAPED quotes
(`\",\"lsp_injected_session\":\"…\"`) — backslash escapes exist only
in JSON SOURCE TEXT. Object serialization cannot inject them; the
corruption therefore predates serialization: the MODEL duplicated the
tail of a similar prior send_message call — a call made VISIBLE in
context by the plugin's injected field (the model then imitates and
sometimes duplicates it; this worker's two misfires were exactly
this, live). `scripts/1530-corrupt-args-liveprobe.sh` drives agentd
raw-wire with the exact shapes: all fail cleanly with resolution
errors — the message is LOST, but the intent (leading target id) is
mechanically recoverable.

## The fix — agentd-side recovery + warning (the #1525 pattern)

`splitDuplicatedArgFragment` (mcp_tools.go) detects the exact
fragment mark; the leading id is recovered as the target, the send
DELIVERS, and the result warns loudly (omit-when-clean — composed
with the #1525 self-send warning into one field). The fragment's
embedded origin is never trusted (mismatch is surfaced as cosmetic;
the resolved origin — injected or declared — wins attribution).
Recovered targets still pass SessionExists; anything that is not the
exact shape takes the original path unchanged.

Tests (red-first — the initial failure reproduced the production
error byte-for-byte, including the `failed to resolve session
ses_X\",\"lsp_injected_session\":\"ses_Y\"}` signature):
RecoversTargetAndWarns (delivered payload carries no fragment bytes),
NoBraceVariant, MismatchedEmbeddedOriginStillDelivers,
AbsentOnCleanIDs (no mangling; clean bogus ids keep the plain
not-found path). Full package green (304.6s), vet/gofmt clean.

## Byproduct — filed as its own issue, not fixed here (#1561)

Probe B's first draft found agentd's HTTP layer SALVAGES invalid JSON
bodies (unescaped garbage mid-string parses as a prefix; trailing
keys silently dropped → "message is required" masks transport
corruption). Orchestrator ruling: separate issue. #1561 filed.

## Honest boundaries

- The race could still bite a FUTURE async-unsafe plugin rewrite —
  probe A ships as the standing regression harness for exactly that.
- Recovery is shape-exact, not heuristic: a fragment WITHOUT a
  well-formed leading id still fails as today (by design — the guard
  must never guess).
- The model-side trigger (field visibility in transcript) is
  upstream harness behavior; agentd defense is the actionable layer.
