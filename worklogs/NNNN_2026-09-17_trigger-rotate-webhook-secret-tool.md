# Worklog: trigger_rotate_webhook_secret — the automation surface's 12th tool

**Date:** 2026-09-17
**Session:** The v0.32.1 webhook-fix verification loop exposed a gap: the agent can create webhook triggers but can never obtain a signing secret (create doesn't return one; rotate existed only on the user API). Without it the workspace agent cannot demonstrate—or configure—an end-to-end webhook. Add the missing delegation.
**Status:** Complete

---

## Objective

Expose webhook-secret rotation through the pod-identity automation surface so the owner's agent can complete webhook setup end-to-end: create trigger → rotate (get secret + URL) → hand the secret to the external sender → signed delivery → fire.

## Work Completed
- API: `POST /internal/v1/automation/triggers/:id/rotate-secret` — resolve + delegate to the EXISTING `UserRotateWebhookSecret` (same pattern as every automation route; contract allowlist + fixture).
- Seam: `TriggerRotateWebhookSecret` (POST, query workspaceID, ID validated pre-dial).
- Tool: `trigger_rotate_webhook_secret` {id} — response {webhookSecret, webhookUrl} surfaces VERBATIM. The credential disclosure is deliberate and scoped: the owner's own secret, delivered to the owner's own agent over the pod-identity channel (identical to what the owner sees via the user API); description instructs handing it to the EXTERNAL sender and the X-Hub-Signature-256 signing shape.

## Key Decisions
- Rotate (not create-returns-secret): matches the user API's existing security posture; every credential issuance is an explicit action — and now an AUDITED one (review r2: rotateWebhookSecret had no audit event; added trigger.rotate_webhook_secret mirroring logCreate, actor = the resolved owner, never the secret itself).
- No new domain logic anywhere — delegation only.

## Blockers
None.

## Tests Run
- Seam wire pin (method/path/query/auth + verbatim credential passthrough).
- API delegated rotate through the REAL handler: 200, non-empty secret, trigger-id URL advertised, store carries enc:<new secret>.
- agentd tool passthrough + inventory (25 tools) + auth gate.
- handlers/server/opencode suites green.

## Next Steps
- Ships in 0.32.1 with the receiver fix (#1404); live e2e: rotate → sign → POST → 202 → fire → workflow run.

## Files Modified
- api/internal/handlers/pod_automation.go (+_test.go), api/internal/server/router.go (+contract test)
- pkg/agent/opencode/loopback.go (+_test.go)
- cmd/workspace-agentd/mcp_tools.go, mcp_server.go (+tests)
