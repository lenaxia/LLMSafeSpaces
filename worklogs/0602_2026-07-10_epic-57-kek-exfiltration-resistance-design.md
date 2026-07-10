# Worklog: Epic 57 — KEK Exfiltration Resistance (Design)

**Date:** 2026-07-10
**Session:** Design an epic that closes the largest unbuilt gap against the threat model's dominant threat (API-pod RCE → permanent KEK exfiltration for offline DB decrypt), by adding cloud KMS (AWS + GCP) as alternative master KEK providers.
**Status:** Complete

---

## Objective

Pick up Epic 50's explicitly-deferred H3 finding ("No KMS / Vault / HSM provider exists"). Epic 50 deferred with the rationale that no deployment target demanded it and the `RootKeyProvider` interface was already shaped for a future provider. With the broader conversation about RCE-resistance priorities (vs. zero-knowledge at rest, which provides meaningful but marginal benefit and doesn't address the dominant RCE threat), this epic became the right next security investment.

---

## Work Completed

### Phase 1 — Threat-model alignment

Started by re-reading `pkg/secrets/README.md:42-51` and `design/stories/epic-17-security-review/THREAT-MODEL.md` attack tree 2.4 (lines 141-146). Confirmed the honest framing: the dominant threat is API-pod RCE, no local-provider design prevents in-process decrypt abuse while the RCE is live, and the value of KMS is **exfiltration limitation + audit** — the KEK never leaves the HSM, so the capability dies with the RCE. This is the framing the epic adopts throughout, not "KMS prevents RCE."

### Phase 2 — Decision: cloud KMS, not Vault/OpenBao

Epic 50's deferral section recommended Vault/OpenBao Transit. After analyzing the actual deployment targets:

- **Self-hosters (homelab, small org, single-node k3s)**: don't want to run another always-on HA service. The existing `SealedKeyProvider` (`pkg/secrets/root_key.go:149`) is the right answer — file-based, passphrase-wrapped, zero services.
- **Cloud-deployed users**: AWS KMS and GCP KMS are fully managed, ~$1/key/month, HA handled by the cloud, audit logging built in. No operator-run service to maintain.
- **Vault/OpenBao**: real but niche. Possible as a future community contribution via the same `RootKeyProvider` interface — one new file, no redesign.

Decision: ship AWS KMS + GCP KMS, leave Vault/OpenBao as a documented future contribution. Self-hosters keep using `SealedKeyProvider`.

### Phase 3 — CompositeProvider dispatch mechanism

The hard part of the epic isn't writing one KMS provider — it's the migration path from local to external, executed zero-downtime. Designed the `CompositeProvider` that dispatches `Decrypt` based on ciphertext prefix:

| Ciphertext shape | Owning provider |
|---|---|
| *(no prefix, raw 12-byte nonce + AES-GCM)* | Static/Sealed (legacy) |
| `lkms:v1:` + base64(raw-blob) | Static/Sealed (new writes) |
| `aws-kms:v1:` + base64(kms-ciphertext) | AWS KMS |
| `gcp-kms:v1:` + base64(kms-ciphertext) | GCP KMS |

New sentinel `ErrNotMyCiphertext` distinguishes "wrong format" from "wrong key" so the composite routes cleanly. Legacy un-prefixed rows decrypt via fallback — this is what makes migration zero-downtime.

### Phase 4 — Four-path wiring model (stress-test finding)

Initial draft assumed three provider-construction sites. Stress-testing against `api/internal/app/app.go` revealed **four distinct paths**:

- P1: `newPurposeProvider("provider-credentials")` → admin provider credentials
- P2: `newPurposeProvider("org-credentials")` → org credentials
- P3: `newRootKeyProvider(cfg, log)` → upgraded to `StaticKeyProviderMultiVersion`; **shared by `api_keys` AND `org_sso_configs.client_secret`** via `sso.New(... KeyProvider: apiKeyProv ...)` at `app.go:608`
- P4: `deriveServerKey("oidc-state-cookie")` → HMAC-only for PKCE cookies, never a KEK

Three KMS keys map to P1-P3; P4 stays local. Both provider-construction functions (`newRootKeyProvider` and `newPurposeProvider`) must learn about KMS via a shared helper `newCompositeForPurpose(cfg, log, purpose)` — touching only one would silently leave admin/org credentials on static while only API keys migrate.

