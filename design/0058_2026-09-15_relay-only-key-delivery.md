# 0058 — Relay-only key delivery: LLM provider keys never land in uid-1000 space

**Status:** Proposed (2026-09-15) — design stage, holds for review; implementation tracked in Epic 72 (`design/stories/epic-72-relay-only-key-delivery/`)
**Date:** 2026-09-15
**Issue:** #820 (P1 · Epic 67 security review #819) · **Folder:** `design/stories/epic-72-relay-only-key-delivery/`
**Elaborates:** the owner's 2026-08-27 decision record on #820. Decisions 1–3 there are **fixed inputs** — this design implements them, it does not re-litigate them.
**Depends on:** #910 (relay injector re-arm — load-bearing precondition, §4.8), design 0057 / Epic 70 (one-builder batch + pod-bootstrap token machinery), design 0051 (sidecar mount topology), Epic 50/58 (master KEK / `RootKeyProvider`), design 0049 / US-65.1 (`AgentConfigWriter` seam), #1078 (deploy-drain pattern), #852 (session-aware restart)
**Composes with:** Epic 42 / Inference Relay Fleet (a *different* subsystem — see §1.3), design 0053 (platform overlay delivery), #821 (egress allowlist — explicitly out of scope, §3)
**Supersedes:** nothing merged. The unmerged 2026-08-14 draft (`feat/issue-820-llm-key-broker-design`, an in-pod key broker) is **withdrawn** in favor of relay-through per the decision record (§9).

---

## 1. Problem

### 1.1 The threat (confirmed live, 2026-08-14 evidence on #820)

The agent process is the untrusted party: it executes arbitrary code from user repositories and is one prompt injection away from exfiltration. Today a raw LLM provider API key (e.g. `thekaocloud`, `sk--…`) is delivered **decrypted** into files the agent can read, and `agent-config.json` wires the provider **directly** to its public `baseURL`. The key is *unused-but-live* in the container — exactly the credential a rogue agent exfiltrates. The exfiltration sink is open while #821 (egress allowlist) remains unfixed.

### 1.2 Current exposure surfaces — re-verified on main @ `17a2f288`, 2026-09-15

