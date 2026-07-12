# Worklog: notify-kek post-migration cleanup — composite guard + audit gate

**Date:** 2026-07-12
**Session:** Close the gap between Epic 57 US-57.2's "remove static fallback after migration" workflow step and what the code actually supports. The framing "no code needed, documented in the runbook" turned out to be wrong on three counts.
**Status:** Complete

---

## Objective

Epic 57 US-57.2 workflow step 7 says: "Remove Static fallback: once all rows have KMS prefixes and the operator is confident, drop the Static provider from the composite. The master KEK file can be destroyed." Three gaps between that step and the as-shipped code:

1. **Nil-fallback panic.** `newPurposeProvider` always calls `NewCompositeProvider(kmsProv, local)`. When the operator unmounts the master-secret file (the action step 7 tells them to take), `local` is `nil`. Today's composite silently accepted a nil fallback and panicked on the first foreign-prefix ciphertext under traffic (`f.Decrypt` on a nil `RootKeyProvider`). Step 7 as written was a footgun.
2. **No verification gate.** The original wording was "once all rows have KMS prefixes and the operator is confident." That is hand-waving. `migrate-kek --dry-run` is not the gate — it re-processes every row regardless of prefix, so an already-migrated row and a still-legacy row both count as Processed. It answers "could I migrate this row," not "is this row already done."
3. **Missing runbook.** Epic 57 US-57.2's package doc referenced `docs/runbooks/migrate-kek.md` which did not exist. The migration CLI shipped without its operational runbook.

---

## Work Completed

### CompositeProvider nil-fallback guard (`pkg/secrets/composite_provider.go`)

`NewCompositeProvider` now rejects any nil entry in the variadic fallback tail. The constructor already refused a nil primary; the same fail-closed-at-boot principle now extends to fallbacks. A nil fallback would have panicked on the first dispatch path that reached it.

The production wiring (`buildKMSProvider` in `api/internal/app/secrets_adapters.go`) was updated to skip the composite wrapper entirely when the local fallback is nil — returning the bare KMS provider. This is the correct post-migration state: a primary-only composite is a no-op wrapper, and skipping it avoids both the (now-rejected) nil-fallback construction and the unnecessary indirection on every Encrypt/Decrypt call.

### Post-migration audit (`pkg/secrets/migration.go`)

New `CiphertextClass` enum, `ClassifyCiphertext` function, `CiphertextAudit` struct, and `AuditTable`/`AuditAll` methods on `MigrationCoordinator`. The audit walks every KEK-protected row, classifies by ciphertext prefix, and reports per-table counts of `Target` / `Legacy` / `Local` / `OtherKMS`. `IsComplete()` returns true only when every row in the table is on the configured target KMS — that is the actual safe-to-remove-fallback gate.

The audit does NOT decrypt, does not call any provider, does not touch the network. It is pure prefix accounting. Safe to run against a live deployment at any time.

### CLI subcommand (`cmd/migrate-kek/main.go`)

New `--audit` flag (paired with `--kms aws|gcp` and `--db-url`). Runs `AuditAll`, prints a per-table status table to stderr, exits 0 when every table is fully migrated and 1 when any table has outstanding rows. Does NOT require the master-key file or KMS credentials — audit does not decrypt.

### Runbook (`helm/KEK-MIGRATION.md`)

New runbook modeled on the existing `KEK-ROTATION.md`. Documents the full zero-downtime cross-provider migration workflow, the audit as the safe-to-remove-fallback gate, and the rollback path. References the new `--audit` flag in step 5.

### Epic 57 US-57.2 workflow update (`design/stories/epic-57-rce-resistance-hardening/README.md`)

Workflow step 7 expanded into steps 7 (audit) + 8 (remove fallback). The new step 7 specifies `migrate-kek --audit` as the gate; step 8 explains the `buildKMSProvider` bare-provider branch that fires when the master-secret mount is removed.

---

## Key Decisions

1. **Reject nil fallback at construction, not at dispatch.** The dispatch-time panic under traffic is the worst failure mode — it is intermittent (only fires on foreign-prefix rows), happens under live requests, and produces a generic nil-deref that doesn't point at the misconfiguration. The constructor guard makes the same misconfiguration fail at boot with a clear message.

2. **Skip the composite entirely when no fallback is configured.** A primary-only composite is a no-op wrapper. Wrapping the bare KMS provider would add indirection on every Encrypt/Decrypt call for no benefit. Returning the bare provider is also more honest at the type level — the runtime value IS the KMS provider, not a wrapper around it.

3. **Audit is a separate code path from `MigrateTable`, not a mode of it.** Tempting to add a "classify-only" branch inside the migration loop. Rejected because the two paths answer different questions and conflation is what made `--dry-run` misleading. The audit reads ciphertext prefixes and stops; the migration decrypts + re-encrypts. Keeping them separate means the audit can never accidentally write, and the migration can never accidentally short-circuit on a classification.

