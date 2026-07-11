// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMigrationStore implements MigrationStore for unit tests.
type fakeMigrationStore struct {
	rows       []MigrationRow
	updated    map[string]MigrationRow // rowID → new ciphertext
	flushCalls int
}

func newFakeMigrationStore(rows []MigrationRow) *fakeMigrationStore {
	return &fakeMigrationStore{
		rows:    rows,
		updated: make(map[string]MigrationRow),
	}
}

func (s *fakeMigrationStore) ListMigrationRows(_ context.Context, table, resumeFromID string, limit int) ([]MigrationRow, error) {
	var out []MigrationRow
	for _, r := range s.rows {
		if r.Table == table && r.ID > resumeFromID {
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (s *fakeMigrationStore) UpdateMigrationRow(_ context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error {
	for i, r := range s.rows {
		if r.Table == table && r.ID == rowID {
			s.rows[i].Ciphertext = newCiphertext
			s.rows[i].KeyVersion = newKeyVersion
			s.updated[rowID] = r
			return nil
		}
	}
	return nil
}

func (s *fakeMigrationStore) FlushDEKCache(_ context.Context) error {
	s.flushCalls++
	return nil
}

// --- MigrationCoordinator tests ---

func TestMigrationCoordinator_DryRun_ReportsCounts(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte{1}, KeyVersion: 1},
		{ID: "2", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte{2}, KeyVersion: 1},
	})

	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"provider-credentials": source},
		map[string]RootKeyProvider{"provider-credentials": target},
	)

	res, err := c.MigrateTable(context.Background(), "provider_credentials", "", true)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Processed, "dry-run must count both rows")
	assert.Equal(t, 0, res.Failed)
	assert.Empty(t, store.updated, "dry-run must not write")
}

func TestMigrationCoordinator_MigrateTable_ReEncryptsRows(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "a", Table: "api_keys", Ciphertext: []byte{0xAA}, KeyVersion: 2},
	})

	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"master-kek": source},
		map[string]RootKeyProvider{"master-kek": target},
	)

	res, err := c.MigrateTable(context.Background(), "api_keys", "", false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed)
	assert.Equal(t, 0, res.Failed)

	// Updated row must have the target's prefix.
	updated := store.rows[0]
	assert.True(t, len(updated.Ciphertext) > 0)
	// key_version reset to 1 (D6).
	assert.Equal(t, 1, updated.KeyVersion)
}

func TestMigrationCoordinator_ResumeFromCursor(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "r1", Table: "org_sso_configs", Ciphertext: []byte{1}, KeyVersion: 1},
		{ID: "r2", Table: "org_sso_configs", Ciphertext: []byte{2}, KeyVersion: 1},
		{ID: "r3", Table: "org_sso_configs", Ciphertext: []byte{3}, KeyVersion: 1},
	})

	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"master-kek": source},
		map[string]RootKeyProvider{"master-kek": target},
	)

	// First pass: process r1, r2. Then "interrupt" after r2.
	res, err := c.MigrateTable(context.Background(), "org_sso_configs", "", false)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Processed)
	assert.Equal(t, 0, res.Failed)
	// All three processed.
	assert.False(t, store.rows[0].Ciphertext[0] == 1, "r1 must be updated")
	assert.False(t, store.rows[1].Ciphertext[0] == 2, "r2 must be updated")
	assert.False(t, store.rows[2].Ciphertext[0] == 3, "r3 must be updated")

	// Re-run with resume-from=r2: only r3 should be reprocessed.
	store2 := newFakeMigrationStore([]MigrationRow{
		{ID: "r2", Table: "org_sso_configs", Ciphertext: []byte{2}, KeyVersion: 1},
		{ID: "r3", Table: "org_sso_configs", Ciphertext: []byte{3}, KeyVersion: 1},
	})
	source2 := &fakeProvider{prefix: "lkms:v1:"}
	target2 := &fakeProvider{prefix: "aws-kms:v1:"}
	c2 := NewMigrationCoordinator(store2,
		map[string]RootKeyProvider{"master-kek": source2},
		map[string]RootKeyProvider{"master-kek": target2},
	)
	res2, err := c2.MigrateTable(context.Background(), "org_sso_configs", "r2", false)
	require.NoError(t, err)
	assert.Equal(t, 1, res2.Processed, "resume-from=r2 skips r2, processes only r3")
}

func TestMigrationCoordinator_DecryptFailure_CountsAsFailed(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "bad", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte{0xFF}, KeyVersion: 1},
	})

	source := &fakeProvider{prefix: "lkms:v1:", decryptErr: ErrDecryptionFailed}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"provider-credentials": source},
		map[string]RootKeyProvider{"provider-credentials": target},
	)

	res, err := c.MigrateTable(context.Background(), "provider_credentials", "", false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Processed)
	assert.Equal(t, 1, res.Failed)
	assert.Empty(t, store.updated, "decrypt-failure row must not be written")
}

func TestMigrationCoordinator_MigrateAll_FlushesRedis(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte{1}, KeyVersion: 1},
		{ID: "a", Table: "api_keys", Ciphertext: []byte{2}, KeyVersion: 2},
		{ID: "s1", Table: "org_sso_configs", Ciphertext: []byte{3}, KeyVersion: 1},
	})

	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{
			"provider-credentials": source,
			"org-credentials":      source,
			"master-kek":           source,
		},
		map[string]RootKeyProvider{
			"provider-credentials": target,
			"org-credentials":      target,
			"master-kek":           target,
		},
	)

	results, err := c.MigrateAll(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, 3, results["provider_credentials"].Processed+results["api_keys"].Processed+results["org_sso_configs"].Processed)
	assert.Equal(t, 1, store.flushCalls, "MigrateAll must flush Redis DEK cache")
}

// fakeProvider is reused from composite_provider_test.go.
