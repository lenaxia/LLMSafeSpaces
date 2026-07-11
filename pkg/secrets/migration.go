// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
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
	Skipped   int
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
