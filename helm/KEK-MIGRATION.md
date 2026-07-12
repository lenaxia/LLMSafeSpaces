# KEK Migration Runbook (cross-provider)

This document describes how to migrate the master KEK from a local provider
(`static` or `sealed`) to a cloud KMS provider (`aws-kms` or `gcp-kms`)
zero-downtime using the `migrate-kek` CLI.

This is the **cross-provider dual** of [`KEK-ROTATION.md`](KEK-ROTATION.md),
which rotates the KEK within a single provider type (old static key → new
static key). Use this runbook when adopting AWS or GCP KMS on a deployment
that already has encrypted rows under a local provider.

The design and rationale for the cross-provider migration are in
[`design/stories/epic-57-rce-resistance-hardening/README.md`](../design/stories/epic-57-rce-resistance-hardening/README.md)
(US-57.2). The threat-model impact is in
[`pkg/secrets/README.md`](../pkg/secrets/README.md) §"Threat model".

## Prerequisites

1. **A populated deployment** currently running under `static` or `sealed`.
   Every row in `provider_credentials`, `api_keys`, and `org_sso_configs`
   is currently a raw un-prefixed blob (pre-US-57.1) or `lkms:v1:`-prefixed
   (post-US-57.1).
2. **Three KMS keys already created** in your target cloud, one per purpose:
   `providerCredentials`, `orgCredentials`, `masterKek`. See
   `pkg/secrets/README.md` §"Provider implementations" for why three keys
   (not one, not four) — domain separation narrows the blast radius of a
   single key compromise.
3. **Network access to PostgreSQL** from the machine running the CLI.
   Redis access is also needed for the post-migration cache flush.
4. **The API pod stays running** throughout — the migration is zero-downtime
   by design (the CompositeProvider decrypts both old and new formats during
   the transition window).

## Threat-model change you are accepting

Read `pkg/secrets/README.md` §"Threat model" before starting. Moving from a
local provider to cloud KMS converts API-pod RCE from **permanent KEK
exfiltration** (attacker reads the unsealed key from process memory and
decrypts DB backups offline forever) to **ephemeral compromise bounded by
the RCE window** (key material never leaves the HSM; once the RCE is evicted
the capability is gone, and stolen DB backups are useless). The tradeoff is
a new availability dependency on the cloud KMS API — see
`pkg/secrets/README.md` §"Cloud KMS availability (D9)".

## Workflow

### 1. Provision the KMS keys and credentials

AWS:

```bash
# One symmetric key per purpose (SYMMETRIC_DEFAULT, $1/month each).
for purpose in provider-credentials org-credentials master-kek; do
  aws kms create-key --description "llmsafespaces-$purpose" --output text --query KeyMetadata.Arn
done

# A dedicated IAM user with kms:Encrypt + kms:Decrypt on those ARNs only.
# Save access-key-id + secret-access-key to a file the chart can mount.
cat > /tmp/aws-credentials <<EOF
[default]
aws_access_key_id = AKIA...
aws_secret_access_key = ...
EOF
chmod 0400 /tmp/aws-credentials
```

GCP: same shape — create three symmetric keys under one keyRing, grant
`roles/cloudkms.cryptoKeyEncrypterDecrypter` on each to a dedicated service
account, download the SA JSON.

### 2. Deploy dual-provider (KMS fallback added, not yet primary)

Add the KMS configuration to your Helm values but keep the local provider
primary for now:

```yaml
security:
  rootKeyProvider: static   # unchanged — local stays primary during migration
  # ... existing master-secret mount stays in place ...
  kms:
    aws:
      region: us-east-1
      credentialsFileSecret: aws-kms-credentials
      keyArns:
        providerCredentials: arn:aws:kms:us-east-1:123:key/abc
        orgCredentials:      arn:aws:kms:us-east-1:123:key/def
        masterKek:           arn:aws:kms:us-east-1:123:key/ghi
```

Deploy. The API pod boots with `CompositeProvider(local-primary,
local-fallback)` semantics — KMS isn't wired yet because `rootKeyProvider`
is still `static`. This step confirms the KMS config parses and the
credentials file mounts; it does not yet produce KMS ciphertexts.

### 3. Flip the root key provider to KMS (KMS becomes primary)

```yaml
security:
  rootKeyProvider: aws-kms   # flipped
```

Deploy. The API pod boots with `CompositeProvider(KMS-primary,
static-fallback)`. New writes produce `aws-kms:v1:`-prefixed ciphertext;
legacy rows still decrypt via the static fallback. The master-secret file
mount is still required — the static fallback reads it at boot.

### 4. Run the migration

```bash
migrate-kek \
  --db-url "postgres://user:pass@host:5432/dbname?sslmode=require" \
  --kms aws \
  --master-key-file /var/run/secrets/llmsafespaces/master-secret \
  --aws-region us-east-1 \
  --aws-credentials-file /path/to/aws-credentials \
  --aws-key-arn-provider arn:aws:kms:us-east-1:123:key/abc \
  --aws-key-arn-org      arn:aws:kms:us-east-1:123:key/def \
  --aws-key-arn-master   arn:aws:kms:us-east-1:123:key/ghi \
  --redis-url "redis://host:6379" \
  --dry-run
```

The dry-run walks every KEK-protected row, attempts a decrypt via the
source composite, and reports the count per table. No writes occur.
Confirm the counts match your expectations.

