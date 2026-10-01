# Worklog: Credential-reload restart gate + cross-uid restart-reason marker (the 2026-10-01 restart-storm fixes)

**Date:** 2026-10-01
**Session:** Fix the two agentd defects behind the dda717cb restart storm: ungated control-path credential restarts, and the restart-reason marker's cross-uid write hole. Scope deliberately excludes the wedge detector (design discussion landed on storm prevention only; the wedge stays latent until any future storm, at which point a manual pod bounce remains the remedy).
**Status:** Complete

---

## Objective

After the v0.34.13 relay-staging fix went live, workspace dda717cb received its first credential handoff — and agentd restarted opencode **6 times in 11 seconds** (18:20:47–58: five `control: restart requested | credential_reload` from the control-socket path plus one xdg-watcher restart). The storm killed an in-flight turn and left the surviving opencode wedged (every subsequent turn aborted at birth, fresh sessions included) until a manual pod bounce. Root-cause the restart mechanics and fix them at the right level; leave the (functional-death) wedge detector as an explicitly out-of-scoped follow-up.

---

## Work Completed

### Finding: the watcher was already gated — the control path was not

The xdg agent-config watcher (xdg_config_layer.go:398) already has BOTH defenses: a content fingerprint (`cur == last: continue`) AND a 60s cooldown. The storm did not come from it (it fired once, correctly). The storm came from `managedProcAdapter.Restart` (supervise_opencode.go): the sidecar's credential events fan out one control-socket `Restart("credential_reload")` per push, and that path had **no gate of any kind** — every push, however redundant, restarted the child. The five pushes in the storm window all carried the same revision (the handoff content didn't change again until 18:23); four of the six restarts were pure waste.

### Fix 1: the credential_reload rev gate (`supervise_opencode.go`)

`Restart` now consults `suppressRedundantCredentialRestart()` before touching the child — but only for `credential_reload`. The gate **peeks** the spawn-env mux read-only via a new narrow `credentialPuller` interface (production wires `*spawnEnvPuller`; `pullBounded` is a pure read — the commit to `currentDelta`/anchor happens only in `preSpawn` at spawn time, so peeking cannot corrupt the anchor invariant) and suppresses the restart iff the exact triple holds: clean pull ∧ not degraded ∧ `anchoredPrefix(servedRev) == servedEnvRevAnchor` — i.e. the mux is serving precisely what the current child already spawned with. Every doubtful case (pull error, degrade latch, unanchorable rev, anchor mismatch, nil puller) fails OPEN and restarts exactly as before. Suppressed requests are counted on a new `workspace_restarts_suppressed_total{workspace_id,reason}` counter. OBSERVABILITY PLACEMENT (review r1 finding 1 — the first cut of this PR incremented a counter no scraper could reach): the supervisor process serves no HTTP at all, and the PodMonitor scrapes the SIDECAR's admin /metrics — so the sidecar's `socketReloadProc.restart()` now records the outcome-truthful pair itself from the socket response (`restarted` → `workspace_restarts_total`; `{restarted:false, in_progress:false}` → `workspace_restarts_suppressed_total`), and the shared reload handler SKIPS its request-time `RecordRestart` for the socket topology (secrets.go type-switch) so the scraped series can no longer over-count suppressed pushes. Non-zero suppressed under controller push churn is the gate working, not lost restarts.

Why rev-comparison rather than content-hashing: the push protocol already carries revisions, the adapter already anchors the spawned revision (`servedEnvRevAnchor`, US-70.2), and the anchors' meaning ("exactly what this child booted with") is precisely the predicate a redundant-restart gate needs. Content hashing would re-derive a worse version of the same signal.