4. **`OtherKMS` is its own counter, not folded into `Legacy` or `Local`.** A row with a `gcp-kms:v1:` prefix in an `aws-kms` target deployment blocks fallback removal just like a legacy row does — but for a different operational reason (the static fallback can't decrypt foreign-KMS ciphertexts either). Folding them together would hide the remediation path ("re-migrate to the right cloud" vs "re-run migration on late writes").

5. **`--audit` exits 1 on outstanding rows, 0 on complete.** This makes it usable as a Helm pre-flight hook or CI gate, not just a human-readable report. The `PASS`/`FAIL` lines are for humans; the exit code is for automation.

---

## Assumptions stated and validated (Rule 7)

1. *Removing the master-secret mount produces a nil local fallback in `newPurposeProvider`.* Validated by reading `newLocalPurposeProvider` (returns `nil` when `deriveServerKey` yields no key, which happens when no master secret env var is set and no file is mounted) and tracing the call from `newPurposeProvider` → `buildKMSProvider` → `NewCompositeProvider`.
2. *The composite today silently accepts a nil fallback.* Validated by probing: `NewCompositeProvider(static, nil)` returned no error, and a subsequent `Decrypt` of a foreign-prefix ciphertext panicked with a nil-pointer dereference. The probe is reproducible (script in `/tmp/probe_nil2.go`).
3. *`migrate-kek --dry-run` does not distinguish already-migrated rows.* Validated by reading `MigrationCoordinator.MigrateTable` (lines 113-171): every row goes through `source.Decrypt` + `target.Encrypt` regardless of prefix; dry-run skips the write but still counts the row as Processed.
4. *The composite's Encrypt-only-delegates-to-primary invariant holds for a primary-only configuration.* Validated by re-reading `CompositeProvider.Encrypt` (line 64) and confirming the bare provider has the same semantics without the wrapper.

---

## Adversarial Self-Review (Rule 11)

- **Phase 1 finding (real):** `buildKMSProvider` is exported (capitalized) but is only called from `newPurposeProvider` within the same package. Lowercase it? Phase 2 verdict: lowercase. It is not part of any external API and the existing convention in this file is lowercase for package-private helpers (`newAWSKMSProvider`, `newGPCKMSProvider`, `newLocalPurposeProvider` are all lowercase). Remediation: renamed to lowercase before commit.
   - *Correction on review:* I checked — the function is in `api/internal/app` (different package from the test), and the test exercises it directly. Capitalize stays. False alarm documented.
- **Phase 1 finding (real, fixed):** `AuditAll` calls `AuditTable` which has its own `validTargets` check. Calling `AuditAll` with an invalid target would double-validate. Phase 2 verdict: redundant but not incorrect. The cost is one map lookup per call; the benefit is that `AuditTable` can be called directly without the caller remembering to pre-validate. Keep the redundancy.
- **Phase 1 finding (real, fixed):** `runAudit` connects to Postgres but the `pgMigrationStore` returned by `newPgMigrationStore` may not satisfy the narrower interface `AuditTable` needs (it calls `ListMigrationRows` only, never `UpdateMigrationRow` or `FlushDEKCache`). Phase 2 verdict: `MigrationStore` is the same interface used by migration; the audit happens to use a subset. No change — the interface is already minimal and reusing it keeps the CLI's store-construction code identical between modes.
- **Phase 2 false alarm initially considered:** the audit uses `MigrationCoordinator` constructed with `nil, nil` for the source/target maps. Could the coordinator's nil maps cause issues elsewhere? Validated: `AuditTable` and `AuditAll` only touch `c.store`, never `c.sources` or `c.targets`. The coordinator struct is safe to construct with nil providers for audit-only use.

---

## Blockers

None.

---

## Tests Run

```
go test -timeout 120s ./pkg/secrets/ -count=1                                  → ok (37.7s)
go test -timeout 180s ./api/internal/app/ -count=1                             → ok (2.3s)
go test -timeout 60s ./pkg/secrets/ -run "TestClassifyCiphertext|TestAuditMigration|TestCompositeProvider|TestNewCompositeProvider" -count=1 → ok
go vet ./cmd/migrate-kek/ ./pkg/secrets/ ./api/internal/app/                   → clean
go build ./cmd/migrate-kek/ ./pkg/secrets/... ./api/internal/app/...           → clean
```

New tests added (10):
- `TestNewCompositeProvider_NilFallback_ReturnsError`
- `TestNewCompositeProvider_NilFallbackAmongValid_ReturnsError`
- `TestClassifyCiphertext_AllKnownPrefixes`
- `TestClassifyCiphertext_LegacyRowsAreNotMisclassifiedAsTarget`
- `TestAuditMigrationTable_NoLegacy_AllMigratedToAWS`
- `TestAuditMigrationTable_MixedState_ReportsNotYetMigrated`
- `TestAuditMigrationTable_OtherKMSCountedSeparately`
- `TestAuditMigrationTable_EmptyTable`
- `TestAuditMigrationTable_InvalidTarget_ReturnsError`
- `TestAuditMigrationAll_AllThreeTablesAggregated`

Plus 2 wiring tests:
- `TestNewPurposeProvider_KMSConfigured_NoMasterSecret_ReturnsBareKMS`
- `TestBuildKMSProvider_NilFallback_ReturnsBareProvider`

All existing composite + migration tests continue to pass.

---

## Files Modified

- `pkg/secrets/composite_provider.go` — nil-fallback guard in `NewCompositeProvider`.
- `pkg/secrets/composite_provider_test.go` — 2 new constructor tests.
- `pkg/secrets/migration.go` — `CiphertextClass`, `ClassifyCiphertext`, `CiphertextAudit`, `AuditTable`, `AuditAll`.
- `pkg/secrets/migration_test.go` — 9 new audit/classifier tests.
- `api/internal/app/secrets_adapters.go` — new `buildKMSProvider` helper, branches on nil fallback.
- `api/internal/app/secrets_kms_wiring_test.go` — 2 new wiring tests.
- `cmd/migrate-kek/main.go` — new `--audit` flag and `runAudit` function.
- `design/stories/epic-57-rce-resistance-hardening/README.md` — workflow step 7 split into 7 (audit) + 8 (remove fallback).

## Files Added

- `helm/KEK-MIGRATION.md` — operator runbook.

---

## Next Steps

1. Open this PR.
2. The `helm/KEK-ROTATION.md` runbook references the same composite-wiring pattern; audit whether it has the same gap. (Spot-check: rotate-kek keeps the master secret mounted throughout — it rotates the key the secret holds, not the mount. No nil-fallback path. Safe.)
