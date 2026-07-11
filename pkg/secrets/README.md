# pkg/secrets — Root Key Providers

This package implements the platform's at-rest credential encryption. The
**root key** (KEK) is the root of trust for every credential stored at rest:
admin/org LLM API keys, org SSO client secrets, and (via the Redis DEK cache)
every user DEK while it lives in Redis.

The `RootKeyProvider` interface (`Encrypt/Decrypt`) is the single abstraction
over how the root key is held. Two local implementations ship today; an external
provider (Vault / OpenBao Transit) is planned.

## Provider implementations

| Provider | `rootKeyProvider` value | Where the key material lives | Use when |
|----------|-------------------------|------------------------------|----------|
| `StaticKeyProvider` | `""` (Helm default) or `"static"` | In a Kubernetes Secret, delivered as a **read-only file mount** (Epic 50 US-50.1 default) or legacy env var, held in API-process memory for the pod's lifetime | **Development only.** Single key, no rotation. The file mount removes `/proc/1/environ` exposure; the legacy env path remains as a deprecated opt-in (`masterSecret.deliveryMethod=env`). Emits a startup warning. |
| `SealedKeyProvider` | `"sealed"` | In a sealed file on disk; the root key is wrapped by an Argon2id KEK derived from an operator-supplied passphrase | **Production (self-hosted).** The root key is not present in env vars; an attacker who reads the sealed file but not the passphrase cannot recover it. |
| `AWSKMSProvider` | `"aws-kms"` (Epic 57 US-57.1) | In AWS KMS — the key material **never leaves AWS**. Every Encrypt/Decrypt is a network round-trip to the KMS API. | **Production (AWS).** Converts API-pod RCE from permanent KEK exfiltration to ephemeral compromise bounded by the RCE window. File-mounted static AWS credentials (not IRSA — narrower trust surface per US-50.1's pattern). |
| `CompositeProvider` | *(internal, wraps any of the above)* | Dispatches Decrypt by ciphertext prefix (`lkms:v1:`, `aws-kms:v1:`). Primary for Encrypt; primary + fallbacks for Decrypt. | Enables zero-downtime migration between providers. The composite's static fallback decrypts legacy rows during migration. |

Selection is read in `api/internal/app/secrets_adapters.go` (`newRootKeyProvider`)
from `cfg.Security.RootKeyProvider` (env: `LLMSAFESPACES_SECURITY_ROOTKEYPROVIDER`).

### Cloud KMS availability (D9)

Under KMS, every `Decrypt` call is a network round-trip. Sustained KMS
unavailability (regional outage, network partition) causes all KEK-dependent
decrypts to fail simultaneously. This is an inherent trade-off of cloud KMS.
**Multi-region KMS key replicas are recommended for HA deployments.** The
`CompositeProvider`'s static fallback does NOT mitigate KMS-primary
unavailability — it only runs on ciphertext prefix mismatch (legacy rows).

## Threat model

"Mitigated?" assumes the listed attacker is the *only* vector in play. Defense
in depth requires assuming they are not.

| Attacker capability | Static | Sealed | External (planned) |
|---|---|---|---|
| Read-only filesystem access (no process memory) | **No** — key is in a kubelet Secret (file-mounted or env) | **Yes** — sealed file is useless without the passphrase | Yes |
| Node-level disk read (stolen disk, snapshot) | **No** | **Yes** | Yes |
| Read `/proc/<api-pid>/environ` from the node | **Partial** — file-mount default (US-50.1) keeps the key out of env; legacy `deliveryMethod=env` still leaks it | **Yes** — the *root key* is never in env vars; the passphrase is, but the sealed file alone is useless without it | Yes |
| Process-level access to the API pod (RCE) | No | **No** — the unsealed root key lives in process memory; an attacker calls `Decrypt()` exactly as legitimate code does | **Partial** — the key never leaves the HSM, but decrypt is still callable; the value is *exfiltration-limitation + audit*, not prevention |
| Full memory dump of the API pod | No | No | Partial (same as above) |
| Ciphertext exfiltration (DB backup leak) | No — without rotation the leak is permanent | No (same — until rotation exists) | Best — rotation + audit; the HSM prevents offline decrypt of the backup |

**Key takeaway:** the dominant threat is RCE in the API pod. No local provider
fully mitigates that — once an attacker runs code in the pod they can decrypt as
the application does. The sealed provider's real value is preventing *offline*
recovery after disk/env-var exfiltration, and removing the root key from
`/proc/1/environ`. An external (Transit) provider adds exfiltration-limitation
and audit; it is not a complete RCE defense.

## Choosing a provider

- **Local development / CI:** static (the default). The startup warning is
  expected; suppress it with `LLMSAFESPACES_SECURITY_SKIPMASTERKEYWARNING=true`
  only for environments that genuinely cannot surface logs.
- **Production today:** sealed. Generate the sealed file with `cmd/seal-key`
  (the root key is never printed unless `-print-key` is passed) and mount the
  sealed file plus a passphrase Secret into the API pod.
- **Production, regulated / high-value (when available):** the planned external
  (Transit) provider.

## Sealed-key file format

`cmd/seal-key` writes the root key sealed under an Argon2id KEK derived from the
passphrase:

- **V1 (current, US-50.11):** `magic "LSKP-S"` ‖ `salt(32)` ‖ `nonce(12)` ‖ `ciphertext`.
  The KEK is Argon2id over the passphrase, with the HKDF info string
  `llmsafespaces-sealed-root` mixed into the salt for domain separation (see
  `DeriveSealedKEK` in `crypto.go`).
- **V0 (legacy):** `salt(32)` ‖ `nonce(12)` ‖ `ciphertext`, KEK = Argon2id with
  no info string. `NewSealedKeyProvider` still reads V0 files, so deployments
  upgraded in place keep working.

The magic prefix is the one place a ciphertext-format version is justified:
sealed-key files are standalone artifacts detached from any database row's
`key_version` column, so the version must travel with the file.
