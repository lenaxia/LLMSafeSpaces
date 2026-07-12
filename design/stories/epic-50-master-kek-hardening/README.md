# Epic 50: Master KEK Hardening

**Status:** Planning
**Created:** 2026-06-20
**Priority:** High (3 HIGH-severity findings on the root of trust for all at-rest credentials)
**Depends On:** None (self-contained; stories ordered by dependency within this epic)

---

## Problem Statement

The master KEK (`LLMSAFESPACES_MASTER_SECRET`) is the root of trust for every
credential the platform stores at rest: admin provider keys, org provider keys,
org SSO client secrets, API-key DEKs, and (via the DEK cache) every user DEK
while it lives in Redis. A security assessment (2026-06-20) identified eight
findings against this KEK, three of them HIGH.

### Architectural finding (discovered during epic review)

The codebase has **two parallel crypto abstraction layers** for master-KEK-derived
encryption. This was not visible until the epic's first review traced every
decrypt call site. The two layers protect different data, use different
interfaces, and the original epic plan addressed only one.

| Layer | Interface | Purpose strings | Protects | Decrypt sites |
|---|---|---|---|---|
| **1 — `RootKeyProvider`** | `Encrypt/Decrypt(ctx, []byte)` interface | `"dek-cache"` (via `dekMasterKey()`) | `api_keys.key_ciphertext`, `org_sso_configs.oidc_client_secret` | 3 (`key_service.go:560`, `auth.go:575`, `sso.go:514`) |
| **2 — `AdminKeyDeriver`** | `func(label string) []byte` callback returning raw key | `"provider-credentials"`, `"org-credentials"` | `provider_credentials.ciphertext` — **all admin, org, and auto-bound LLM API keys** (Anthropic, OpenAI, etc.) | 6+ (`credential_ops.go:60`, `admin_provider_credentials.go:95,266`, `org_credentials.go:202`, `injection.go:171` — the workspace-boot hot path) |
| **3 — Redis DEK cache** | Direct `DecryptSecret(masterKey, ct)` | `"dek-cache"` | User DEKs while cached in Redis | 1 (`redis_cache.go:63`) |

Layer 2 is the largest credential store in the system and was completely missed
by the initial epic draft. Any rotation, audit, or versioning mechanism that
only touches Layer 1 leaves every LLM API key unrotatable and unaudited.

**This epic unifies Layers 1 and 2 under the `RootKeyProvider` interface**
(US-50.2). Layer 3 (Redis DEK cache) remains separate — it uses the same derived
key as Layer 1's `"dek-cache"` purpose but is a different consumer; it is
addressed by the domain-separation story (US-50.7) and flushed during rotation.

### Findings addressed

| ID | Severity | Finding | Story |
|---|---|---|---|
| H1 | HIGH | Master KEK delivered as env var (`secretKeyRef → env`) instead of a file mount | US-50.1 |
| H2 | HIGH | No rotation support; rotating the KEK is effectively destructive (Postgres ciphertexts orphaned) | US-50.3, US-50.4, US-50.5, US-50.6 |
| H3 | HIGH | No KMS / Vault / HSM provider exists | **Done (Epic 57)** — see below |
| M1 | MEDIUM | `static` deprecation warning fires only on explicit `"static"`, not on the Helm-empty default | US-50.8 |
| M2 | MEDIUM | `RootKeyProvider` collapses domain separation — reuses the `dek-cache` key for the API-key DEK store | US-50.7 |
| M3 | MEDIUM | Sealed provider only changes disk storage, not in-memory exposure; its existence implies more than it delivers | US-50.9 |
| L1 | LOW | `seal-key` CLI prints the generated root key to stderr | US-50.10 |
| L2 | LOW | Sealed provider's KEK derivation uses no HKDF info string | US-50.11 |
| — | NEW | No audit logging of decrypt operations — exfiltration via legitimate API is undetectable | US-50.12 |
| — | NEW | Two parallel crypto layers (`RootKeyProvider` + `AdminKeyDeriver`) — hardening one does not harden the other | US-50.2 |

### H3 — External Providers (resolved by Epic 57)

H3 ("no KMS / Vault / HSM") was deferred at Epic 50 planning time. It is now
**resolved by Epic 57** (`design/stories/epic-57-rce-resistance-hardening/README.md`),
which shipped `AWSKMSProvider` (US-57.1), the `migrate-kek` cross-provider CLI
(US-57.2), and `GPCKMSProvider` (US-57.3). The original deferral rationale is
preserved below for history; Epic 57 superseded the "single TransitProvider"
recommendation with cloud-native KMS.

Historical rationale (Epic 50 planning, 2026-05):

1. **The dominant threat is RCE in the API pod.** KMS does not prevent this —
   an attacker with process access calls `provider.Decrypt()` exactly as
   legitimate code does. KMS's real value is narrower: it prevents permanent
   offline decryption after exfiltration (the key never leaves the HSM) and
   provides audit logs. That is a meaningful control, but it is exfil-limitation
   + forensics, not prevention. MEDIUM is the honest severity.

2. **No known deployment target requires it yet.** Building AWS KMS + GCP KMS +
   Vault Transit providers is speculative generality (Rule 4). The
   `RootKeyProvider` interface is already shaped for a future Transit provider —
   Vault Transit's API is "ciphertext in, plaintext out" with versioning handled
   server-side. Adding an external provider later is one new implementation, not
   a redesign.

3. **When a deployment target is known**, implement a single `TransitProvider`
   wrapping HashiCorp Vault / OpenBao Transit (preferred — MPL license fits the
   AGPL codebase, gives free audit logging and one-command rotation). Do NOT
   hand-roll envelope encryption — wrap the provider's native format.

Epic 57's superseding decision (2026-07-09, see its §"Scope decision: cloud KMS,
not self-hosted Vault/OpenBao"): cloud-deployed users get more value from native
AWS/GCP KMS (managed, ~$1/key/month, cloud-side audit built in) than from
self-hosted Vault, and self-hosters are already served by `SealedKeyProvider`.
OpenBao/Vault Transit remains a possible future community contribution following
the same `RootKeyProvider` shape — the `CompositeProvider` prefix dispatch makes
it trivially pluggable.

US-50.12 (decrypt audit logging) shipped alongside this deferral and remains
valuable under KMS: application-level audit and cloud-side audit are
dual-sourced, so compromise detection does not depend on a single log.

---

## Validated Assumptions

Each verified against live code at planning time.

