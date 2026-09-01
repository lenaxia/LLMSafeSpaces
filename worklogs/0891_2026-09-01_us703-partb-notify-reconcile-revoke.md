# Worklog — US-70.3 Part B: notify-pull flip, reconcile loop, revocation fan-out (API side)

**Date:** 2026-09-01
**Branch:** `feat/us-70-3-notify-reconcile`
**Story:** #1207 (Part B; Part A — agentd resync endpoint — landed in parallel on the same branch)

## What

1. **agentpush.Notify** (`api/internal/services/agentpush/`): live change
   delivery flipped from batch-body push to empty authenticated POST
   `/v1/resync-secrets` on the pod user mux (`agentd.AgentdPort`). 429 is
   success-shaped (pod rate-limits, will resync). `Push`, `BatchBuilder`
   and `Result` deleted — zero in-tree callers remained after the flip
   (grep evidence in the session report; pod-side `/v1/reload-secrets`
   remains for mixed fleet, Part A keeps the handler).
2. **Call-site flips**: SecretsHandler `SetBindings` (bind-time notify),
   `ReloadSecrets` (returns the pod's resync outcome), workspace env
   PUT/DELETE (notify added — there was no live delivery before), MCP
   push adapters in app.go, and the pod-recreation auto-push adapter
   (`wsAgentPusherAdapter` now notifies).
3. **secretsreconcile** (`api/internal/services/secretsreconcile/`):
   level-triggered loop, default 60s (`LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL`),
   immediate first pass, compares the stored revision row vs the
   `secretsDelivery.spawnedRev` seq prefix — zero decrypts (AC-12b pin:
   1,000-workspace pass over a real `SecretService` with panicking
   decrypt deps, within period). Per-workspace exponential backoff
   (5s×2ⁿ cap 10m, +25% jitter), reset on observed convergence.
   Convergence rules: seq match; empty spawnedRev converged IFF stored
   hash equals the owner's empty-set manifest hash; bare/unparseable
   rev = legacy_format (notify once, backoff bounds the mixed-fleet
   loop); no row / seq mismatch = missing_rev / stale_seq.
4. **ForceRevokeSecret** (`pkg/secrets`): plain DELETE by the owner is a
   revoke-all-workspaces (I12). PG cascade already removed every
   binding (validated: migration 000001 `user_secret_bindings_secret_id_fkey
   ON DELETE CASCADE`); the addition is the per-affected-workspace
   revision refresh (ManifestFor — decrypt-free — + EnsureRevision) so
   expected moves immediately, plus handler-side notify fan-out that
   can never fail the delete. Env-var DELETE uses the same path.
5. **Metrics**: `llmsafespaces_secrets_notify_total{outcome}`,
   `llmsafespaces_secrets_delivery_converged{workspace_id}`,
   `llmsafespaces_secrets_reconcile_passes_total{result}`,
   `llmsafespaces_secrets_delivery_divergent_total{reason}`,
   `llmsafespaces_secrets_reconcile_last_pass_success_timestamp`.
   `api_secret_auto_push_total` outcomes updated to
   success/no_pod/notify_failed.
6. **Alerts** (`helm/templates/prometheus-rules.yaml` +
   promtool scenarios in `helm/tests/alerts_promtool_test.yaml`):
   `LLMSafeSpacesSecretsDeliveryDivergent` (>15m, critical, replica-deduped),
   `LLMSafeSpacesSecretsConvergenceSLOBurn` (<99% over 5m, warning,
   empty-fleet-guarded), `LLMSafeSpacesSecretsReconcileStalled`
   (no successful pass in 3× period incl. an absent() arm, critical).
   Validated with real promtool 2.53.4 (downloaded to /tmp/opencode/bin).

## Assumptions stated and validated

- agentd resync contract shape — validated against Part A's
  `cmd/workspace-agentd/resync_secrets.go` landing in the shared tree
  (200 applied/not_modified, 429 rate_limited, 502 pull_failed).
- `user_secret_bindings` cascades on secret delete — validated in
  migration 000001 (line 1691); the unit-test mock already modeled this.
- Rebind-same-set produces no manifest change → pod 304 no-op — holds
  by construction (manifest hash over binding refs; EnsureRevision
  returns the same seq for the same hash).
- promtool's synthetic clock starts at epoch 0 — discovered via a red
  scenario, adjusted the Stalled eval_time to 10m.

## Known residual gap (closed in-tree — corrected by validation m3)

An earlier revision of this worklog recorded a residual gap: "the
stored revision row refreshes lazily at build/pull for bind/update
mutations, so a failed mutation-time notify with no later pull leaves
the row and the pod EQUALLY stale, and a row-vs-pod reconcile compare
would call that converged." That gap is CLOSED by the live-manifest
loop as it landed: each pass derives the LIVE manifest from the rows
as they are NOW (the same decrypt-free derivation the batch builder
runs), converges the stored row to it (minting drift as a new seq),
and only then compares against the pod's applied seq — so a failed
notify plus a never-run pull flips to divergence on the very next
pass and the loop re-notifies under backoff. Effective-set changes no
handler covers (org/global-default flips) are covered by the same
mechanism. Revoke paths never shared the gap — ForceRevokeSecret
refreshes the row eagerly.

## Testing

`go build ./...`; `go test -race ./api/internal/services/... ./api/internal/handlers/ ./api/internal/app/`
(all green; pre-existing `shadowconsumer` race-timing flake re-ran green
in isolation); `go test ./pkg/secrets/`; `go test ./helm/...` with real
helm+promtool; gofmt/goimports/vet clean.
