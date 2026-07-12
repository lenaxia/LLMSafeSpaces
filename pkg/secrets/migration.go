// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"fmt"
)

// MigrationRow is a generic row from any KEK-protected table. The
// coordinator re-encrypts Ciphertext from the source format (any provider
// — local or KMS) to the target format (KMS only).
type MigrationRow struct {
	ID         string
	Table      string
	OwnerType  string
	Ciphertext []byte
	KeyVersion int
}

// KEKMigrationResult summarizes a migration run.
type KEKMigrationResult struct {
	Processed int
	Failed    int
	Errors    []KEKMigrationError
}

// KEKMigrationError records a per-row failure.
type KEKMigrationError struct {
	RowID string
	Table string
	Error error
}

// MigrationStore abstracts the three KEK-protected tables for the
// migration CLI. Each method returns rows in a consistent order (by
// ID ASC). Mirrors RotationStore — see rotation.go.
type MigrationStore interface {
	// ListMigrationRows returns rows from the given table, ordered by
	// ID ASC, starting after resumeFromID (empty = from the beginning).
	// limit caps the batch size (0 = unlimited).
	ListMigrationRows(ctx context.Context, table, resumeFromID string, limit int) ([]MigrationRow, error)

	// UpdateMigrationRow writes newCiphertext + newKeyVersion atomically
	// for the given row. Each call is its own transaction.
	UpdateMigrationRow(ctx context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error

	// FlushDEKCache flushes the Redis DEK cache so stale DEKs (wrapped
	// under the old KEK) are evicted.
	FlushDEKCache(ctx context.Context) error
}

// purposeForMigrationRow returns the HKDF purpose string for a
// MigrationRow. Mirrors purposeForTable.
func purposeForMigrationRow(row MigrationRow) string {
	switch row.Table {
	case "provider_credentials":
		if row.OwnerType == "org" {
			return "org-credentials"
		}
		return "provider-credentials"
	case "api_keys":
		return "master-kek"
	case "org_sso_configs":
		return "master-kek"
	default:
		return ""
	}
}

// MigrationCoordinator re-encrypts KEK-protected rows from source
// (CompositeProvider: KMS-primary + local-fallback) to target
// (KMS-only provider). It is table-agnostic and supports dry-run
// + resume-from-cursor for safe operation.
type MigrationCoordinator struct {
	store   MigrationStore
	sources map[string]RootKeyProvider // purpose → CompositeProvider (decrypt)
	targets map[string]RootKeyProvider // purpose → KMS provider (encrypt)
}

// NewMigrationCoordinator constructs a coordinator. Each map is
// keyed by purpose string and should contain the same set of keys.
func NewMigrationCoordinator(
	store MigrationStore,
	sources map[string]RootKeyProvider,
	targets map[string]RootKeyProvider,
) *MigrationCoordinator {
	return &MigrationCoordinator{
		store:   store,
		sources: sources,
		targets: targets,
	}
}

// MigrateTable re-encrypts every row in the given table from source
// format to target format. If dryRun is true, no writes occur — only
// counts are reported. resumeFromID allows resuming after an
// interrupted run.
func (c *MigrationCoordinator) MigrateTable(ctx context.Context, table, resumeFromID string, dryRun bool) (KEKMigrationResult, error) {
	result := KEKMigrationResult{}
	batchSize := 100

	for {
		rows, err := c.store.ListMigrationRows(ctx, table, resumeFromID, batchSize)
		if err != nil {
			return result, fmt.Errorf("list %s rows: %w", table, err)
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			resumeFromID = row.ID

			purpose := purposeForMigrationRow(row)
			source, ok := c.sources[purpose]
			if !ok || source == nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKMigrationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("no source provider for purpose %q", purpose),
				})
				continue
			}
			target, ok := c.targets[purpose]
			if !ok || target == nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKMigrationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("no target provider for purpose %q", purpose),
				})
				continue
			}

			plaintext, err := source.Decrypt(ctx, row.Ciphertext)
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKMigrationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("decrypt: %w", err),
				})
				continue
			}

			if dryRun {
				result.Processed++
				continue
			}

			newCT, err := target.Encrypt(ctx, plaintext)
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKMigrationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("encrypt: %w", err),
				})
				continue
			}

			// key_version is reset to 1 under KMS — it's cosmetic (D6).
			if err := c.store.UpdateMigrationRow(ctx, table, row.ID, newCT, 1); err != nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKMigrationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("update row: %w", err),
				})
				continue
			}
			result.Processed++
		}

		if len(rows) < batchSize {
			break
		}
	}

	return result, nil
}

