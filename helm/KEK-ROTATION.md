# KEK Rotation Runbook

This document describes how to rotate the master KEK (`LLMSAFESPACES_MASTER_SECRET`)
using the `rotate-kek` CLI. Rotation is an **annual** operation (or after a
suspected compromise).

## Prerequisites

1. **Access to the old + new master KEK files.** The old file is the one
   currently mounted at `/var/run/secrets/llmsafespaces/master-secret`. The
   new file is a freshly generated key (e.g. `openssl rand -hex 32`).
2. **Network access to PostgreSQL and Redis** from the machine running the CLI.
3. **The API pod is running** with the old key (the rotation is zero-downtime —
   the multi-key provider decrypts both old + new ciphertexts during the
   transition window).

## Steps

### 1. Generate the new key

```bash
openssl rand -hex 32 > /tmp/new-master-key
chmod 0400 /tmp/new-master-key
```

### 2. Dry-run to preview the work

```bash
rotate-kek \
  --old-master-file /var/run/secrets/llmsafespaces/master-secret \
  --new-master-file /tmp/new-master-key \
  --database-url "postgres://user:pass@host:5432/dbname?sslmode=require" \
  --redis-url "redis://host:6379" \
  --dry-run
```

This reports how many rows across `provider_credentials`, `api_keys`, and
`org_sso_configs` will be re-wrapped. No writes occur.

### 3. Mount the new key alongside the old (rotation window)

Update the Helm chart to mount BOTH keys (colon-separated path):

```yaml
masterSecret:
  deliveryMethod: file
  fileMountPath: /var/run/secrets/llmsafespaces/master-secret
```

Set the env var `LLMSAFESPACES_MASTER_SECRET_FILE` to
`/var/run/secrets/llmsafespaces/master-secret-old:/var/run/secrets/llmsafespaces/master-secret-new`.

Deploy the updated chart. The API pod restarts with the multi-key provider;
all existing ciphertexts decrypt via the old key, new encrypts use the new key.

### 4. Run the rotation

```bash
rotate-kek \
  --old-master-file /var/run/secrets/llmsafespaces/master-secret-old \
  --new-master-file /var/run/secrets/llmsafespaces/master-secret-new \
  --database-url "postgres://..." \
  --redis-url "redis://..." \
  --target-version 2
```

Each row is re-wrapped in its own transaction. If the CLI is interrupted:

- Resume with `--resume-from <last-row-id>` (the CLI prints the last processed
  row ID per table on exit).

### 5. Verify

After the CLI completes:
- `key_version = 2` on all rows across all three tables.
- The Redis DEK cache is flushed (automatic on success).
- API credentials decrypt correctly (test a workspace boot).

### 6. Remove the old key

Update the Helm chart to mount ONLY the new key (remove the old file from the
path). Redeploy. The API pod restarts with only the new key.

### 7. Archive the old key

Securely archive or destroy the old master key. It is no longer needed.

## Purpose mapping

The CLI automatically selects the correct HKDF purpose string for each table:

| Table | Purpose | Notes |
|---|---|---|
| `provider_credentials` (owner_type='admin') | `provider-credentials` | |
| `provider_credentials` (owner_type='org') | `org-credentials` | Same table, different purpose |
| `api_keys.key_ciphertext` | `master-kek` (post-US-50.7) | |
| `org_sso_configs.oidc_client_secret` | `dek-cache` | Same provider as api_keys pre-US-50.7 |

## Troubleshooting

- **"decrypt: decryption failed"** — the old master key file doesn't match the
  key used to encrypt the row. Verify the old key file is the one currently
  mounted in the API pod.
- **Rows stuck at version 1 after rotation** — the CLI was interrupted. Re-run
  with `--resume-from <last-row-id>`.
- **API fails to decrypt after removing the old key** — some rows weren't
  rotated. Re-mount the old key, re-run the CLI, then remove again.
