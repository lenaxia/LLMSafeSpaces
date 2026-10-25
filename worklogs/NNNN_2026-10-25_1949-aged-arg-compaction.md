# Worklog: #1949 relay-router aged-arg compaction — the poisoned-context emission cure

## Objective

Implement the CURE half of the #1849/#1929 split: stub AGED
`llmsafespaces_*` tool-call arguments in the in-context history at the
relay-router (BYO resolve path), keeping call identity, tool name, and
outcome semantics; recent window untouched; on-disk audit trail
untouched; every fire loudly observable. Checked-in replay harness per
the #1849 adjudication's synthetic-harness plan (constructed transcript
+ signature scorer, no DB dependency).

## Validate-first (owner ruling) — performed BEFORE implementing

1. **Compaction does not exist on main** (the stop-report condition):
   `grep -rn 'compact' cmd/relay-router/ pkg/` — every hit is
   session-ledger/opencode session compaction (a different mechanism);
   the BYO path (`byo_server.go`) has no request-body history rewrite.
   #1929's harness is NOT checked in (PR touched only agentd catalog).
2. **Un-stubbed poisoned-context behavior**: the live-model legs could
   NOT re-fire the runaway on today's pathway — full evidence:

| leg | context | runs | verdict |
|---|---|---|---|
| synthetic poisoned (old-shape bulk-first args, new catalog) | constructed | 3 | CLEAN 3/3 |
| exact 10-25 window (epic_activate runaway context, 27 old-shape calls, live 55-tool catalog + platform tools) | opencode.db reconstruction | 3 | CLEAN 3/3 |
| exact 10-24 window (poison-heaviest: 109 llmsafespaces_* calls, 704KB) | opencode.db reconstruction | 3 | CLEAN 3/3 |
| direct latch probe — the RECORDED 29.6KB `acknowledgeInactiveDeps`×157 duplicate-key args re-sent mid-tile, "re-emit the call" | mechanism probe | 3 | CLEAN 3/3 (72B valid args) |
| exact 10-25 + TRUE pinned-binary persona (8532B extracted from the opencode binary) | max fidelity | 1 | CLEAN |

   13 runs, 0 malformations, 0 gateway integrity markers. The recorded
   production corruption from THIS MORNING is on-disk in the pod's
   opencode.db (`invalid` tool parts: `board_epic_activate`
   duplicate-key ×157, 29.6KB, 10-25 02:43:01; `send_message`
   `from_session_id` tiling ×100+; five 19–55KB runaways on 10-23) —
   the class is real, recent, and mechanistically proven by #1929's
   merged 12/12 + 3/3 table; the serving path simply does not
   reproduce it today (best explanation: the gateway's glm-5.3
   serving changed between 10-24 (#1929's runs) and 10-25 — #1849's
   independent validation already showed constructed contexts going
   0/5 on 10-23, i.e. the latch was always pathway-fragile).
   Non-reproduction is documented here as an evidence limit of the
   LIVE leg, not a disproof; the acceptance discriminator is pinned
   deterministically in Go (below) and the live harness ships for
   re-verification whenever the pathway latches again.

## The cure (this PR)

`cmd/relay-router/agedarg.go` — `agedArgCompactor.Compact(body)`:

- Stubs `arguments` of every `llmsafespaces_*` tool call older than the
  recent window (last N tool calls of ANY tool, positional/flattened —
  default 30; `BYO_AGED_ARG_RECENT_TOOL_CALLS` env: 0 = stub all,
  negative = kill switch; threshold-teachable per the issue).
- Stub: `{"note":"arguments removed by relay aged-arg compaction (full
  text preserved in the session audit trail)"}` — single-field,
  bulk-last-safe; byte-identical to the python mirror
  (`TestAgedArgStubMatchesReplayHarness`).
- Preserves: call `id`, `function.name`, role:`tool` result messages
  verbatim, non-llmsafespaces_* calls verbatim (platform tools are the
  proven-safe precedent), unknown top-level fields, untouched messages
  byte-exact (RawMessage fidelity).
- Audit-preserving: the router never persists bodies (K7); the
  workspace's on-disk store keeps every original arg.
- Passthrough-safe: no prefix match / no messages / non-object /
  malformed entries → body forwarded byte-identical; compaction never
  rejects or breaks a request. Idempotent across re-sent turns
  (already-stubbed check; second pass byte-identical, 0 stubs).
- Deterministic output (ordered marshaling — map-randomization would
  break turn-to-turn byte stability).

Wired at byo_server step (4.5): POST /chat/completions only, before
staged-key redaction (what forwards is compacted AND redacted); quota
byte accounting follows the compacted body. Fleet path untouched: it
streams request bodies unbuffered to relay VMs (free-tier); the
poisoned-context class is proven on the credential-backed BYO path
(design 0058 §4.5 — key-delivered credential-backed providers; the K1 carve-out kinds stay on the raw fleet path).

Observability (silent history rewriting is the failure mode):
metadata-only log line per fire (ws/slug/count/window/bytes) + three
Prometheus counters: `llm_relay_byo_aged_arg_compactions_total`,
`..._calls_stubbed_total`, `..._bytes_saved_total` {workspace,provider}.

## Regression pins

- `TestAgedArgCompaction_StubsAgedLLMSafeSpacesArgs` — aged stubbed,
  ids/names survive, results verbatim, bash untouched, recent intact
- `TestAgedArgCompaction_RecentWindowCountsAllTools` — window counts
  all tools positionally; window-covers-all → byte-identical
- `TestAgedArgCompaction_IdempotentAcrossTurns`
- `TestAgedArgCompaction_PassthroughSafety` (5 shapes + kill switch)
- `TestAgedArgCompaction_UntouchedFieldsBytePreserved`
- `TestAgedArgStubMatchesReplayHarness` (Go ↔ python mirror parity)
- `TestAgedArgCompaction_MalformedEntriesLeftUntouched`
- `TestAgedArgCompactorDefaults` (+ nil-safe passthrough)
- Wire path: `TestAgedArgCompactionFiresOnTheWirePath` (upstream
  receives the stubbed body; all three metrics move),
  `..._CleanBodyUntouchedOnTheWire` (byte-identical, no telemetry),
  `..._KillSwitchOnTheWire`
- `TestAgedArgSyntheticReplayCure` — the discriminator, checked in:
  the recorded production runaway shapes (duplicate-key ×157 tiling,
  old-shape bulk-first charters) through the compaction → scorer gates
  (strict-parseable, no key repetition, no ≥10x tiling, bulk gone,
  recent + results intact)
- `TestAgedArgRunConfigEnvKnob`

`scripts/1949-aged-arg-replay.py` — the replay harness (env-driven:
`REPLAY_BASE_URL`/`REPLAY_API_KEY`/`REPLAY_MODEL`, never hardcodes
credentials): deterministic synthetic poisoned transcript + exact-mode
(reconstructed wire context) + the compaction mirror + the signature
scorer (duplicate keys, ≥10x tiling, malformed, native-markup leak,
splice artifacts, size blowup). Arms: neutral / poisoned / compacted.

## Review round 1 (CHANGES_REQUESTED → fixed)

All four findings fixed:

1. **Go↔Python byte parity made TRUE and EXECUTED** (was a false
   claim): the python mirror now emits the stub with compact
   separators (`separators=(",", ":")`); `TestAgedArgStubMatchesReplayHarness`
   now RUNS `scripts/1949-aged-arg-replay.py --print-stub` and
   compares bytes (skip only when python3 is absent); the script
   header's claim scoped to the stub payload.
2. **Production construction pinned**:
   `TestBuildBYOServerPinsAgedArgCompactor` (the
   `TestBuildBYOServerPinsNonNilRedactor` precedent) — non-nil
   compactor + window threading (7 via config; default 30) on the
   `buildBYOServer` path, so dropping the wiring assignment goes red.
3. **Worklog renamed to the `NNNN_` sentinel** (author-picked 1228
   violated the numbering mandate; the post-merge bot assigns the real
   number).
4. **Dead config field removed** (`byoServerConfig.agedArgRecentToolCalls`
   was never read; the live knob is `byoRunConfig` → compactor).

Nits taken: production path uses `agedArgStubJSONString()` (no inline
re-derivation); `slices.Sort` replaces the hand-rolled insertion sort.
Re-run: full package green, vet/gofmt clean, `--print-stub` byte-checks
against the Go literal.

## Review round 2 (CHANGES_REQUESTED → fixed)

- **Blocking: harness import crash on fresh clones** —
  `_PERSONA_FALLBACK` was referenced before definition (the pod-local
  persona file masked it here; this was ALSO the true cause of the
  earlier CI exit-1, not a python-less runner). Persona loading is now
  lazy (exact-mode only); `--help`/`--print-stub` verified with the
  persona file absent.
- **Skip misclassification**: the parity test now FAILS when an
  interpreter exists but the script exits non-zero (captured output in
  the failure); it skips only when neither python3 nor python is on
  PATH.
- `scripts/1949-build-exact-context.py` checked in, documented as
  pod-store lab tooling (the synthetic arm remains the checked-in
  discriminator).
- Phrasing accuracy: K1 carve-out (`bedrock`/`vertex`/`azure_openai`/
  `opencode`-kind stay on the raw mixed-fleet path — NOT covered by
  this cure; corrected on the PR thread); "re-emitted verbatim" →
  value-level fidelity comment (canonical key order on rewritten
  envelopes).

## Test runs

- `go test ./cmd/relay-router/ -count=1` — ok (full package, incl. all
  prior BYO/fleet suites)
- `go vet ./cmd/relay-router/` — clean; `gofmt` — clean
- `go test -race` local pass deferred per the fleet memory directive
  (pod OOM-reap 2026-10-25; heavy-slot order 1947→1946→1948→1949→1836c);
  the PR's CI runs the race leg on GitHub runners
- Live replay legs: table above (13 runs across 5 legs, this pod,
  glm-5.3 via the workspace gateway, 2026-10-25)

## Scope notes

- NO overlap with #1837/#1849 adjudication items (atomic-create 1+6
  live in #1837's lane — untouched here).
- The `w/` BYO path is where platform-provider traffic flows
  (design 0058); direct/personal-key providers bypass the relay and
  are out of the relay-router's reach by design.
