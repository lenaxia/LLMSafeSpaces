# Runbook: agentd session-state authority flip (design 0055, Epic 69 close-out)

**Owner:** platform operator
**Mechanical half:** `local/authority-flip.sh` (preflight / park / unpark / flip / rollback — same procedure, committed)

**Preconditions (all mandatory, in order):**

1. **Single-regime fleet (D4).** Design 0053 S3's mandatory digest pins (`controller.agentdDelivery.image` + `controller.opencodeDelivery.image`) are deployed — the unpinned/BYO row is a boot-time guard, not a supported mode. A dual delivery regime is not maintained: every pod is either pre-flip (0052 world) or post-flip (ledger terminus), fleet-wide.
2. **Release carrying Epic 69** (US-69.1–.13): the sessionstate authority, the ABI surface, the outbox terminus switch, the admin park/unpark/in-flight endpoints, and the statusz `ledger_in_flight` field.
3. **Flag matrix respected (M4, `ValidateDeliveryFlags`).** `AGENTD_STATE_AUTHORITY` **requires** `OPENCODE_V2_DELIVERY=1`; authority-on with V2-off is an illegal combination and the API **exits non-zero at boot** — a mis-set values file fails the rollout loudly, it never comes up half-armed. Both flags live in `api.extraEnv` (see `helm/values.yaml`).

**Flip (ordered):**

1. **Check in-flight** — per canary workspace, then per wave:
   `GET /api/v1/admin/authority/inflight/{workspaceId}` (or `local/authority-flip.sh preflight <ws>`).
   `inFlight` is the pod's unresolved ledger count (ledgered + admitted + stalled), read from statusz — same domain, no cross-store verify.
2. **Park if needed** — a non-zero count that will not drain before the flip:
   `POST /api/v1/admin/authority/park {"workspaceId","reason"}` (or `... park <ws> <reason>`). Entries move to `parked` carrying the `mode_transition: <reason>` marker — held visible, no auto-retry, never auto-re-sent across a mode change.
3. **Flip via values — never `kubectl set env`.** Set `api.extraEnv` in the release values (`AGENTD_STATE_AUTHORITY=1`; `OPENCODE_V2_DELIVERY=1` stays on) and `helm upgrade`. An imperative env edit drifts from the release and the next upgrade silently reverts it; the values path is the only one ValidateDeliveryFlags sees on every rollout. `local/authority-flip.sh flip on <ws>` prints the values block and (with `EXECUTE=1`) runs the upgrade, preflight first.
4. **Verify:**
   - `GET /api/v1/workspaces/{id}/contract-events` answers **200, not the typed 501** (501 = flag off — the surface is capability-gated per pod);
   - a canary send: one prompt on a canary workspace promotes through the contract stream (admitted → promoted), ledger queue depth visible in the snapshot;
   - the gates gauge `llmsafespaces_agentd_gate_duration_seconds` is scraped and non-zero on the flipped pods (agentd :4098 PodMonitor).

**Rollback:** flag off (`AGENTD_STATE_AUTHORITY` unset/`0`, via values) → **unpark** (`POST .../unpark` — re-arms exactly the `mode_transition` parks to pending) → the ledger **back-drains via the 0052 path**: the outbox delivers the re-armed entries through the V2 store, and verification falls back to the text-scan oracle **retained behind the adapter seam as the documented rollback fallback** (deleted outright only when the admission-ID spike's pool runs close it — see design 0055 §Open items). **User-visible loss: none** (R8) — accepted prompts were parked with reason, not dropped; the ledger remains readable throughout. `local/authority-flip.sh rollback <ws>` runs the sequence.

**Triage (US-69.12 alerts — see [monitoring](../operator/monitoring.md#session-state-alert-triage)):**

| Alert | Meaning during/after a flip | Action |
|---|---|---|
| `LLMSafeSpacesSeqStalled` (critical) | seq stalled >5m with ledgered work pending — starvation or a wedged harness, not the flip itself | Check CFS throttling / watchdog suppression; exec `curl :4097` GetSnapshot on the pod |
| `LLMSafespacesStalledEntries` (warning) | an admitted entry crossed the promotion deadline and the wake did not resolve it | `GetDeliveryStatus(entryId, attempt)`; if the transcript never landed, re-drive — or park + this runbook's rollback |
| `LLMSafespacesWakeFailures` (warning) | the stall-wake reseed is failing (opencode unreachable / store read errors) — stalled entries will not self-resolve | Fix the harness path first (agentd logs `stall wake failed`) |
