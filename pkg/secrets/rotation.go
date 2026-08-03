// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
)

// RotationRow is a generic row from any KEK-protected table. The coordinator
// re-wraps Ciphertext from oldKeyVersion to newKeyVersion.
type RotationRow struct {
	ID         string
	Table      string // "provider_credentials", "api_keys", "org_sso_configs", "user_keys"
	OwnerType  string // for provider_credentials: "admin" or "org"; "" for other tables
	Ciphertext []byte
	KeyVersion int
	// DEKSource is users.dek_source for user_keys rows ("server_kek" / "passkey"
	// / "password"). Password-tier user_keys rows are NOT rotatable by the KEK
	// rotation CLI (their wrap key is the server master KEK, which the
	// CLI does not hold) and MUST be excluded by the RotationStore's
	// ListRotationRows("user_keys", ...). Empty for non-user_keys tables.
	DEKSource string
}

// KEKRotationResult summarizes a rotation run.
type KEKRotationResult struct {
	Processed int
	Skipped   int // rows already at target version
	Failed    int
	Errors    []KEKRotationError
}

// KEKRotationError records a per-row failure.
type KEKRotationError struct {
	RowID string
	Table string
	Error error
}

// RotationStore abstracts the three KEK-protected tables for the rotation CLI.
// Each method returns rows in a consistent order (by ID ASC) so resume-from-
// cursor works deterministically.
type RotationStore interface {
	// ListRotationRows returns rows from the given table whose key_version is
	// below targetVersion, ordered by ID ASC, starting after resumeFromID
	// (empty = from the beginning). limit caps the batch size (0 = unlimited).
	ListRotationRows(ctx context.Context, table, resumeFromID string, targetVersion, limit int) ([]RotationRow, error)

	// UpdateRotationRow re-wraps a single row: writes newCiphertext and
	// newKeyVersion atomically. Each call is its own transaction.
	UpdateRotationRow(ctx context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error

	// FlushDEKCache flushes the Redis DEK cache so stale DEKs (wrapped under
	// the old KEK) are evicted.
	FlushDEKCache(ctx context.Context) error
}

// purposeForTable returns the HKDF purpose string for a given table + owner_type.
// The CLI uses this to select the right old/new provider pair.
func purposeForTable(row RotationRow) string {
	switch row.Table {
	case "provider_credentials":
		if row.OwnerType == "org" {
			return "org-credentials"
		}
		return "provider-credentials"
	case "api_keys":
		// US-50.7: new rows use "master-kek"; legacy rows (v1) use "dek-cache".
		// The old provider set must include both for decryption to succeed.
		return "master-kek"
	case "org_sso_configs":
		return "dek-cache"
	case "user_keys":
		// Only server_kek/passkey rows reach here (password-tier rows are
		// excluded by the store — see RotationRow.DEKSource). Both wrap the DEK
		// under the same master-kek purpose as api_keys, so they re-wrap with
		// the master-kek provider.
		return "master-kek"
	default:
		return ""
	}
}

// RotationCoordinator re-wraps KEK-protected rows from old providers to new
// providers. It is table-agnostic: the caller provides the store + provider sets.
type RotationCoordinator struct {
	store        RotationStore
	oldProviders map[string]RootKeyProvider // purpose → provider (decrypt)
	newProviders map[string]RootKeyProvider // purpose → provider (encrypt)
}

// NewRotationCoordinator constructs a coordinator with the old (source) and
// new (target) provider sets. Each map is keyed by purpose string.
func NewRotationCoordinator(store RotationStore, oldProviders, newProviders map[string]RootKeyProvider) *RotationCoordinator {
	return &RotationCoordinator{
		store:        store,
		oldProviders: oldProviders,
		newProviders: newProviders,
	}
}

// RotateTable rotates all rows in the given table that are below targetVersion.
// If dryRun is true, no writes occur — only counts are reported. resumeFromID
// allows resuming from a specific row ID after an interrupted run.
func (c *RotationCoordinator) RotateTable(ctx context.Context, table, resumeFromID string, targetVersion int, dryRun bool) (KEKRotationResult, error) {
	result := KEKRotationResult{}
	batchSize := 100

	for {
		rows, err := c.store.ListRotationRows(ctx, table, resumeFromID, targetVersion, batchSize)
		if err != nil {
			return result, fmt.Errorf("list %s rows: %w", table, err)
		}
		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			resumeFromID = row.ID

			if row.KeyVersion >= targetVersion {
				result.Skipped++
				continue
			}

			purpose := purposeForTable(row)
			oldProv, ok := c.oldProviders[purpose]
			if !ok || oldProv == nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKRotationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("no old provider for purpose %q", purpose),
				})
				continue
			}
			newProv, ok := c.newProviders[purpose]
			if !ok || newProv == nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKRotationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("no new provider for purpose %q", purpose),
				})
				continue
			}

			plaintext, err := oldProv.Decrypt(ctx, row.Ciphertext)
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKRotationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("decrypt: %w", err),
				})
				continue
			}

			if dryRun {
				result.Processed++
				continue
			}

			newCT, err := newProv.Encrypt(ctx, plaintext)
			if err != nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKRotationError{
					RowID: row.ID, Table: table,
					Error: fmt.Errorf("encrypt: %w", err),
				})
				continue
			}

			if err := c.store.UpdateRotationRow(ctx, table, row.ID, newCT, targetVersion); err != nil {
				result.Failed++
				result.Errors = append(result.Errors, KEKRotationError{
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

// RotateAll rotates all KEK-protected tables sequentially. The Redis DEK cache is
// flushed after all tables complete successfully.
//
// "user_keys" is included (Epic 58): the store's ListRotationRows("user_keys")
// returns ONLY server_kek/passkey rows — password-tier rows are excluded because
// they wrap the DEK with a server-side master KEK the rotation CLI re-wraps.
func (c *RotationCoordinator) RotateAll(ctx context.Context, targetVersion int, dryRun bool) (map[string]KEKRotationResult, error) {
	tables := []string{"provider_credentials", "api_keys", "org_sso_configs", "user_keys"}
	results := make(map[string]KEKRotationResult, len(tables))

	for _, table := range tables {
		res, err := c.RotateTable(ctx, table, "", targetVersion, dryRun)
		if err != nil {
			return results, fmt.Errorf("rotate %s: %w", table, err)
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