| # | Assumption | Verified At | Result |
|---|---|---|---|
| A1 | Master secret is delivered to the API pod via `secretKeyRef → env`, not a file mount | `charts/llmsafespaces/templates/api-deployment.yaml:63-67` | Confirmed |
| A2 | The master secret is consumed by **only** the API pod (controller, MCP, workspace pods do not receive it) | grep of all chart templates for `master-secret`/`MASTER_SECRET` | Confirmed — blast radius already correctly scoped |
| A3 | `deriveServerKey` reads the env var directly via `os.Getenv` | `api/internal/app/secrets_adapters.go:471-497` | Confirmed |
| A4 | The default Helm deployment takes the `static` provider path because the chart never sets `rootKeyProvider` | `charts/llmsafespaces/values.yaml` has no `rootKeyProvider` key; `secrets_adapters.go:429` `case "static", "":` matches empty string | Confirmed |
| A5 | `provider_credentials` already has a `key_version INTEGER NOT NULL DEFAULT 1` column | `api/migrations/000015_unified_credential_model.up.sql:20` | Confirmed — but never incremented by writers (US-50.6 fixes that) |
| A6 | `api_keys` and `org_sso_configs` have **no** `key_version` column | `api/migrations/000019_api_key_dek_wrapping.up.sql`; `api/migrations/000038_org_sso_configs.up.sql:10-20` | Confirmed — US-50.3 adds them |
| A7 | The existing `RootKeyProvider` interface is `Encrypt/Decrypt(ctx, []byte) ([]byte, error)` | `pkg/secrets/root_key.go:16-19` | Confirmed — kept unchanged (no rename, no signature change) |
| A8a | Layer 1 decrypt sites (RootKeyProvider.Decrypt): exactly 3 — `key_service.go:560`, `auth.go:575`, `sso.go:514` | grep for `\.Decrypt(ctx` on RootKeyProvider/keyProvider consumers | Confirmed |
| A8b | Layer 2 decrypt sites (DecryptSecret with AdminKeyDeriver-derived key): 6 — `credential_ops.go:60`, `admin_provider_credentials.go:95,266`, `org_credentials.go:202`, `injection.go:171` (admin + org paths via `decryptBinding`) | grep for `DecryptSecret` in handlers + injection | Confirmed — US-50.2 unifies both layers under RootKeyProvider |
| A9 | `provider_credentials.key_version` is read by **zero** code paths today (always defaults to 1); `org_credentials.go:106` hardcodes `KeyVersion: 1` | grep for `key_version` in `pkg/secrets/` and `api/internal/` | Confirmed — US-50.3 wires write-path; US-50.6 makes it rotation-capable |
| A10 | The `RootKeyProvider` held by `KeyService` is built from `deriveServerKey("dek-cache")` under the Helm default — the same purpose string used for the Redis DEK cache | `secrets_adapters.go:415-447` → `case "static", ""` → `dekMasterKey()` → `deriveServerKey("dek-cache")` | Confirmed — US-50.7 fixes the domain-separation collision |
| A11 | `cmd/seal-key/main.go:59` prints the generated root key to stderr | Read of `cmd/seal-key/main.go` lines 1-60 | Confirmed |
| A12 | `SealedKeyProvider`'s `DeriveKEKFromPassword` call uses no `info` parameter | `pkg/secrets/root_key.go:79, 104` | Confirmed — `DeriveKEKFromPassword(password, salt)` only |
| A13 | AES-256-GCM `Decrypt` with a wrong key fails cleanly via auth-tag mismatch (no false positives, no panic) | `pkg/secrets/crypto.go:155` — `gcm.Open` returns `ErrDecryptionFailed` | Confirmed — "try all keys" rotation approach is safe |
| A14 | The secrets audit log table (`secret_audit_log`) already exists and is written by the secrets service via `PgSecretStore.LogAudit` → `INSERT INTO secret_audit_log`. This is **distinct** from the org-level `audit_log` table (migration 000028), which is used by `pg_org_store.go` for org admin actions. US-50.12 extends the `secret_audit_log` pattern, not `audit_log`. | `api/migrations/000008_user_secrets.up.sql`; `pkg/secrets/pg_secret_store.go:418` (`INSERT INTO secret_audit_log`) | Confirmed — corrected from initial draft which cited 000028 (the wrong table). Identified by PR #305 AI reviewer re-review. |
| A15 | `AdminKeyDeriver` is consumed at 8 production call sites + 13 test call sites | grep for `AdminKeyDeriver\|deriveAdminKey\|SetAdminKeyDeriver\|orgKeyDeriver` across `.go` files | Confirmed — bounded surface for US-50.2 unification |
| A16 | Boot order constructs `RootKeyProvider` (line 372) AFTER multiple earlier consumers: the Redis DEK cache at line 240/251 (`dekMasterKey()` → `NewRedisDEKCache`), the admin handler (317), free-tier seeding (323), and `secretService.SetAdminKeyDeriver` (357) | `api/internal/app/app.go:240,253,319,325,359,374` | Confirmed — US-50.2 reorders boot to construct per-purpose providers before the Redis cache, the earliest consumer (line 240) |
| A17 | `credential_ops.go:getCredentialForProbe` uses a `credentialKeyResolver func(ctx) (key []byte, ...)` callback — it returns a raw key, not a provider; this is the shared probe helper used by both admin and org credential model-list endpoints | `api/internal/handlers/credential_ops.go:15-65` | Confirmed — US-50.2 changes this resolver to return a decrypt function |
| A18 | The latest migration is `000040_user_email_verified` | `api/migrations/` directory listing | Confirmed — new migrations start at `000041` |
| A19 | The **earliest** consumer of `dekMasterKey()` is the Redis DEK cache construction at `app.go:240/253` — not the admin handler at line 319. `dekMasterKey()` → `secrets.NewRedisDEKCache(dekCacheClient, mk)` runs 70+ lines before handler wiring. After unification, per-purpose providers must be constructed before line 240, not line 319. | `api/internal/app/app.go:240,253` | Confirmed — discovered in adversarial review of the prior epic draft, which incorrectly cited line 319 as the earliest consumer |
| A20 | There are **two** `else` branches using `dekMasterKey()` when `rkp == nil`: `app.go:383` (`authSvc.SetMasterKey(dekMasterKey())` for the auth service) and `app.go:424-430` (the apiKeyStore fallback for the key service). US-50.2 must migrate both; US-50.7's domain separation must account for both. | `api/internal/app/app.go:383,424-430` | Confirmed — the prior epic draft missed line 383, which would have left a divergent unmigrated code path |
| A21 | `app.go:396` uses `deriveServerKey("oidc-state-cookie")` — a fourth purpose string. Verified at `sso.go:542,559`: it is an **HMAC signing key** for PKCE state cookies (`hmac.New(sha256.New, s.stateKey)`), not an encryption key. It signs transient cookies and protects no data at rest. The rotation CLI (US-50.5) correctly omits it; the omission is intentional and documented in D8. | `api/internal/app/app.go:396`; `api/internal/services/sso/sso.go:542,559` | Confirmed — `stateKey` is used only as an HMAC key, never as an AES key. Not subject to rotation because it does not wrap persisted secrets. |
| A22 | The constant `sealedKeyInfoStr = "llmsafespaces-sealed-root"` is defined at `pkg/secrets/root_key.go:13` but never used anywhere in the codebase — pre-existing dead code. US-50.11 consumes this constant rather than duplicating it, removing the dead code as a side effect. | grep for `sealedKeyInfoStr` across `.go` files returns one hit: the definition itself | Confirmed — identified by PR #305 AI reviewer |

---

## Design Decisions

### D1 — No interface rename, no signature change

The existing `RootKeyProvider` interface (`Encrypt/Decrypt(ctx, []byte)
([]byte, error)`) is kept unchanged. No rename to `MasterKeyProvider`, no
`ActiveVersion()` method on the interface. Multi-version decrypt is handled
internally by the provider holding multiple keys. This means a future external
provider (Vault Transit) drops in without touching callers.

### D2 — Column-based versioning, not ciphertext-format versioning

The `key_version` column on each table is the source of truth for which key
version a row uses. No magic-prefix ciphertext format (`LSKP` + version byte).
The rotation CLI reads the column; the provider holds all keys during the
transition window so decrypt works regardless. Column-based wins on simplicity —
every ciphertext lives in a table row that carries its version.

### D3 — Rotation is a CLI + runbook, not a Helm hook

KEK rotation is an annual operation. A Helm `post-upgrade` hook adds template
complexity, partial-failure recovery semantics, and API-pod-restart
coordination that an annual operation does not warrant. A documented CLI
(`rotate-kek`) + runbook with resume-from-cursor is right-sized.

### D4 — Multi-key provider for zero-downtime rotation

**Requirement:** KEK rotation must not take the API offline. The platform
serves live workspaces; suspending the API during an annual rotation is not
acceptable. This is an explicit product requirement, not a "nice to have."

During rotation, the API pod must decrypt both old and new ciphertexts (not all
rows are re-wrapped atomically — the rotation CLI processes rows in batches
while the API continues serving traffic). The provider holds
`[]keyEntry{{version, key}}` sorted by version descending. `Decrypt` tries each
key until one succeeds. With 2 keys during a rotation window, worst case is 2
AES-256-GCM operations per decrypt — negligible. GCM auth-tag mismatch on a
wrong key returns `ErrDecryptionFailed` (A13) — no false positives, no panics.

**Why not offline rotation (stop API → rotate → restart):** considered and
rejected. Offline rotation is simpler but violates the zero-downtime
requirement. The multi-key complexity is the price of keeping the API live
during rotation, and the price is justified by the requirement.

