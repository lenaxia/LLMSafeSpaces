# Runbook: relay-only key delivery flip (Epic 72 / US-72.5, design 0058)

**Owner:** platform operator
**Closes:** the delivery half of #820 (the sweep close-out is US-72.6)

The chart default flipped to `relayOnlyKeyDelivery.enabled: true` in the
US-72.5 PR. This runbook is the live-fleet companion: the gate evidence,
the canary waves for existing clusters upgrading to the flipped chart,
the stop conditions, and the rollback.

## What the flip does

- The controller seals bound BYO provider credentials into per-workspace
  envelope Secrets in `llm-relay` and stages per-pod tokens
  (`CredentialsStaged` / `CredentialStale` / `CredentialRejected`
  conditions; revocation = Secret deletion).
- The API's batch builder emits `apiKey = token`, `baseURL = router URL`
  for llm-provider entries — the raw provider key never enters the pod
  (K1).
- agentd stays non-decrypt-capable (K4); boot-time router failure
  degrades loudly (`relay_unreachable`) and re-arms (US-72.0's loop) —
  no raw-key fallback exists under the flag (design §4.8).

## Gate evidence (code-side, closed)

| Gate | Evidence |
|---|---|
| #910 relay injector re-arm | merged via #1401 (US-72.0) |
| Envelope staging (`StagingProvider`) | #1407 (US-72.1) |
| Router: token mint/verify, per-request resolve, sanitization, quota/alerts, drain | #1432 (US-72.2) — sanitization suite + drain drill green |
| Controller staging + conditions + startup guard | #1448 (US-72.3) |
| Token-only emission + relay-only liveness | #1529 (US-72.4) |
| Rollback drill | `local/us-72-relay-only-flip-drill.sh` (US-72.5) — flip → canary → rollback (positive control: the raw key RETURNS) → re-flip |
| Default-flip pin | `helm/llm_relay_chart_test.go` `TestRelayOnlyKeyDelivery_DefaultFlippedOn` |

## Live-fleet canary (operator, post-upgrade)

Existing clusters take the new default on their next `helm upgrade`
(unless values pin the flag). The drill's cluster-level rehearsal
validates mechanics; the fleet waves validate SLO:

1. **Canary one Active workspace** with a bound BYO credential:
   suspend → activate (fresh pod, fresh batch), then verify:
   - `CredentialsStaged=True` on the Workspace;
   - in-pod: `agent-config.json` apiKey is the token, baseURL is the
     router; **zero raw-key bytes** in any uid-1000-readable path
     (the drill's `sweep_hits` is the reference sweep);
   - one chat turn through the router (resolve + forward healthy).
2. **Waves (≤25% of BYO-credential workspaces per wave)**, same
   verification per wave. Stop conditions — the real router series
   (`cmd/relay-router/byo_metrics.go`, alert group
   `llmsafespaces.llm-relay` in `helm/templates/prometheus-rules.yaml`;
   CounterVec-absent-while-zero semantics apply to the counters):
   - `llm_relay_byo_requests_total{status=~"401|403|429"}` climbing on
     canary workspaces (auth/scope/quota rejections — the alert fires at
     >20 per workspace per 10m);
   - `llm_relay_byo_requests_total{status=~"5.."}` (router errors — the
     alert fires at >10 per workspace per 10m);
   - resolve/forward latency regression vs the pre-flip baseline (the
     #1432 e2e budget: <10ms added vs direct proxy).

## Rollback

`--set relayOnlyKeyDelivery.enabled=false` (or pin the value pre-upgrade):

- staged envelope Secrets go inert (the router stops resolving them);
  raw-key batches resume; **pods heal on the next batch apply** —
  suspend/activate the workspace (the drill's R3 leg pins exactly this:
  the raw key RETURNS and the workspace is functional);
- no data migration to undo — tokens die with their pods, envelopes are
  inert until deleted (GC rides the workspace-unbind path).

## Known interactions

- **Free-tier Zen provider**: untouched (`shouldSkipRelay` is the
  personal-Zen-key concern, orthogonal by verification in US-72.4).
- **Sidecar mode**: the batch file was already uid-isolated there; the
  flip removes the remaining S1/S2 surfaces for both modes.
- **#821 egress allowlist**: separate epic; the router's
  NetworkPolicy carve-out ships with the chart (US-72.2).