The 2026-08-14 verification comment lists four files. US-70.5 (merged 2026-09-01, #1224) demolished one of them; the batch-file surfaces were reshaped by US-70.2/70.3. Corrected inventory:

| # | Surface | Path(s) | Evidence (file:line, main @ `17a2f288`) | Agent-readable? |
|---|---|---|---|---|
| S1 | Provider `options.apiKey` in the merged agent config | `/sandbox-runtime/agent-config.json` (single-container) · `/agentd-config/agent-config.json` (sidecar) | rendered `pkg/agent/opencode/format.go:71` (`APIKey: p.APIKey` → `options.apiKey`, field at `format.go:152`); written by the ConfigWriter at `pkg/agent/opencode/configwriter.go:652` (atomic rename, mode 0640) | **Yes, both modes** — necessarily: opencode (uid 1000) is the reader. Sidecar mode's RO workspace mount is an *integrity* property (design 0051 US-4b), not confidentiality — 0640 + shared gid 1000 keeps it readable. |
| S2 | Provider `key` in opencode's auth store | `/sandbox-runtime/rt/auth.json` ← symlink from `/workspace/.local/opencode/auth.json` (tmpfs, #1296 mode **0660**) | boot-time merge `cmd/workspace-agentd/secrets.go:601-612` (`writeStagedProvidersToAuthStoreW`, chmod 0660 at `secrets.go:1495-1498`); reload path stages via opencode `PUT /auth/:providerID` (`pkg/agent/opencode/client.go:191`, URL at `client.go:276`), payload `Key: p.APIKey` (`client.go:261`) | **Yes, both modes** — 0660 shared-gid read+write across the uid split (the #1296 fleet-wide-provider-failure fix depends on it). |
| S3 | The secret **batch file** (bootstrap output = the durable credential state since US-70.5) | single-container: `/sandbox-cfg/secrets.json` (bootstrap write) **and** `/sandbox-runtime/rt/secrets.json` (resync write, relocated by US-70.3 B1) · sidecar: `/sandbox-runtime/rt/secrets.json` only | bootstrap default out + 0600 write: `cmd/workspace-agentd/bootstrap.go:86,173`; single-container resync relocation: `controller/internal/workspace/pod_builder.go:331-343`; resync atomic write 0600: `cmd/workspace-agentd/resync_secrets.go:177`; "the batch file IS the durable state": `cmd/workspace-agentd/secrets.go:519-522`. The batch carries decrypted `LLMProviderData` incl. `apiKey` (`pkg/secrets/types.go:243`; decrypt at `pkg/secrets/injection.go:482-518`) | **Single-container (today's fleet default, `agentdSidecar.enabled: false` at `helm/values.yaml:376-377`): yes** — the bootstrap init and the main-container agentd both run uid 1000 (`pod_builder.go:605`), so the 0600 files are same-uid readable. **Sidecar: no** — written by the uid-2000 sidecar (`agentd_sidecar.go:99,189`), 0600, and `/agentd-secrets` is never mounted in the workspace container. |
| ~~S4~~ | ~~`/sandbox-runtime/last-reload-secrets.json` reload cache~~ | — | **Demolished by US-70.5** (#1224). Survives only as a repolint deleted-symbol guard (`pkg/repolint/deleted_symbols.go:48`). Its replacement is S3 — the resync pull persists every applied envelope to the batch file. | n/a — the Aug-14 comment's 4th surface no longer exists; the *function* (durable plaintext batch) lives on in S3. |

Non-surfaces, verified: `secrets-env` never carries llm-provider material — the Materializer routes by type, and llm-provider goes exclusively to the config + auth store (`pkg/agentd/secrets/secrets.go:653-654`, "AgentConfigPath is exclusively managed by FlushProviders" at `secrets.go:703`); the enricher cache holds model lists, not keys.

**Net: on today's default (single-container) fleet, the raw key sits agent-readable in three files (S1, S2, S3×2 paths); on sidecar-mode pods, in two (S1, S2 — both *structurally* unavoidable while opencode itself holds the real key).** The only fix that removes S1/S2 is to stop putting the real key in the pod at all.

### 1.3 Two relay subsystems — keep the distinction crisp

README-LLM §Relay Config Subsystem ≠ §Inference Relay Fleet. The former is how `agent-config.json` is *built in the pod*; the latter is the *external* Epic 42 VM fleet (opt-in, free-tier Zen IP rotation). This design **bridges** them: it reuses the config-subsystem's writer seam to point providers at a **new resolve-capable router**, and it borrows the fleet-router's proven proxy machinery (weighted selection, 429-storm detection, fallback, router-side key injection) — but the BYO-key router is a **separate deployment in a dedicated namespace**, not the Epic 42 fleet (the fleet's VMs are free-tier Zen relays; customer keys must not transit third-party VMs outside the operator's trust boundary — §3).

---

## 2. The decision record (fixed, 2026-08-27 — owner, on #820)

1. **D1 (product): relay-through as the default** for BYO-key providers. The platform relays customer keys to their own provider accounts; traffic visibility to the platform is the accepted trade in exchange for eliminating the primary threat (prompt-injected/rogue agent exfiltrating the raw key — confirmed live in the issue evidence).
2. **D2 (crypto): per-request resolve at the relay router, no long-lived plaintext cache.** Steady-state unwrap is a local AES-GCM operation (µs) against informer-cached ciphertext; KMS is O(boot), never O(request). Revocation = Secret deletion.
3. **D3 (trust boundary): agentd stays non-decrypt-capable.** It receives only an ephemeral scoped token (scope = baseURL + model allowlist + workspace, TTL'd). Decrypting in the pod would require the user DEK (which unwraps *all* user secrets) and would turn agentd into a loopback decrypt oracle sitting next to the attacker.

Named components this design must elaborate: `pkg/secrets.StagingProvider` (KMS-envelope prod / HPKE dev), dedicated `llm-relay` namespace, 2-replica router with #1078-style stream drain, Workspace conditions (`CredentialsStaged` / `CredentialStale` / `CredentialRejected`), PVC scrub for legacy `auth.json` keys at migration, mandatory router request sanitization. Residuals to document: exfil-through-relay channel (quota/size/alert bounded), relay-VM key visibility (protocol v2 seam reserved for future blind-tunnel mode).

---

## 3. Threat model

**What this kills.** The in-pod raw-key read → exfil chain (S1–S3): zero provider-key bytes exist in uid-1000-readable space, in agentd's memory, or on the PVC. A prompt-injected agent that cats every file and ptraces its own pod finds only scoped, TTL'd tokens. Corollary win: the pod no longer needs direct egress to provider endpoints — the *destination* moves to the router (defense-in-depth against #821 remaining open; #821 is still needed for every other exfil sink).

**What it accepts (documented residuals).**
- **Platform traffic visibility:** the router (operator) sees prompt/completion plaintext for BYO-key providers. Accepted by D1 explicitly.
- **Exfil-through-relay content channel:** a rogue agent can *encode* data into prompts it sends through the router to its own provider account (the token is scoped to the user's own providers — exfil to an *attacker's* account is not possible; the channel is "data in prompts to the user's own provider"). Bounded by: per-workspace request quota + request/response size caps + anomaly alerts at the router (§4.7). Residual, not eliminated — prompt content is load-bearing by definition.
- **Token theft = use, not key disclosure:** a stolen token lets the attacker burn the workspace's own quota at its own providers until TTL expiry or revocation. Structurally equivalent to what the agent can already do (talk to the model), minus the permanent credential loss.
- **Relay-VM key visibility (Epic 42 interplay):** irrelevant to the BYO router path — BYO keys never transit Epic 42 VMs. For *future* combinations (BYO key + fleet VM egress), the router→VM protocol keeps a v2 seam for blind-tunnel mode (decision record; not built here).

**What it does NOT address.**
- **#821 egress allowlist** — arbitrary public destinations remain reachable; secrets *other* than provider keys (env-secrets, git credentials, user files) remain exfiltrable until #821 lands. Cross-referenced, not scoped here.
- **Memory scraping of router/controller processes** — the plaintext KEK exists in controller + router memory (as it does in the API server today for the master-KEK paths). Trusted-plane compromise is out of scope (same line design 0051 §3 draws).
- **The user's own secrets/history in uid-1000 space** — theirs by design (design 0051 §3).
- **Side-channel/DoS at the router** (a workspace spamming the router) — bounded by quota/alerts; not a credential threat.

---

## 4. Design

### 4.1 Topology and data flow

```
                     TRUSTED PLANE                          │  UNTRUSTED
                                                           │  (uid 1000)
┌────────────┐  seal (StagingProvider)  ┌────────────────┐  │ ┌──────────────────┐
│ controller │────────────────────────► │ K8s Secret     │  │ │ workspace pod    │
│ (workspace │   per-workspace, per-    │ llm-relay ns   │  │ │ opencode ────────┼─┐
│ reconciler)│   provider envelope      │ (ciphertext)   │  │ │ apiKey = TOKEN   │ │
└─────┬──────┘                          └───────▲────────┘  │ │ baseURL = router│ │
      │ mint (internal API)                     │ informer  │ └──────────────────┘ │
      ▼                                         │ cache     │   token only (D3)    │
┌─────────────────────────┐  resolve per req:   │          │                      ▼
│ llm-relay-router ×2     │◄────────────────────┘          │   POST /w/<ws>/<slug>/v1/…, Bearer TOKEN
│ (llm-relay ns)          │◄──────────────────────────────────────────────────────────┘
│ verify token → scope    │        sanitize (§4.7): strip headers, size cap, model allowlist
│ local AES-GCM unwrap    │        inject REAL key (upstreamAuth precedent, proxy.go:49)
│ (µs; KEK in memory only)│─────────────────────────►  https://ai.thekao.cloud/v1  (upstream)
└─────────────────────────┘
```

Six hops: (1) controller seals each bound provider credential into a per-workspace Secret in `llm-relay`; (2) controller obtains an ephemeral scoped token from the router's internal mint endpoint and stages it into the workspace-namespace Secret consumed by the existing batch machinery; (3) the one builder (US-70.2) emits llm-provider batch entries whose `apiKey` is the **token** and whose `baseURL` is the **router** — agentd materializes them through the unchanged `FormatProviders` → `AgentConfigWriter.Apply` seam; (4) opencode calls the router with `Bearer <token>`; (5) the router validates the token, resolves the envelope from its informer cache via local AES-GCM unwrap, sanitizes the request, injects the real key, forwards upstream; (6) revocation = delete the `llm-relay` Secret → the informer drops the ciphertext (bounded by watch propagation latency, §4.4) → the next resolve fails `CredentialStale`/401 → conditions surface it.

**Coverage (answers the 2026-09-15 triage question on #820):** relay-only covers **every llm-provider credential regardless of owner type** — user, admin, and org bindings all flow through the same batch construction (`decryptBindingWithDEK` switches on `owner_type ∈ {user, admin, org}`, `pkg/secrets/injection.go:482-518`) and are rewritten identically. The triage's alternative — a per-credential policy that *refuses* raw-key injection only when the model is relay-reachable — is rejected (§9): it reintroduces a per-credential bypass decision and leaves S1–S3 live for every credential that misses the check. Uniform server-side routing is the policy.

### 4.2 `pkg/secrets.StagingProvider` — envelope sealing (D2)

New provider alongside the existing KEK machinery (`pkg/secrets/root_key.go:35` `RootKeyProvider`; KMS precedents `kms_aws_provider.go`, `kms_gcp_provider.go`; ciphertext format prefix `lkms:v1:` at `root_key.go:19-32`).

- **Envelope shape:** `ciphertext = AES-256-GCM(providerKey; localKEK)` where `localKEK` is a 32-byte router-scoped key stored **only KMS-wrapped** in the `llm-relay-kek` Secret (`llm-relay` ns). Plaintext `localKEK` exists only in controller and router memory. The wire format carries a version tag (`stg:v1:`), the algorithm, and a plaintext **`keyID`** (the rotation mechanism below depends on it); the `(algorithm, keyID)` tuple is GCM **AAD**, so the discriminator is integrity-bound — an envelope cannot claim a key it wasn't sealed under.
- **Prod (KMS):** controller and router each unwrap `localKEK` once at boot via cloud KMS Decrypt — **KMS is O(boot), never O(request)** (D2). Sealing is local AES-GCM (µs) per credential stage.
- **Dev (HPKE):** no cloud dependency — the router keypair is generated at first boot; the controller seals with the router's public key (HPKE seal), so the controller *never* holds decrypt capability in dev. Wire format is a versioned envelope (`stg:v1:` prefix, algorithm discriminated) so prod/dev are interchangeable per deployment. Pinned specifics (the 2026-09-15 review's finding 2): **library** — Go has no stdlib HPKE and `go.mod` carries no HPKE dependency today (verified: no `circl`/`hpke` in `go.mod`); the candidate is `github.com/cloudflare/circl` (HPKE, RFC 9180), subject to US-72.1's dependency review (govulncheck + Trivy already gate CI; the review must record version pin + license). **Keypair Secret payloads are generation-tagged and self-contained:** `llm-relay-hpke-key` carries `{privateKey, publicKey, generation}` (monotonic counter); `llm-relay-hpke-pub` carries `{publicKey, generation}`. **Key distribution — RBAC-authenticated, not trust-on-first-use, replica-symmetric (the router runs ×2, §4.3):** first boot is **create-or-adopt** — a replica attempts to create `llm-relay-hpke-key` with its generated keypair; on `AlreadyExists` it reads and adopts the existing key (both replicas converge on one keypair; no leader election, the API server's create semantics are the serializer) and runs the assert. The private key lives in that Secret under the §4.3 name-scoped write carve-out; the public key is published to `llm-relay-hpke-pub`, which the controller reads. **Every replica loads keys from the Secrets via its watch and re-runs the assert on every watch-delivered private-key load** — no replica has privately-generated state that survives as authoritative, and keypair corruption is detected without waiting for a restart. **The assert is strictly self-contained** — derive the public key from the loaded private key and compare against the *co-located* `publicKey` copy in `llm-relay-hpke-key`, plus generation monotonicity (**non-strict `≥`** — informer re-list redelivers the same object; strict `>` would misfire on redelivery). It **never compares across Secrets**: the pub Secret is consumed solely by the controller (resolve uses the private key only), so the deliberate private-then-pub torn window of a routine rotation (below) can never read as corruption to a peer. A torn-but-genuine rotation is distinguishable from corruption *by generation*: the rotate call returns the new generation, and the controller validates the pub payload's generation against that return at seal time — a pub read whose generation ≠ the rotate response is rejected, never sealed against. **Pub-Secret corruption residual (stated, with its terminating trigger):** shape-invalid pub bytes fail seal-time parsing; shape-*valid* but wrong pub bytes are undetectable at seal time by construction and surface as `CredentialStale` on the first failed resolve. **The reconcile-time predicate that bounds every DR/residual window:** at each pass the controller re-gets `llm-relay-hpke-pub` and compares its `generation` against the last-sealed generation (persisted with its staging state) — changed → re-seal every envelope; unchanged while `CredentialStale` persists **and the staleness is corruption/wrong-pub-class** — the one cause rotation can repair — **under an intact lineage**: the workspace's envelope Secrets are present AND the child has spawned with the staged revision (the conjunct reads the terminal, spawn-layer verification — `spawned_rev`-class, self-computed at what the agent actually spawned with — NOT the batch-apply anchor, so the #852 deferral window, where the fresh token is applied but the restart waits behind busy sessions, reads as pending-delivery and never escalates) → escalate to a rotate request, anti-storm-bounded (controller-initiated rotations spaced ≥ the prior-key retention default, 10m). **Escalation never fires for any other `CredentialStale` cause:** revocation-class (envelope deleted — D2's designed terminal state), delivery-class (batch not applying / restart deferred — a fault or wait rotation cannot repair, owned by reconcile and #852 respectively), or token-expiry-class (renewal owns it; rotation re-seals envelopes and never mints tokens). Rotating on a non-repairable cause would be cluster-wide churn paid by every workspace for nothing. This is the wire-up that makes "bounded, fail-closed window" terminating, not merely promised. **Rotation (controller-driven, window bounded, not zero):** the reconciler calls `POST /internal/v1/keys/rotate` on the internal API (allocated alongside mint, §4.4); the receiving replica generates the new keypair, updates the **same two Secret names in place** (carve-out intact; the update is **generation-preconditioned** — current generation == the generation the receiving replica has loaded, so a concurrent rotate/DR attempt loses the private-key write and aborts before pub, exactly as the DR serialization below describes), and both replicas' watches load the new private key, each retaining its previously-loaded key as *prior*. Envelopes therefore resolve under either key on every replica after at most the **watch-propagation bound** — the same bound K5 pins for revocation, test-pinned for rotation too; the "no unresolvable window" property is bounded-by-watch, not absolute, and stated as such. **Stated failure modes of that bound:** (i) a replica that *restarts* between the in-place Secret update and re-seal completion loads only the new key (the prior key is memory-only) — old-`keyID` envelopes then fail on that replica until re-seal completes; fail-closed, self-healing, and exactly the rolling-deploy-overlapping-rotation window; (ii) if re-seal outlives the retention interval (very large fleet, controller outage mid-re-seal), prior keys drop while old-`keyID` envelopes remain — fail-closed as `CredentialStale` until reconcile completes the re-seal; the default 10m is sized ≫ watch bound + re-seal SLO and the violation *behavior* is stated rather than prevented. **Update ordering (torn-write pin):** rotation updates the private-key Secret first, then the pub Secret; the controller reads the pub only after the rotate call returns — a mid-update read can never confirm a torn rotation. The controller re-seals every staged envelope against the new public key and confirms completion **by metadata alone from its own completed writes** — the controller is the sole writer of the envelope Secrets, so write-acks suffice (every envelope's `keyID` equals the new key id); no decrypt, no resolve probe, no read-back, no capability change. **Prior-key retention is time-bounded and replica-local-uniform** (no cross-replica ack protocol): each replica drops its prior key a fixed interval after observing the new one (default 10m ≫ watch bound + re-seal SLO; deployment-tunable). **Disaster recovery** (keypair Secret lost or corrupted): *lost* — surviving replicas and the next boot fall back to create-or-adopt regeneration. *Corrupted* — the self-contained assert (boot or watch-time) fails on the private-key Secret, and the replica performs a **self-issued rotate**: the same generation-preconditioned two-Secret update an operator-driven rotation uses, with the precondition "current generation == the generation I observed failing". This serializes concurrent recoveries on a single lineage: the private-key Secret's update is the gate — **only the replica whose private-key update wins (optimistic-concurrency/resourceVersion) writes the pub Secret**; the loser's precondition fails, it aborts before writing pub, and its next watch delivery re-adopts the winner's self-consistent payload (assert passes). A split keypair across the two Secrets is therefore unreachable by construction, and no first-boot-only serializer is relied upon (adopt and the DR path share the same recovery tail). Either way, envelopes sealed to the dead key are unresolvable until reconcile re-seals them — an honest, bounded, fail-closed window surfaced as `CredentialStale`, distinct from routine rotation.
- **No long-lived plaintext cache:** the router decrypts per request and discards; the informer cache holds ciphertext only (D2). Revocation = Secret deletion (D2) — tokens remain HMAC-valid but resolve fails closed (§4.4).

### 4.3 `llm-relay` namespace + the 2-replica router

- **Dedicated namespace** `llm-relay` (Helm-created). The router Deployment (2 replicas, PDB `maxUnavailable: 1`) and Service `llm-relay-router` live there; the `llm-relay-kek` and staged provider Secrets live there; workspace pods get exactly one egress carve-out (NetworkPolicy: DNS + `llm-relay-router` FQDN, mirroring the existing `relay-router-networkpolicy.yaml` namespaceSelector pattern). RBAC: the router's ServiceAccount may `get/list/watch` Secrets in `llm-relay`, plus a **name-scoped write carve-out for exactly the two HPKE keypair Secrets** (`llm-relay-hpke-key`, `llm-relay-hpke-pub` — §4.2's bootstrap needs it, and nothing else); the controller may create/update/delete **all other `llm-relay` Secrets** (the staged envelopes, `llm-relay-kek`) and **`get` on `llm-relay-hpke-pub`** (it reads the public key to seal; it never reads the private-key Secret).
- **New binary role, existing machinery:** extend `cmd/relay-router` with a **BYO resolve mode** (separate listener/port or separate Deployment from the Epic 42 fleet router — implementation story decides; the fleet router stays as-is). Reused as-is: hop-by-hop header stripping (`proxy.go:66-75` `routerHopHeaders`), router-side key injection (`applyUpstreamAuth`, `proxy.go:40-56` — the `controller.inferenceRelay.upstreamAuth.keySecret` precedent generalized from one fleet key to per-workspace resolve), stream handling, 429-storm detection and weighted selection are *not* needed for BYO (single upstream per token) and stay fleet-only.
- **#1078-style stream drain:** `terminationGracePeriodSeconds` sized to the longest legitimate stream (a cap, not a delay — the #1078 property), in-flight streams complete before exit, preStop marks the pod not-ready. Rolling deploys never kill a live turn. Two replicas make the drain non-disruptive (one drains, one serves).

### 4.4 Ephemeral scoped tokens (D3)

- **Minted by the router** (`POST /internal/v1/tokens`, authenticated by a controller-held Secret — the API/controller are the only mint callers). The router is the validator; making it the minter keeps exactly one holder of the signing key. The internal API also allocates `POST /internal/v1/keys/rotate` (§4.2).
- **Format:** opaque `lrt_<base64url(payload)>_<HMAC-SHA256>`, payload = `{workspaceID, providerSlug, baseURL, modelAllowlist, iat, exp, keyID}`. No encryption needed — nothing secret in the payload; the HMAC is the integrity bound.
- **Scope enforcement per request (all three, all mandatory):** workspace (token↔staged-Secret binding), baseURL (the token names exactly one upstream — SSRF-proof by construction, same pinning property as the withdrawn broker draft), model allowlist (router parses the request's `model` field against the allowlist; the allowlist already exists per-binding at `pkg/secrets/credential_store.go:50` and is already enforced batch-side at `injection.go:525+` — the router re-enforces it, defense-in-depth).
- **Validity = HMAC ∧ staged-Secret-present ∧ not-expired.** The staged-Secret conjunct is what makes revocation = Secret deletion (D2): an HMAC-valid token without ciphertext is a 401 with reason `credential_stale`. Revocation takes effect **bounded by informer watch propagation** (typically seconds; the US-72.2 test pins an upper bound) — not instantly, and `exp` evaluation tolerates clock skew (skew bound pinned by test; router replicas are the only validators, so the skew domain is intra-cluster NTP).
- **TTL & renewal:** TTL is deployment-tunable (default 24h). Token expiry participates in the batch manifest tier (US-70.2): the manifest hash changes at ~TTL/2, the existing conditional-pull/resync machinery delivers a fresh token, the ConfigWriter seam applies it, and the session-aware restart decision (#852) defers the opencode restart behind busy sessions — all existing machinery, no new delivery path.
- **agentd emits, never mints or decrypts** (D3). In single-container mode agentd is uid 1000: an attacker can read the token out of the config — accepted (§3, token theft = use, not disclosure). No signing key, no KEK, no DEK ever enters uid-1000-reachable memory, in either container mode.

### 4.5 agentd: token-only emission through the `AgentConfigWriter` seam

The batch's llm-provider entries arrive with `apiKey = token`, `baseURL = http://llm-relay-router.llm-relay.svc.cluster.local/w/<workspaceID>/<providerSlug>/v1` (router FQDN + routing path; scheme/port per chart — the path terminates in `/v1` so OpenAI-compatible clients append `/chat/completions` and friends, the shape `@ai-sdk/openai-compatible` induces whenever `baseURL != ""`, `format.go:74-83`). Everything downstream is the existing path: `Materializer.FormatProviders` (`pkg/agentd/secrets/secrets.go:965`) → `FlushProviders` → boot merge into auth store (`secrets.go:601-612`) / `StageCredentials` on reload (`client.go:191`) — **no formatter change needed**; the token is opaque to the seam, which is precisely why the seam exists (Rule 12). Model enrichment (`EnrichProviders`) hits the router's `/models`, which the router serves from the staged credential's `Models` list (or a cached upstream fetch, server-side) — so custom-endpoint model discovery keeps working and stays key-free pod-side.

agentd-side work is real but narrow (US-72.4): relay-only liveness (the #910 re-arm applied to the router reachability check — boot-time router failure must degrade *loudly* and re-arm, never strand silently), healthz/readyz degrade codes (`relay_unreachable`, `token_expired`) surfaced via the existing statusz → conditions path, and the e2e sweep instrumentation. `shouldSkipRelay` (`relay_injector.go:98-131`) is untouched — it governs the *free-tier Zen* relay, orthogonal to BYO relay-through.

### 4.6 Workspace conditions

Three new condition types on the Workspace CRD (alongside `CredentialsAvailable` / `CredentialsApplyPending`, `workspace_types.go:264-294`):

| Condition | Meaning | Set by |
|---|---|---|
| `CredentialsStaged` | envelope + token staged for every bound BYO provider; carries the staged revision | controller reconciler |
| `CredentialStale` | token expired / staged Secret absent / batch not applying the staged revision / corruption-class resolve failure (wrong or corrupt keypair lineage — §4.2's one escalating cause) (resolve fails closed) | controller, from reconcile + agentd degrade codes |
| `CredentialRejected` | router explicitly rejected a token (bad scope, sanitization refusal) — an operator-visible anomaly signal | controller, from router rejection telemetry |

Surface follows the US-70.1 precedent (degrade codes healthz → CRD, the `SecretsDelivery` path at `workspace_types.go:439`).

### 4.7 Mandatory router request sanitization

Per request, before upstream forward: (1) **Authorization replaced** with the resolved real key (`applyUpstreamAuth` semantics — client `Bearer` values never forwarded); (2) **hop-by-hop + identity headers stripped** (extend `routerHopHeaders`, `proxy.go:66-75`: workspace IDs, platform headers, client-supplied `X-Relay-*`); (3) **request body size cap** (deployment-tunable, default 10 MiB) and response size cap; (4) **model allowlist check** (§4.4); (5) **method/path allowlist** (chat-completions-class endpoints + `/models` only — the router is not a general proxy); (6) **quota + alerts**: per-workspace request rate/byte counters with configurable caps and Prometheus alerts — the bound on the exfil-through-relay residual (§3). Violations are `CredentialRejected`-class events, never silent.

**Persistence posture (invariant — the router is a plaintext-bearing trusted component):** the router logs and persists **metadata only** — workspace ID, provider slug, token keyID, status codes, latencies, byte counts, rejection reasons. Request/response **bodies are never logged, sampled, traced, or buffered to disk** at any verbosity; error diagnostics that could carry body-adjacent content must pass `pkg/redact` or are forbidden. *(Amended 2026-09-17, §4.9: "pass `pkg/redact`" means the combined static + dynamic staged-key pipeline — a resolved key echoing back through a diagnostic or proxied payload is redacted by its own registered dynamic rule.)* This is the design-0027 proxy-as-trust-boundary principle (`pkg/redact` exists for exactly this class) made an enforceable invariant (epic-72 K7), so US-72.2 cannot improvise it.

### 4.8 Sequencing: #910 first (load-bearing)

With relay-only as the default there is **no raw-key fallback**: a boot-time relay fetch failure that used to mean "fall back to direct" now means "no LLM path at all." #910 (relay injector re-arm on terminal fetch failure: bounded backoff 5m→30m, no busy sessions, `HasRelay()` re-check, per-outcome metric) is the machinery that converts a transient router outage from a pod-lifetime outage into a bounded retry. **Epic 72 gates the default flip on #910 being merged** (US-72.0), and generalizes its re-arm to the BYO router reachability check (US-72.4). Until then the flip stays off and raw-key delivery remains the (documented, condition-surfaced) fallback.

### 4.9 Addendum (2026-09-17, owner direction): dynamic staged-key redaction at the payload seam

Redaction (payload hygiene) and staging (credential delivery) are separate mechanisms that interface at exactly one point: the staging provider is the one place the platform legitimately knows a staged key's plaintext — at seal time (controller) and resolve time (router). **Each staged key therefore registers itself as a DYNAMIC exact-value redaction rule** in the `pkg/redact` engine, in addition to the static 16-rule regex pipeline:

- **Surface (shipped with US-72.1):** `pkg/redact` gains `RegisterDynamic(rules ...DynamicRule)` / `UnregisterDynamic(id)` — exact-value (non-regex, byte-exact) rules grouped by ID; re-registration replaces the group; **dynamic rules run BEFORE the static pipeline** so a static pattern cannot fragment a secret before the exact match removes it atomically. `pkg/secrets` exposes the `StagedKeyRedactor` seam, the `RedactStagedKeys` adapter over `*redact.Redactor`, and `StagedKeyRedactionID(envelope)` — the envelope-derived rule-group ID both sides compute identically. Registration covers the plausible echo encodings of the material (raw, standard base64, URL-safe base64), so a key smuggled base64-wrapped through a payload is caught even when no static pattern matches its shape.
- **Lifecycle:** register at seal and at resolve; unregister on revocation — and revocation is Secret deletion (D2, unchanged), with the informer's DeletedFunc deriving the same envelope ID to unregister. A registration failure fails the seal/resolve loudly — a staged key that escaped the redaction engine is never delivered.
- **Consumption (US-72.2):** the router's mandatory sanitization stage (§4.7) applies the **combined static + dynamic pipeline** to proxied traffic, so a resolved key can never be echoed back through the relay. US-72.1 ships the surface plus the seal/resolve-side wiring and its tests; the router-side application lands with US-72.2.
- **`pkg/redact` home:** stays in `pkg/`. #842 once planned relocating it into `cmd/redact`; that binary has since been folded into workspace-agentd's `redact` subcommand (#1152, design 0053 S2 / #1116), and with the staging provider as a second genuine consumer the package is shared platform machinery, not a CLI internal.

---

## 5. Stage 0 disposition: EXCLUDED

The 2026-08-27 staging comment allows a <50-line bar-raiser (0400 perms + opencode deny rules for `/sandbox-cfg/*` + `/sandbox-runtime/*`) while noting the earlier verification rejected it as a boundary. **This design excludes Stage 0**, on re-verified technical grounds:

1. **It is not a boundary** — re-confirmed: every pod process runs uid 1000 in single-container mode (`pod_builder.go:605`); the agent reads its own uid's files regardless of mode bits, and opencode permission rules gate *tool invocations*, not arbitrary code (SEC-6/#825's policy-only gap; a Python one-liner opens the file directly).
2. **The 0400 arithmetic is broken in both directions.** In single-container mode, 0400 on uid-1000-owned files changes nothing (same uid). In sidecar mode, the ConfigWriter (uid 2000) writes `agent-config.json` 0640 for the shared-gid reader (opencode, uid 1000 + gid 1000) — 0400 would *break opencode's read* and wedge every provider. There is no mode value that raises the bar without breaking the reader.
3. **It would be dead config within the same epic.** Post-flip, S1/S2 carry tokens and S3 is token-bearing too; the deny rules would guard files that no longer contain keys, at the cost of permanent test surface and a false sense of a boundary.

**Consequence:** the bar-raiser the staging comment conditioned on "<50 lines" is declined; the exposure is closed by the flip itself (Epic 72's shortest path). Flagged for the owner in the PR body — this is a delegated decision, not a fixed one.

---

## 6. Migration

1. **Mechanism reuse, no new delivery path:** tokens ride the existing batch → materialize → ConfigWriter → resync machinery (Epic 70); the staged envelope rides controller reconcile. Mixed fleet is the W15 precedent: pre-flip pods keep receiving raw-key batches (legacy path); post-flip pods receive token batches. The builder picks per deployment policy flag, not per pod.
2. **Default flip gates (US-72.5):** #910 merged; router resolve path green on SLO (p99 resolve latency, error rate); `CredentialsStaged` observed True on a canary workspace set; sanitization + quota/alerts verified; rollback drill executed.
3. **PVC scrub (US-72.6):** pre-US-35.7 workspaces may carry a *real file* (not symlink) at `/workspace/.local/opencode/auth.json` with plaintext keys on the PVC; opencode's own auth subsystem may also have persisted keys through the XDG path historically. On reconcile/resume post-flip, the controller schedules a one-shot scrub (agentd exec of a narrow scrub subcommand) that removes `key` material from the legacy platform-shaped files at known paths only — `/workspace/.local/opencode/auth.json` (regular file), and `agent-config.json` copies if found under `/workspace/.local`. Scoped strictly to platform-written shapes; user-authored files are never touched (design 0051 §3). Scrub outcome is an event + condition, idempotent.
4. **Rollback:** flip the policy flag back. Staged envelopes and tokens are inert without the flag (raw-key batches resume); the router deployment can remain (no traffic). Existing pods heal on next batch apply. Rollback is exercised as part of the canary (flip on → validate → flip off → validate, the D6.1 pattern).

---

## 7. Test strategy (cross-cutting inventory; per-story TDD in Epic 72)

| Suite | Proves | Type |
|---|---|---|
| `staging_roundtrip_kms_hpke`, `kek_never_in_secret`, `ciphertext_only_informer` | D2 envelope: seal→resolve roundtrip; plaintext KEK only in memory | unit + integration |
| `firstboot_two_replica_adopt` (simultaneous cold start → one adopted keypair, fingerprint match), `rotation_dual_key_window_bounded` (both replicas resolve old+new keys across re-seal; window ≤ watch bound), `rotation_replica_restart_mid_reseal` (old-`keyID` fails on the restarted replica only, until re-seal completes), `reseal_completes_before_retention_expiry` (default settings; overrun degrades to `CredentialStale`, never silently), `rotation_torn_update_unconfirmable` (private-then-pub ordering; a pre-return pub read never confirms), `rotation_assert_quiesced_in_torn_window` (peer private-key load during the torn window never fires DR — self-contained, generation-tagged assert), `pub_sealtime_generation_validated` (pub generation ≠ rotate response → rejected at seal time; shape-valid wrong-pub residual surfaces as `CredentialStale`), `prior_key_retention_expiry` (uniform drop after the interval), `dr_keypair_loss_failclosed_recovery` (Secret loss *and* corruption paths → `CredentialStale` window → re-seal recovery; corruption detected by the **watch-time** assert with **no restart** — a running fleet converges on the bounded recovery), `dr_dual_replica_recovery_single_keypair` (simultaneous assert failures on both replicas → generation-preconditioned single lineage; the pub-write loser adopts the winner, no split), `dr_window_reconcile_terminates` (pub-generation change → re-seal; corruption/wrong-pub-class stale with intact spawn-layer lineage and unchanged generation → anti-storm-bounded rotate escalation; escalation SUPPRESSED for revocation-class (envelope deleted), delivery-class (batch not applying OR #852-deferred restart), and token-expiry-class staleness), `envelope_keyid_aad_bound` (tampered `keyID` fails GCM auth) | §4.2 replica-symmetric bootstrap/rotation/DR incl. stated failure modes; metadata-only confirmation | integration, fault-injection |
| `resolve_is_local_aesgcm` (benchmark: KMS call count == 0 at steady state) | KMS O(boot), never O(request) | benchmark pin |
| `revocation_secret_delete_401` | D2 revocation: HMAC-valid token + deleted Secret → `credential_stale` 401, condition flips | integration |
| `token_scope_matrix` (wrong workspace / wrong baseURL / off-allowlist model / expired / forged HMAC) | D3 scope enforcement, all conjuncts | unit + table-driven |
| `router_sanitization_suite` (header strip incl. client Authorization, size caps, method/path allowlist, quota alerts) | §4.7 mandatory sanitization | unit + integration |
| `deploy_drain_two_replica` (rolling update mid-stream: stream completes, zero resets) | #1078 pattern at the router | e2e |
| `boot_relay_outage_rearm` (router down at boot → loud degrade + bounded re-arm; no strand) | §4.8 / #910 generalization | fault-injection e2e |
| `agent_readable_files_keyfree` — the rogue-agent sweep: exec in a live post-flip pod, cat/grep/ptrace-sweep every uid-1000-readable path + `/proc/*/environ` for provider-key bytes (planted canary key) | **Epic exit criterion: zero key bytes in pod** (#820 close-out) | e2e (`local/us-72-rogue-agent-sweep.sh`, nightly-wired) |
| `pvc_scrub_legacy_authjson` (real-file legacy auth.json → scrubbed, event emitted; user files untouched) | §6.3 | integration |
| `mixed_fleet_batches` (pre-flip pod legacy batch, post-flip pod token batch, same deployment) | §6.1 W15 pin | integration |
| `rollback_drill_relay_only` | §6.4 | e2e drill |

---

## 8. Assumptions (validated — Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | The exposure surfaces are S1–S3 as tabulated (§1.2), incl. US-70.5's demolition of the reload cache | read on main @ `17a2f288`; file:line in §1.2 |
| A2 | Single-container mode is the fleet default today | `helm/values.yaml:376-377` `agentdSidecar.enabled: false` (flip pending its own L3 gate) |
| A3 | The AgentConfigWriter seam passes provider entries through verbatim (`Formatted []byte`), so token-bearing entries need no seam change | `pkg/agent/agentconfig.go:85-147`; `format.go:71` renders whatever `APIKey` carries |
| A4 | Router-side key injection is an established pattern | `cmd/relay-router/proxy.go:40-56` + `controller.inferenceRelay.upstreamAuth.keySecret` (README-LLM §Inference Relay Fleet) |
| A5 | A per-binding model allowlist already exists and is enforced batch-side | `pkg/secrets/credential_store.go:50`; `pkg/secrets/injection.go:525+` (`applyModelAllowlist`) |
| A6 | Token renewal can ride the existing revision/conditional-pull machinery | US-70.2 manifest tier + US-70.3 resync loop (README-LLM §Relay Config Subsystem; design 0057 R1) |
| A7 | Cloud KMS providers exist as precedent for the envelope | `pkg/secrets/kms_aws_provider.go`, `kms_gcp_provider.go`; `RootKeyProvider` at `root_key.go:35` |
| A8 | Workspace pods can be granted narrow cross-namespace egress to a platform service | `relay-router-networkpolicy.yaml` namespaceSelector pattern + `controller.inferenceRelay.workspaceRouterURL` FQDN derivation (README-LLM §Inference Relay Fleet) |
| A9 | #910's re-arm shape (backoff, session gating, `HasRelay` re-check) is implementable without SSE disruption | #910 issue body proposes exactly this; #852 session-aware restart machinery exists |
| A10 | The router can serve `/models` for enrichment without the real key leaving the trusted plane | staged `LLMProviderData.Models` (`types.go:245`) is already the batch-side catalog source; enricher is optional per provider (`EnrichProviders`, `secrets.go:575-581`) |

---

## 9. Rejected alternatives

| Alternative | Rejected because |
|---|---|
| **In-pod key broker** (the withdrawn 2026-08-14 draft): agentd holds keys in memory behind a localhost proxy | Superseded by the decision record: decrypt-in-pod makes agentd a loopback decrypt oracle next to the attacker (D3); the broker also kept plaintext keys in uid-1000-reachable *memory* (ptrace) — relay-through removes them from the pod entirely. The draft's SSRF-proof destination pinning and per-pod-token ideas survive, relocated server-side (§4.4). |
| **Raw-key delivery behind per-workspace opt-in** (`spec.llmKeyDelivery: direct`) | D1 chose relay-through as the default. Opt-in remains technically available as the rollback mode (§6.4) — it is the flip mechanism, not a supported steady state. |
| **Stage 0 bar-raiser** (0400 + deny rules) | §5 — not a boundary; mode arithmetic broken; dead config post-flip. |
| **Per-credential conditional refusal** (refuse raw-key injection only when the model is relay-reachable — the 2026-09-15 triage alternative) | §4.1 — a per-credential bypass decision leaves S1–S3 live for every credential that misses the check (new provider kinds, misclassified reachability); uniform server-side routing has no missed-check class. |
| **Decrypt in agentd with a scoped DEK** (decrypt only the provider key, not all secrets) | Requires shipping a unwrap capability into uid-1000-adjacent space in single-container mode; still a loopback oracle (D3); strictly dominated by server-side resolve. |
| **Extending the Epic 42 fleet VMs to proxy BYO keys** | Fleet VMs are third-cloud VMs for free-tier Zen IP rotation; routing customer keys through them widens the relay-VM key visibility residual from "operator" to "cloud VM fleet". The dedicated `llm-relay` in-cluster router keeps BYO keys inside the operator's trust boundary. |
| **Per-workspace rate limits at the router as a v2** | The exfil-through-relay bound (§3/§4.7) is load-bearing for accepting D1 — shipping the channel unbounded would silently weaken the recorded trade. Quota/size/alerts ship with the flip (US-72.3). |

---

## 10. Open items (owner questions, non-blocking for the design)

1. **First-party (no-`baseURL`) providers:** the token design covers them by pointing at the Zen upstream (same host the fleet uses). If the owner prefers, v1 can restrict relay-only to `baseURL != ""` providers and leave first-party keys raw (the withdrawn draft's restriction) — **this design assumes full coverage** (uniform threat model); flag if you want the narrower v1.
2. **Token TTL default (24h proposed)** and quota defaults (10 MiB request cap; per-workspace req/min) are deployment-tunable first guesses — operator review requested at US-72.3.
3. **Self-hosted single-node deploys without cloud KMS:** HPKE dev mode doubles as the no-KMS production mode. In that configuration the HPKE keypair Secret's confidentiality becomes the root of trust for staged envelopes — **the same threat class as the master-KEK file mount** (US-50.1: a projected read-only Secret in the trusted plane), not a weaker one: both reduce to "an operator-plane Secret protects operator-plane keys." Rotation is reconcile-driven (§4.2). Confirm acceptable.
4. **Epic numbering:** the decision record's draft folder name `epic-68-relay-only-key-delivery` collides with the shipped `epic-68-chat-file-attachments` (itself renumbered 67→68 at closure). This design uses **Epic 72** and renames the draft's US-68.x → US-72.x. The repo's epic-number hygiene (two 0052 docs, two epic-55/64 folders on disk) is noted for the already-filed hygiene follow-up.
