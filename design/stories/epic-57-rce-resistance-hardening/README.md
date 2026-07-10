# Epic 57: KEK Exfiltration Resistance

**Status:** Planning
**Created:** 2026-07-09
**Priority:** High — closes the largest unbuilt gap against the threat model's dominant threat (API-pod RCE → permanent KEK compromise for offline DB decrypt)
**Depends On:** Epic 50 (master KEK hardening — shipped; consumes its `RootKeyProvider` interface and the shipped `rotate-kek` CLI)
**Does NOT depend on:** Epic 51 (tenant isolation), Epic 56 (durable DEK)

---

## Problem Statement

`design/stories/epic-17-security-review/THREAT-MODEL.md` is explicit (lines 141-146, 386) that the **dominant threat is process-level access to the API pod**. Once an attacker runs code in the pod, they call `Decrypt()` exactly as legitimate code does. No at-rest encryption scheme prevents in-process abuse while the RCE is live — the honest framing from `pkg/secrets/README.md:46-51`.

What a local-provider design (Static, Sealed) cannot prevent: **permanent KEK exfiltration.** An attacker with API-pod RCE reads the unsealed KEK from process memory, takes it home, decrypts DB backups offline forever after eviction. The blast radius is permanent; rotation after the fact doesn't help if the backup was already copied.

What an HSM/KMS-backed provider adds: the KEK never leaves the cloud service. RCE in the API pod can call `Decrypt` while running, but the moment the RCE is killed, the capability is gone. DB backups stolen separately are useless without re-acquiring RCE. The improvement is **exfiltration limitation + cloud-side audit of every decrypt call**, not prevention of in-process abuse — and that is the largest improvement available without changing the in-process threat surface.

This epic lands that improvement. It does not duplicate Epic 51's gVisor work (the single biggest RCE-resistance win, owned there).

---

## Scope decision: cloud KMS, not self-hosted Vault/OpenBao

Epic 50's "Deferred — External Providers" section listed Vault/OpenBao Transit as the future implementation. **This epic supersedes that recommendation.** Three reasons:

1. **Self-hosters don't want to run Vault.** Vault in production mode needs HA storage (Consul / integrated Raft), regular snapshots, monitoring — that's an SRE tax that homelabbers and small self-hosters reject. The existing `SealedKeyProvider` (`pkg/secrets/root_key.go:149`) is the right answer for them.
2. **Cloud-deployed users get more value from native cloud KMS.** AWS KMS and GCP KMS are fully managed, ~$1/key/month, HA handled by the cloud, audit logging built in, no operator-run service to maintain.
3. **OpenBao remains possible as a community contribution later** if a self-hoster specifically wants it. The `RootKeyProvider` interface is unchanged; one new file later.

This epic ships:
- AWS KMS support (including the `CompositeProvider` dispatch mechanism that makes migration zero-downtime)
- `migrate-kek` CLI for cross-provider migration (dual to the shipped `rotate-kek`)
- GCP KMS support (parallel shape, trivial alongside AWS)

Self-hosters continue using `SealedKeyProvider`. Cloud users opt into KMS via Helm values. No forced migration.

---

## Threat model alignment

Reference: `design/stories/epic-17-security-review/THREAT-MODEL.md`.

| Threat-model row | Current state | After this epic |
|---|---|---|
| Attack tree 2.4 — Read master KEK from API process memory → permanent offline decrypt of DB backup | "Residual" (line 144-146); KMS deferred by Epic 50 | **Mitigated.** KEK never leaves KMS. RCE window is bounded; permanent compromise requires retaining live RCE. |
| Attack tree 2.5 — KEK compromise → mass credential decryption | Partial: rotation supported (US-50.x shipped) | **Improved.** Cloud-side audit log records every decrypt independently of app-level audit. Compromise detection is dual-sourced. |
| Attack tree 3.x — From database (SQL injection, DB compromise) | Already mitigated for user_secrets (no DB KEK material); partial for KMS-protected tables | **Improved.** DB-alone compromise now useless without re-acquiring live RCE. |

**What this epic does NOT do:** prevent in-process decrypt abuse during an active RCE. That framing is in `pkg/secrets/README.md:42-44` and is unchanged. The honest improvement is exfiltration limitation + audit, not prevention.

---

## Architecture: prefix-sniffing dispatch

The hard part of this epic is not writing one KMS provider — it's the migration path from local to external, executed zero-downtime. The mechanism is a `CompositeProvider` that dispatches `Decrypt` based on the ciphertext's prefix. It ships as part of US-57.1 (AWS KMS) because it only earns its keep once a second provider exists — a composite of one provider is just a wrapper, and shipping it standalone would violate the codebase's "code must be wired into the live request path" rule (`README-LLM.md:93`).

### Ciphertext formats

Each provider's `Encrypt` output carries a self-identifying prefix. `Decrypt` routes by prefix.

| Ciphertext shape | Owning provider |
|---|---|
| *(no prefix, raw 12-byte nonce + AES-GCM)* | `StaticKeyProvider` / `SealedKeyProvider` (legacy, readable forever) |
| `lkms:v1:` + base64(raw-blob) | `StaticKeyProvider` / `SealedKeyProvider` (new writes from a composite-configured deployment) |
| `aws-kms:v1:` + base64(kms-ciphertext) | `AWSKMSProvider` |
| `gcp-kms:v1:` + base64(kms-ciphertext) | `GPCKMSProvider` |