// MigrateAll re-encrypts all three tables sequentially. The Redis DEK
// cache is flushed after all tables complete successfully.
func (c *MigrationCoordinator) MigrateAll(ctx context.Context, dryRun bool) (map[string]KEKMigrationResult, error) {
	tables := []string{"provider_credentials", "api_keys", "org_sso_configs"}
	results := make(map[string]KEKMigrationResult, len(tables))

	for _, table := range tables {
		res, err := c.MigrateTable(ctx, table, "", dryRun)
		if err != nil {
			return results, fmt.Errorf("migrate %s: %w", table, err)
		}
		results[table] = res
	}

	if !dryRun {
		if err := c.store.FlushDEKCache(ctx); err != nil {
			return results, fmt.Errorf("flush DEK cache: %w", err)
		}
	}

	return results, nil
}

// --- Post-migration verification (the cleanup gate) ---
//
// After `migrate-kek` reports success, an operator needs to confirm every
// KEK-protected row actually carries the target KMS prefix BEFORE removing
// the static fallback from the composite provider (Epic 57 US-57.2 workflow
// step 7: "Remove Static fallback"). The migration CLI's `--dry-run` is NOT
// this check — it re-processes every row regardless of prefix, so an
// already-migrated row and a still-legacy row both count as "Processed."
// That conflates "I could migrate this row" with "this row is already done."
//
// The audit below classifies rows by ciphertext prefix and returns the count
// of not-yet-migrated rows. That count == 0 is the actual gate.

// CiphertextClass categorizes a row's ciphertext by which provider wrote it.
type CiphertextClass int

const (
	// ClassLegacy is an un-prefixed raw blob — the pre-US-57.1 production
	// format produced by Static/Sealed providers before the composite added
	// self-identifying prefixes.
	ClassLegacy CiphertextClass = iota
	// ClassLocal is an `lkms:v1:`-prefixed ciphertext written by a local
	// provider (Static/Sealed) after US-57.1.
	ClassLocal
	// ClassAWSKMS is an `aws-kms:v1:`-prefixed ciphertext from AWSKMSProvider.
	ClassAWSKMS
	// ClassGCPKMS is a `gcp-kms:v1:`-prefixed ciphertext from GPCKMSProvider.
	ClassGCPKMS
)

// String returns the human-readable class name for log/CLI output.
func (c CiphertextClass) String() string {
	switch c {
	case ClassLegacy:
		return "legacy"
	case ClassLocal:
		return "local"
	case ClassAWSKMS:
		return "aws-kms"
	case ClassGCPKMS:
		return "gcp-kms"
	default:
		return "unknown"
	}
}

// ClassifyCiphertext inspects a ciphertext's prefix and returns the
// provider class that wrote it. Side-effect-free — does not call any
// provider, does not decrypt, does not touch the network. Safe to call
// on every row in the database at any time.
//
// Classification is by prefix only. A corrupt ciphertext with a valid
// prefix (e.g. `aws-kms:v1:` followed by garbage) still classifies as
// ClassAWSKMS — that's the same ambiguity Decrypt has, and the audit's
// job is prefix accounting, not integrity checking. Integrity is the
// decrypt path's responsibility.
func ClassifyCiphertext(ciphertext []byte) CiphertextClass {
	switch {
	case bytes.HasPrefix(ciphertext, []byte(awsKMSCiphertextPrefix)):
		return ClassAWSKMS
	case bytes.HasPrefix(ciphertext, []byte(gcpKMSCiphertextPrefix)):
		return ClassGCPKMS
	case bytes.HasPrefix(ciphertext, []byte(staticCiphertextPrefix)):
		return ClassLocal
	default:
		return ClassLegacy
	}
}

