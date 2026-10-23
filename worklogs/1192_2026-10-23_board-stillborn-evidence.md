# Worklog: Task Board stillborn turn-evidence guard (#1832 WS4)

## Scope
The NARROW evidence fix for stillborn-spawn claim hygiene (the
platform-derived liveness REWORK is #1847's lane — untouched): a spawn
that mints a session but produces ZERO turns consumed a claim, and a
lease-keeper renewed the corpse because heartbeat extension trusted
the evidence TIMESTAMP alone (spawn-moment updated_at qualified).

- pkg/board stillborn.go: TurnEvidenceSource seam (a session's
  message/output count) + WithTurnEvidenceSource. stage.go Heartbeat:
  when the seam is installed AND the row carries an assignee, a
  POSITIVE zero-turn report REFUSES the extension (HeartbeatResult.
  Reason names the class; the wall clock reclaims the stillborn's
  lease through the ordinary RC1 path). A seam ERROR fails OPEN
  (logged) — transient infra must not kill live workers' renewals;
  #1847 owns the stronger posture. Nil seam: today's semantics
  (regression-pinned). No wire-form change (worker compat untouched;
  the heartbeat RESPONSE gains an optional `reason` field only).
- api/internal/app board_turn_evidence.go: the production source over
  session_index.message_count (agentd-maintained counter; a missing
  row is positive-zero — never indexed = never turned), installed in
  app.go's spawn options.
- The seam response: BoardHeartbeat renders the reason.

## Tests
- pkg/board stillborn_evidence_integration_test.go (red-first over
  the stored-but-unconsulted skeleton): zero-turn + fresh timestamps
  (exactly what a lease-keeper sends) → NO extension, watermark and
  lease untouched; real turns → extends; seam error → fails open;
  nil seam → today's semantics.

## Gates
go build/vet clean; pkg/board + handlers suites green; no schema
change; no migration; worker seam wire sets untouched.