The legacy no-prefix format is the current production state. The CompositeProvider treats un-prefixed ciphertexts as "try the legacy Static/Sealed provider" — this is what makes migration zero-downtime.

### CompositeProvider shape

```go
// pkg/secrets/composite_provider.go

type CompositeProvider struct {
    primary  RootKeyProvider  // Encrypt target; also Decrypt for its prefix
    fallback []RootKeyProvider // Decrypt-only, tried in order when primary doesn't match
}

func (c *CompositeProvider) Encrypt(ctx, plaintext) ([]byte, error) {
    return c.primary.Encrypt(ctx, plaintext)  // primary's output carries its prefix
}

func (c *CompositeProvider) Decrypt(ctx, ciphertext) ([]byte, error) {
    // 1. If prefix matches primary's, route there.
    // 2. Otherwise iterate fallbacks in order; each returns ErrNotMyCiphertext
    //    for non-matching prefixes, ErrDecryptionFailed for matching-prefix
    //    but wrong-key. The first non-error wins.
}
```

The composite does NOT implement `VersionedProvider` — versioning is per-provider (Static has it; KMS providers track versions via cloud-side key versions, exposed differently).

### Provider wiring under KMS (four-path, three-key)

Today's production code constructs the master-KEK-derived providers via **four distinct paths** at boot, not one. Each path is a separate code site that the CompositeProvider migration must touch. Missing any one leaves that purpose on the local KEK while the others migrate — a silent correctness bug.

Reference: `api/internal/app/app.go:308-618`, `api/internal/app/secrets_adapters.go:437-497`.

| Path | Today | Used by | KMS key |
|---|---|---|---|
| **P1 — `providerCredsProv`** | `newPurposeProvider("provider-credentials")` → HKDF-derived | `secretService.SetAdminProvider`, `adminProvCredHandler` | KMS key 1 |
| **P2 — `orgCredsProv`** | `newPurposeProvider("org-credentials")` → HKDF-derived | `secretService.SetOrgProvider`, `orgCredsHandler` | KMS key 2 |
| **P3 — `apiKeyProv`** | `newRootKeyProvider(cfg, log)` → sealed/static; when static, upgraded to `StaticKeyProviderMultiVersion` (v1=`dek-cache` legacy, v2=`master-kek` active) | `authSvc.SetRootKeyProvider` (API-key DEK unwrap), `ssoSvc.KeyProvider` (SSO client secret encryption — **shares this provider with API keys**) | KMS key 3 (shared by `api_keys` and `org_sso_configs.client_secret`) |
| **P4 — `stateKey`** | `deriveServerKey("oidc-state-cookie")` | HMAC-only for PKCE state cookies, **never a KEK** | Stays local; not migrated |

**Three KMS keys, not four.** `apiKeyProv` (P3) is shared by API keys and SSO client secrets today and stays shared under KMS. The SSO service at `api/internal/services/sso/sso.go:608` receives `apiKeyProv` as its `KeyProvider`; migrating them separately would require a new config path and a new KMS key with no threat-model benefit.

**The multi-version upgrade at `app.go:544-566` is skipped under KMS.** Today when `rkp` is a static provider, the boot code upgrades it to `StaticKeyProviderMultiVersion` to maintain backward compat with `api_keys` rows encrypted under the legacy `dek-cache`-derived key. Under KMS, this upgrade is moot: cloud-side versioning makes `master-kek` vs `dek-cache` purpose-derivation irrelevant, and legacy rows decrypt via the composite's static fallback. The boot code branches: when `rkp` is a KMS provider, skip the upgrade block entirely; when it's static, run it as today.

**The Redis DEK cache key stays local.** `NewRedisDEKCache(client, mk)` at `api/internal/app/app.go:330` takes a raw key (`mk := dekMasterKey()`), not a provider. The cache is volatile (regenerable from `jwt_sessions` or user password on miss) and a KMS call per cache lookup would be expensive. This is an accepted residual — the threat it addresses (DB-alone compromise) doesn't apply to volatile cache contents.

The chart exposes three KMS keys (one per provider path P1-P3):

```yaml
kms:
  aws:
    keyArns:
      providerCredentials: "arn:aws:kms:us-east-1:123:key/abc-def"  # P1
      orgCredentials:      "arn:aws:kms:us-east-1:123:key/ghi-jkl"  # P2
      masterKek:           "arn:aws:kms:us-east-1:123:key/mno-pqr"  # P3 (api_keys + org_sso_configs.client_secret)
```

Each provider path constructs its own KMS provider instance at boot with its specific key identifier. Same shape as today; just the keys live in KMS instead of being HKDF-derived from one master.

---

## Stories

### US-57.1: AWS KMS-backed master KEK (H3)

**Goal:** A platform operator deploying LLMSafeSpaces on AWS can configure AWS KMS as the master KEK provider via Helm, so that an API-pod compromise cannot permanently exfiltrate the KEK for offline DB decryption after the RCE is evicted.

This story includes the `CompositeProvider` dispatch mechanism because (a) AWS KMS support is meaningless if every existing row can't be decrypted during the transition, and (b) the composite only earns its keep once a second provider exists. They ship together as one user-facing feature.