### D5 — Unify Layers 1 and 2 under `RootKeyProvider`

The two parallel crypto abstractions (`RootKeyProvider` interface vs.
`AdminKeyDeriver` callback) are the root cause of the initial epic draft's
gaps. Unifying them eliminates an entire class of inconsistency: rotation,
audit, versioning, and domain separation all work through one mechanism.

The unification replaces `AdminKeyDeriver func(label string) []byte` with
per-purpose `RootKeyProvider` instances constructed at boot. Each purpose
(`"provider-credentials"`, `"org-credentials"`, `"dek-cache"`/`"master-kek"`)
gets its own provider. Consumers call `provider.Encrypt/Decrypt` instead of
receiving a raw key and calling `EncryptSecret`/`DecryptSecret` directly.

**Boot-order change (A16, A19):** Per-purpose providers must be constructed
**before line 240** — the earliest consumer is the Redis DEK cache
(`dekMasterKey()` at `app.go:240` → `NewRedisDEKCache` at `app.go:253`), not
the admin handler at line 319. (A prior epic draft incorrectly cited line 319;
adversarial review found the Redis cache 70+ lines earlier.) The
`newRootKeyProvider` factory is expanded to produce a `map[string]RootKeyProvider`
keyed by purpose, constructed once at the very start of the secrets-bootstrap
block.

**Probe helper change (A17):** `credentialKeyResolver` changes from
`func(ctx) (key []byte, ...)` to `func(ctx) (decrypt func(ctx, []byte) ([]byte,
error), ...)` so it can return either a provider-bound decrypt closure
(admin/org) or a DEK-bound closure (user credentials).

**Two `else` branches (A20):** Both `app.go:383` (`authSvc.SetMasterKey`)
and `app.go:424-430` (apiKeyStore fallback) currently call `dekMasterKey()`
when `rkp == nil`. US-50.2 migrates both to the per-purpose provider pattern.
A prior draft missed line 383, which would have left a divergent unmigrated
auth-service path.

### D6 — US-50.2 is a high-blast-radius enabler, not a direct security fix

The unification (US-50.2) refactors crypto plumbing across 21 call sites with
a boot-order change. A subtle bug — wrong provider wired to wrong handler,
transposed purpose string, nil provider reaching a hot path — means credentials
silently fail to decrypt, which is data loss for the affected users/orgs. This
is a high-blast-radius change to security-critical code.

**Why do it anyway:** the unification is required for US-50.12 (decrypt audit
logging), which needs a single decrypt chokepoint. Without unification, Layer 2
callers do `DecryptSecret(deriveServerKey(label), ct)` directly and there is no
single place to hook audit. The audit log is the detection layer that justifies
the entire rotation story (without it, rotation is calendar theater). The
security value chain is: unification → audit → meaningful rotation.

**Risk mitigation is structural, not incidental.** US-50.2's testing strategy
(see the story detail) is built around the specific failure modes of this
refactor: round-trip compatibility tests that prove no ciphertext is orphaned,
purpose-isolation tests that prove providers cannot cross-decrypt, boot-order
integration tests, and a staged rollout (the legacy `AdminKeyDeriver` type is
retained as a deprecated adapter during the transition so a rollback is
possible). The story does not merge until the full test matrix passes with
`-race -count=1`.

### D7 — Audit (US-50.12) lands before rotation completion (US-50.5)

Without audit, rotation is calendar theater — operators rotate on a schedule
but never know if rotation was *needed*. The detection layer must land with or
before the recovery layer. The execution strategy places US-50.12 in Phase 3
(alongside the versioning groundwork) and US-50.5 (rotation CLI) in Phase 4.
This means: by the time an operator can rotate, they can also detect whether
rotation is warranted.

### D8 — The OIDC state key (`"oidc-state-cookie"`) is intentionally not rotated

`app.go:396` derives a fourth purpose string, `"oidc-state-cookie"`, used by
the SSO service. Verified at `sso.go:542,559`: this is an **HMAC signing key**
for transient PKCE state cookies, not an AES encryption key wrapping persisted
secrets. It does not protect data at rest. Rotating it would invalidate
in-flight SSO flows for no security benefit (the cookies are short-lived and
contain no sensitive data). The rotation CLI (US-50.5) intentionally omits
this purpose string. This decision is documented explicitly so reviewers do
not flag the omission as a gap.

---

## Stories

| Story | Title | Severity | Effort | Depends On |
|---|---|---|---|---|
| US-50.1 | Deliver master KEK as a file mount, not an env var | HIGH (H1) | 1d | None |
| US-50.2 | Unify the two crypto layers under `RootKeyProvider` | HIGH (new) | 1.5d | None |
| US-50.3 | Add `key_version` columns + write-path population | HIGH (H2) | 0.5d | US-50.2 |
| US-50.4 | Multi-key support in `RootKeyProvider` for rotation | HIGH (H2) | 0.5d | US-50.2 |
| US-50.5 | `rotate-kek` CLI + runbook | HIGH (H2) | 1d | US-50.3, US-50.4 |
| US-50.6 | Rotation-aware write path (populate `key_version` on encrypt) | HIGH (H2) | 0.5d | US-50.3, US-50.4 |
| US-50.7 | Domain-separate `RootKeyProvider` from the Redis DEK-cache key | MEDIUM (M2) | 0.25d | US-50.4, US-50.5 |
| US-50.8 | Fix `static` deprecation warning to fire on empty default | MEDIUM (M1) | 0.25d | None |
| US-50.9 | Document sealed provider's in-memory exposure + threat model | MEDIUM (M3) | 0.25d | None |
| US-50.10 | Stop printing the root key in `seal-key` | LOW (L1) | 0.25d | None |
| US-50.11 | Add HKDF info string to sealed provider KEK derivation | LOW (L2) | 0.25d | None |
| US-50.12 | Decrypt audit logging | NEW | 0.5d | US-50.2 |

Total estimated effort: ~6.75 days.

---

## Dependency Graph

```
US-50.1 (file mount)           ─── can start immediately
US-50.2 (unify layers)         ─── can start immediately (FOUNDATIONAL)
US-50.8 (M1 warning)           ─── can start immediately
US-50.9 (M3 threat-model doc)  ─── can start immediately
US-50.10 (L1 seal-key)         ─── can start immediately
US-50.11 (L2 sealed HKDF info) ─── can start immediately

US-50.2 ──┬──> US-50.3 (key_version cols + write-path)
          ├──> US-50.4 (multi-key provider)
          └──> US-50.12 (decrypt audit logging)

US-50.3 + US-50.4 ──┬──> US-50.5 (rotate-kek CLI)
                    └──> US-50.6 (rotation-aware write path)

US-50.4 + US-50.5 ─────> US-50.7 (domain separation + rotate)
```

Six stories have no dependencies. US-50.2 (unification) is the critical path —
three stories depend on it.

---

## Execution Strategy

**Phase 1 — Quick wins (day 1):** US-50.8, US-50.9, US-50.10, US-50.11.
Four sub-half-day fixes with no dependencies. Ship as one PR.

**Phase 2 — Foundational (days 1-3):** US-50.1 (file mount) and US-50.2
(unification) in parallel. Both are independently shippable; both eliminate
entire classes of exposure. US-50.2 is the critical path for Phases 3-4 and
carries the highest risk (see D6) — it lands with its full test matrix or not
at all.

**Phase 3 — Detection + versioning (days 3-5):** US-50.12 (decrypt audit),
US-50.3 (key_version columns), US-50.4 (multi-key provider). All depend on
US-50.2 and proceed in parallel. **Audit lands here, before the rotation CLI,
per D7** — by the time an operator can rotate, they can also detect whether
rotation is warranted.

**Phase 4 — Rotation + domain separation (days 5-7):** US-50.5 (CLI + runbook),
US-50.6 (rotation-aware write path), US-50.7 (domain separation). Rotation
completes the recovery loop; domain separation is the final hardening.

