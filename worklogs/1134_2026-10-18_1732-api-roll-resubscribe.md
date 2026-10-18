# Worklog: #1732 — agentd never re-subscribes /api/v1/events after an api roll (deploy wedges in-flight sessions)

**Date:** 2026-10-18
**Session:** w35 — fix worker for #1732 (scope-amended by the owner mid-lane)
**Status:** Complete (PR pending review)
**Branch:** `fix/1732-api-roll-resubscribe`

---

## Objective

Close #1732 as amended: the lane's GATING deliverable was a request-path
map (where provider calls hop; whether any hop terminates on the api
pod) plus halt-mechanism confirmation from the deploy evidence — THEN
fix both legs against the map: (A) request-path survival across rolls
with an abort-classification so infra loss never counts as user abort,
and a bounded auto-resume; (B) the original event-stream re-subscribe
scope. Acceptance: red-first integration test replacing api/relay
mid-session → turn SURVIVES or auto-resumes with roll-classified loss,
session unwedged, usage-gate state returns; unit pins for the reconnect
state machine's peer-replacement arm; runbook row.

## The request-path map (posted as issue comment 6252847335 — the gating deliverable)

Deterministic code-path mapping at main `0b3f0897`, cross-referenced
with the 07:10Z metrics export (#1724 comment 6247075096):

1. **Provider paths, three variants.** (a) Free relay models: opencode
   (workspace pod) → `relay-router.<ns>.svc:8080` (fleet router, 1
   replica) → relay VMs (untouched by deploys) → upstream. (b) BYO
   staged keys: opencode → `llm-relay-router` (2 replicas, PDB, preStop,
   TGS 630, drain 600s — #1078-hardened). (c) BYO direct: egress
   direct. **No provider hop terminates on the api pod** — the api sits
   on the session-request and MCP-tool paths only.
2. **The halt mechanism, code-level confirmation.** The v0.38.0 roll
   replaced the fleet relay-router (RS `relay-router-6b9c9dbc5b` @
   05:47:35). `cmd/relay-router/main.go`'s SIGTERM path ran
   `server.Shutdown` on a **10-second** window; the chart carried no
   preStop, no TGS override, no PDB. In-flight SSE generations
   (minute-long by design) were hard-cut; opencode treats mid-stream
   death as a turn error; errored turns are terminal-by-design; no
   auto-resume exists → the workers halted. The BYO plane's #1078
   hardening is the existence proof that drain-parity was simply never
   applied to the fleet plane.
3. **Attribution corrections that reshaped leg B.** No agentd-side
   `/api/v1/events` subscriber exists in the tree (its clients are the
   frontend — which HAS full reconnect+Last-Event-ID machinery — and
   SDKs). The persistent streams that genuinely never re-establish
   after an api roll are the **api's own outbound usagestream gates**:
   pod-local, deliberately activity-gated (D1-B/#902: no deploy
   fan-in), with no re-arm path for sessions already in flight at roll
   time. The observed 8–12 replay-endpoint calls are the existing
   per-session/per-turn client reconnects (frontend + in-pod MCP
   `awaitTurnCompletion`) — that machinery already survives peer
   replacement (snapshot-first + seq-discard on each fresh connection).

## The wire probe (drives the classifier — both sides wire-proven)

Real opencode 1.18.10 via `StartIntegrationServer`, mock provider
hijack-closed mid-generation (no finish_reason, no [DONE]):

- **Infra cut**: NO `session.error` at all. The assistant message
  completes with `"finish":"unknown"` (`time.completed` anchored), then
  `session.status idle` + `session.idle`; the live registry clears.
  First turn (clean) on the same harness: `"finish":"stop"`.
- **User abort** (from #1722's probe, 1.18.10 + 1.18.15):
  `session.error {name:"MessageAbortedError"}` arrives BEFORE the idle
  burst.

**The asymmetry is the classifier: abort is an EVENT, infra loss is a
FINISH.** This is why "abort-classification" needed no error-signature
heuristics — the harness already tells the two apart on the wire.

## The fix

**Leg A1 — fleet-router drain parity (#1078 semantics):**
- `cmd/relay-router/main.go`: serve/drain lifecycle extracted into
  `serveFleetOn` (the `serveBYOOn` twin); SIGTERM drains in-flight
  streams for `DRAIN_GRACE` (default **10m** — the BYO sizing rationale
  is minute-long generations; a cap, not a delay) instead of the 10s
  hard cut.
- Chart (`relay-router-deployment.yaml` + values): preStop sleep 5
  (endpoint-removal head start), `terminationGracePeriodSeconds: 630`,
  `DRAIN_GRACE` env (default 10m, strictly < TGS), operator knobs
  `controller.inferenceRelay.router.{drainGrace,terminationGracePeriodSeconds}`.
- The amendment's "evaluate graceful drain (preStop) and likely dismiss
  it" resolved as **PROVE, not dismiss** — the platform already commits
  to exactly this drain budget on the BYO plane; the fleet plane just
  never got the treatment.

**Leg A2 — abort-classification + ONE bounded auto-resume (agentd):**
- Wire seam (`pkg/agent/opencode/wire`): `ParseSessionError` (name +
  message) and `ParseAssistantTurnEnd` (assistant terminal upsert →
  finish), with `SessionErrorNameUserAbort`/`TurnFinishUnknown`
  constants; probe-captured fixtures pin the contract.
- `cmd/workspace-agentd/turn_resume.go`: the supervisor classifies off
  the tracker's new `onTurnEvent` feed. Roll-classified loss (finish
  unknown, no session.error within the 5-min recency window, no user
  message after the interruption, 10-min cooldown, ≤3/rolling-hour)
  fires ONE re-prompt with a fixed audit text via
  `OpenCodeClient.ResumeTurn` (POST /session/:id/message —
  admit-and-schedule). User aborts NEVER resume. Ledger stamps only on
  successful prompts. Kill-switch `AGENTD_TURN_AUTO_RESUME=0`; metric
  `workspace_agentd_turn_resumes_total`. Wired in single-container
  (`main.go`) and sidecar (`sidecar_mode.go`) modes.
- Non-drain cuts (node loss, crash) are what this leg covers — no drain
  can save them; the amendment's acceptance allows exactly this
  "auto-resumes with roll-classified loss" arm.

**Leg B — gate re-arm after an api roll (the re-subscribe scope):**
- `api/internal/handlers/proxy_state_reconciler.go`: the reconciler's
  pass (now also run once IMMEDIATELY at startup, not only on the 60s
  tick) re-arms usagestream gates for Active workspaces with live
  active-session records in the ws_state store — Redis-backed in
  production, so the truth survives the roll that killed the gates.
  `usagestream.Consumer.HasGate` keeps it idempotent.
- D1-B preserved by construction: idle workspaces carry no
  active-session records (SREM deletes on empty; 30-min TTL bounds
  strays) → a rolled api still fans NOTHING in; stray records open a
  gate the fold's own 30s idle-drop closes. Store errors fail closed
  (read as no-live-sessions, retried next tick).
- With the gate re-opened, everything downstream returns on the
  existing machinery: the ABI stream reconnects (2s retry),
  snapshot/replay re-seeds the fold, the #1666 bridge seed-diff
  re-emits lost busy-truth, billing resumes. That IS the platform-side
  persistent-subscription re-establishment the issue asked for — the
  map showed the subscriber is the api, not agentd.

## Red-first record (verified RED on the skeleton commit `98727920`, GREEN on `6d1e8eb7`)

| Layer | File | Pins | Red |
|---|---|---|---|
| wire contract | `wire/turn_end_1732_test.go` | session.error parse (user-abort fixture); assistant terminal parse (cut=`unknown`, clean=`stop`, mid-flight rejected, user-role rejected) | contract pins (probe fixtures; the parsers are new pure functions — the behavioral red lives in the layers below) |
| router | `fleet_drain_1732_test.go` | default grace = 10m (#1078 parity); DRAIN_GRACE env; wiring (in-flight completes through SIGTERM-equivalent, listener closes); cap semantics | default-parity RED (skeleton kept 10s); chart pins RED (chart unchanged) |
| chart | `relay_router_drain_1732_chart_test.go` | preStop sleep 5; TGS 630 > DRAIN_GRACE 600; knob overrides | RED (preStop/TGS/env absent on main) |
| agentd unit | `turn_resume_1732_test.go` | fires-exactly-once (no double on replayed upserts); NEVER on user abort (before AND after the finish); abort recency blocks later cuts; clean turn no-resume; cooldown + rolling-hour cap + ledger aging; human-takeover cancel; nil-prompter disabled | 3 behavioral pins RED (resume inert) |
| agentd integration (real 1.18.10) | `turn_resume_1732_integration_test.go` | mid-generation provider hard-cut with production wiring → ONE auto-resume lands on the healed provider, session settles idle; REAL user abort on a live tool turn → nothing resumes, #1727 settle machinery untouched | resume pin RED (the halted worker), abort pin correctly green pre+post |
| api handlers | `state_reconciler_rearm_1732_test.go` | full-wiring api ROLL (old instance stopped+gates CloseAll'd, new instance boots against the shared store, NO activity) → gate re-established within one pass + HasGate returns; D1-B idle-workspace silence; armed-gate idempotence (no churn) | api-roll pin RED (rearm inert) |

## Verification (all run in this lane, this tree)

- `go test ./cmd/relay-router/ ./api/internal/services/usagestream/ ./pkg/agent/opencode/wire/ -count=1` — green.
- `go test ./cmd/workspace-agentd/ -count=1 -timeout 600s` — green (324s; includes the #1342/#1685/#1672/#1727 suites — no publication-semantics regression).
- `go test ./cmd/workspace-agentd/faultmatrix/... -count=1` — green.
- `go test ./cmd/workspace-agentd/sessionstate/... ./pkg/agent/opencode/ -count=1` — green.
- `go test ./api/internal/handlers/ -count=1` — green (82s; the #902 D1-B pins unaffected — the re-arm opens nothing for idle workspaces).
- Integration: `go test -tags integration -run TestIntegration1732 ./cmd/workspace-agentd/ -count=1` against a real 1.18.10 binary (`OPENCODE_BINARY=/usr/local/bin/opencode`; the harness default 1.18.10 matches — the #1722 probe verified both pins share the wire).
- Chart: `go test ./helm/ -run Test1732 -count=1` with helm v3.16.4 on PATH — green.
- `golangci-lint run --new-from-rev=origin/main` on every touched package — 0 issues; gofmt/goimports clean; pre-commit gates green on both commits.

## Regressions / interactions considered

- **#1727 (busy-clear publication)**: untouched — `applyLocked` seq
  fanout and the LiveBusy gates are not on any path I modified; the
  full agentd + sessionstate + usagestream suites ran green. The abort
  integration test doubles as a #1727 non-regression row (real abort →
  error settle → idle).
- **#902 D1-B (no fan-in on deploy)**: preserved by construction (the
  re-arm is live-session-gated, not blanket); the existing
  `Test902_E2E_ReconcilerDoesNotReArmIdleGates` and my new D1-B pin both
  hold.
- **#1666 (bridge reconcile)**: unchanged and now actually reachable
  after a roll (the gate re-opening is what feeds it).
- **Router drain vs pod budget**: drain is a cap — a stream that
  outlives it is cut when the bound expires (pinned); TGS 630 > 600
  keeps the SIGKILL from firing mid-drain.
- **Auto-resume semantics**: the re-prompt lands as a visible user-shape
  message with a fixed audit prefix (never silent); bounded 1/turn, 3/h,
  10-min cooldown; disabled per-pod via env. A persistently-broken
  provider path cannot hot-loop (pinned).
- **Frontend / SDK clients of /api/v1/events**: untouched — the map
  shows they already carry reconnect + replay; the fix is server-side
  truth re-establishment.

## Files

- `cmd/relay-router/main.go` — drainGrace config + serveFleetOn lifecycle
- `helm/templates/relay-router-deployment.yaml`, `helm/values.yaml` — preStop/TGS/DRAIN_GRACE + knobs
- `pkg/agent/opencode/wire/wire.go` — ParseSessionError / ParseAssistantTurnEnd
- `cmd/workspace-agentd/turn_resume.go` — the supervisor
- `cmd/workspace-agentd/{session_tracker,client,main,sidecar_mode,ops_metrics}.go` — feed, prompt, wiring, metric
- `api/internal/handlers/proxy_state_reconciler.go`, `api/internal/services/usagestream/consumer.go` — re-arm + HasGate
- 6 pin/test files + runbook row in `helm/MONITORING-OPERATIONAL.md`

## Review rounds

**r0 (commit `b0672af4`):** CI's full-suite race detector caught the
reconciler goroutine's new read of `h.agentdTerminus` racing the test
rigs' post-`Start()` `SetAgentdTerminus` writes (the field predates any
background reader; the re-arm pass added the first). Fixed by
converting the flag to `atomic.Bool` (mechanical Load/Store, 24 read
sites + the setter + one test write; semantics and test-visibility
unchanged). Verified: `go test ./api/internal/handlers/ -count=1 -race`
full suite green (144s) in this lane; the reviewer independently
reproduced the race red-on-parent and green-on-head.

**r1 (the review's one blocking finding):** the resume-action layer's
failure branch — the deliberate "ledger stamps ONLY on a successful
prompt" property that keeps a wedged harness retryable — shipped with
zero coverage (`fakeResumePrompter.err` was never set by any test).
Added `Test1732_TurnResume_PromptFailureKeepsSessionRetryable`: a
failing prompt must (a) stamp no ledger (no cooldown, no budget), (b)
not increment `workspace_agentd_turn_resumes_total`, and (c) leave the
session eligible so the NEXT interrupted turn re-fires without waiting
the cooldown — with the successful retry then stamping both the ledger
and the metric (also closing the reviewer's non-blocking metric-coverage
gap; `WORKSPACE_ID` is pinned via t.Setenv for a deterministic label).
Also addressed the flagged non-blocking comment items: the rearm doc
now states the bounded stray-record churn honestly (open → idle-drop →
re-open per 60s tick, ≤~30 short-lived connects until the 30-min TTL
expires), and the integration test's self-contradictory mid-edit
comment is rewritten to state the actual timing contract. Non-blocking
residuals acknowledged and left as recorded by the reviewer (the
unobserved-abort false-resume window is the classifier's one honest
blind spot — narrow, bounded, kill-switchable).
