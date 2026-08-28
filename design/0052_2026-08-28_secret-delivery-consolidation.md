# 0052 — Secret delivery consolidation: one builder, pull-based materialization

**Status:** Draft v1 (design review; holds, never auto-merges)
**Date:** 2026-08-28
**Issue:** #1087 (primary), follow-ups #1104 and the 2026-08-28 v0.25.1 incident below
**Supersedes:** nothing shipped. Retires mechanics from Epic 35 (secretless injection), Epic 56 (durable jwt_sessions), design 0045 (boot-time user-DEK delivery), #443 (reload cache), worklog 0591 (auto-push) — each cited where demolished.
**Author:** agent session 2026-08-28; architecture settled with the operator after the v0.25.1 forensic round (evidence runbook: paste.thekao.cloud/mike/851c4ae583cc498883877cde915334cc)

---

## 1. Decision record (the policy this design executes)

> **If the workspace is live, its secrets exist in it. No user-presence gate.**

Long-running autonomous agentic runs are a first-class workload: a workspace may
run for days while its owner is logged out. The historical gate — "user-DEK
content delivers only when an authenticated session exists" — was a
**cryptographic necessity** of the password-DEK era (the server genuinely could
not decrypt user secrets without the user). The server-KEK migration (SSO/passkey,
`dek_source=server_kek`) deleted that necessity: every user DEK is now
server-recoverable at rest via the master RootKeyProvider. What remained was an
ACL defending a constraint that no longer existed — and, on 2026-08-28, a
liability (§2).

The boundary that DOES remain, unchanged: workspace identity (SA token +
TokenReview on pod-bootstrap; workspace password on agentd muxes), in-pod uid
separation (design 0051), tmpfs-only plaintext (US-35.7), and user-DEK
encryption at rest. This design changes **delivery**, not **custody**.

## 2. The forcing incident (2026-08-28, v0.25.1)

Two consecutive workspace resumes returned the 1287-byte sessionless batch —
no ssh key, no `GH_TOKEN`. Root cause (audit-verified, see runbook):

- The owner's `user_keys.wrapped_dek` row was a June-28-era 60-byte
  un-prefixed wrap the US-57.1 re-wrap window never touched (last login
  predated it; the migration was login-gated).
- v0.25.1's `GetDEKServerSide` (#1104) correctly attempted the master-provider
  unwrap, failed, and **silently** degraded to sessionless — by design.
- Meanwhile bind/rebind delivery kept working via the warm `dek:<jti>` session
  cache (set at login, 30-day TTL) — a second DEK source that never touches
  the bad row. The rebind-after-every-resume loop was the visible signature.
- Diagnosis required in-cluster forensics across API logs, `secret_audit_log`,
  and Redis key TTLs, because the degrade path emits nothing.

Three lessons, all architectural:

1. **Two DEK sources that can disagree** (session cache vs `user_keys` unwrap)
   produce behavior that looks contradictory (rebind works, resume doesn't).
2. **Silent degrade** turns a five-minute fix into a forensic exercise.
3. **Login-gated migration** cannot converge rows whose owners never come
   back; only a login-independent reconciler can.

## 3. Verified current state (main @ 55705b8, v0.25.1)

### 3.1 Batch builders (three)

| Builder | Entry points | DEK source |
|---|---|---|
| `InjectSecrets` | bind push (`pushSecretsToAgent`), reload-secrets endpoint | session: `GetDEK(jti)` cache/rehydrate |
| `InjectSessionlessSecrets` | API-key SDK surface; degrade fallback | none — user entries audited-and-skipped |
| `InjectSecretsForPodBootstrap` | pod-bootstrap (#1104) | `GetDEKServerSide` → master provider over `user_keys` |

### 3.2 DEK sources (five)

| # | Source | Era | Verdict |
|---|---|---|---|
| K1 | Redis session-cache hit (`GetDEK(jti)`) | password-DEK | keep as builder-internal perf shortcut only |
| K2 | `rehydrateDEKFromJWTSession` (durable jwt_sessions row + signing key) | Epic 56 | **demolish** — obsolete post-migration |
| K3 | `GetDEKForUser` live-session walk | worklog 0590 | **demolish** — obsolete post-migration |
| K4 | soft-unlock at login (`UnlockDEKWithSigningKey` durable write) | Epic 56 | **demolish the durable-write half**; login keeps caching (K1) |
| K5 | `GetDEKServerSide` (`user_keys` + master provider) | #1104 | **the** background source |

### 3.3 Delivery paths (five)

| # | Path | Purpose | Verdict |
|---|---|---|---|
| D1 | Live push of batch body on bind/rebind | update running pods | **becomes notify** (pod re-pulls) |
| D2 | Pod-bootstrap pull at boot/resume | initial materialization | **the** delivery path |
| D3 | Sessionless degrade | never-block-boot invariant | keep, as a LOUD error state |
| D4 | Replay cache (`last-reload-secrets.json`, #443) | survive container restart | **demolish** — pull replaces replay |
| D5 | `secretautopush` + `UserCredsPresent` heuristic | heal degraded boots | **demolish** — loud degrade + retry-on-notify replaces guessing |

### 3.4 Known-broken state this explains

- `dek_source` labels lie (mike's row: `server_kek`, unwrappable-by-current).
- `UserCredsPresent` is defined as "reload cache non-empty" (types.go:86-94) —
  a heuristic that design 0045 Change 3 already poisoned once (the
  `skipped_ucp_true` deadlock, closed by #1104) and that dies with D5.
- The multi-version provider window (W1: `{1: dek-cache, 2: master-kek}`)
  exists solely for un-migrated rows; it retires when the reconciler converges
  them (§5 Phase 1).

## 4. Target architecture

**One builder. Two delivery triggers. One DEK source. Loud failure.**

```
                    ┌────────────────────────────────────────────┐
                    │  API: BuildWorkspaceBatch(workspaceID)      │
                    │  - org/admin: server-KEK providers          │
                    │  - user: GetDEKServerSide (K5)              │
                    │  - emits batch + revision hash              │
                    └───────▲────────────────────▲───────────────┘
                            │ pull              │ notify → pull
              boot/resume   │                   │ bind/unbind/rotate
            (SA + TokenReview)              (agentd re-pulls with SA token)
                            │                   │
                 ┌──────────┴───────────────────┴─┐
                 │ agentd materialize → tmpfs     │
                 │ stamps revision; retries fails │
                 └────────────────────────────────┘
```

1. **One batch builder.** `BuildWorkspaceBatch(workspaceID)` is the only code
   that constructs a secrets batch. Session identity leaves the builder
   signature — request auth decides *whether the caller may invoke it*, never
   *what decrypts*. K1 (session-cache DEK hit) may live inside the builder as
   a cache layer; K2–K4 are deleted. `InjectSessionlessSecrets` collapses
   into "the builder, minus user entries, plus a loud error" — the degrade
   remains (Epic 35 invariant: never block pod boot) but is one code path
   with a reason, not a parallel builder.
2. **Pull at boot** — pod-bootstrap as today (D2), now the sole initial
   materializer.
3. **Notify on change** — bind/rebind/rotate do not push batch bodies; they
   bump a server-side revision and notify the pod's agentd (existing
   authenticated channel), which re-pulls. D1-as-push, D4 cache, and D5
   autopush die together: a pod that can always re-pull from the source of
   truth needs no replay cache and no presence heuristics.
4. **Revision stamps.** Every build carries a content hash; agentd stamps the
   materialized revision (observable in status/healthz). "Is the pod current?"
   becomes a comparison, replacing `UserCredsPresent`'s cache-file existence
   check.
5. **Re-wrap reconciler** (login-independent): startup + periodic walk of
   `user_keys`; unwrap each row with the current provider; on failure, attempt
   recovery from any available source (session cache K1, jwt_sessions K2
   while it still exists), re-wrap at the active version, audit the heal;
   rows that recover from nothing are surfaced as alerts with per-user
   "key unwrappable" state — never silently labeled.

## 5. Sequencing (each phase independently shippable)

**Phase 1 — convergence + observability** (unblocks everything; heals the
incident class immediately)
- Re-wrap reconciler as above.
- Degrade observability: `InjectSecretsForPodBootstrap` (and the collapsed
  builder later) audits + error-logs every non-"owner-has-no-secrets"
  degrade with the underlying error (the 2026-08-28 lesson).
- Acceptance: mike-class row converges without login; a forced unwrap
  failure produces an audit row naming the workspace and cause; full suite
  + e2e green.

**Phase 2 — builder collapse**
- `BuildWorkspaceBatch` with the three call sites migrated (bind push,
  reload endpoint, pod-bootstrap). No behavior change beyond: user entries
  present in ALL batches for live workspaces (policy §1).
- Acceptance: one builder in the tree; session identity absent from batch
  construction; e2e resume/bind/bootstrap all deliver identical batches.

**Phase 3 — notify-pull**
- Bind/unbind/rotate → revision bump + agentd notify; agentd re-pull via SA
  token. Delete `last-reload-secrets.json` and the reload handoff machinery
  it supported.
- Acceptance: rebind round-trip with no push payload; container restart
  re-materializes by pull (regression: the #443 scenario, now by design).

**Phase 4 — demolition**
- Delete K2 (`rehydrateDEKFromJWTSession`), K3 (`GetDEKForUser` walk — after
  Phase 1 it has no callers), K4 durable-write half, D5 (`secretautopush`
  service + watcher callback), `UserCredsPresent` CRD field + controller
  scrape + `hasUserCreds`; retire W1 multi-version window once `user_keys`
  v1 rows read zero.
- Agentd gains the revision stamp (Phase 2/3 groundwork) surfaced in healthz.
- Acceptance: grep-clean for removed symbols; kind suspend/resume gate
  (bind env-secret → suspend → resume → var present, no session, no
  re-injection — the original #1087 acceptance, now by construction).

## 6. What deliberately does NOT change

- User-DEK encryption at rest; server-KEK wrapping of DEKs.
- US-35.7: plaintext only on tmpfs, wiped on pod death.
- Epic 35 invariant: bootstrap/pod-boot never blocks on secret failure.
- Trust models: TokenReview + SA-name verification (pod-bootstrap), workspace
  ownership middleware (user routes), agentd mux auth (0051).
- The API-key SDK surface keeps returning only what its callers own —
  `BuildWorkspaceBatch` is workspace-scoped; API-key handlers do not gain
  pod-delivery semantics.

## 7. Risks and honest trade-offs

- **No user-presence ACL on pod content** (the explicit §1 decision). The
  remaining protections are pod identity + uid separation + tmpfs lifetime.
  Recorded here so the decision is discoverable, not implicit.
- **Notify requires a reachable pod.** A pod mid-restart misses a notify;
  the revision comparison on next pull covers it (eventual consistency by
  construction, which is also why D4/D5 become redundant).
- **Reconciler re-wrap races a concurrent login write.** Both write the same
  derived wrap under the same active version; last-write-wins is idempotent
  (identical plaintext DEK, same key version). Guarded by the existing
  workspace-scoped advisory-lock pattern if needed.
- **W1 retirement gates on zero v1 rows.** The reconciler surfaces the count;
  retirement is a separate PR after the counter reads zero in prod.