Then run for real (drop `--dry-run`). Each row is re-wrapped in its own
transaction. If the CLI is interrupted, re-run with `--resume-from
<last-row-id>` (the CLI prints the last processed row ID per table on
exit). The Redis DEK cache is flushed automatically on success.

### 5. Audit (the safe-to-remove-fallback gate)

```bash
migrate-kek \
  --db-url "postgres://..." \
  --kms aws \
  --audit
```

This classifies every row in the three KEK-protected tables by ciphertext
prefix and prints:

```
TABLE                  TOTAL   TARGET   LEGACY    LOCAL    OTHER  STATUS
provider_credentials       5        5        0        0        0  OK
api_keys                   8        8        0        0        0  OK
org_sso_configs            2        2        0        0        0  OK

PASS: every KEK-protected row is on aws-kms. Safe to remove the static fallback (US-57.2 step 7).
```

`--audit` is NOT the same as `--dry-run`. The dry-run re-processes every
row regardless of prefix, so an already-migrated row and a still-legacy
row both count as Processed — it answers "could I migrate this row," not
"is this row already done." The audit answers the second question by
inspecting prefixes only (no decrypt, no KMS calls, safe to run against a
live deployment at any time).

If any table reports OUTSTANDING > 0, rows were written between step 4 and
the audit (or step 4 was interrupted). Re-run step 4 and audit again.

### 6. Verify against live traffic

Test a workspace boot, an API-key login, and an SSO callback. All three
exercise different KMS keys (P1/P2/P3 in the Epic 57 design). Check
CloudTrail / Cloud Audit Logs shows the expected Encrypt and Decrypt
calls against your configured key ARNs / names.

### 7. Remove the static fallback (the cleanup step)

Once step 5's audit passes for all three tables, unmount the master-secret
file from the API deployment. With no master secret mounted,
`newLocalPurposeProvider` returns `nil`, and `buildKMSProvider` returns
the bare KMS provider — the composite is skipped entirely because a
primary-only composite is a no-op wrapper, and the constructor now refuses
a nil fallback (so the misconfiguration fails closed at boot, not under
traffic).

In your Helm values, remove the `masterSecret` volume mount and any
externalSecret that populated it. Deploy. The API pod must boot cleanly —
if it fails to start with "CompositeProvider fallback #0 is nil", the
audit lied or you removed the mount before re-deploying. Re-mount, audit
again, retry.

### 8. Archive the old master secret

Securely archive or destroy the old master KEK file. It is no longer
referenced by any code path and no rows reference its derived keys.

## Rollback

Until step 7, rollback is a Helm `rootKeyProvider: static` flip + redeploy.
All rows decrypt: the migrated ones via the static fallback (which is why
the master-secret mount must stay in place through step 6), and any
un-migrated ones via the static primary. Re-running `migrate-kek` later
brings you back to the migrated state.

After step 7, rollback requires re-mounting the master secret (you archived
it, per step 8 — retrieve it from the archive). Then follow steps 2–3 in
reverse to make static primary again. The migrated rows still decrypt via
the static fallback because the composite was constructed with both
providers before you removed it; once you've re-flipped to static-primary,
re-running migrate-kek is unnecessary (the static fallback handles all
rows during the rollback window).

## Troubleshooting

- **`--audit` reports OUTSTANDING rows after a "successful" migration.**
  Rows were written between the migration pass and the audit. This is
  expected if your deployment served traffic during the window. Re-run
  the migration and audit again. The composite handles mixed-state
  correctly; only step 7 requires a clean audit.

- **`--audit` reports rows under OTHER that match neither your target nor
  legacy/local.** You have ciphertexts from a different KMS provider than
  the one configured (e.g. `gcp-kms:v1:` rows in an `aws-kms` deployment).
  Either complete a cloud-to-cloud migration first or keep the static
  fallback mounted until those rows are re-migrated. The static fallback
  cannot decrypt foreign-KMS ciphertexts.

- **API fails to decrypt after step 7 with a `nil pointer dereference`
  panic.** The audit was not actually clean, or the master-secret mount
  was removed before step 7. Re-mount the master secret; the pod will
  boot with the composite and decrypt the legacy rows. Re-run the audit;
  do not retry step 7 until it passes.

- **API fails to boot with `CompositeProvider fallback #0 is nil`.** You
  configured `rootKeyProvider: aws-kms` but did not mount the master
  secret AND the constructor guard caught the nil fallback before traffic.
  This is the correct fail-closed behavior — either mount the master
  secret (to keep the fallback for legacy rows) or run the migration +
  audit first (to make the fallback unnecessary).

## Purpose mapping

The CLI automatically selects the correct HKDF purpose string for each
table — same mapping as `rotate-kek`:

| Table | Purpose | KMS key flag |
|---|---|---|
| `provider_credentials` (`owner_type='admin'`) | `provider-credentials` | `--aws-key-arn-provider` / `--gcp-key-name-provider` |
| `provider_credentials` (`owner_type='org'`) | `org-credentials` | `--aws-key-arn-org` / `--gcp-key-name-org` |
| `api_keys` | `master-kek` | `--aws-key-arn-master` / `--gcp-key-name-master` |
| `org_sso_configs.oidc_client_secret` | `master-kek` (shared with `api_keys`) | same as `api_keys` |

The `master-kek` purpose builds a `StaticKeyProviderMultiVersion` local
fallback with both v1 (`dek-cache`-derived, pre-US-50.7) and v2
(`master-kek`-derived, post-US-50.7) keys, so legacy `api_keys` rows from
before US-50.7 decrypt correctly during migration.
