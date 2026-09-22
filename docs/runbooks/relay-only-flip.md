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

### The fast lever: `relayOnlyKeyDelivery.api.enabled=false`

When only the EMISSION side regresses (bad batches, registry fallout)
and staging + the router are healthy, the narrow lever is
`--set relayOnlyKeyDelivery.api.enabled=false`: the API Deployment drops
the builder env and resumes raw-key batches within one reconcile/resync
apply, while the controller staging, the envelopes, and the router stay
up (design 0058 §6.4; pinned by `helm/relay_api_emission_chart_test.go`
`TestRelayEmission_APIRollbackLever`). Staged material is inert the
moment the builder stops reading it — no namespace teardown, no
re-seal on the way back. Use the full flag off (above) when the
controller/router legs themselves are the problem.

### Namespace-termination budget on the full rollback

The full flag off deletes the chart-gated `llm-relay` namespace with
everything in it (envelopes, keypair, mint key). Namespace termination
waits out the router pods' `terminationGracePeriodSeconds` (630s — a cap
that drains in seconds when idle but can take the full window under
load), and `helm --wait` does NOT wait for namespace deletion. **Do not
flip the flag back on until `kubectl get ns llm-relay` returns NotFound**
— recreating the namespace while it is Terminating fails. On re-flip,
the router bootstraps a FRESH keypair; the controller's pub-generation
check sees the change and force-re-seals every envelope (the design §4.2
re-seal path — expect one staging pass per workspace).

## Known behaviors (flip-state accuracy)

- **Non-frontable provider kinds stay raw under the flag (K1 carve-out,
  accepted):** `bedrock` / `vertex` / `azure_openai` / `opencode`-kind
  credentials cannot ride the router (US-72.3 D5) and keep the raw
  mixed-fleet path **with the flag ON** — pods with only these kinds are
  NOT key-free post-flip. Every such raw emission is audited
  (`relay_raw_emission` rows: credentialID, slug, kind, reason) — the
  observable signal for the residual raw window. "The raw provider key
  never enters the pod" holds for the frontable kinds; the US-72.6 sweep
  scopes accordingly.
- **Relay conditions freeze on flag-off:** the staging pass no-ops when
  the controller runs without the relay flags, and no cleanup branch
  removes `CredentialsStaged`/`CredentialStale`/`CredentialRejected`
  from existing Workspace CRs — they persist as inert metadata after a
  rollback. Ignore or clear manually.
- **`CredentialStaged=False/PubUnreadable` is ambiguous (KMS wiring
  deferred):** the controller cannot distinguish a KMS-mode deployment
  (`llm-relay-kek` present, pub absent) from an unbootstrapped router —
  design 0058 trade-off (iii) named US-72.5's KMS wiring for the split;
  the board scope omitted it and it is deferred to an owner decision.
  Treat PubUnreadable as "router not bootstrapped or unreadable"; check
  the router pods first.
- **No live resolve-latency histogram:** the router exposes counters
  (`llm_relay_byo_requests_total{workspace,provider,status}`,
  `…_response_bytes_total`, `…_internal_requests_total{path,status}`,
  gauge `…_envelopes_cached`) but no latency histogram — a live resolve
  p99 is not queryable. The latency gate rides the #1432 e2e overhead
  budget (<10ms added vs direct proxy); adding a histogram to
  `byo_metrics.go` is a small follow-up if SLOs tighten.
- **Relay liveness probes consume the router's per-workspace quota**
  (~1 request/provider per 30s against the 120/60s default budget, ~3%
  for two providers — US-72.4 sizing): if quotas are tightened, size
  `LLMSAFESPACES_RELAY_LIVENESS_INTERVAL` accordingly.

## Post-flip evidence: the rogue-agent sweep (US-72.6, #820)

The epic's exit criterion — zero provider-key bytes in any
uid-1000-readable path in a live post-flip pod (K1) — is proven by
`local/us-72-rogue-agent-sweep.sh`, wired into the nightly AFTER the
flip drill (it rides the drill's flipped-ON end state: relay on,
namespaced scope, router up). The step SKIPs loudly until the sweep
script merges (its vehicle is #1537); a SKIP row means the exit
criterion is UNVERIFIED for that run, not passed.

- **R1** binds a planted-canary credential (frontable kind), boots a
  post-flip workspace, and greps every uid-1000-readable surface +
  `/proc/*/environ` for the canary's bytes: zero hits is the criterion.
- **R2** is the positive control (the #1505 rule — a negative sweep
  must prove it can fail): plant the canary in the pre-US-35.7 legacy
  shape, the sweep FINDS it, the in-pod `scrub-legacy-keys` exec
  removes it, and the post-sweep read is zero.
- **K1 carve-out (owner-accepted):** zero-raw-bytes is ASSERTED for
  frontable kinds only; non-frontable kinds
  (bedrock/vertex/azure_openai/opencode) are EXCLUDED pending the
  owner's K1 decision.
- **#820 state (recorded 2026-09-22):** the issue was AUTO-CLOSED at
  #1534's merge (09:04Z, its body said "closes #820") — BEFORE the
  sweep ever executed. The design's exit criterion (a recorded green
  sweep run) therefore remains UNMET; reopening #820 is an owner
  decision. This nightly row is the evidence lane either way: the
  first green run satisfies the criterion, a red run is the
  regression signal.

## Known interactions

- **`rbac.scope=cluster` installs**: the flipped default renders the
  `llm-relay` RBAC, which hard-fails the render under cluster scope
  (the fail-loud guard in `helm/templates/llm-relay-rbac.yaml` — cluster
  scope would grant cluster-wide Secret reads and defeat design 0058
  §4.3's no-read guarantee). An existing cluster-scope install MUST
  either pin `relayOnlyKeyDelivery.enabled=false` or migrate to
  namespaced scope before its next `helm upgrade`. Fail-loud is the
  right posture; this note is the operational half.
- **`networkPolicy.enabled=false`**: relay-only's own ingress policy
  (`llm-relay-router-allow-workspaces`) renders on the relay-only flag,
  NOT the chart-level networkPolicy master toggle — a networkPolicy=false
  install still gets the llm-relay policy (the router is otherwise
  unreachable from workspaces). Pin `relayOnlyKeyDelivery.enabled=false`
  if the dev posture must omit ALL policies.
- **Free-tier Zen provider**: untouched (`shouldSkipRelay` is the
  personal-Zen-key concern, orthogonal by verification in US-72.4).
- **Sidecar mode**: the batch file was already uid-isolated there; the
  flip removes the remaining S1/S2 surfaces for both modes.
- **#821 egress allowlist**: separate epic; the router's
  NetworkPolicy carve-out ships with the chart (US-72.2).