### Phase 5 — AuditedProvider composition order (stress-test finding)

Today's boot code at `app.go:380-381, 572-574` wraps each per-purpose provider in `AuditedProvider` before wiring. Under the composite design, this would double-audit legacy-row decrypts (once when the KMS primary returns `ErrNotMyCiphertext`, once when the static fallback succeeds). Fix: construction order changes — construct composite first, then wrap composite in `AuditedProvider`, then wire. Documented as D8 with a dedicated regression test (`TestCompositeProvider_AuditedWrapperComposition_LegacyRowDecryptProducesExactlyOneAuditRow`).

### Phase 6 — Cut stories after stress-testing

Earlier drafts included three additional stories that were cut:

- **Redaction wiring** (`pkg/redact` → proxy stream): cut because the existing `base64-≥40` pattern (`pkg/redact/redact.go:47`) would mangle legitimate agent output — git commit hashes (40-64 hex chars), SHA-256 hashes in lockfiles, base64-encoded image data, long UUIDs. The library exists and is tested but needs pattern-scoping work (split into narrow proxy-safe + aggressive bounded-error-only tiers) before it's safe to wire. Belongs in a separate epic.
- **PVC `nosuid` validating webhook**: cut because `THREAT-MODEL.md:348` and `phase-3/findings.md:79-86` already classify G23 as "defence-in-depth only" — `runAsNonRoot + NoNewPrivs + cap-drop ALL` close the SUID escalation path empirically (verified by pentest evidence `RT-3.5.json`). The existing `WorkspaceValidator.AllowedStorageClassNames` is the right surface if more storage control is needed.
- **LUKS / PVC block-level encryption**: rejected; cloud provider encryption-at-rest (EBS, GCP PD) covers the same threat at lower operational cost.

### Phase 7 — PR review iteration

Opened PR #510. Reviewer (AI) verified all 16+ code references and confirmed the four-path wiring model is complete (no fifth provider-construction site). Three actionable findings, all addressed:

1. **THREAT-MODEL.md line 386** is the Sandbox Pod STRIDE row, not the API-pod dominant threat — fixed the citation to use `pkg/secrets/README.md:46-51` as the primary source.
2. **`sso.go:608`** was a coincidental line-number match across the wrong file — the `KeyProvider: apiKeyProv` wiring is at `app.go:608`, not `sso.go:608`. Fixed.
3. **Runtime KMS unavailability** as a single-point-of-failure availability dependency — added as D9 with acceptance criterion #11 requiring runbook documentation and explicit note that the static fallback does NOT mitigate KMS-primary unavailability (it only runs on prefix mismatch).

---

## Key Decisions

- **Cloud KMS over Vault/OpenBao** for the production target. Self-hosters keep `SealedKeyProvider`.
- **Symmetric KMS keys** (AWS `SYMMETRIC_DEFAULT`, GCP `ENCRYPT_DECRYPT`) — asymmetric buys nothing for `Encrypt`/`Decrypt` with the key held by the cloud.
- **File-mounted static credentials** (AWS credentials file / GCP service-account JSON), not IRSA or Workload Identity Federation — narrower trust surface per US-50.1's pattern, per explicit operator preference.
- **Three KMS keys** (one per provider path P1-P3), preserving today's HKDF domain separation.
- **Redis DEK cache key stays local** — `NewRedisDEKCache` takes a raw key, the cache is volatile, KMS-per-lookup would be expensive.
- **`key_version` column becomes a write counter** under KMS — cosmetic but harmless; cloud-side versioning via the ciphertext prefix is authoritative.
- **`migrate-kek` is a separate CLI from `rotate-kek`** — different inputs (key ARNs vs master key files), different deploy-both-providers step, different ciphertext formats. Conflating them adds branches without sharing meaningful code.
- **GCP KMS is a parallel story (~150 lines)** to AWS KMS — both cloud KMS services expose the same `Encrypt`/`Decrypt(keyId, bytes) → bytes` shape.

---

## Files

- `design/stories/epic-57-rce-resistance-hardening/README.md` (new — 417 lines, the epic itself)

---

## Blockers

None. PR #510 addressed all reviewer comments.

---

## Next Steps

Implementation proceeds per-story:
- US-57.1 (AWS KMS + CompositeProvider) — ~1 week
- US-57.2 (`migrate-kek` CLI) — ~2-3 days
- US-57.3 (GCP KMS provider) — ~2 days
