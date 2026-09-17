# Worklog: webhook triggers 404 at their advertised URL — receiver resolves the wrong ID

**Date:** 2026-09-17
**Session:** Live verification of the v0.32.0 automation tools surfaced a hard-broken webhook path: POST to a freshly created trigger's webhookUrl returns 404. Root-caused in source, fixed receiver-side, verified by tests that now encode the advertised contract.
**Status:** Complete

---

## Objective

Make webhook triggers reachable at the URL the platform hands out. The receiver resolved the path param as the `webhooks.id` UUID, but every URL ever advertised (create `triggers.go:318`, rotate `:590`) carries the TRIGGER UUID — and the webhook row is created with an independent `uuid.New()` (`triggers.go:298`). Two UUIDs that can never be equal → every webhook trigger ever created 404s at its advertised URL, before HMAC is even reached.

## Work Completed

- `webhook_receiver.go`: interface method swapped `GetWebhook(webhookID)` → `GetWebhookByTriggerID(triggerID)` (already implemented in the store, `store.go:494`); `HandleWebhook` resolves the row by the path param AS the trigger id. Dedup (`RecordWebhookDelivery`) now keys on the resolved `hook.ID` — the real webhook row — so dedup is stable regardless of URL spelling.
- `webhook_receiver_test.go`: mock keyed by `TriggerID`; every request now hits `/api/v1/hooks/<triggerID>` with hook-row IDs deliberately distinct from trigger IDs (the bug shape). Previously the tests seeded and called the webhook ROW id — encoding the broken contract; they could never catch this.
- Receiver-side (not create-side) is the correct fix: it repairs every existing webhook with zero migration. Stamping `WebhookRow.ID = triggerID` at create would fix only new rows.

## Key Decisions
- Resolve by trigger_id, keep the webhook row's own UUID as the internal/dedup identity.
- No schema/data migration required.

## Blockers
None.

## Tests Run (after review round 1)
- `TestWebhookE2E_AdvertisedURLDelivers` — the composed seam: ONE store shared by the real TriggersHandler (create → advertised webhookUrl verbatim; rotate → the signing secret) and the real WebhookReceiverHandler (signed POST to that URL → 202 + fire row + queued run). Unhappy legs: bad signature → 401 + no fire; unknown id → 404; duplicate delivery → 200 "duplicate" + no second fire. Red against the pre-fix receiver by construction (the URL is the trigger id; the old lookup keyed the row id).
- All 14 webhook receiver/trigger tests green; full handlers + pkg/workflows suites green.
- Review hygiene: dead `Store.GetWebhook` removed (zero prod callers); stale contract comments corrected (store.go header, integration test); unused mock method dropped.

## Next Steps
- PR + review; ships with next release (needs API redeploy — receiver runs in the API, not agentd).
- Live re-test: rotate secret on the kept `agentd-e2e-webhook` trigger, sign, POST — expect 202 + fire row.
- Separate observation (not fixed here): `UserListFires` doesn't surface the stored error/result blob — the old Test1we 600s-timeout failures are diagnosable only via duration fingerprint.

## Files Modified
- api/internal/handlers/webhook_receiver.go
- api/internal/handlers/webhook_receiver_test.go
