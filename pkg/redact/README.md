# pkg/redact — payload redaction pipeline

A regex + exact-value scrubbing pipeline that removes credential-shaped
content from payloads (agent output, relay diagnostics, anything
body-adjacent). It is payload hygiene machinery only — it knows nothing
about how secrets are stored or delivered.

## Static pipeline (16 rules)

Applied in order by `NewRedactor` (see `redact.go` for the authoritative
list): URL-embedded credentials, `Bearer` tokens, GitHub tokens, JSON
`"password":` values, `password=`/`token=`/`secret=`/`api[_-]key=`/
`x-api-key:` assignments, PEM private keys, age keys, `sk-` keys, AWS access
keys, JWTs, `authorization:` headers, and long base64 blobs. Extra patterns
can be layered via `NewRedactor(extra)` or a JSON config file
(`NewRedactorFromFile`; the agent-side default path is
`/sandbox-cfg/redact-patterns.json`, absent → defaults only).

Static behavior is pinned byte-exact by the golden corpus in
`redact_dynamic_test.go` (`TestStaticPipelineGolden`, one row per rule).

## Dynamic rules (staged-key surface, 2026-09-17)

`RegisterDynamic(rules ...DynamicRule)` / `UnregisterDynamic(id)` manage
**exact-value** rules — byte-exact literal matches (no regex
metacharacters), grouped by `ID`: registering an existing ID replaces its
group atomically; unregistering removes it. Rules are goroutine-safe against
concurrent `Redact`.

Two invariants:

1. **Dynamic rules run before the static pipeline, longest value first.**
   A static pattern (e.g. `token=…`) could otherwise fragment a secret
   before the exact match sees it, and a registered value that prefixes
   another registered value could fragment the longer one — ordering by
   descending value length keeps removal atomic and output deterministic.
   Pinned by `TestDynamicPrefixOverlapAtomicAndDeterministic`.
2. **Static behavior never changes when dynamic rules are registered** —
   pinned by the golden corpus run in both modes.

The primary consumer is the relay-key staging provider
(`pkg/secrets/staging_provider.go`, design 0058 §4.9): at seal and resolve
time it registers the staged key material (raw, standard-base64, and
URL-base64 encodings) under an envelope-derived rule-group ID, and the
router's sanitization stage (US-72.2) applies the combined pipeline to
proxied traffic so a key can never be echoed back through the relay.
Revocation (Secret deletion) unregisters the group.

## Consumers

| Consumer | Use |
|---|---|
| `cmd/workspace-agentd` | agent-output scrubbing + the `redact` subcommand (the former `cmd/redact` binary, folded in #1152 / design 0053 S2, #1116) |
| `pkg/secrets` (staging provider) | dynamic staged-key registration (seal/resolve seam) |

**Home:** stays in `pkg/`. Issue #842 once planned relocating it into
`cmd/redact`; that binary has since been folded into workspace-agentd, and
with the staging provider as a second genuine consumer the package is
shared platform machinery, not a CLI internal.
