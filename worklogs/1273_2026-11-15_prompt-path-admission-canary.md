# Worklog: Prompt-path admission canary for wedged sessions

**Date:** 2026-11-15
**Session:** issue #2045 — agentd prompt-path admission canary (wedge-suspect detection → session-aware supervised restart)
**Status:** Complete

---

## Objective

opencode answers plain HTTP quickly (fast 401s) while its per-session prompt pipeline is dead — the supervisor's health model (TCP dial + `/global/health`) cannot see the wedge, so a session-level prompt-path failure persisted for hours on Nov 13-15 and needed a manual restart. Add admission-path health: repeated `POST /session/:id/message` admission failures escalate to the EXISTING session-aware supervised restart machinery, and the turn-resume supervisor's silent give-up becomes a visible wedge-suspect signal.

**Provenance note:** the original `/implement` run (Actions run 43674367228) completed this work on the ephemeral runner but the branch never landed on origin — the commit was unrecoverable. This session re-implemented the validated design in-workspace; the design, the adversarial-review findings (timestamped re-arm vs boolean latch, construction-time poll-interval capture, fixture/session-id fidelity), and the docs text below follow that run's recorded evidence, re-derived and re-verified against the current tree.

---

## Assumptions (stated, then validated — Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | `makeSessionAwareRestartDecision` is generic over prompt-path restarts (not config-reload-only): it takes any `restartableProcess` + `deferBusySource`. | Read `cmd/workspace-agentd/secrets.go:286-421`; exercised through the new seam in `TestWedgeRestartFunc_BusySessions_DeferThroughExistingSeam` (real seam, mock proc). R5's "verify" satisfied. |
| A2 | The wedge's wire signature is "awaiting headers" timeout: admission failure = `c.client.Do` error with `url.Error.Timeout()` / `context.DeadlineExceeded`; a slow BODY is a successful admission. | Pinned both directions with fake opencode: `TestResumeTurn_SlowHeadersTimeout_ClassifiedAsAdmissionTimeout` (hang → classified), `TestResumeTurn_SlowBody_IsSuccessfulAdmission_NeverClassified` (headers land + flush → drain-only, nil). Fixture note: the slow-body harness must Flush after the first write — Go's server buffers the response in bufio, so without it nothing reaches the client until the handler returns, which is the slow-headers shape in disguise (found by the e2e pin going red on restart count). |
| A3 | The production `OpenCodeClient` http.Client timeout (5s, main.go) always fires before the 30s turn-resume ctx; both are the timeout class. | Read `main.go` and `turn_resume.go` resume(); both shapes pinned (`TestClassifyAdmission_Table`). |
| A4 | The e2e send paths in agentd are exactly the two named in the issue: `ResumeTurn` (turn-resume supervisor) and the send_message detached delivery (`mcp_tools.go:2028` vicinity). | Read both call sites; wired exactly those two. The session-notifier deliverer and the ABI admitter are different subsystems and were NOT wired (faithful to the ask's two-path scope). |
| A5 | The supervisor accepts new reason strings via the closed enum without special handling. | Read `supervise_opencode.go` `managedProcAdapter.Restart` — any enum reason is logged and honored (special-casing exists only for `credential_reload`'s rev gate). Pinned in `TestRestartReasonPromptPathWedge_EnumTriplePin` and `TestWedgeRestartProc_ControlSocket_CarriesWedgeReason`. |
| A6 | Sidecar and supervisor roll together (same digest-pinned artifact), so the new enum member carries no durable skew. | README-LLM: the controller pins the same artifact into both containers. Residual risk bounded to the rollout window; noted in the PR. |
| A7 | `restartDeferralsFired` counts deferrals regardless of restart class — the busy-deferral suppression metric the acceptance pin asks for. | Read `secrets.go:363` (incremented inside `makeSessionAwareRestartDecision` on every defer); asserted in the deferral pin. |

---

## Work Completed

### TDD red-first (fake opencode harness)

- `cmd/workspace-agentd/admission_watch_test.go` (new): classification pins (slow-headers classified / fast-refusal not / success not / slow-body not / canceled-ctx not + `classifyAdmission` table + already-classified pass-through), canary pins (3-strike crossing, sub-threshold silence, window aging, session isolation, unclassified never count, per-window suspect dedupe, nil-fire observability, concurrency), the born-red e2e pins through the REAL wiring (slow-headers → restart with session id in the reason + marker; slow-body never counts; provider refusals never restart), the busy-deferral pin through the real `makeSessionAwareRestartDecision` seam, attribution pins (marker + metric + restart-log reason carrying session id(s), enum triple-pin), the sidecar `wedgeRestartProc` pin against the real control-socket fixture, and the R4 turn-resume pins (metric-asserted per the acceptance criteria, plus the ERROR-log field pin).
- Red confirmed: `go vet` failed with `undefined: admissionWatch` before implementation.

### Implementation

- `admission_watch.go` (new): `errAdmissionTimeout` + `classifyAdmission` + `isAdmissionTimeout` (deadline / `url.Error.Timeout()`; `context.Canceled` explicitly NOT a deadline; already-classified errors pass through); `admissionWatch` — per-session strike ring (10-min window, 3 strikes), suspect declaration deduped per session per window, storm guard anchored on the last FIRE (30 min), timestamped per-window request re-arm (never a boolean latch), async fire (send paths never block on a supervised restart), `pkgAdmissionWatch` singleton + install seams; `wedgeRestartFunc` (marker + `RecordRestart` + restart-log reason `prompt_path_wedge sessions=<ids>` + `makeSessionAwareRestartDecision`, construction-time capture of the poll interval); `wedgeRestartProc` (sidecar control-socket restarter carrying the wedge reason, outcome-truthful metric pair, marker + restart log at the decision site).
- `client.go`: `ResumeTurn` classifies `Do` errors via `classifyAdmission`.
- `turn_resume.go`: watch injected (`pkgAdmissionWatch` default, `newTurnResumeSupervisorWithWatch` test seam); failed resumes feed the canary and an R4 ledger (timeout-class only, `ResumeCooldown` window) — the second failure within the window escalates to ERROR + `wedge_suspect=true` + suspects counter; first failure stays WARN; non-timeout failures never declare a suspect (R5).
- `mcp_tools.go`: send_message background-delivery failure feeds `pkgAdmissionWatch.record(classifyAdmission(...))`.
- `ops_metrics.go`: `agentd_message_admission_failures_total` + `agentd_session_wedge_suspects_total` (workspace_id label; names exactly per R3), Record methods, Help text for `workspace_restarts_total`/`workspace_restarts_suppressed_total` extended.
- `restart_reason.go`: `RestartReasonPromptPathWedge = "prompt_path_wedge"`.
- `control_socket.go`: the wedge reason joins the closed enum (A.2/A.4 invariant preserved — new member, not an opening).
- Wiring: `main.go` (single-container: managed process) and `sidecar_mode.go` (sidecar: `wedgeRestartProc` over the control socket) install the fire trigger after deps construction.
- `secrets.go`: `restartIdleCheckInterval` const → var (test seam; production value unchanged) — the established healthz_cache.go pattern.
- Docs: README-LLM Architecture Overview gains the prompt-path wedge runbook paragraph; CHANGELOG (Unreleased) entry; this worklog.

### Key design decisions

1. **Reuse, not reinvention (the issue's pin):** the wedge restart goes through `makeSessionAwareRestartDecision`, so busy deferral, the progress-keyed stall force path, interrupt-first grace, and the deferral metric apply unchanged. Only the marker/metric/restart-log attribution is wedge-specific at the decision site (the `relayKillWithMarker` pattern — every restart class writes its own marker).
2. **Storm guard anchored on the last FIRE, not the last attempt** (pinned by `TestAdmissionWatch_StormGuardAnchoredOnFire_PersistentWedgeKeepsRetrying` and the continuous-stream pin: 2h of 1-min-cadence strikes → 4 fires, 8 suppressions): anchoring on attempts would let a continuously-suspect wedge suppress itself forever — the exact opposite of the incident's recovery goal.
3. **Two detectors, one counter, window-deduped:** the strike ring (3 in 10 min → restart request) and the turn-resume double failure (2 in cooldown → ERROR + counter) both declare through the same dedupe; the suspects counter ticks once per session per window regardless of who declared first. The restart request comes only from the ring's threshold crossing (R4 asks for visibility, not restart; the incident's recovery fires at the 3rd strike).
4. **Post-threshold strikes within the same suspect window are ring entries, not new requests** (pinned): the ongoing wedge stays visible through `agentd_message_admission_failures_total` (per failure), not through inflated suspect/suppression counts.
5. **Async fire:** `record()` runs on the SSE tracker read loop (turn-resume path) and detached delivery goroutines (send path) — neither may block seconds on a supervised restart; the trigger goroutine is untracked like the health watchdog's `go restarter.restart()`, and the deferred poll goroutine inside the decision remains bgWg-tracked as before.

---

## Blockers

None.

---

## Tests Run

- `go build ./cmd/workspace-agentd/` — OK.
- `go vet ./cmd/workspace-agentd/` — OK (after the born-red phase).
- `go test -count=1 -run 'TestAdmissionWatch|TestResumeTurn_|TestAdmissionCanary|TestWedgeRestart|TestClassifyAdmission|TestRestartReasonPromptPathWedge|TestPkgAdmissionWatch|TestInstallAdmissionWatchRestart|Test1732_TurnResume_Repeated|Test1732_TurnResume_First|Test1732_TurnResume_NonTimeout' ./cmd/workspace-agentd/` — all new pins green (post red).
- `go test -count=1 -race ./cmd/workspace-agentd/` — full package suite green (includes all pre-existing #1732/#1342/#1760/US-44 pins).
- Red evidence: pre-implementation `go vet` failure (`undefined: admissionWatch`).
- Fixture fidelity catches (Rule 11 phase 1): (a) the slow-body harness originally never flushed — Go's response bufio held the headers until handler return, making it the slow-headers shape in disguise (the e2e slow-body pin went red on restart count; fixed with `http.NewResponseController(w).Flush()`); (b) `classifyAdmission` originally did not pass through already-classified `*errAdmissionTimeout` errors, so the R4 pin's fake prompter produced unclassified failures and no escalation — fixed with an `errors.As` pass-through at the top (also the correct production semantic for re-fed classifications).

---

## Next Steps

- Live-fire note (post-deploy, from the issue's acceptance list): if the sushi-class wedge recurs, verify it self-recovers within canary window + restart (~1 min) without board involvement; watch `agentd_session_wedge_suspects_total` and the survives-restart operator signal documented in README-LLM.
- If wedge restarts recur >1 per 30 minutes for the same session repeatedly, consider surfacing the surviving-session state (abort/delete tooling) — filed as follow-up territory, not in this change's scope.

---

## Files Modified

- `cmd/workspace-agentd/admission_watch.go` (new)
- `cmd/workspace-agentd/admission_watch_test.go` (new)
- `cmd/workspace-agentd/client.go`
- `cmd/workspace-agentd/turn_resume.go`
- `cmd/workspace-agentd/mcp_tools.go`
- `cmd/workspace-agentd/ops_metrics.go`
- `cmd/workspace-agentd/restart_reason.go`
- `cmd/workspace-agentd/control_socket.go`
- `cmd/workspace-agentd/secrets.go`
- `cmd/workspace-agentd/main.go`
- `cmd/workspace-agentd/sidecar_mode.go`
- `README-LLM.md`
- `CHANGELOG.md`