Each phase ends with the full test suite green (`make test && make build && make lint`)
and a worklog entry.

---

## Per-Story Detail

### US-50.1: Deliver master KEK as a file mount, not an env var (H1)

**Goal:** Eliminate `/proc/1/environ` exposure of the master KEK. Project the
K8s Secret as a read-only file volume; read it from disk in `deriveServerKey`.

**Files:**
- `charts/llmsafespaces/templates/api-deployment.yaml` — replace the `LLMSAFESPACES_MASTER_SECRET` env var block (lines 63-67) with a projected volume + volumeMount, mode `0440`, mounted at `/var/run/secrets/llmsafespaces/master-secret`
- `api/internal/app/secrets_adapters.go` — `deriveServerKey` reads from `LLMSAFESPACES_MASTER_SECRET_FILE` (file path env var) first; falls back to `LLMSAFESPACES_MASTER_SECRET` (legacy value env var) with a one-release deprecation log; same fallback for `LLMSAFESPACES_DEK_MASTER_KEY`
- `api/internal/app/app.go` — `validateMasterSecret` updated to validate whichever source is configured
- `api/internal/app/app_master_key_test.go` — new tests for the file path
- `charts/llmsafespaces/values.yaml` — document the new volume mount in comments

**Rotation-window file format:** During rotation (US-50.4/US-50.5), the file
mount path `LLMSAFESPACES_MASTER_SECRET_FILE` accepts a colon-separated list of
paths. Each file is a single value (the key material, no version line) — the
version is determined by file order (last file = highest version). The rotation
CLI documents which file is old vs. new. For non-rotation operation, a single
file path is the default and is treated as version 1.

**Backward compatibility:** The legacy env-var path is retained for one release
so non-Helm deployments (bare `kubectl apply`, docker-compose, local dev) keep
working. A structured Warn is logged on startup if the env-var path is used
(detected by absence of the file path env var).

**Acceptance criteria:**
- After `helm install`, `kubectl exec <api-pod> -- env | grep MASTER` returns empty
- After `helm install`, the secret is readable at `/var/run/secrets/llmsafespaces/master-secret` with mode `0440`
- Pod boots successfully; credentials decrypt correctly
- An explicit `LLMSAFESPACES_MASTER_SECRET` env var still works for one release (with deprecation log)
- E2E: rotate the K8s Secret value, restart the pod, verify all credentials still decrypt

**Tests (TDD):**
- `TestDeriveServerKey_FromFile` — file path env set, file exists → returns derived key
- `TestDeriveServerKey_FromFile_MissingFile_FallsBackToEnv` — file path set but file missing → legacy env path, with Warn log
- `TestDeriveServerKey_LegacyEnv_DeprecationWarning` — only env var set → works, logs Warn
- `TestDeriveServerKey_FilePathEmpty_UsesEnv` — file path env unset, env var set → works
- `TestDeriveServerKey_MultiFile_RotationWindow` — colon-separated paths, both files present → both keys loaded (US-50.4 integration)
- Helm template test: rendered manifest has no `LLMSAFESPACES_MASTER_SECRET` env var in the default config; has the projected volume
- Helm template test: setting `masterSecret.deliveryMethod: env` (legacy opt-in) restores the env-var block

### US-50.2: Unify the two crypto layers under `RootKeyProvider` (FOUNDATIONAL)

**Goal:** Eliminate the `AdminKeyDeriver func(label string) []byte` callback.
Replace it with per-purpose `RootKeyProvider` instances. Every credential
encrypt/decrypt in the system goes through one interface.

**Current state (verified):**

Layer 2 consumers of `AdminKeyDeriver` (8 production sites, A15):

| Site | Current code | After unification |
|---|---|---|
| `admin_provider_credentials.go:110,114,118-123` | `deriveKey AdminKeyDeriver`, `deriveKey("provider-credentials")` → raw key | `provider RootKeyProvider`, `provider.Encrypt/Decrypt` |
| `admin_provider_credentials.go:73,95` | `buildCredentialResponse(row, key)`, `DecryptSecret(key, ct)` | `buildCredentialResponse(row, provider)`, `provider.Decrypt` |
| `admin_provider_credentials.go:258,266` | `h.kek()` → raw key, `DecryptSecret(kek, ct)` | `h.provider.Decrypt(ctx, ct)` |
| `org_credentials.go:34,39-40,62-67` | `orgKeyDeriver AdminKeyDeriver`, `orgKeyDeriver("org-credentials")` → raw key | `provider RootKeyProvider`, `provider.Encrypt/Decrypt` |
| `org_credentials.go:137,202` | `buildCredentialResponse(row, orgKEK)`, `DecryptSecret(orgKEK, ct)` | `provider.Decrypt` |
| `credential_ops.go:15-18,41,60` | `credentialKeyResolver func(ctx) (key []byte, ...)`, `DecryptSecret(key, ct)` | `credentialDecryptResolver func(ctx) (func(ctx, []byte) ([]byte, error), ...)`, `decryptFn(ctx, ct)` |
| `credential_ops.go:71-86` | `encryptCredentialData(key, ...)` | `encryptCredentialData(ctx, provider, ...)` |
| `secret_service.go:21,37-41` | `deriveAdminKey AdminKeyDeriver`, `SetAdminKeyDeriver(d)` | `adminProvider RootKeyProvider`, `orgProvider RootKeyProvider`, `SetAdminProvider(p)`, `SetOrgProvider(p)` |
| `injection.go:62-71,149-180` | `s.deriveAdminKey("provider-credentials")`, `s.deriveAdminKey("org-credentials")` → raw keys → `decryptBinding(..., adminKEK, orgKEK)` → `DecryptSecret(key, ct)` | `s.adminProvider.Decrypt(ctx, ct)`, `s.orgProvider.Decrypt(ctx, ct)` |
| `secrets_adapters.go:627-648` | `deriveServerKey("provider-credentials")` → raw key → `EncryptSecret(kek, pt)` | `provider.Encrypt(ctx, pt)` using injected provider |
| `app.go:319` | `NewAdminProviderCredentialsHandler(pgStore, deriveServerKey)` | `NewAdminProviderCredentialsHandler(pgStore, providerCredsProvider)` |
| `app.go:359` | `secretService.SetAdminKeyDeriver(deriveServerKey)` | `secretService.SetAdminProvider(providerCredsProvider)` + `secretService.SetOrgProvider(orgCredsProvider)` |
| `app.go:240,253` (A19) | `dekMasterKey()` → `NewRedisDEKCache(dekCacheClient, mk)` — earliest consumer | Redis cache receives `redisCacheKey` derived from the same per-purpose construction block |
| `app.go:383` (A20) | `authSvc.SetMasterKey(dekMasterKey())` — `else` branch when `rkp == nil` | `authSvc.SetRootKeyProvider(apiKeyProv)` — both branches unified |
| `app.go:424-430` (A20) | `apiKeyStore` fallback `else` when `rkp == nil`, uses `dekMasterKey()` | `keyService.SetAPIKeyStore(..., apiKeyProv)` — both branches unified |

**Boot-order change (A16, A19):**

Currently `newRootKeyProvider` runs at `app.go:374`, AFTER multiple earlier
consumers. The **earliest** is the Redis DEK cache at `app.go:240/253`
(`dekMasterKey()` → `NewRedisDEKCache`), which runs ~135 lines before
`newRootKeyProvider`. The admin handler (317), free-tier seeding (323), and
`SetAdminKeyDeriver` (357) also predate it. A prior draft of this story
incorrectly cited line 319 as the earliest consumer; adversarial review found
the Redis cache construction 70+ lines earlier at line 240.

After unification, per-purpose providers must be constructed at the **very
start** of the secrets-bootstrap block (before line 240). New boot sequence:

```
1. Read master key from file/env (US-50.1)
2. Construct per-purpose providers IMMEDIATELY (before Redis cache):
     providerCredsProv = newLocalProvider(deriveServerKey("provider-credentials"))
     orgCredsProv     = newLocalProvider(deriveServerKey("org-credentials"))
     apiKeyProv       = newLocalProvider(deriveServerKey("dek-cache"))  // US-50.7 changes this
     redisCacheKey    = deriveServerKey("dek-cache")  // for Redis DEK cache (line 240)
3. Construct Redis DEK cache (line 253) with redisCacheKey
4. Wire providers into handlers, services, and key service
5. ensureFreeTierCredential uses providerCredsProv
6. All subsequent wiring receives providers instead of deriveServerKey
7. Both `else` branches (app.go:383 authSvc, app.go:424 apiKeyStore) use apiKeyProv
```

**Two `else` branches (A20):** Both `app.go:383` (`authSvc.SetMasterKey`)
and `app.go:424-430` (apiKeyStore fallback) currently call `dekMasterKey()`
when `rkp == nil`. US-50.2 migrates both to use `apiKeyProv`. A prior draft
missed line 383, which would have left the auth-service fallback on a
divergent code path.

**Files:**
- `pkg/secrets/credential_store.go` — remove `AdminKeyDeriver` type; add deprecation comment pointing to `RootKeyProvider`
- `pkg/secrets/secret_service.go` — replace `deriveAdminKey AdminKeyDeriver` field + `SetAdminKeyDeriver` with `adminProvider RootKeyProvider` + `orgProvider RootKeyProvider` fields + setters
- `pkg/secrets/injection.go` — `PrepareSecretsForInjection` uses `s.adminProvider.Decrypt` / `s.orgProvider.Decrypt` directly; `decryptBinding` signature changes to accept decrypt functions instead of raw keys
- `api/internal/handlers/admin_provider_credentials.go` — `deriveKey` field → `provider RootKeyProvider`; `kek()` method removed; `buildCredentialResponse` takes provider; `Create`/`Update` call `provider.Encrypt/Decrypt`
- `api/internal/handlers/org_credentials.go` — same pattern as admin handler
- `api/internal/handlers/credential_ops.go` — `credentialKeyResolver` → `credentialDecryptResolver` (returns decrypt closure); `encryptCredentialData` takes provider
- `api/internal/app/secrets_adapters.go` — `ensureFreeTierCredential` takes provider; add `newPurposeProvider(masterKey []byte, purpose string) RootKeyProvider` factory
- `api/internal/app/app.go` — construct per-purpose providers early; wire into all consumers; remove `deriveServerKey` passes to handlers/services
- Test files: `credential_precedence_test.go` (13 call sites), `org_credentials_test.go`, `admin_provider_credentials_test.go`, `user_provider_credentials_test.go` — all `SetAdminKeyDeriver` calls replaced with `SetAdminProvider`/`SetOrgProvider`

**Acceptance criteria:**
- `AdminKeyDeriver` type no longer exists in production code (grep returns zero hits outside deprecation comments)
- Every credential encrypt/decrypt goes through `RootKeyProvider.Encrypt/Decrypt`
- `provider_credentials`, `api_keys.key_ciphertext`, and `org_sso_configs` all decrypt correctly after the refactor
- Workspace boot (injection path) successfully decrypts admin + org credentials
- Free-tier credential seeding works via the provider
- All existing tests pass (updated to use providers instead of deriver callbacks)

#### Risk Mitigation & Testing Strategy

US-50.2 is a high-blast-radius refactor of security-critical crypto plumbing
(see D6). The test matrix below is organized by failure mode, not by function.
Every failure mode must have at least one test that would catch it. The story
does not merge until the full matrix passes with `go test -race -count=1`.

**Failure mode 1 — Ciphertext orphaned by the refactor (data loss).**

A ciphertext encrypted by the OLD code path (`AdminKeyDeriver` + `DecryptSecret`)
must decrypt under the NEW code path (`provider.Decrypt`). This is the critical
safety property. Test via golden-file round-trips for every (table, owner_type,
purpose) combination:

| Test | Encrypts with | Decrypts with | Proves |
|---|---|---|---|
| `TestRoundTripCompatibility_ProviderCredentials_Admin` | old `deriveServerKey("provider-credentials")` + `EncryptSecret` | new `providerCredsProv.Decrypt` | admin LLM keys decryptable |
| `TestRoundTripCompatibility_ProviderCredentials_Org` | old `deriveServerKey("org-credentials")` + `EncryptSecret` | new `orgCredsProv.Decrypt` | org LLM keys decryptable |
| `TestRoundTripCompatibility_APIKeyCiphertext` | old `dekMasterKey()` + `EncryptSecret` | new `apiKeyProv.Decrypt` | api_keys.key_ciphertext decryptable |
| `TestRoundTripCompatibility_OrgSSOSecret` | old `deriveServerKey("dek-cache")` + `EncryptSecret` | new `apiKeyProv.Decrypt` | org_sso_configs decryptable |
| `TestRoundTripCompatibility_FreeTierCredential` | old `ensureFreeTierCredential` encrypt path | new provider decrypt | free-tier seed decryptable |

These tests use a fixed master key and fixed plaintext so they are deterministic.
If any fails, the refactor has orphaned production data — do not merge.

**Failure mode 2 — Wrong provider wired to wrong handler (cross-decrypt or silent failure).**

A handler holding provider A must NOT successfully decrypt ciphertexts meant
for provider B. If it does, either (a) it silently returns wrong data, or
(b) the wiring is transposed. Both are bugs.

| Test | Setup | Asserts |
|---|---|---|
| `TestPurposeIsolation_AdminCannotDecryptOrg` | admin provider, org-encrypted ciphertext | `Decrypt` returns `ErrDecryptionFailed` |
| `TestPurposeIsolation_OrgCannotDecryptAdmin` | org provider, admin-encrypted ciphertext | `Decrypt` returns `ErrDecryptionFailed` |
| `TestPurposeIsolation_APIKeyProvCannotDecryptProviderCreds` | api-key provider, admin-cred ciphertext | `Decrypt` returns `ErrDecryptionFailed` |
| `TestPurposeIsolation_ProviderCredsProvCannotDecryptAPIKey` | admin-cred provider, api-key ciphertext | `Decrypt` returns `ErrDecryptionFailed` |

These tests prove the per-purpose providers produce cryptographically distinct
keys (distinct HKDF info strings → distinct keys → GCM auth-tag mismatch on
cross-decrypt). They catch transposed-purpose-string wiring bugs.

**Failure mode 3 — Nil provider reaches a hot path (503 or panic).**

A handler whose provider was not wired (boot-order bug, config error) must
return a clean 503, not panic.

| Test | Setup | Asserts |
|---|---|---|
| `TestAdminHandler_NilProvider_Returns503` | handler with nil provider, POST create | 503, no panic |
| `TestOrgHandler_NilProvider_Returns503` | handler with nil provider, POST create | 503, no panic |
| `TestSecretService_NilAdminProvider_SkipsAdminCredentials` | nil admin provider, injection call | admin creds skipped with audit event, no panic |
| `TestSecretService_NilOrgProvider_SkipsOrgCredentials` | nil org provider, injection call | org creds skipped, no panic |

**Failure mode 4 — Boot-order regression (providers constructed after consumers wired).**

Full `app.New()` integration test verifying every credential path resolves a
non-nil provider after boot. This catches the boot-order bug class directly.
The critical assertion is that providers exist **before line 240** (the Redis
DEK cache construction — the earliest consumer per A19), not just before line
317 (the admin handler — a later consumer).

| Test | Asserts |
|---|---|
| `TestAppBoot_AllCredentialProvidersWired` | after `app.New()`, admin handler, org handler, secret service, key service, **Redis DEK cache**, and auth service all have non-nil providers |
| `TestAppBoot_ProvidersConstructedBeforeRedisCache` | the per-purpose provider map is non-nil before `NewRedisDEKCache` is called (the earliest consumer at line 240) — catches the prior epic draft's incorrect "line 319 is earliest" assumption |
| `TestAppBoot_BothElseBranchesMigrated` | neither `app.go:383` (authSvc) nor `app.go:424-430` (apiKeyStore) call `SetMasterKey`/`dekMasterKey` post-migration — both use `apiKeyProv` (A20) |
| `TestAppBoot_FreeTierSeedingUsesProvider` | free-tier credential row present and decryptable post-boot |
| `TestAppBoot_ProviderPurposeStringsCorrect` | each handler's provider derives from the correct purpose string (assert via test-only introspection or known-answer test) |

