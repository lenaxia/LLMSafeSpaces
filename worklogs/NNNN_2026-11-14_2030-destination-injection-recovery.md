# Worklog: #2030 — generalized destination-injection recovery (post-v3 emission class)

## Context

v0.44.13's aged-arg restructure (PR #2024) eliminated the megastorm
class (60/60 pre-rollout events → 0 post-rollout). The residual class
(#2030, opened by the merry-finch-loses gauntlet agent with its own
data): send_message with medium+ bodies at 3rd+ turn position —
duplicate-key storms, safely rejected, sends lost. Two PARSEABLE live
losses (2026-11-14 04:59 / 06:05) showed the new fragment vocabulary:
escaped-JSON structural fragments injected INTO the destination string
value (`TARGET","message":"<prose>"}`), with a clean leading id and an
intact message field. The #1530 recovery didn't fire (its shape
matched only the old lsp_injected_session vocabulary).

## What shipped

recoverInjectedDestination: split at the first quote (a clean ref can
never contain one), recover the leading ref (phrase/ses_ form,
re-validated by SessionExists), deliver with a loud warning;
malformed-after-quote and non-ref prefixes fall to the normal error
path unmangled. Precedence pinned: a valid-origin #1530 shape keeps
the OLD extractor and its richer embedded-origin warnings.

## Root cause framing (issue #2030 adjudication)

The "context-bleed sampler" the gauntlet agent hypothesized is the
in-context pattern-continuation mechanism proven by the #1849
factorial and direct-to-z.ai replays (no relay in path, 3/3
reproduction) — attention draws fragments from adjacent context
(recent tool results, reasoning) during long escaped-JSON string
emission. NOT the relay seam: v3 touches request history, and the
failures reproduce byte-identically without any relay. The restructure
removed the tool-call template (megastorm class dead); the remaining
attractor is escaped-JSON-in-string emission itself — long bodies stay
fragile (the gauntlet's own verdict), which is why this recovery
(rather than prevention) is the right seam for the parseable subclass.

## Tests

Direct unit table (9 rejection rows incl. the tail-guard arm,
boundaries; 6 acceptance rows incl. the v1-junk-origin continuity
claim), precedence pin (v1-valid keeps the rich warning), handler
pins for both live shapes (+ warning field), recovered-nonexistent
fails loudly naming the STRIPPED id, clean-destination non-mangling.