Why the gate lives in the adapter, not the sidecar consumer: the component that owns the process owns the restart policy. This one site covers both the socket path and any future credential_reload caller, in both topologies that route through the supervisor adapter, with no protocol change (the socket's Restart payload carries no rev).

### Fix 2: the restart-reason marker's cross-uid rotation hole (`restart_reason.go`)

The shared marker path (`/sandbox-runtime/last-restart-reason.json`) sits in a **sticky**, root-owned dir, and the file is 0640 — group-READ for the shared group, never write. Sidecar agentd (uid 2000) and supervisor agentd (uid 1000) are both writers; whichever uid wrote the marker last owns it until the pod dies, and every write from the other uid fails EACCES (the sticky bit also blocks the unlink/rename that would otherwise replace it). Observed live as `agent-config watcher: marker write failed ... permission denied` (the code's own comment documents the two-writer intent; the write path just couldn't honor it).

- `writeRestartReasonMarker` now falls back to a per-uid sibling (`<path>.uid<N>`, same 0640) when the primary write fails; attribution survives while each writer stays inside its own file.
- `readRestartReasonMarker` resolves the **newest** marker across the primary + fallback names (the other uid's fallback can be newer than a stale primary; freshest = truthful).
- `logRestartReason` (the boot-time one-shot) consumes the newest and sweeps stale siblings so they cannot re-surface as stale attributions on later boots.

This is a diagnostics fix, honestly labeled: the marker write failure did NOT cause the storm (nothing reads the marker for control decisions — the watcher's cooldown is in-memory). It makes restart attribution truthful in sidecar mode, which during an incident like this one is the difference between seeing five credential restarts and seeing one.

---

## Key Decisions

1. **Storm prevention only — no wedge detector.** The design review explicitly scoped down: the functional-death wedge (opencode vitally alive, turns aborting at birth) deserves its own detector in the existing health-watchdog framework, but it is a separate change with its own risk profile. With the storm gone, the wedge's only known trigger is gone with it; a future storm requires a manual pod bounce (documented remedy).
2. **Gate fails OPEN.** A wrongly-suppressed restart would silently starve credentials; a wrongly-allowed restart is merely yesterday's behavior. The predicate therefore requires certainty and the counter makes suppressions observable.
3. **Rev comparison over content hashing** (rationale above) and **adapter-level gating over consumer-level** (rationale above).
4. **Per-uid fallback files over loosening permissions.** Changing the dir mode or the marker to 0660 broadens an attack surface for a diagnostics artifact; unique files keep each writer inside its own uid while the newest-wins reader preserves the single-reason contract. Sticky-dir semantics make rename-replace impossible cross-uid anyway.

### Assumptions (Rule 7)

- The spawn-env mux is updated by the sidecar BEFORE its consumer issues the control-socket Restart (same process, push handler ordering), so a peek can never "see the future" — if it ever raced, the peek would return the OLD rev == anchor and suppress a restart whose new rev lands unapplied. Ordering is asserted by the existing spawn_env push flow; a future protocol change should re-examine this.
- `anchoredPrefix` semantics (3-part `seq:manifestHash:contentHash`) are load-bearing for the comparison; a bare content hash yields "" and fails the gate open by construction.

---

## Blockers

None.

### Review rounds (PR #1613, automated reviewer)

- **r1 (REQUEST CHANGES):** (1) the new counter fired only in the supervisor process, which serves no HTTP — the PodMonitor scrapes the sidecar, so the observability deliverable was inert AND the sidecar's request-time `RecordRestart` (secrets.go) over-counted suppressed pushes; fixed by outcome-truthful sidecar recording (`recordSocketReloadOutcome`) + the socket-topology skip in the shared handler. (2) The `credentialPuller` insertion had split `refreshFiles`'s doc comment; moved below the function. (3) Noted the ~2.5s bounded peek inside the socket's restartMu window (comment added). (4) Missing coverage added: counter assertions via testutil.ToFloat64, the storm scenario end-to-end at the socket seam (`TestSocketRestart_SuppressionRoundTrip`), and the outcome-recorder table (`TestRecordSocketReloadOutcome`). Reviewer also validated the gate mechanics, fail-open matrix, and TOCTOU-safety of the peek; flagged (pre-existing, not blocking) that the boot-time `logRestartReason` consume/sweep is reachable only in single-container mode, so the newest-wins boot read never runs in sidecar topology — the write-time/real-time attribution path is the one this fix repairs there.

- **r3 (REQUEST CHANGES, against the r2 fix commit):** the r2 Go-side fixes were verified sound (deadline arithmetic re-checked for both production callers, slow-restart mutation red/green reproduced), but the round's F7 e2e leg was DEAD ON ARRIVAL — it asserted scraped SIDECAR counters for traffic driven through the SUPERVISOR's socket, and the supervisor's registry has no HTTP surface (pkgOpsMetrics is per-process; the only sidecar-side recording comes from sidecar-initiated restarts). Both metric assertions were unsatisfiable in the topology the pool deploys; the `manual` leg recorded nothing anywhere. Third consecutive round of an observability claim presented as tested without validating the process topology — the worklog now carries the rule: **any assertion about pkgOpsMetrics must name the process that increments it.** Fixes: F7 rewritten to assert only what each process can observe — (a) suppression via the socket response shape + `status` child_pid unchanged (supervisor-verifiable), (b) a sidecar-INITIATED restart (new env value → controller push → socketReloadProc) counted on the scraped surface + converged in the child. Also: the r2 wiring pin was predicate-only (mutation: removing the secrets.go guard left zero test failures) — `TestApplyBatch_RequestTimeMetricOnlyForNonSocketProc` now drives the real applySecretsBatch handler with each proc kind and pins the request-time count to the non-socket topology (mutation-verified red/green); two stale comments fixed (RecordRestart doc now enumerates credential_reload; the exec-test comment no longer claims the 2s timeout governs Restart).
- **r2 (REQUEST CHANGES, against the r1 fix commit):** (1) the recording path under-counted — `newControlClient`'s 2s default is a CONN deadline and the supervisor answers `restart` synchronously after the full SIGTERM→grace window, so any real restart slower than 2s returned a deadline error and was counted NOWHERE; fixed with `restartCallBudget` (grace + slack, clamped) arming the round-trip deadline in `controlClient.Restart`, plus `TestSocketReload_SlowRestartStillCounted` (SIGTERM-ignoring child, grace 4 → 5.06s round trip, counter still increments). (2) False "logged at the call site" comment + swallowed transport error — the caller now Warns explicitly ("restart outcome unobservable"). (3) Stale help text — `workspace_restarts_total` help now enumerates `credential_reload`; NOTE the label-semantics change on the socket topology: these events were previously request-time-counted as `env_secrets`/`api_key` and are now outcome-counted as `credential_reload` (no chart consumer filters by reason today — verified against podmonitor-agentd.yaml + prometheus-rules). (4) Wiring pins: `TestShouldPreRecordRestartMetric` (deleting the secrets.go skip double-counts) and the slow-restart test (deleting the recorder call fails it). (5) e2e gap: F7 added to the us-70 kind harness — same-rev socket restart suppressed + `workspace_restarts_suppressed_total` visible on the scraped sidecar /metrics with `workspace_restarts_total` flat; ungated `manual` restart fires and counts (the storm class closed at pod level).

---

## Tests Run

- `go test ./cmd/workspace-agentd/ -run TestCredentialReloadGate -count=1` — PASS (suppress-same-rev, five fail-open cases, other-reasons-ungated, nil-puller).
- Mutation red/green: disabling the gate call makes `TestCredentialReloadGate_SuppressesSameRev` FAIL; re-enabled, PASS.
- `go test ./cmd/workspace-agentd/ -run 'TestWriteRestartReasonMarker_ForeignOwned|TestReadRestartReasonMarker_NewestWins|TestLogRestartReason_ConsumesNewest' -count=1` — PASS (0444-primary fallback, newest-wins both directions, one-shot sweep).
- `go test ./cmd/workspace-agentd/ -count=1 -short` — one failure: `TestRestart1342_ProgressStops_ForcePathFires`, a load-sensitive timing flake in the session-aware restart suite (zero references to credential_reload or the adapter; passes in isolation at 0.37s). Pre-existing class, same family as the usagestream race flake documented on PR #1611.
- `go vet ./cmd/workspace-agentd/`, `gofmt -l` — clean.

---

## Next Steps

1. Merge + release (v0.34.14 candidate) + prod bump; then watch `workspace_restarts_suppressed_total` on the agentd PodMonitor — expect non-zero on controller push churn.
2. Wedge detector as its own lane if storms recur from a new trigger: consecutive zero-token instant-aborts observed at the delivery seam, firing through the existing health-watchdog framework (rate-limited, session-aware).
3. API-side push-storm source (why five pushes in 11s for one revision) — harmless under the gate, but the churn deserves its own investigation.

---

## Files Modified

- `cmd/workspace-agentd/supervise_opencode.go` — `credentialPuller` seam, the rev gate in `Restart`, `suppressRedundantCredentialRestart`.
- `cmd/workspace-agentd/ops_metrics.go` — `workspace_restarts_suppressed_total` + `RecordRestartSuppressed`.
- `cmd/workspace-agentd/restart_reason.go` — per-uid write fallback, newest-wins read, boot-time sibling sweep.
- `cmd/workspace-agentd/cred_restart_gate_test.go` — gate unit tests (mutation-verified).
- `cmd/workspace-agentd/restart_marker_crossuid_test.go` — marker cross-uid tests.
- `cmd/workspace-agentd/spawn_env_pull_adapter_test.go` — mechanical: hold the concrete puller in a local before assigning the now-interface field.
- `COORDINATE.md` — claim row.
- This worklog.