// validTargets is the set of strings accepted as the `target` argument
// to AuditTable/AuditAll. Matched against CiphertextClass.String() so
// the operator-facing flag value ("aws-kms", "gcp-kms") is the same
// string the classifier returns. A typo here ("awd-kms") fails loudly
// rather than silently classifying every row as non-target.
var validTargets = map[string]bool{
	"aws-kms": true,
	"gcp-kms": true,
}

// CiphertextAudit summarizes the prefix distribution of one table after
// (or during) a migration. Used as the gate for removing the static
// fallback from the composite provider.
type CiphertextAudit struct {
	Table    string
	Total    int
	Target   int // rows whose prefix matches the migration target
	Legacy   int // un-prefixed pre-US-57.1 rows
	Local    int // lkms:v1:-prefixed rows (local provider, post-US-57.1)
	OtherKMS int // rows with a KMS prefix that isn't the target
}

// IsComplete returns true when every row in the table is on the target
// KMS provider. This is the safe-to-remove-static-fallback condition:
// the composite's static fallback can decrypt only legacy + lkms rows;
// any non-zero Legacy/Local/OtherKMS count means the fallback is still
// load-bearing.
func (a CiphertextAudit) IsComplete() bool {
	return a.Total > 0 && a.Target == a.Total || a.Total == 0
}

// Outstanding returns the count of rows the static fallback still owns
// (Legacy + Local + OtherKMS). Equivalent to Total - Target but named
// for the operational question.
func (a CiphertextAudit) Outstanding() int {
	return a.Total - a.Target
}

// AuditTable walks the given table and returns the prefix distribution.
// `target` is the operator's intended final KMS ("aws-kms" or "gcp-kms")
// and determines which prefix counts as Target vs OtherKMS. No writes,
// no provider calls, no decrypts — pure prefix accounting. Safe to run
// at any time, including against a live deployment mid-traffic.
func (c *MigrationCoordinator) AuditTable(ctx context.Context, table, target string) (CiphertextAudit, error) {
	if !validTargets[target] {
		return CiphertextAudit{}, fmt.Errorf("invalid target %q: must be one of aws-kms, gcp-kms", target)
	}
	audit := CiphertextAudit{Table: table}
	batchSize := 100
	resumeFromID := ""

	for {
		rows, err := c.store.ListMigrationRows(ctx, table, resumeFromID, batchSize)
		if err != nil {
			return audit, fmt.Errorf("list %s rows: %w", table, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			resumeFromID = row.ID
			audit.Total++
			switch ClassifyCiphertext(row.Ciphertext) {
			case ClassAWSKMS:
				if target == "aws-kms" {
					audit.Target++
				} else {
					audit.OtherKMS++
				}
			case ClassGCPKMS:
				if target == "gcp-kms" {
					audit.Target++
				} else {
					audit.OtherKMS++
				}
			case ClassLocal:
				audit.Local++
			case ClassLegacy:
				audit.Legacy++
			}
		}
		if len(rows) < batchSize {
			break
		}
	}
	return audit, nil
}

// AuditAll walks all three KEK-protected tables and returns their audits
// as a map keyed by table name. The static fallback can be safely removed
// only when every table's IsComplete() returns true.
func (c *MigrationCoordinator) AuditAll(ctx context.Context, target string) (map[string]CiphertextAudit, error) {
	if !validTargets[target] {
		return nil, fmt.Errorf("invalid target %q: must be one of aws-kms, gcp-kms", target)
	}
	tables := []string{"provider_credentials", "api_keys", "org_sso_configs"}
	out := make(map[string]CiphertextAudit, len(tables))
	for _, table := range tables {
		audit, err := c.AuditTable(ctx, table, target)
		if err != nil {
			return out, fmt.Errorf("audit %s: %w", table, err)
		}
		out[table] = audit
	}
	return out, nil
}