**Design decisions:**

- **D1 — Symmetric KMS, not asymmetric.** AWS KMS `SYMMETRIC_DEFAULT` is $1/month, faster, and sufficient for `Encrypt`/`Decrypt` with the key held by AWS. Asymmetric buys nothing here (no public-key distribution requirement) and costs more.
- **D2 — File-mounted static AWS credentials, not IRSA / Pod Identity.** Per the explicit decision to avoid wider access surfaces (workspace conversation 2026-07-09): the API pod mounts a credentials file from a K8s Secret, same property as US-50.1's master-secret file mount. IRSA's trust surface ("anyone with the API pod's ServiceAccount token can assume the IAM role") is wider than a file mount visible only to that pod. The IAM policy on the static credentials is scoped to `kms:Encrypt` and `kms:Decrypt` on the specific key ARNs configured — least privilege. Operator rotates credentials externally (90-day AWS best practice; via ExternalSecrets / Vault Agent / manual).
- **D3 — Prefix-sniffing dispatch via `CompositeProvider`.** Each provider's `Encrypt` output carries a self-identifying prefix (`aws-kms:v1:` for AWS KMS, `lkms:v1:` for new Static writes, raw-blob for legacy Static). `Decrypt` routes by prefix. New sentinel `ErrNotMyCiphertext` distinguishes "wrong format" from "wrong key" so the composite can route cleanly. Legacy un-prefixed rows decrypt via fallback — this is what makes migration zero-downtime.
- **D4 — Three per-purpose KMS key ARNs across four wiring paths.** The three providers P1-P3 (see "Provider wiring under KMS" above) each get their own KMS key ARN: `providerCredentials` (P1), `orgCredentials` (P2), `masterKek` (P3). P3 is shared by `api_keys` and `org_sso_configs.client_secret` — they share a provider today and there is no threat-model benefit to separating them. Preserves the domain separation that `deriveServerKey(<purpose>)` HKDF gives today; without it, compromise of one KMS key would decrypt every purpose's rows. P4 (PKCE state cookie HMAC key) is never a KEK and stays local.
- **D5 — Redis DEK cache key stays local.** The cache is volatile; a KMS call per cache lookup would be expensive. Documented as accepted residual — the threat it addresses (DB-alone compromise) doesn't apply to volatile cache contents.
- **D6 — `key_version` column becomes a write counter, not a version, under KMS.** Cloud KMS versions are opaque to the application; `ActiveVersionOf` returns 1 for the composite by default. The existing write-path code at `admin_provider_credentials.go:374` and `org_credentials.go:272,289` continues to increment `KeyVersion` on every ciphertext write — this is cosmetic under KMS (the column will say "version 5" for a row that's KMS-internal-version-1) but harmless. Accepted as lower-complexity than branching every write site to detect the provider type. Cloud-side versioning via the ciphertext prefix is authoritative.
- **D7 — Two provider-construction functions must learn about KMS, sharing a common helper.** The production code has two entry points: `newRootKeyProvider(cfg, log)` at `secrets_adapters.go:449` (used by P3) and `newPurposeProvider(purpose)` at `secrets_adapters.go:437` (used by P1, P2). Both call `deriveServerKey(purpose)` directly. Touching only `newRootKeyProvider` would leave admin/org credentials on static while only API keys migrate — silent correctness bug. Fix: extract `newCompositeForPurpose(cfg, log, purpose)` that both entry points route through; the helper constructs KMS provider if `cfg.KMS.*.KeyArns[purpose]` is set, else falls back to today's `deriveServerKey(purpose)` static provider, then wraps in CompositeProvider with the static provider as fallback for legacy rows.
- **D8 — AuditedProvider wraps the composite externally, not each member.** Today's boot code at `app.go:380-381, 572-574` wraps each per-purpose provider in `AuditedProvider` *before* wiring. Under the composite design, this ordering produces wrong audit volume: a legacy-row decrypt in a KMS-primary deployment would audit twice (once when the KMS primary returns `ErrNotMyCiphertext`, once when the static fallback succeeds). Correct order: (1) construct primary provider (KMS or Static), (2) construct fallback provider if any, (3) construct `CompositeProvider` from them, (4) wrap composite in `AuditedProvider`, (5) wire into consumers. One audit row per decrypt regardless of how many providers were tried internally.

**Files (new + modified):**

- `pkg/secrets/composite_provider.go` (new) — `CompositeProvider`, `ErrNotMyCiphertext`, prefix parsing/wrapping helpers.
- `pkg/secrets/errors.go` (modified) — add `ErrNotMyCiphertext`.
- `pkg/secrets/root_key.go` (modified) — `StaticKeyProvider`/`SealedKeyProvider` gain prefix-aware decrypt: check `lkms:v1:` prefix, unwrap, decrypt; raw-blob fallback retained for legacy rows. Encrypt wraps with `lkms:v1:` prefix.
- `pkg/secrets/kms_aws_provider.go` (new) — `AWSKMSProvider` holding an `aws-sdk-go-v2/services/kms` client + a single key ARN. Encrypt wraps output as `aws-kms:v1:` + base64. Decrypt strips prefix, calls KMS, returns plaintext. Prefix mismatch returns `ErrNotMyCiphertext`.
- `pkg/secrets/composite_provider_test.go` (new) — TDD for the composite (uses a mocked KMS provider; no live AWS calls).
- `pkg/secrets/kms_aws_provider_test.go` (new) — TDD against a fake KMS server (`httptest.Server` returning canned KMS API responses). No live AWS calls in unit tests.
- `api/internal/app/secrets_adapters.go` (modified) — **both** `newRootKeyProvider` and `newPurposeProvider` route through a new shared helper `newCompositeForPurpose(cfg, log, purpose)` (D7). The helper reads `cfg.KMS.AWS.KeyArns[purpose]` and constructs a KMS provider when set, else falls back to today's `deriveServerKey(purpose)` static provider. Both entry points wrap their primary in a CompositeProvider with the static provider as fallback for legacy rows.
- `api/internal/app/app.go` (modified) — D7: the multi-version upgrade block at lines 544-566 (`if apiKeyProv == nil ... else if sp, ok := apiKeyProv.(*secrets.StaticKeyProvider); ok`) gains a branch that skips when `apiKeyProv` is a CompositeProvider whose primary is KMS-backed. D8: the `AuditedProvider` wrapping at lines 380-381, 572-574 moves from wrapping each per-purpose provider to wrapping the constructed CompositeProvider. Boot order documented in D8.
- `api/internal/config/config.go` (modified) — new `KMS` config block.
- `go.mod` (modified) — add `github.com/aws/aws-sdk-go-v2`, `github.com/aws/aws-sdk-go-v2/config`, `github.com/aws/aws-sdk-go-v2/services/kms`.
- `charts/llmsafespaces/values.yaml` (modified) — new `kms.aws` block: `enabled`, `region`, `credentialsFileSecret`, `keyArns.{providerCredentials,orgCredentials,masterKek}`.
- `charts/llmsafespaces/templates/api-deployment.yaml` (modified) — when `kms.aws.enabled`: mount the credentials-file Secret at `/var/run/secrets/llmsafespaces/aws-credentials`; set `AWS_REGION`, `AWS_CREDENTIALS_FILE`, key ARN env vars. **The master-secret mount is retained** — it is still consumed by `NewRedisDEKCache` for the volatile DEK cache per D5.
- `charts/llmsafespaces/templates/NOTES.txt` (modified) — preflight warning when `kms.aws.enabled` but no credentials Secret referenced. Explicit note: "Under KMS, the master secret file mount protects only the Redis DEK cache; KMS-protected tables (provider_credentials, api_keys, org_sso_configs.client_secret) are encrypted by the configured KMS keys."
- `pkg/secrets/README.md` (modified) — add `AWSKMSProvider` and `CompositeProvider` rows; threat-model matrix updated.

**Acceptance criteria (user-visible):**

1. An operator configures `kms.aws.enabled: true` with valid key ARNs and credentials Secret in Helm; the API boots and serves login + workspace-creation traffic with no decrypt failures.
2. **All four provider paths produce KMS ciphertexts.** Creating (a) an admin provider credential, (b) an org credential, (c) an API key, and (d) an org SSO config with client secret — each produces a ciphertext that, when base64-decoded and string-inspected, begins with `aws-kms:v1:`. Verifies that both `newRootKeyProvider` and `newPurposeProvider` route through `newCompositeForPurpose` (D7); missing any of the four means one entry point was not migrated.
3. CloudTrail logs every `Encrypt` and `Decrypt` call against the configured key ARNs (verified by operator spot-check, documented in the runbook — not an automated test because CloudTrail is operator-controlled).
4. A deployment migrated from Static to AWS KMS (via US-57.2) decrypts both legacy un-prefixed rows and new `aws-kms:v1:` rows correctly during the transition window, with no user-visible errors.
5. US-50.12 audit logging (`AuditedProvider` wrapper) emits exactly one `secret_audit_log` row per decrypt when wrapping the composite. **A decrypt of a legacy un-prefixed row in a KMS-primary deployment produces exactly ONE audit row** — not two (which would indicate AuditedProvider wrapped a composite member instead of the composite itself, per D8).

**Acceptance criteria (technical):**

6. `ErrNotMyCiphertext` is returned by a provider whose prefix doesn't match; `ErrDecryptionFailed` is returned when the prefix matches but the key is wrong (corruption / wrong key version).
7. AWS region misconfiguration → clean boot failure with a useful error message (not a panic, not silent).
8. KMS throttling (HTTP 429 from AWS) → `Decrypt` returns error; the API returns 5xx with no body leak; logged at Warn.
9. Default deployment (no KMS configured): `CompositeProvider` wraps the existing Static/Sealed provider. New writes produce `lkms:v1:`-prefixed ciphertexts. Existing un-prefixed rows decrypt correctly via fallback.
10. The multi-version upgrade block at `app.go:544-566` is skipped when the primary provider is KMS-backed (verified by a boot-order test asserting `apiKeyProv` is a CompositeProvider with KMS primary, not a `StaticKeyProviderMultiVersion`).

**Tests (TDD):**

Composite:
- `TestCompositeProvider_PrimaryOnly_PrefixedWrites`
- `TestCompositeProvider_PrimaryOnly_LegacyFallbackDecrypt`
- `TestCompositeProvider_KMSPrimary_StaticFallback_RoutesByPrefix`
- `TestCompositeProvider_NoMatchingProvider_ReturnsErrNotMyCiphertext`
- `TestStaticKeyProvider_PrefixMismatch_ReturnsErrNotMyCiphertext`
- `TestStaticKeyProvider_PrefixedDecrypt_RoundTrip`
- `TestStaticKeyProvider_LegacyUnprefixedCiphertext_StillDecrypts` — backward compat
- `TestCompositeProvider_EncryptUsesPrimary_DecryptTriesAllInOrder`

AuditedProvider composition (D8):
- `TestCompositeProvider_AuditedWrapperComposition_StillAudits`
- `TestCompositeProvider_AuditedWrapperComposition_LegacyRowDecryptProducesExactlyOneAuditRow` — a decrypt that dispatches through fallback must produce one audit row, not two; this is the regression test for D8

Wiring (D7):
- `TestNewCompositeForPurpose_AllFourPathsProduceKMSCiphertexts` — when `cfg.KMS.AWS.KeyArns` is fully populated, every provider path (P1 providerCredsProv, P2 orgCredsProv, P3 apiKeyProv including SSO client secret) produces `aws-kms:v1:`-prefixed ciphertext. Integration test exercising the actual boot wiring.
- `TestBoot_SkipsMultiVersionUpgradeWhenKMSPrimary` — when the primary is KMS-backed, `apiKeyProv` is a CompositeProvider, not a `StaticKeyProviderMultiVersion` (acceptance criterion 10).

AWS KMS:
- `TestAWSKMSProvider_RoundTrip_EncryptThenDecrypt`
- `TestAWSKMSProvider_PrefixMismatch_ReturnsErrNotMyCiphertext`
- `TestAWSKMSProvider_Decrypt_KMSUnavailable_ReturnsError`
- `TestAWSKMSProvider_Decrypt_Throttled429_ReturnsError`
- `TestAWSKMSProvider_AuthFromCredentialsFile`
- `TestAWSKMSProvider_Encrypt_WrapsWithAwsKmsV1Prefix`
- `TestE2E_AWSKMSProvider_LiveKMS` — gated behind `LLMSAFESPACES_E2E_AWS_KMS_KEY_ARN` env var; skips if unset. Live test verifies round-trip against real AWS.

**Out of scope for US-57.1:**

- LocalStack integration tests (live AWS E2E is sufficient and avoids LocalStack's KMS implementation quirks).
- KMS key rotation automation (runbook only — annual operation per Epic 50 D3).
- IAM policy generation (operator responsibility; chart documents the required policy).
- GCP KMS support (US-57.3).
- `migrate-kek` CLI for cross-provider migration (US-57.2).

---

### US-57.2: `migrate-kek` CLI for zero-downtime cross-provider migration

**Goal:** A platform operator moving from a local KEK (Static / Sealed) to AWS KMS can migrate every existing ciphertext row without taking the API offline, so that adopting KMS is a configuration change rather than a destructive re-encryption event.

This story depends on US-57.1 — the CompositeProvider's dual-decrypt capability is what makes migration zero-downtime. Without it, migration would require taking the API offline while every row is rewritten.

**Why a separate CLI, not a `--to-kms` flag on `rotate-kek`:**

`rotate-kek` (US-50.5, shipped — `cmd/rotate-kek/main.go`, `pkg/secrets/rotation.go`) is built around the assumption that both old and new providers are the same type — it loads two master key files and constructs two `StaticKeyProvider` instances. Cross-provider migration has different inputs (AWS key ARNs, not master key files), different ciphertext formats, and a different deploy-both-providers-first step. Conflating them in one CLI adds branches and flag combinations without sharing meaningful code; separating them keeps each operation's surface area honest.

**Workflow (zero-downtime):**

1. **Pre-migration:** deployment running with Static/Sealed as primary. All DB rows are un-prefixed raw blobs. Note: `api_keys` rows specifically may be encrypted under either `dek-cache`-derived (legacy v1) or `master-kek`-derived (current v2) static keys, per the multi-version upgrade at `app.go:549-552`; the composite's static fallback handles both via `StaticKeyProviderMultiVersion`.
2. **Deploy dual-provider:** Helm update adds the KMS provider as fallback in the CompositeProvider. The API boots with primary=Static, fallback=KMS — but KMS-encrypted rows don't exist yet, so the fallback is dormant. New writes still go to Static (`lkms:v1:` prefix from D3 of US-57.1).
3. **Run `migrate-kek`:** the CLI connects to Postgres with both providers configured, walks each KEK-protected table, decrypts each row via the composite, re-encrypts with the target KMS provider, writes back with the new prefix. Resumable from cursor (same pattern as `rotate-kek`). Migrated rows have `key_version = 1` (D6 — the column is cosmetic under KMS).
4. **Verify:** spot-check decrypts via the live API succeed. Audit log shows the migration's decrypt calls.
5. **Flip primary:** Helm update sets primary=KMS, fallback=Static. API restarts. New writes go to KMS. Old rows still decrypt via fallback during the cleanup window.
6. **Run `migrate-kek` again** (or rely on step 3 being complete) to migrate any rows written between step 3 and step 5.
7. **Remove Static fallback:** once all rows have KMS prefixes and the operator is confident, drop the Static provider from the composite. The master KEK file can be destroyed.

**Failure recovery:**

- If `migrate-kek` fails mid-run, the cursor lets it resume. Partially-migrated state is safe — the composite decrypts both formats.
- If the operator wants to abort migration entirely, flip primary back to Static. Already-migrated rows still decrypt via the Static fallback if it's still configured; if not, run `migrate-kek --to-static` (same CLI, different target flag — mechanical to add if a real operator needs it).

**Files (new + modified):**

- `cmd/migrate-kek/main.go` (new) — CLI structure mirrors `cmd/rotate-kek/main.go`.
- `cmd/migrate-kek/store.go` (new) — PG/Redis store reuse; the table-walking logic is identical to `rotate-kek`'s and should be extracted to a shared helper in `pkg/secrets/rotation.go` if not already.
- `pkg/secrets/migration.go` (new) — `MigrationCoordinator` parallel to `RotationCoordinator` (`pkg/secrets/rotation.go`). The transform function decrypts via the source composite and re-encrypts via the target provider only.
- `pkg/secrets/migration_test.go` (new) — TDD tests.
- `docs/runbooks/migrate-kek.md` (new) — operator runbook with the workflow above, verification steps, and rollback procedure.

**Acceptance criteria (user-visible):**

1. An operator runs `migrate-kek --dry-run --table all` against a populated Static-encrypted DB and sees correct row counts per table without any writes occurring.
2. The operator runs `migrate-kek --table all` with target=AWS KMS; the CLI completes; every row in `provider_credentials`, `api_keys`, and `org_sso_configs` now has an `aws-kms:v1:` prefix when inspected.
3. During migration, the API continues serving login + workspace-creation traffic without 5xx errors (verified by an integration test that runs requests against the API while migration is in progress).
4. If the CLI is interrupted mid-table, re-invoking it with `--resume-from <cursor>` continues from where it stopped; no rows are double-migrated.
5. Rows written between migration pass 1 and pass 2 (still Static-prefixed) are caught by a second `migrate-kek` invocation.

**Acceptance criteria (technical):**

6. Audit log records every decrypt during migration (via `AuditedProvider` composition) — same audit shape as production decrypts.
7. The CLI works identically against GCP KMS as the target once US-57.3 lands — verified by parameterizing the target provider in tests (mocked; live GCP test gated behind env var).

**Tests (TDD):**

- `TestMigrationCoordinator_DryRun_ReportsCounts`
- `TestMigrationCoordinator_FullRun_StaticToAWSKMS`
- `TestMigrationCoordinator_FullRun_StaticToMockedGCPKMS` — proves the CLI is provider-agnostic before US-57.3 lands
- `TestMigrationCoordinator_ResumeFromCursor`
- `TestMigrationCoordinator_SecondPassCatchesLateWrites`
- `TestMigrationCoordinator_ZeroDowntime_APIServesDuringMigration` — integration test
- `TestE2E_MigrateKek_LiveAWS` — gated behind env var.

**Out of scope for US-57.2:**

- Reverse migration (`migrate-kek --to-static`). Mechanical to add if needed; the source/target roles just swap. Not built preemptively.
- Helm hook automation. Per Epic 50 D3, rotation/migration is a CLI + runbook, not a Helm hook — annual operation doesn't warrant the partial-failure recovery complexity.

---

### US-57.3: GCP KMS-backed master KEK

**Goal:** A platform operator deploying LLMSafeSpaces on Google Cloud can configure GCP KMS as the master KEK provider via Helm, with the same threat-model properties as AWS KMS (US-57.1), so that cloud-deployed users on GCP get the same exfiltration-resistance as those on AWS.

This story is parallel to US-57.1 and meaningfully smaller because the CompositeProvider dispatch and migration tooling already exist. The provider code is ~150 lines parallel to the AWS implementation.

**Why this is small alongside AWS KMS:**

Both cloud KMS services expose the same shape — `Encrypt(keyId, plaintext) → bytes`, `Decrypt(keyId, bytes) → plaintext`. The Go SDKs (`cloud.google.com/go/kms/apiv1` vs. `aws-sdk-go-v2/services/kms`) wrap nearly-identical RPCs.

| | AWS KMS (US-57.1) | GCP KMS (US-57.3) |
|---|---|---|
| Key identifier | key ARN (`arn:aws:kms:...`) | resource name (`projects/.../cryptoKeys/...`) |
| Auth file | credentials file (access-key-id + secret) | service-account JSON |
| Encrypt API | `client.Encrypt(ctx, &kms.EncryptInput{...})` | `client.Encrypt(ctx, &kmspb.EncryptRequest{...})` |
| Decrypt API | `client.Decrypt(ctx, &kms.DecryptInput{...})` | `client.Decrypt(ctx, &kmspb.DecryptRequest{...})` |
| Ciphertext prefix | `aws-kms:v1:` | `gcp-kms:v1:` |
| SDK dep | `aws-sdk-go-v2/services/kms` | `cloud.google.com/go/kms/apiv1` |

**Design decisions:**

- **D1 — File-mounted service-account JSON, not Workload Identity Federation.** Same reasoning as US-57.1 D2: the file mount's trust surface is narrower than WIF's ("anyone with the API pod's SA token can act as the GCP SA"). The SA has `roles/cloudkms.cryptoKeyEncrypterDecrypter` on each configured key.
- **D2 — Three KMS key resource names across four wiring paths, parallel to US-57.1 D4.** Three keys (P1 providerCredentials, P2 orgCredentials, P3 masterKek shared by `api_keys` + `org_sso_configs.client_secret`); P4 (stateKey) stays local. Same provider-wiring shape as AWS KMS via `newCompositeForPurpose` (D7 of US-57.1).
- **D3 — Symmetric KMS** (`projects/.../cryptoKeys/...` with purpose `ENCRYPT_DECRYPT`). Same reasoning as US-57.1 D1.

**Files (new + modified):**

- `pkg/secrets/kms_gcp_provider.go` (new) — `GPCKMSProvider` parallel to `AWSKMSProvider`.
- `pkg/secrets/kms_gcp_provider_test.go` (new) — TDD against a fake GCP KMS server.
- `api/internal/app/secrets_adapters.go` (modified) — new `case "gcp-kms"` branch.
- `api/internal/config/config.go` (modified) — `KMS.GCP` block parallel to `KMS.AWS`.
- `go.mod` (modified) — add `cloud.google.com/go/kms/apiv1`.
- `charts/llmsafespaces/values.yaml` (modified) — `kms.gcp` block: `enabled`, `serviceAccountFileSecret`, `keyNames.{providerCredentials,orgCredentials,masterKek}`.
- `charts/llmsafespaces/templates/api-deployment.yaml` (modified) — mount SA-JSON Secret when `kms.gcp.enabled`.
- `charts/llmsafespaces/templates/NOTES.txt` (modified) — preflight warning.
- `pkg/secrets/README.md` (modified) — add `GPCKMSProvider` row.

**Acceptance criteria (user-visible):**

1. An operator configures `kms.gcp.enabled: true` with valid key resource names and SA-JSON Secret in Helm; the API boots and serves login + workspace-creation traffic with no decrypt failures.
2. Creating a provider credential, an org SSO config (with client secret), and an API key all produce ciphertexts that, when base64-decoded and string-inspected, begin with `gcp-kms:v1:`.
3. Cloud Audit Logs every `Encrypt` and `Decrypt` call against the configured key resource names (verified by operator spot-check, documented in the runbook).
4. A deployment migrated from Static to GCP KMS (via US-57.2) decrypts both legacy un-prefixed rows and new `gcp-kms:v1:` rows correctly during the transition window, with no user-visible errors.

**Acceptance criteria (technical):**

5. Same shape as US-57.1's technical criteria, with GCP equivalents (Cloud Audit Logs for audit; quota errors for throttling).

**Tests (TDD):** mirror US-57.1 with GCP equivalents.

- `TestGPCKMSProvider_RoundTrip_EncryptThenDecrypt`
- `TestGPCKMSProvider_PrefixMismatch_ReturnsErrNotMyCiphertext`
- `TestGPCKMSProvider_Decrypt_KMSUnavailable_ReturnsError`
- `TestGPCKMSProvider_Decrypt_QuotaExceeded_ReturnsError`
- `TestGPCKMSProvider_AuthFromServiceAccountJSON`
- `TestGPCKMSProvider_Encrypt_WrapsWithGcpKmsV1Prefix`
- `TestGPCKMSProvider_AuditedWrapperComposition`
- `TestE2E_GPCKMSProvider_LiveKMS` — gated behind `LLMSAFESPACES_E2E_GCP_KMS_KEY_NAME` env var.

**Out of scope for US-57.3:**

- Same as US-57.1 with GCP equivalents.

---

## Already satisfied (no work — documented for completeness)

| Item | Status | Evidence |
|---|---|---|
| `RootKeyProvider` interface | Shipped, unchanged | `pkg/secrets/root_key.go:19-22`. US-57.1/2/3 add implementations, no interface change. |
| Multi-version decrypt for zero-downtime within-provider rotation | Shipped | `StaticKeyProvider` in `pkg/secrets/root_key.go:62-118` (US-50.4). |
| `key_version` columns on every KEK-protected table | Shipped | `provider_credentials`, `api_keys`, `org_sso_configs` (US-50.3). |
| **`rotate-kek` CLI + runbook** | **Shipped** | `cmd/rotate-kek/main.go`, `cmd/rotate-kek/store.go`; commit `c08b3ec2`, PR #371. US-57.2 is the cross-provider dual, not a replacement. |
| Decrypt audit logging (US-50.12) | Shipped | `pkg/secrets/audited_provider.go`. All US-57.x verify composition. |
| `SealedKeyProvider` for self-hosters | Shipped | `pkg/secrets/root_key.go:149`. Default for self-hosted deployments; unaffected by this epic. |
| `StaticKeyProvider` for dev | Shipped | `pkg/secrets/root_key.go:62`. Dev default; unaffected. |

---

## Out of scope

| Item | Owner | Why deferred |
|---|---|---|
| **Vault / OpenBao Transit provider** | Future PR / community contribution | Self-hosters who specifically want a self-hosted KMS can implement `TransitProvider` following the same `RootKeyProvider` shape. The CompositeProvider dispatch makes this trivially pluggable. Not built in this epic because the deployment targets are AWS/GCP, and self-hosters are served by `SealedKeyProvider` today. |
| **Azure Key Vault provider** | Future | Same shape as AWS/GCP KMS. Not built in this epic because no deployment target demands it. |
| **age / SOPS-style file provider** | Future | A different provider shape (file-oriented, not API-call-oriented) for GitOps self-hosters. Materially more design work; not needed while `SealedKeyProvider` covers the self-hoster segment. |
| **IRSA / Workload Identity auth modes** | Rejected | Wider access surface than file-mounted credentials, per explicit decision (workspace conversation 2026-07-09). Documented for operators who prefer it; they patch the chart if needed. |
| **Application-side envelope encryption** | Rejected | Epic 50 D2 already rejected this for within-provider rotation; same rationale applies. KMS calls are direct encrypt/decrypt, no per-DEK wrapping. |
| **KMS key rotation automation** | Runbook only | Annual operation per Epic 50 D3; cloud-side rotation is one API call. |
| **Per-org KMS keys** | Future (Epic 43 follow-up) | Today every org's `org_credentials` rows are encrypted under the same per-purpose KEK. Per-org KMS keys would require row-level key tracking and add operational complexity; not warranted while multi-tenancy is at the org-membership level, not the org-isolation level. |

### Considered and rejected (different epic or no work)

| Item | Verdict | Rationale |
|---|---|---|
| **Redaction wiring (`pkg/redact` → proxy)** | Different epic | Separate concern (agent-output exfiltration, not KEK exfiltration). The current `pkg/redact` library needs pattern-scoping work before it's safe to wire into the proxy path — the base64-≥40 pattern would mangle legitimate agent output (git hashes, lockfile hashes, base64 images). Belongs in a different epic once the pattern set is split into narrow (proxy-safe) and aggressive (bounded-error-context-only) tiers. |
| **PVC `nosuid` validating webhook** | Rejected, no work | Threat model (`THREAT-MODEL.md` line 348 + `phase-3/findings.md` lines 79-86) explicitly classifies G23 as "defence-in-depth only" — `runAsNonRoot + NoNewPrivs + cap-drop ALL` already closes the SUID escalation path. The existing `WorkspaceValidator.AllowedStorageClassNames` (`controller/internal/webhooks/workspace_webhook.go:68`) is the right surface for storage policy if more control is needed. |
| **LUKS / PVC block-level encryption** | Rejected | Cloud provider encryption-at-rest (EBS, GCP PD) covers the same threat at lower operational cost. See workspace conversation 2026-07-09. |

---

## Sequencing

```
US-57.1 (AWS KMS + CompositeProvider)    ─── no dependencies; can start immediately.
                                             Required by US-57.2 and US-57.3.

US-57.2 (migrate-kek CLI)                ─── depends on US-57.1.
                                             Needs a target KMS provider to migrate to.

US-57.3 (GCP KMS provider)               ─── depends on US-57.1 (CompositeProvider).
                                             Parallel to / independent of US-57.2.
```

Recommended order: US-57.1 → US-57.2 (production migration tooling, needed for any real AWS deployment) → US-57.3 (GCP in parallel or after, since no paying customer demands it yet).

US-57.1 is the critical path. US-57.2 makes US-57.1 adoptable in practice (without it, AWS-only deployments would have to wipe the DB to switch providers). US-57.3 is the smallest story and can land whenever GCP support is wanted.

---

## Definition of Done

This epic is complete when **all** of the following hold:

1. US-57.1, US-57.2, and US-57.3 merged with passing tests (`make test && make build && make lint`).
2. **CompositeProvider dispatches by prefix:** a unit test demonstrates a single composite decrypting Static-prefixed, AWS-KMS-prefixed, and GCP-KMS-prefixed ciphertexts, routing each to the correct provider.
3. **AWS KMS provider deploys via Helm** against a live AWS KMS key and serves traffic with no decrypt failures; CloudTrail logs every decrypt.
4. **GCP KMS provider deploys via Helm** against a live GCP KMS key and serves traffic with no decrypt failures; Cloud Audit Logs every decrypt.
5. **Cross-provider migration is zero-downtime:** an integration test runs API traffic against a deployment migrated from Static to AWS KMS via `migrate-kek`, verifying both legacy and new-format rows decrypt during the transition window and the API does not return 5xx during migration.
6. `pkg/secrets/README.md` updated with `AWSKMSProvider`, `GPCKMSProvider`, and `CompositeProvider` rows in the providers table; the threat-model matrix updated to reflect exfiltration limitation under KMS.
7. `design/stories/epic-17-security-review/THREAT-MODEL.md` Attack tree 2.4 row updated from "Residual" to reflect KMS-backed exfiltration limitation.
8. `design/stories/epic-50-master-kek-hardening/README.md` "Deferred — External Providers" section updated to reference this epic as the pickup, with the cloud-KMS-not-Vault decision rationale.
9. Worklog entries created per the Worklog Requirements in `README-LLM.md`.

The epic does **not** require removing the Static or Sealed providers — both remain as defaults for dev and self-hosted deployments. KMS is the production recommendation for cloud-deployed users, not a forced migration.