**Failure mode 5 — Concurrency (provider shared across goroutines).**

`RootKeyProvider.Decrypt` is called from concurrent request handlers. The
provider must be goroutine-safe.

| Test | Asserts |
|---|---|
| `TestProvider_ConcurrentDecrypt_NoRace` | 100 goroutines calling `Decrypt` concurrently with different ciphertexts | passes `-race`, all decrypt correctly |
| `TestProvider_ConcurrentEncryptDecrypt_NoRace` | mixed encrypt/decrypt across goroutines | passes `-race` |

**Failure mode 6 — E2E workspace boot (the injection hot path).**

The highest-stakes path: `PrepareSecretsForInjection` decrypts admin + org +
user credentials at workspace boot. This must work end-to-end.

| Test | Asserts |
|---|---|
| `TestPrepareSecretsForInjection_AllOwnerTypesDecrypt_E2E` | seed admin + org + user bindings, call injection | all three decrypt, valid secrets JSON produced |
| `TestPrepareSecretsForInjection_AdminOnly_NoUserSession` | admin bindings only, no session (controller-initiated restart) | admin creds decrypt, no panic on missing DEK |
| `TestPrepareSecretsForInjection_MixedAdminAndOrg_PriorityOrder` | admin + org creds for same provider | correct priority winner decrypts |

**Rollback safety:** The legacy `AdminKeyDeriver` type is retained as a
deprecated adapter (one-release deprecation window). If US-50.2 reveals a
production bug post-merge, the adapter allows wiring the old callback path back
without reverting the interface change. The adapter is removed in a follow-up
release after US-50.2 has run in production without incident.

### US-50.3: Add `key_version` columns + write-path population (H2)

**Goal:** Add `key_version` to the two tables that lack it, and make the
existing column on `provider_credentials` actually reflect the active key
version via the unified provider.

**Files:**
- `api/migrations/000041_api_keys_key_version.up.sql` / `.down.sql` — `ALTER TABLE api_keys ADD COLUMN key_version INTEGER NOT NULL DEFAULT 1`
- `api/migrations/000042_org_sso_key_version.up.sql` / `.down.sql` — `ALTER TABLE org_sso_configs ADD COLUMN key_version INTEGER NOT NULL DEFAULT 1`
- `pkg/secrets/pg_credential_store.go` — `Create`/`Update` write `key_version` from the provider's active version; `Get`/`List` return it
- `api/internal/services/database/pg_org_store.go` — same for SSO config writes/reads
- `pkg/secrets/key_service.go` — `rewrapAPIKeyDEKs` writes the active version
- `pkg/types/types.go` — add `KeyVersion int` to `ProviderCredential`, `OrgSSOConfig`, `APIKey` transfer objects

**Note:** After US-50.2, handlers hold `RootKeyProvider` instances. The
provider's `ActiveVersion()` method (internal, not on the interface — see D1)
supplies the version to write. A `VersionedProvider` wrapper or a concrete-type
assertion provides this. See US-50.4 for the multi-key type that exposes
`ActiveVersion()`.

**Acceptance criteria:**
- After a fresh `Create`, `provider_credentials.key_version = <active>`
- After a fresh SSO config write, `org_sso_configs.key_version = <active>`
- After `rewrapAPIKeyDEKs`, `api_keys.key_version = <active>`
- Existing rows remain at version 1; no data migration in this story
- Down migrations drop the columns cleanly

**Tests (TDD):**
- `TestPgCredentialStore_Create_PopulatesKeyVersion`
- `TestPgCredentialStore_Update_BumpsKeyVersion`
- `TestPgOrgStore_SaveSSOConfig_PopulatesKeyVersion`
- `TestKeyService_RewrapAPIKeyDEKs_WritesActiveVersion`
- Migration safety test: both new migrations apply idempotently and roll back cleanly

### US-50.4: Multi-key support in `RootKeyProvider` for rotation (H2)

**Goal:** Enable the local provider to decrypt ciphertexts from multiple key
versions during the rotation transition window.

