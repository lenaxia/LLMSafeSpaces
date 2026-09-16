# Epic 72 — Relay-only key delivery (design 0058)

**Status:** Planning (design accepted pending review — implementation stories filed against this folder)
**Created:** 2026-09-15
**Priority:** P1 (security — closes #820)
**Tracking:** GitHub issue #820 (epic label `[epic-72]` to be applied when the epic issues are filed — #820 currently carries `P1`/`security`/`agent-integration` only); this document is the authoritative story map. Execution status lives on GitHub.
**Design:** [`design/0058_2026-09-15_relay-only-key-delivery.md`](../../0058_2026-09-15_relay-only-key-delivery.md) · **Normative decisions:** the owner's 2026-08-27 decision record on #820 (D1 relay-through default, D2 per-request resolve / no plaintext cache, D3 agentd non-decrypt-capable) — fixed inputs.

> **Renumbered 68 → 72 (2026-09-15).** The 2026-08-27 decision record drafted this
> epic as `epic-68-relay-only-key-delivery`. Epic 68 was already taken on disk by
> **chat file attachments** (`design/stories/epic-68-chat-file-attachments/` — itself
> renumbered 67→68 at its closure, 2026-08-27). This folder takes the next free
> number, **72** (70 = secret-delivery-v2, 71 = input-delivery-robustness; 69 has no
> on-disk folder — its stories US-69.1–.14 live in issues #1135–#1148 (epic tracking issue #1134)). All story IDs
> from the decision record's draft (`US-68.0`…`US-68.6`) are renamed `US-72.0`…`US-72.6`
> here, one-for-one, same order. The repo's epic-number hygiene (two `0052` docs, two
> `epic-55`/`epic-64` folders on disk) is the separately-filed hygiene follow-up named
> in the decision record.

---

## Problem statement

A raw LLM provider API key is delivered **decrypted** into uid-1000-readable files in
every workspace pod (`agent-config.json`, `auth.json`, the batch file — design 0058
§1.2, surfaces S1–S3 with file:line), and the config wires the provider **directly**
to its public `baseURL`. The agent process is the untrusted party (arbitrary code
from user repos); the key is one prompt injection away from exfiltration (#820
live evidence; the exfil sink itself is #821). Relay-only key delivery removes the
key from the pod entirely: providers point at a resolve-capable router, agent
config carries only an ephemeral scoped token, and the router holds the unwrap.
Coverage is uniform across owner types — user, admin, and org llm-provider
credentials all ride the same batch construction (`injection.go:482-518`) and are
rewritten identically; this is the design's answer to the 2026-09-15 triage
question on #820 (per-credential "refuse raw-key when relay-reachable" rejected —
design §4.1/§9).

## What this epic is NOT

- **Not the Inference Relay Fleet (Epic 42).** The fleet is opt-in free-tier Zen IP
  rotation across cloud VMs. Epic 72's router is a dedicated in-cluster deployment
  in a new `llm-relay` namespace. Shared: proxy hygiene patterns (hop-by-hop
  stripping, router-side key injection). Not shared: VMs, 429 rotation, fallback.
- **Not #821.** Egress allowlisting stays its own epic; this epic removes the
  *credential* from the pod, not the network channel.
- **Not a new delivery mechanism.** Tokens ride the Epic 70 batch machinery
  (one builder → materialize → `AgentConfigWriter` → resync) unchanged.

---

## Story status

| Story | Scope | State | Sizing | Depends on |
|---|---|---|---|---|
| US-72.0 | #910 relay injector re-arm (load-bearing precondition) | open | M | #910 issue |
| US-72.1 | `pkg/secrets.StagingProvider` — KMS-envelope prod / HPKE dev | open | M | — |
| US-72.2 | `llm-relay` namespace + 2-replica BYO resolve router (token mint/verify, per-request local resolve, sanitization, drain) | open | L | US-72.1 |
| US-72.3 | Controller staging + Workspace conditions (`CredentialsStaged`/`CredentialStale`/`CredentialRejected`) + policy flag + quota/alerts | open | M | US-72.1, US-72.2 |
| US-72.4 | agentd token-only emission (builder rewrite) + relay-only liveness + degrade codes | open | M | US-72.0, US-72.2, US-72.3 |
| US-72.5 | Lifecycle/policy gates + default flip + canary + rollback drill | open | M | US-72.0–.4 |
| US-72.6 | Migration: PVC scrub of legacy `auth.json` keys + e2e rogue-agent sweep (#820 close-out) | open | S/M | US-72.5 |

Sequencing (fixed by the decision record): **#910 first** — a relay-only default
makes the one-shot injector's failure modes load-bearing; without re-arm, a
boot-time relay fetch failure strands a workspace with no raw-key fallback. Then
StagingProvider → router resolve → controller staging → agentd token emission →
lifecycle/policy gates → migration.

---

## US-72.0 — Relay injector re-arm (#910), generalized precondition

**Goal:** implement #910 exactly as its issue proposes (re-triggerable injector:
bounded backoff 5m→30m cap, gated on no busy sessions via the existing tracker,
`HasRelay()` re-check each cycle, ctx-alive, per-outcome metric), and land the
generalized reachability re-arm that US-72.4 consumes: a reusable "terminal fetch
failure → loud degrade → bounded re-arm" loop, not injector-specific code.

**Scope/files:** `cmd/workspace-agentd/relay_injector.go` (state machine), the
session-aware kill-deferral interplay (`defaultMaxDefer` policy: re-arm attempts
must not stack behind deferred kills — #910's named constraint), readyz
`RelayInjected` semantics re-checked per cycle, metric `relay_rearm_outcome`.
Design note in worklog before implementation (per #910).

**Acceptance criteria:**
- Boot-window router outage: injector re-arms and applies relay config without pod
  recreation; re-arm outcome observable per cycle.
- No mid-turn SSE drops: re-arm never fires while a session is busy; deferred-kill
  window respected; no stacked restarts (exactly one in-flight re-arm).
- `HasRelay()` short-circuit preserved once applied.

**Test plan (TDD):** red-first unit tests for the backoff state machine (terminal
failure → retry schedule; success → disarm; ctx cancel → stop); fault-injection e2e
(router down at T+0, up at T+3m → relay applied, no manual restart); regression pin
for the 2026-08-16 incident class (workspace 946a442f — free-models fetch EOF →
permanent unresolvable default; now: bounded recovery); `-race` on the re-arm loop.

**Sizing:** M. **Dependencies:** #910 (this story *is* its implementation under this epic's umbrella).

---

## US-72.1 — `pkg/secrets.StagingProvider` (envelope sealing)

**Goal:** the D2 envelope: seal a provider key under a router-scoped local KEK;
KMS-wrap the KEK (prod) or HPKE-seal to the router's public key (dev); the
plaintext KEK exists only in controller/router memory.

**Scope/files:** `pkg/secrets/staging_provider.go` (+tests) alongside the KEK
machinery (`root_key.go`, `kms_aws_provider.go`, `kms_gcp_provider.go` precedents);
versioned envelope wire format (`stg:v1:` prefix, algorithm-discriminated) with a
**plaintext `keyID` discriminator bound as GCM AAD** (integrity-bound — envelopes
cannot claim a key they weren't sealed under; design §4.2's rotation depends on it);
`llm-relay-kek` Secret contract (KMS-wrapped blob + key id); HPKE mode: candidate
library `github.com/cloudflare/circl` (RFC 9180) — **dependency review is part of
this story** (version pin, license, govulncheck/Trivy evidence recorded in the
worklog; no stdlib HPKE exists and `go.mod` carries no HPKE dependency today);
HPKE keypair machinery per design §4.2 (create-or-adopt first boot, Secret-watch
key loading, dual-key resolve window, time-bounded prior-key retention, DR
regeneration — the router-side halves land in US-72.2; this story owns the
sealing leg and the envelope format the machinery speaks).

**Acceptance criteria:**
- Seal→resolve roundtrip both algorithms; envelope versioning discriminates.
- The KMS client is invoked only at provider construction (boot) — a benchmark pin
  asserts zero KMS calls at steady-state resolve.
- `llm-relay-kek` Secret never contains plaintext KEK bytes (test greps all
  produced artifacts).
- HPKE mode: controller (sealer) holds no decrypt capability (compile/behavior pin).

**Test plan (TDD):** red-first unit tests per AC (`staging_roundtrip_kms_hpke`,
`kek_never_in_secret`, `ciphertext_only_informer`, `resolve_is_local_aesgcm`
benchmark with KMS-call-count pin); tampered-ciphertext → loud error, never
plaintext fallback; cross-algorithm envelope confusion rejected.

**Sizing:** M. **Dependencies:** none (Epic 50/58 KEK providers exist on main).

---

## US-72.2 — `llm-relay` namespace + 2-replica BYO resolve router

**Goal:** the router that validates tokens, resolves envelopes per request (local
AES-GCM against informer-cached ciphertext — D2), sanitizes requests (§4.7),
injects the real key upstream, and drains gracefully on deploy (#1078 pattern).
**Scope/files:** extend `cmd/relay-router/` with BYO resolve mode (deployment-shape
decision — separate Deployment in `llm-relay` vs a second listener — settled in
this story's worklog, leaning separate Deployment: blast radius and quota models
differ from the fleet router); token mint/verify (`POST /internal/v1/tokens`,
HMAC-SHA256, payload `{workspaceID, providerSlug, baseURL, modelAllowlist, iat,
exp, keyID}` — validity = HMAC ∧ staged-Secret-present ∧ not-expired) and key
machinery (`POST /internal/v1/keys/rotate`, create-or-adopt first boot with
fingerprint assert, Secret-watch key loading, dual-key resolve, time-bounded
prior-key retention — design §4.2, replica-symmetric for the ×2 topology);
Secret informer cache (ciphertext only); request pipeline: client-Authorization
replace (generalize `applyUpstreamAuth`, `proxy.go:40-56`), hop-by-hop + identity
header strip (extend `routerHopHeaders`, `proxy.go:66-75`), method/path allowlist
(chat-completions-class + `/models`), request/response size caps, model-allowlist
enforcement (`model` field against token scope), per-workspace quota counters +
Prometheus alerts; `/models` served from staged `LLMProviderData.Models`;
routing path `/w/<workspaceID>/<providerSlug>/v1/…` (terminates in `/v1` so
OpenAI-compatible clients append `/chat/completions` — design §4.5 is
authoritative on the shape); **log/persistence posture (K7)**: metadata-only
logging (workspace, slug, keyID, status, latency, bytes, rejection reason) —
request/response bodies never logged, sampled, or buffered to disk;
body-adjacent diagnostics must pass `pkg/redact` or are forbidden; Helm:
`llm-relay` namespace,
Deployment ×2 + PDB `maxUnavailable: 1`, `terminationGracePeriodSeconds` sized to
max stream duration (a cap not a delay — #1078), preStop not-ready, Service,
NetworkPolicy (workspace-ns egress carve-out via namespaceSelector, the
`relay-router-networkpolicy.yaml` pattern), RBAC (router SA: get/list/watch
Secrets in `llm-relay`, plus the §4.3 name-scoped write carve-out for exactly the
two HPKE keypair Secrets; controller SA: create/update/delete other `llm-relay`
Secrets + `get` on `llm-relay-hpke-pub`).

**Acceptance criteria:**
- All three scope conjuncts enforced per request (workspace, baseURL, model
  allowlist); violations → 401/403 with machine-readable reasons.
- Steady-state resolve performs zero KMS calls and zero upstream fetches for
  `/models` when the staged catalog is present; p99 resolve+forward overhead
  measured and within budget (<10ms added vs direct proxy, e2e leg).
- Rolling deploy mid-stream: in-flight streams complete; zero client resets
  (two-replica drain drill).
- Sanitization suite green: client-supplied Authorization never reaches upstream;
  identity headers stripped; oversize bodies refused; disallowed methods/paths 404.
- HPKE machinery replica-symmetric on the ×2 topology: simultaneous cold start
  converges on one adopted keypair (fingerprint asserted); rotation keeps both
  replicas resolving old+new envelopes within the watch-propagation bound —
  with the design §4.2 stated failure modes pinned: a replica restarting
  mid-re-seal resolves only the new key until re-seal completes; a re-seal
  overrunning the retention interval degrades to `CredentialStale`, never
  silently; the private-then-pub update ordering makes torn rotations
  un-confirmable; prior keys drop uniformly after the retention interval;
  DR (keypair Secret loss *or* corruption — corruption detected by the
  watch-time **self-contained** assert on private-key loads, not
  first-boot-only, never cross-Secret) regenerates with an honest
  `CredentialStale` window until re-seal.

**Test plan (TDD):** red-first table-driven `token_scope_matrix` (wrong workspace /
wrong baseURL / off-allowlist model / expired / forged HMAC / deleted-Secret →
`credential_stale`); `router_sanitization_suite`; `revocation_secret_delete_401`
— **asserting an upper bound on the deletion→401 window** (informer watch
propagation latency, design §4.4) — plus a token `exp` clock-skew test (skew
tolerance bound pinned); `router_logs_metadata_only` (K7: drive a full
request/response, capture every log/metric emission, assert zero body bytes);
key-machinery tests (this story owns the ROUTER side — rotate receipt,
preconditioned updates, adopt/watch/assert/resolve/retention; BOTH
controller-side behaviors — the reconcile predicate that terminates DR
windows AND seal-time pub-generation validation — are owned and tested by
US-72.3): `firstboot_two_replica_adopt`, `rotation_dual_key_window_bounded`
(window ≤ watch bound, both replicas), `rotation_assert_quiesced_in_torn_window`
(peer private-key load during the private-then-pub window never fires DR — the
assert is self-contained, generation-tagged; generation checks against the pub
Secret live only at the controller: vs the rotate response at seal time, vs
last-sealed at reconcile), `rotation_replica_restart_mid_reseal`
(old-keyID fails on the restarted replica only until re-seal completes),
`reseal_completes_before_retention_expiry` (default settings; overrun degrades
to `CredentialStale`, not silent), `rotation_torn_update_unconfirmable`
(private-then-pub ordering; a pub read predating the rotate return never
confirms), `prior_key_retention_expiry`, `dr_keypair_loss_failclosed_recovery`
(including the corrupted-Secret overwrite path, detected by the watch-time
fingerprint assert with no restart — a running fleet converges on the bounded
recovery), `dr_dual_replica_recovery_single_keypair` (simultaneous failures →
generation-preconditioned single lineage, pub-write loser adopts the winner),
`envelope_keyid_aad_bound`
(design §7 rows); `deploy_drain_two_replica` e2e; quota-alert firing test (prometheus rule
unit); informer-drop test (Secret deleted → cache evicted → next request 401).

**Sizing:** L. **Dependencies:** US-72.1.

---

## US-72.3 — Controller staging + conditions + policy flag

**Goal:** the controller seals bound BYO credentials into `llm-relay` Secrets
(per-workspace, per-provider), obtains tokens from the router mint endpoint,
stages them for the batch builder, sets the three new conditions, and owns the
deployment policy flag + quota/alert defaults.

**Scope/files:** `controller/internal/workspace/` (staging reconcile step; token
handoff Secret in the workspace namespace — the batch builder's token source);
`pkg/apis/llmsafespaces/v1/workspace_types.go` (condition types
`CredentialsStaged` / `CredentialStale` / `CredentialRejected` alongside
`CredentialsAvailable`); Helm `relayOnlyKeyDelivery.enabled` (default **false**
until US-72.5) + fail-loud controller startup guard when enabled without the
`llm-relay` router (the `rbac.scope=cluster` gate precedent); token TTL wiring
(default 24h) with expiry participating in the manifest tier (renewal rides
US-70.2/70.3 conditional pull — no new delivery path); quota/size/alert defaults.

**Acceptance criteria:**
- Bound provider → envelope Secret in `llm-relay` + token staged; unbind/credential
  delete → Secret deleted (revocation = Secret deletion, D2) → `CredentialStale`
  flips on next resolve.
- Conditions observable end-to-end: staged (True/revision), stale (token expiry /
  missing envelope / batch not applying staged revision / corruption-class
  resolve failure — §4.2's one escalating cause), rejected (router
  rejection telemetry → controller event).
- Flag off: zero behavior change (raw-key path, byte-identical batches); flag on
  without router: controller startup refuses, loud.
- Token renewal: a pod alive past TTL/2 receives a fresh token via resync batch
  and applies it behind the session-aware restart decision (#852), no manual
  intervention.
- **DR/residual window terminator (design §4.2 reconcile predicate):** at each
  pass the staging reconcile re-gets `llm-relay-hpke-pub` and compares its
  `generation` against the last-sealed generation (persisted with the staging
  state) — changed → re-seal every envelope; unchanged while `CredentialStale`
  persists **and the staleness is corruption/wrong-pub-class** (the one cause
  rotation repairs) **under an intact spawn-layer lineage** (envelope Secrets
  present AND the child spawned with the staged revision — the `spawned_rev`
  terminal signal, not the batch-apply anchor, so the #852 deferral window
  never escalates) → rotate escalation, anti-storm-bounded (≥10m floor);
  **never** for revocation-class (envelope deleted — D2 terminal state),
  delivery-class (batch not applying OR #852-deferred restart), or
  token-expiry-class (renewal owns it) staleness.
- **Seal-time generation validation (design §4.2):** the pub payload's
  `generation` is validated against the rotate response before any envelope
  is sealed against it (shape-invalid pub bytes fail parsing outright).

**Test plan (TDD):** `dr_window_reconcile_terminates` (pub-generation change →
re-seal; corruption/wrong-pub-class stale with intact spawn-layer lineage and
unchanged generation → anti-storm-bounded rotate escalation; escalation
SUPPRESSED for revocation-class, delivery-class (incl. the #852 deferral
window), and token-expiry-class staleness — the controller-side predicate,
homed HERE per design §4.2);
`pub_sealtime_generation_validated` (pub generation ≠ rotate response →
rejected, never sealed against; shape-valid wrong-pub residual surfaces as
`CredentialStale` — also homed HERE: the seal-time check is controller-side,
pairing with the rotate leg);
red-first controller reconcile tests (stage/unbind/rotate —
rotate leg: trigger `POST /internal/v1/keys/rotate`, re-seal every envelope,
confirm completion by envelope `keyID` metadata alone, never decrypt —
confirmation reads the controller's own write-acks, no envelope read-back);
mixed-fleet batch tests (`mixed_fleet_batches`: same deployment serves legacy
bare-key batch to pre-flip semantics and token batch post-flip — the W15 pin
pattern); envtest conditions matrix; helm-render tests for the flag/guard/namespace.

**Sizing:** M. **Dependencies:** US-72.1, US-72.2.

---

## US-72.4 — agentd token-only emission + relay-only liveness

**Goal:** with the flag on, the batch's llm-provider entries carry
`apiKey = token`, `baseURL = router URL`; agentd emits them through the unchanged
seam and stays non-decrypt-capable (D3); boot-time router failure degrades loudly
and re-arms (US-72.0's generalized loop), never strands.

**Scope/files:** batch-builder rewrite is API-side (one builder, US-70.2 invariant
— `pkg/secrets` batch construction swaps token+router-URL for raw key+baseURL
under the flag); agentd: zero formatter/seam changes by construction (pin with
tests — `FormatProviders` passes the token verbatim, `format.go:71`); healthz /
readyz degrade codes `relay_unreachable` / `token_expired` surfaced via the
existing statusz → CRD path (US-70.1 precedent); `EnrichProviders` against the
router's `/models`; `shouldSkipRelay` untouched (free-tier Zen concern, verified
orthogonal); e2e sweep instrumentation hooks.

**Acceptance criteria:**
- Post-flip pod: `agent-config.json` and `auth.json` contain token bytes only;
  zero provider-key bytes in any uid-1000-readable path (canary leg of the
  US-72.6 sweep).
- Router unreachable at boot: workspace boots, conditions surface
  `relay_unreachable`, re-arm recovers within backoff bounds (no strand, no
  manual restart) — the §4.8 no-fallback property, proven.
- Enrichment works through the router (custom-endpoint model lists present,
  zero key bytes pod-side).
- Token renewal applies behind busy sessions (no SSE drops).

**Test plan (TDD):** red-first unit tests for builder swap (flag on/off golden
batches); agentd exec tests: materialize a token batch → assert files contain
token, not key (canary plaintext grep); fault-injection e2e `boot_relay_outage_rearm`;
`-race` on the liveness loop.

**Sizing:** M. **Dependencies:** US-72.0, US-72.2, US-72.3.

---

## US-72.5 — Lifecycle/policy gates + default flip + rollback

**Goal:** the flip runbook: gates, canary, flip, rollback drill — the D6/#1078
discipline applied to relay-only.

**Scope/files:** flip gate checklist (all: #910 merged via US-72.0; router
SLO-green — resolve p99, error rate, drain drill green; `CredentialsStaged` True
on canary set; sanitization + quota/alerts verified; rollback drill executed);
Helm default flip (`relayOnlyKeyDelivery.enabled: true`); runbook
`docs/runbooks/relay-only-flip.md`; rollback = flag off (staged material inert;
raw-key batches resume; pods heal on next batch apply).

**Acceptance criteria:**
- Canary: flip on → validate (sweep green, conditions green, one full
  suspend/resume + one long-lived turn) → flip off → validate legacy pods healthy
  → flip on again (the design 0051 D6.1 exercised-rollback pattern).
- Post-flip: new workspaces token-only by default; the sweep (US-72.6) green on
  the nightly leg.

**Test plan (TDD):** rollback drill as a scripted e2e (`rollback_drill_relay_only`);
flip-gate runbook rehearsal recorded in the story worklog; helm-render pin that
the default flipped with the gate checklist committed in the same PR.

**Sizing:** M. **Dependencies:** US-72.0–.4.

---

## US-72.6 — Migration: PVC scrub + rogue-agent sweep (closes #820)

**Goal:** remove legacy plaintext from PVCs (pre-US-35.7 real-file `auth.json`
keys; platform-shaped `agent-config.json` copies under `/workspace/.local`) and
prove the epic's exit criterion end-to-end: zero provider-key bytes in the pod.

**Scope/files:** agentd `scrub-legacy-keys` subcommand (narrow: known
platform-shaped paths only — `/workspace/.local/opencode/auth.json` as a regular
file, `agent-config.json` copies under `/workspace/.local`; user-authored files
never touched, design 0051 §3 boundary); controller schedules it on
reconcile/resume post-flip; outcome as event + condition (idempotent); e2e sweep
`local/us-72-rogue-agent-sweep.sh` (nightly-wired, the US-70 e2e precedent):
exec in a live post-flip pod — cat/grep every uid-1000-readable path, `/proc/*/environ`,
batch file, config, auth store — for a planted canary key's bytes; asserts zero
hits. #820 closes on this sweep green.

**Acceptance criteria:**
- Workspace with a pre-US-35.7 real-file auth.json on PVC: scrub removes key
  material, emits event, re-run is a no-op.
- User files untouched (fixture workspace with user-authored JSON carrying
  `key`-shaped fields survives byte-identical).
- Sweep green on kind nightly; planted canary found in the pre-flip control run
  (proves the sweep can fail) and absent post-flip.

**Test plan (TDD):** red-first scrub unit tests (real-file/symlink/user-file
matrix); exec-level integration (fixture PVC → scrub → verify); sweep script with
a deliberate positive-control mode.

**Sizing:** S/M. **Dependencies:** US-72.5.

---

## Cross-cutting invariants (enforced in review; violations require a design amendment)

- **K1 — No provider-key plaintext in uid-1000-reachable space** (any mode, any
  path, any process memory) after the flip.
- **K2 — KMS O(boot)**: zero KMS calls per request at steady state (benchmark pin).
- **K3 — Token validity is conjunctive**: HMAC ∧ staged-Secret-present ∧ not-expired.
- **K4 — agentd never decrypts, never signs** (D3): no KEK, no signing key, no DEK
  in agentd's reachable space, either container mode.
- **K5 — Revocation = Secret deletion** (D2): no second revocation mechanism;
  effect bounded by informer watch propagation (test-pinned upper bound).
- **K6 — No new delivery path**: tokens ride the Epic 70 batch machinery only.
- **K7 — Router persistence posture**: metadata-only logging; request/response
  bodies never logged, sampled, traced, or buffered to disk at any verbosity;
  body-adjacent diagnostics pass `pkg/redact` or are forbidden (design 0027
  proxy-as-trust-boundary principle; design 0058 §4.7).
