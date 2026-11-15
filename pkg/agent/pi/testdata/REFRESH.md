# Pinned pi fixtures — the S6 drift-gate regime

pi (`earendil-works/pi`, the second harness) moves at ~2 releases/week
and has **no protocol-version handshake** on its RPC surface — there
is no `initialize`-style versioned exchange (contrast opencode ACP's
`protocolVersion`, design 0063 §3.1); negotiation is de-facto *pinned
binary + fixtures* (scoping report §7). Everything the platform decodes
from the RPC stream is therefore pinned here, against the overlay's
`ARG PI_VERSION` (`runtimes/pi/Dockerfile`), and a bump MUST refresh
these fixtures in the same PR. Two gates enforce that:

1. **CI diff-gate** (`scripts/pi-bump-gate.sh`, wired into `ci.yml`
   beside the opencode #942 gate): `PI_VERSION` changed without a
   change under `pkg/agent/pi/testdata/` fails CI before any test
   runs. `pi-version-bump.yml` opens bump PRs whose merge checklist
   points here.
2. **In-package drift gates** (`driftgate_test.go`): the refreshed
   fixtures are diffed against `rpc_surface.json` (the recorded
   surface) in BOTH directions — tolerance is the adapter's runtime
   posture, detection is the fixture's job (details below).

## Fixture inventory

| Fixture | Surface | How captured |
|---|---|---|
| `events_stream.jsonl` | RPC stdout event stream (what the supervisor's event pump + the adapter's translator consume) | live capture, pinned binary in RPC mode against a fixture OpenAI endpoint (procedure below); one JSON object per line, payload at envelope TOP level |
| `get_state_response.json` | `get_state` response envelope | recorded verbatim from the same capture (`{"type":"response","command":"get_state",...,"data":{...}}`) |
| `get_session_stats_response.json` | `get_session_stats` response envelope | same capture |
| `get_available_models_response.json` | `get_available_models` response envelope (control surface) | same capture |
| `fixture_session.jsonl` | the persisted session file (session-format.md v3) — the real artifact written by the pinned binary under `--session-dir` | copied from the capture's session dir; IDs/paths left as recorded (they are synthetic already) |
| `rich_session.jsonl` | session file with tool/branch richness (VerifyDelivery / tree shapes) | same |
| `rpc_surface.json` | **the recorded surface manifest**: command names + fields, response data fields, event types + the required fields the platform consumes, `piVersion` pinned to the Dockerfile ARG | hand-maintained FROM a capture; `TestDriftGate_ManifestPinnedToOverlay` ties it to the pin |
| `commands_golden.jsonl` | the OUTGOING wire — every RPC command the adapter emits across a scripted full-surface scenario | generated: `REGEN_GOLDEN=1 go test -run TestDriftGate_GoldenCommands ./pkg/agent/pi/` |

## Why pinned in-tree

Hermetic tests (the opencode REFRESH.md rationale, verbatim intent):
repeatable failures across CI/dev/reviewer laptops, a clean git diff
every time the wire changes upstream (reviewers see exactly what pi
added/removed), and bisectability — wire drift is a git-log finding,
not an oracle mismatch.

## Refresh when

1. **A `PI_VERSION` bump lands** (the `pi-version-bump.yml` PR — the
   CI diff-gate rejects a bump without testdata changes), or
2. **A `TestDriftGate_*` failure on main without a seam change** —
   upstream drifted; re-capture before touching the parser.

## How to capture (the pi-eval harness)

The fixtures were captured from the pinned binary (v1.5.2, 2026-11-13)
using the RPC-mode harness validated in the scoping spikes (see
`/workspace/pi-eval` during the S1–S5 work; reproduce anywhere the
pinned binary + python3 exist):

```bash
mkdir -p pi-refresh && cd pi-refresh
# 1. Fixture OpenAI endpoint (SSE streaming, one text + one tool-call
#    round; the fake from the scoping spikes):
python3 fake_openai.py "persisted reply" 0 &      # :18085

# 2. Drive the pinned binary in RPC mode over a fifo:
mkfifo ev.in
./pi/pi --mode rpc --session-dir ./pi-sessions < ev.in > ev.out 2>&1 &
exec 9>ev.in
sleep 6

# 3. One full turn (drives prompt -> the whole event lifecycle), then
#    the command surface:
printf '%s\n' '{"id":"p1","type":"prompt","message":"remember the number 42"}' >&9
sleep 5
printf '%s\n' '{"id":"g1","type":"get_state"}' >&9
printf '%s\n' '{"id":"g2","type":"get_session_stats"}' >&9
printf '%s\n' '{"id":"g3","type":"get_entries"}' >&9
printf '%s\n' '{"id":"g4","type":"get_available_models"}' >&9
sleep 3

# 4. Stop the binary (SIGTERM once — repeat-signal exits immediately,
#    rpc.md #11752 semantics; do not SIGKILL unless wedging on purpose).
```

Then extract, in order:

- `events_stream.jsonl` — the non-`response` lines of `ev.out`, in
  order. **Redaction rules** (byte-shape-faithful apart from these):
  IDs may stay (they are synthetic already) but if the capture used a
  live provider, map every id to a synthetic sequence CONSISTENT
  within the fixture; trim >2000-char strings (system-prompt sections
  carry prompt text that is environment-specific, not wire-shape);
  never subsample or reorder — lifecycle ordering is what the
  translator tests pin.
- `get_*_response.json` — the matching `response` lines, verbatim.
- `fixture_session.jsonl` — the newest `<ts>_<id>.jsonl` under
  `pi-sessions`, with the same trimming rule for long prompt strings.

Finally, refresh the derived artifacts:

```bash
REGEN_GOLDEN=1 go test -run TestDriftGate_GoldenCommands ./pkg/agent/pi/
go test ./pkg/agent/pi/ ./cmd/workspace-agentd/...
```

`commands_golden.jsonl` regenerates from the adapter scenario (it
should NOT change on a pure bump — it pins OUR outgoing wire; if it
changes, that is a platform-side decision, review it separately).

## What the drift gates check (and what they deliberately allow)

pi's own consumer posture is tolerance — "Consumers should treat
unknown `kind` values as normal usage rather than rejecting them"
(session-format.md) — and the adapter follows it (defensive decode,
unknown event types classified as tolerated non-events, one bad line
never kills a stream; `events.go`, `sessionfile.go`). The gates split
along that line:

- **Tolerated (runtime, both directions of time)**: unknown JSONL
  fields on any record; unknown event types; unknown session-entry
  kinds. Pinned by `TestDriftGate_ResponseTolerance_UnknownFields`,
  `TestParseEvent_ToleratesUnknownFieldsAndTypes`,
  `TestScanSessionFile_CRLFTolerantAndDefensive`. A newer pinned
  binary adding fields must not break this reader — pi's own history
  proves the class (the `disposition` field was ADDED to
  prompt/steer/follow_up responses on 2026-09-25; `clear_queue` was
  ADDED on 2026-08-25).
- **Detected (fixture refresh)**: an event type the capture LOST
  (upstream removal/ rename — the U-8 settle:
  `TestDriftGate_FixtureCoverageBidirectional`), a required field the
  capture no longer carries on a known type (rename —
  `TestDriftGate_EventRequiredFieldsPresent`), a response echoing a
  command name we did not send (command rename —
  `TestDriftGate_RecordedResponsesMatchManifest`), a NEW type in the
  capture without a taxonomy/manifest row
  (`TestEventFixtureCoverage_KnownEventTypes` +
  manifest sync), and the manifest/pin coupling
  (`TestDriftGate_ManifestPinnedToOverlay`).

When a refresh trips a detection gate: update `rpc_surface.json` AND
the decoder/taxonomy in the same change, consciously. Never delete a
manifest row to make a test pass — retire it with the evidence (the
upstream commit/release that removed it) in this file's changelog
below.

## `pi --version` -> statusz (fleet currency)

The supervisor probes `<binary> --version` once at construction
(non-fatal; empty = unreported) and it flows
`piSupervisor.agentVersion` -> `HarnessStatus` -> `piStatuszResponse`
-> statusz `AgentVersion` (`pkg/agentd/types.go`, the generic RC-A
fleet-currency field). Pinned by
`TestProbePiVersion`/`TestPiSupervisor_BootVersionFlowsToHarnessStatus`
in `cmd/workspace-agentd`. A bump PR should spot-check statusz on a
staged pod shows the new string.

## Post-bump spot checks (first hour)

- statusz `AgentVersion` = the new pin on staged pods.
- Drive one turn end to end (prompt -> tool call -> settle); the
  supervisor's busy/settled derivation and the adapter's translation
  are pinned to these fixtures — a silent lifecycle-semantics change
  (the #11752/#11812 shutdown class) shows up as a hung
  busy-forever session or a missing settle, not as a decode error.
- Session files still parse: `TestContract_*` + `TestScanSessionFile_*`
  green locally is the pre-merge proxy.

## Fixture changelog

- 2026-11-13 — initial capture against v1.5.2 (the scoping-validated
  pin): events, three response envelopes, session file, surface
  manifest, command golden.