**Design (D4):** The provider holds `[]keyEntry` sorted by version descending.
`Decrypt` iterates and returns the first success. `Encrypt` uses the highest
version. `ActiveVersion()` returns the highest configured version (used by
US-50.3's write path).

**Files:**
- `pkg/secrets/root_key.go` — `StaticKeyProvider` gains `keys []keyEntry` field; constructor `NewStaticKeyProviderMultiVersion(activeVersion int, keyByVersion map[int][]byte)`; existing `NewStaticKeyProvider(key []byte)` wraps to single-entry at version 1; add `ActiveVersion() int` method on the concrete type (not the interface — callers that need it assert the concrete type or use a `VersionedProvider` interface)
- `pkg/secrets/root_key.go` — `SealedKeyProvider` same pattern
- `pkg/secrets/root_key_test.go` — new tests for multi-key routing
- `api/internal/app/secrets_adapters.go` — `newPurposeProvider` reads from the file mount (US-50.1); supports colon-separated multi-file paths for the rotation window (old + new), each parsed as a version key

**Acceptance criteria:**
- A provider with `[{2, keyB}, {1, keyA}]` decrypts ciphertexts encrypted with either key
- `Encrypt` always uses `keyB` (version 2)
- `ActiveVersion()` returns 2
- A ciphertext encrypted with a key NOT in the provider returns `ErrDecryptionFailed`
- Single-key constructor works unchanged (backward compatible)
- `RootKeyProvider` interface signature unchanged

**Tests (TDD):**
- `TestStaticKeyProvider_MultiKey_DecryptRoutesByTrial`
- `TestStaticKeyProvider_MultiKey_EncryptUsesHighestVersion`
- `TestStaticKeyProvider_MultiKey_ActiveVersion_ReturnsHighest`
- `TestStaticKeyProvider_MultiKey_WrongKeyReturnsError`
- `TestStaticKeyProvider_SingleKey_BackwardCompatible`

### US-50.5: `rotate-kek` CLI + runbook (H2)

**Goal:** Make rotation possible without destroying data. After US-50.2
unification, the CLI re-wraps every KEK-protected row through the unified
provider interface — both former Layer 1 (`api_keys`, `org_sso_configs`) and
former Layer 2 (`provider_credentials`) rows.

**New CLI:** `cmd/rotate-kek/main.go`
- Flags: `--old-master-file`, `--new-master-file`, `--dry-run`, `--table` (one of `provider_credentials`, `api_keys`, `org_sso_configs`, or `all`), `--resume-from <id>`
- Constructs two provider sets:
  - Old providers: one per purpose (`provider-credentials`, `org-credentials`, `dek-cache`/`master-kek`) from the old master key, version N
  - New providers: same purposes from the new master key, version N+1
- For each row: identify the purpose from the table + owner_type → select the right old/new provider pair → `old.Decrypt(ct)` → `new.Encrypt(pt)` → write `ct_new, key_version=N+1`
- Idempotent: a row already at the target version is skipped
- Resume-from-cursor: records the last processed row ID per table; on retry, starts from there
- Flushes the Redis DEK cache on success (DEKs were wrapped with the old KEK-derived key)

**Purpose mapping (after US-50.2 unification):**

| Table | Purpose string | Notes |
|---|---|---|
| `provider_credentials` (owner_type='admin') | `provider-credentials` | |
| `provider_credentials` (owner_type='org') | `org-credentials` | Same table, different purpose — the CLI must read `owner_type` to select the right provider |
| `api_keys.key_ciphertext` | `dek-cache` (pre-US-50.7) or `master-kek` (post-US-50.7) | |
| `org_sso_configs.oidc_client_secret` | `dek-cache` (same provider as api_keys — the Layer 1 RootKeyProvider) | |

**Runbook:** `charts/llmsafespaces/KEK-ROTATION.md`

**Files:**
- `cmd/rotate-kek/main.go` + `main_test.go`
- `pkg/secrets/rotation.go` — `RotationCoordinator` with `Rotate(ctx, oldProviders, newProviders map[string]RootKeyProvider, store RotationStore) (RotationResult, error)`
- `pkg/secrets/rotation_store.go` — interface abstracting the three tables
- `charts/llmsafespaces/KEK-ROTATION.md`

**Acceptance criteria:**
- `rotate-kek --dry-run --table all` against a populated DB reports correct counts without writing
- `rotate-kek --table all` rotates every row across all three tables; `key_version` becomes the target version
- After rotation, the API pod restarts with the new key and all credentials decrypt correctly
- Idempotency: running twice is a no-op the second time
- Resume-from-cursor works after a mid-run kill
- A decrypt failure on any row is reported with the row ID; CLI exits non-zero; each row is its own transaction (no partial-row writes)
- Redis DEK cache flushed on success
- The CLI correctly selects the provider purpose based on table + `owner_type` (not just table name)

**Tests (TDD):**
- `TestRotationCoordinator_AllTables_HappyPath`
- `TestRotationCoordinator_ProviderCredentials_SelectsPurposeByOwnerType`
- `TestRotationCoordinator_DryRun_DoesNotMutate`
- `TestRotationCoordinator_Idempotent_SecondRunNoOp`
- `TestRotationCoordinator_ResumeFromCursor_ContinuesFromLastRow`
- `TestRotationCoordinator_DecryptFailure_ReportsRowID_NonZeroExit`
- `TestRotationCoordinator_FlushesRedisDEKCache`
- E2E: rotate a populated Postgres + Redis, verify all reads succeed post-rotation

### US-50.6: Rotation-aware write path (populate `key_version` on encrypt) (H2)

**Goal:** Ensure every new encrypt writes the provider's active version into
`key_version`, so the rotation CLI can identify which rows need re-wrapping.

This is the write-path counterpart to US-50.3 (which adds the columns). After
US-50.2 unification, all encrypt paths go through `provider.Encrypt`. The
handler/store layer must also call `provider.ActiveVersion()` (concrete-type
assertion) and persist it.

**Files:**
- `api/internal/handlers/admin_provider_credentials.go` — `Create`/`Update` write `KeyVersion: provider.ActiveVersion()` instead of hardcoded `1`
- `api/internal/handlers/org_credentials.go:106` — same fix (currently `KeyVersion: 1`)
- `api/internal/services/database/pg_org_store.go` — SSO config write path
- `pkg/secrets/key_service.go` — `rewrapAPIKeyDEKs` writes active version

**Acceptance criteria:**
- After `Create`, every table's `key_version` reflects the provider's active version
- After `Update` with re-encryption, `key_version` is bumped if the active version changed
- No hardcoded `KeyVersion: 1` remains in the codebase

**Tests (TDD):**
- `TestAdminProviderHandler_Create_WritesActiveKeyVersion`
- `TestAdminProviderHandler_Update_BumpsKeyVersion`
- `TestOrgCredentialsHandler_Create_WritesActiveKeyVersion` (replaces hardcoded `1` at `org_credentials.go:106`)
- `TestSSOConfig_Save_WritesActiveKeyVersion`

### US-50.7: Domain-separate `RootKeyProvider` from the Redis DEK-cache key (M2)

**Goal:** After US-50.2 unification, the API-key provider uses the same purpose
string (`"dek-cache"`) as the Redis DEK cache. Give it its own purpose
(`"master-kek"`) so a Redis compromise cannot help unwrap Postgres API-key DEKs.

**After unification this is a one-line change at the provider-construction
site** (US-50.2 wires per-purpose providers; changing the purpose string for
the api_keys provider from `"dek-cache"` to `"master-kek"` is the fix). The
multi-key provider (US-50.4) retains the old `"dek-cache"` key for backward
compatibility; the rotation CLI (US-50.5) re-wraps existing `api_keys` rows to
the new purpose.

**Files:**
- `api/internal/app/secrets_adapters.go` — change the api_keys provider construction from `newPurposeProvider(master, "dek-cache")` to `newPurposeProvider(master, "master-kek")`; the multi-key set includes the old `"dek-cache"` key for decrypting pre-migration rows
- `api/internal/app/app.go` — **both** `else` branches updated (A20): line 383 (`authSvc.SetMasterKey`, when `rkp == nil` for auth) and line 425-429 (apiKeyStore fallback, when `rkp == nil` for key service) use the new-purpose provider. Missing line 383 would leave the auth-service fallback on a divergent purpose string.

**Acceptance criteria:**
- New `api_keys.key_ciphertext` values are encrypted under `deriveServerKey("master-kek")`
- Existing rows (encrypted under `"dek-cache"`) still decrypt (multi-key provider retains old key)
- Redis DEK cache still uses `deriveServerKey("dek-cache")` — unchanged
- After running US-50.5 rotation, all `api_keys` rows are at the new purpose; old key can be pruned

**Tests (TDD):**
- `TestAPIKeyProvider_UsesMasterKEKPurpose`
- `TestRedisCache_UsesDEKCachePurpose` (regression)
- E2E: seed an api_keys row under the old purpose, deploy with multi-key provider, verify decryption succeeds, run rotation, verify row re-wrapped

### US-50.8: Fix `static` deprecation warning to fire on empty default (M1)

**Goal:** The Helm default is `rootKeyProvider: ""`, which silently maps to the
dev-only static path with no warning. Make the warning fire on empty string too.

**Files:**
- `api/internal/app/secrets_adapters.go:429-441` — change `if provider == "static"` to `if provider == "" || provider == "static"`
- Add `cfg.Security.SkipMasterKeyWarning bool` for operators who accept the risk

**Acceptance criteria:**
- A fresh Helm install logs the warning on startup
- Setting `rootKeyProvider: "sealed"` suppresses the warning
- Setting `skipMasterKeyWarning: true` suppresses the warning

**Tests (TDD):**
- `TestNewRootKeyProvider_EmptyDefault_LogsWarning`
- `TestNewRootKeyProvider_ExplicitStatic_LogsWarning`
- `TestNewRootKeyProvider_Sealed_NoWarning`
- `TestNewRootKeyProvider_SkipWarning_Suppresses`

### US-50.9: Document sealed provider's in-memory exposure + threat model (M3)

**Goal:** Create `pkg/secrets/README.md` documenting the threat model so
operators understand that `SealedKeyProvider` defends against disk/node-read
attackers but offers nothing against process compromise.

**Files:**
- `pkg/secrets/README.md` (new) — threat model table: attacker class × provider × mitigated?; provider selection guide; notes that external providers (KMS/Vault) are planned and what they add
- `pkg/secrets/root_key.go` — doc comment on `SealedKeyProvider` explicitly listing what it does and does not defend against
- `api/internal/app/secrets_adapters.go:440` — update warning text to point to the new doc

**Acceptance criteria:**
- A new operator reading `pkg/secrets/README.md` can correctly choose between local and sealed providers
- The sealed provider doc comment names the in-memory exposure explicitly

**Tests (TDD):** None — documentation only. Acceptance is review by a second reader.

### US-50.10: Stop printing the root key in `seal-key` (L1)

**Goal:** `cmd/seal-key/main.go:59` prints the generated root key to stderr.
Stop by default.

**Files:**
- `cmd/seal-key/main.go` — remove unconditional `fmt.Fprintf(os.Stderr, ...)`; add `--print-key` flag that opts in, outputs to stdout with a warning
- `cmd/seal-key/main_test.go` — update tests

**Acceptance criteria:**
- Default invocation produces no output containing the key
- `--print-key` outputs to stdout (not stderr) with a warning line
- Help text documents the risk

**Tests (TDD):**
- `TestSealKey_Default_NoKeyInOutput`
- `TestSealKey_PrintKey_OutputsToStdout`
- `TestSealKey_PrintKey_NotOnStderr`

### US-50.11: Add HKDF info string to sealed provider KEK derivation (L2)

**Goal:** `DeriveKEKFromPassword(passphrase, salt)` at `root_key.go:79, 104`
uses no `info` parameter. Add domain separation via the existing
`sealedKeyInfoStr` constant (`"llmsafespaces-sealed-root"`, already defined at
`root_key.go:13` but currently dead code — never referenced anywhere).

**Pre-existing dead code (Rule 5):** The constant `sealedKeyInfoStr` at
`root_key.go:13` was defined in anticipation of this exact change but never
wired in. US-50.11 consumes it rather than introducing a duplicate. This
removes the dead code as a side effect of the feature.

**Files:**
- `pkg/secrets/root_key.go` — change `DeriveKEKFromPassword(passphrase, salt)` to use the existing `sealedKeyInfoStr` constant; mix info into the derivation (Argon2id has no native info field; derive a sub-salt via HKDF-Expand on the salt with info as context, or append — implementation documented in code)
- `pkg/secrets/crypto.go` — add the new derivation helper if needed

**Breaking change handling:** This changes the derived KEK for sealed-provider
deployments. Existing sealed keys fail to unseal. Version the sealed key file
format with a magic prefix (`LSKP-S`) so both old and new formats are handled.
This is the one justified use of a magic prefix — sealed key files are
standalone artifacts detached from a database row's `key_version` column, so
the version must travel with the file. Document in code.

**Note on the "no known users" assumption:** The sealed provider is a
documented production option (`secrets_adapters.go:418`, `values.yaml:453`).
Claiming zero production users is an unvalidated assumption (Rule 7). The
magic-prefix versioning makes the change non-breaking regardless, so the
assumption does not need to hold.

**Acceptance criteria:**
- New sealed keys use the info-string-derive KEK (via `sealedKeyInfoStr`) and carry the `LSKP-S` magic prefix
- Old sealed keys (no prefix) still unseal correctly
- `sealedKeyInfoStr` is consumed (no longer dead code)
- All sealed-provider tests pass

**Tests (TDD):**
- `TestDeriveKEKFromPasswordSealed_DifferentInfoProducesDifferentKeys`
- `TestSealedKeyProvider_RoundTrip_V1Format`
- `TestSealedKeyProvider_UnsealLegacyV0Format` — old-format sealed key still decrypts

### US-50.12: Decrypt audit logging (NEW)

**Goal:** After US-50.2 unification, all decrypt operations flow through
`RootKeyProvider.Decrypt`. A single `AuditedProvider` wrapper logs every
decrypt call to the existing `secret_audit_log` table (migration 000008,
written by `pg_secret_store.go:418`). This is the detection layer
for authorized-decrypt abuse and partially compensates for the deferred KMS
audit logging.

**Design:** The wrapper decorates any `RootKeyProvider`:

```go
type AuditedProvider struct {
    inner RootKeyProvider
    audit AuditLogger
    label string  // "provider-credentials", "org-credentials", "master-kek"
}
```

Every `Decrypt` call produces one audit entry. Every `Encrypt` call is NOT
logged (encrypt is not a sensitive read operation).

**What is logged:**
- Caller label (which purpose/provider)
- Target table + row ID (passed via context from the calling handler)
- Key version used (from the provider's concrete type)
- Timestamp
- Success/failure boolean

**What is NOT logged:**
- Plaintext, ciphertext, key material — never

**Files:**
- `pkg/secrets/audited_provider.go` (new) — `AuditedProvider` wrapper
- `pkg/secrets/audit.go` — `DecryptAuditEntry` type; writes to existing `secret_audit_log` table (migration 000008) via the same `PgSecretStore.LogAudit` path
- `api/internal/app/app.go` — wrap every per-purpose provider in `AuditedProvider` at boot (after US-50.2 wiring)
- Calling handlers pass context with row identification (table name, row ID) so the audit entry is meaningful

**Acceptance criteria:**
- Every `RootKeyProvider.Decrypt` call across both former layers produces exactly one `secret_audit_log` row
- The audit row contains label, table, row ID, key version, timestamp, success — nothing else
- A failed decrypt is also logged with `success=false`
- Audit write is async (buffered writer, same pattern as existing audit); does not block decrypt
- No byte sequence from plaintext or ciphertext appears in the audit row

**Tests (TDD):**
- `TestAuditedProvider_Decrypt_LogsEntry`
- `TestAuditedProvider_DecryptFailure_LogsEntryWithSuccessFalse`
- `TestAuditedProvider_Encrypt_NotLogged`
- `TestAuditedProvider_NoKeyMaterialInLog` — grep audit row for plaintext/ciphertext bytes; must be absent
- `TestAuditedProvider_AsyncDoesNotBlockDecrypt`

---

## Out of Scope

- **H3 (External KMS / Vault / HSM providers)** — deferred by decision. The
  `RootKeyProvider` interface is kept clean so a future `TransitProvider`
  (wrapping Vault/OpenBao Transit) is one new implementation. See "Deferred —
  External Providers" for full rationale. US-50.12 (decrypt audit logging)
  partially compensates by adding the detection layer that KMS would provide
  for free.

- **L3 (CI hardcoded master secret)** — `.github/workflows/ci.yml:479`
  hardcodes a canary value. Correct for CI: protects nothing real, rotated
  freely, intentionally grep-able. No action.

- **Frontend changes** — no UI behavior changes. The KEK is invisible to users.

- **Per-org KEK** — the platform already eliminated per-org DEKs (migration
  000035). The server KEK is the right abstraction for org credentials (D17-S4).

- **User secret KEK rotation** — user secrets are wrapped by the user's
  password-derived KEK (Argon2id), not the server KEK. Rotation path is
  password change (Epic 34). Out of scope.

- **Replacing `deriveServerKey` HKDF with Argon2id** — the master KEK is
  high-entropy; HKDF is correct (Epic 38 US-38.3). No change.

- **Ciphertext-format versioning for database rows** — considered and rejected
  (D2). Column-based versioning is simpler and sufficient. The one exception
  is the sealed-key file format (US-50.11), which is a standalone artifact
  detached from a database row — magic-prefix versioning is justified there.

- **Helm rotation hook** — considered and rejected (D3). CLI + runbook for an
  annual operation.

---

## Definition of Done

This epic is complete when **all** of the following hold:

1. All 12 stories merged with passing tests (`make test && make build && make lint`)
2. `AdminKeyDeriver` no longer exists in production code (US-50.2)
3. **US-50.2 unification test matrix passes with `-race -count=1`:** all five round-trip compatibility tests (golden-file, every table/owner_type), all four purpose-isolation tests, all four nil-provider tests, all three boot-order integration tests, both concurrency tests, and all three E2E injection tests. No failure mode from the US-50.2 risk matrix is uncovered (D6).
4. A rotation can be demonstrated end-to-end on a kind cluster (zero-downtime, per D4):
   - Populate DB with provider credentials (admin + org), org SSO config, and API keys
   - Deploy with both old + new keys mounted (rotation window per US-50.1/US-50.4)
   - Verify the API continues serving traffic during rotation (zero-downtime)
   - Run `rotate-kek --table all`
   - Verify all rows at target version across all three tables
   - Deploy with only the new key (remove old key file)
   - Verify all data decrypts correctly post-rotation
   - Verify the Redis DEK cache was flushed
5. `kubectl exec <api-pod> -- env | grep MASTER` returns empty on a fresh Helm install
6. The static-provider deprecation warning fires on a fresh install
7. Every decrypt operation produces an audit log row (verifiable via test) — landed in Phase 3 before the rotation CLI (D7)
8. `pkg/secrets/README.md` documents the threat model and provider selection guide
9. Worklog entry created per the Worklog Requirements in README-LLM.md

The epic does **not** require removing the legacy env-var path or the legacy
`AdminKeyDeriver` adapter — both are retained for one release per the no-flag-day
rule. The adapter is removed in a follow-up release after US-50.2 has run in
production without incident.
