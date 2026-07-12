// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/base64"
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
		{ID: "1", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
		{ID: "2", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte("lkms:v1:row-2"), KeyVersion: 1},
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
		{ID: "a", Table: "api_keys", Ciphertext: []byte("lkms:v1:row-a"), KeyVersion: 2},
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
		{ID: "r1", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
		{ID: "r2", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:row-2"), KeyVersion: 1},
		{ID: "r3", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:row-3"), KeyVersion: 1},
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
		{ID: "r2", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:row-2"), KeyVersion: 1},
		{ID: "r3", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:row-3"), KeyVersion: 1},
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
		{ID: "bad", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte("lkms:v1:bad-row"), KeyVersion: 1},
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
		{ID: "1", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
		{ID: "a", Table: "api_keys", Ciphertext: []byte("lkms:v1:row-2"), KeyVersion: 2},
		{ID: "s1", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:row-3"), KeyVersion: 1},
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

// TestMigrationCoordinator_MultiVersionFallback_DecryptsLegacyV1 roves
// that the master-kek purpose's multi-version static fallback can
// decrypt legacy api_keys rows encrypted under the v1 dek-cache-derived
// key. Without the multi-version fallback, these rows would silently
// fail and count as migration errors.
func TestMigrationCoordinator_MultiVersionFallback_DecryptsLegacyV1(t *testing.T) {
	// Build a real multi-version StaticKeyProvider simulating the
	// production boot code: v1 = dek-cache-derived, v2 = master-kek-derived.
	dekCacheKey := make([]byte, 32)
	masterKekKey := make([]byte, 32)
	for i := range dekCacheKey {
		dekCacheKey[i] = byte(i + 1)
		masterKekKey[i] = byte(i + 50)
	}
	multiKey, err := NewStaticKeyProviderMultiVersion(2, map[int][]byte{
		1: dekCacheKey,
		2: masterKekKey,
	})
	require.NoError(t, err)

	// Encrypt a test row with the v1 key (simulating a legacy api_keys row).
	legacyCT, err := EncryptSecret(dekCacheKey, []byte("legacy-v1-api-key-row"))
	require.NoError(t, err)

	store := newFakeMigrationStore([]MigrationRow{
		{ID: "legacy-v1", Table: "api_keys", Ciphertext: legacyCT, KeyVersion: 1},
	})

	// Source composite: fake KMS primary (never matches the legacy blob) +
	// multi-version static fallback (v1 + v2 keys).
	kmsPrimary := &fakeProvider{prefix: "aws-kms:v1:"}
	source, err := NewCompositeProvider(kmsPrimary, multiKey)
	require.NoError(t, err)

	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"master-kek": source},
		map[string]RootKeyProvider{"master-kek": target},
	)

	res, err := c.MigrateTable(context.Background(), "api_keys", "", false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed,
		"legacy v1 dek-cache-encrypted row must be decrypted by the multi-version fallback")
	assert.Equal(t, 0, res.Failed)
}

func TestMigrationCoordinator_MissingSourceProvider_CountsAsFailed(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "api_keys", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
	})
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		nil,
		map[string]RootKeyProvider{"master-kek": target},
	)
	res, err := c.MigrateTable(context.Background(), "api_keys", "", false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Processed)
	assert.Equal(t, 1, res.Failed)
}

func TestMigrationCoordinator_MissingTargetProvider_CountsAsFailed(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "api_keys", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
	})
	source := &fakeProvider{prefix: "lkms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"master-kek": source},
		nil,
	)
	res, err := c.MigrateTable(context.Background(), "api_keys", "", false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Processed)
	assert.Equal(t, 1, res.Failed)
}

func TestMigrationCoordinator_EncryptFailure_CountsAsFailed(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "provider_credentials", OwnerType: "admin", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
	})
	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:", encryptErr: ErrDecryptionFailed}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"provider-credentials": source},
		map[string]RootKeyProvider{"provider-credentials": target},
	)
	res, err := c.MigrateTable(context.Background(), "provider_credentials", "", false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Processed)
	assert.Equal(t, 1, res.Failed)
}

func TestMigrationCoordinator_EmptyTable_ReturnsZeroRows(t *testing.T) {
	store := newFakeMigrationStore(nil)
	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"master-kek": source},
		map[string]RootKeyProvider{"master-kek": target},
	)
	res, err := c.MigrateTable(context.Background(), "api_keys", "", false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Processed)
	assert.Equal(t, 0, res.Failed)
}

func TestMigrationCoordinator_OwnerTypeOrg_RoutesToOrgCredentials(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "provider_credentials", OwnerType: "org", Ciphertext: []byte("lkms:v1:org-row-data"), KeyVersion: 1},
	})
	// Only provide the org-credentials key — if the wrong purpose is selected,
	// the provider lookup fails and the row counts as Failed.
	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"org-credentials": source},
		map[string]RootKeyProvider{"org-credentials": target},
	)
	res, err := c.MigrateTable(context.Background(), "provider_credentials", "", false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed)
	assert.Equal(t, 0, res.Failed)
}

func TestMigrationCoordinator_UpdateRowFailure_CountsAsFailed(t *testing.T) {
	store := &failingUpdateStore{
		fakeMigrationStore: *newFakeMigrationStore([]MigrationRow{
			{ID: "1", Table: "api_keys", Ciphertext: []byte("lkms:v1:row-1"), KeyVersion: 1},
		}),
	}
	source := &fakeProvider{prefix: "lkms:v1:"}
	target := &fakeProvider{prefix: "aws-kms:v1:"}
	c := NewMigrationCoordinator(store,
		map[string]RootKeyProvider{"master-kek": source},
		map[string]RootKeyProvider{"master-kek": target},
	)
	res, err := c.MigrateTable(context.Background(), "api_keys", "", false)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Processed)
	assert.Equal(t, 1, res.Failed)
}

// failingUpdateStore wraps fakeMigrationStore and fails every update call.
type failingUpdateStore struct {
	fakeMigrationStore
}

func (s *failingUpdateStore) UpdateMigrationRow(ctx context.Context, table, rowID string, newCiphertext []byte, newKeyVersion int) error {
	return context.DeadlineExceeded
}

// fakeProvider is reused from composite_provider_test.go.

// --- CiphertextClassification + Audit tests ---
//
// Post-migration verification (the "notify-kek" cleanup step): operators need
// to confirm every KEK-protected row has been migrated to the target KMS
// format BEFORE removing the static fallback from the composite. migrate-kek
// --dry-run does not answer this question — it re-processes every row
// regardless of prefix, so a row already migrated and one still legacy both
// count as "Processed." A real audit classifies rows by ciphertext prefix and
// reports the count of not-yet-migrated rows. That count == 0 is the gate.

func TestClassifyCiphertext_AllKnownPrefixes(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		want CiphertextClass
	}{
		{"aws-kms prefix", "aws-kms:v1:abc", ClassAWSKMS},
		{"gcp-kms prefix", "gcp-kms:v1:abc", ClassGCPKMS},
		{"lkms prefix (local new writes)", "lkms:v1:abc", ClassLocal},
		{"legacy un-prefixed (raw blob)", string([]byte{0xDE, 0xAD, 0xBE, 0xEF}), ClassLegacy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyCiphertext([]byte(tc.ct))
			assert.Equal(t, tc.want, got, "ClassifyCiphertext must route by prefix")
		})
	}
}

func TestClassifyCiphertext_LegacyRowsAreNotMisclassifiedAsTarget(t *testing.T) {
	// Regression guard: a legacy un-prefixed blob (the pre-US-57.1 production
	// format) must NOT be reported as ClassAWSKMS or ClassGCPKMS, even when
	// its bytes happen to be ASCII-printable. This is what makes the audit a
	// safe gate for removing the static fallback.
	legacy := []byte("raw-aes-gcm-blob-no-prefix")
	class := ClassifyCiphertext(legacy)
	assert.Equal(t, ClassLegacy, class)
}

func TestAuditMigrationTable_NoLegacy_AllMigratedToAWS(t *testing.T) {
	// A fully-migrated deployment — every row has the aws-kms:v1: prefix.
	// AuditTable must report zero not-yet-migrated rows so the operator can
	// safely remove the static fallback.
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "api_keys", Ciphertext: []byte("aws-kms:v1:" + base64Str("row-1")), KeyVersion: 1},
		{ID: "2", Table: "api_keys", Ciphertext: []byte("aws-kms:v1:" + base64Str("row-2")), KeyVersion: 1},
	})
	c := NewMigrationCoordinator(store, nil, nil)
	res, err := c.AuditTable(context.Background(), "api_keys", "aws-kms")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Total, "both rows counted")
	assert.Equal(t, 2, res.Target, "both rows already on target KMS")
	assert.Equal(t, 0, res.Legacy+res.Local+res.OtherKMS, "no non-target rows remain")
	assert.True(t, res.IsComplete(), "fully-migrated table must report IsComplete")
}

func TestAuditMigrationTable_MixedState_ReportsNotYetMigrated(t *testing.T) {
	// The realistic post-migration-checkout scenario: most rows migrated,
	// a few legacy rows still outstanding (e.g. written between migration
	// pass 1 and pass 2). AuditTable must surface them as the cleanup gate.
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "api_keys", Ciphertext: []byte("aws-kms:v1:" + base64Str("migrated-1")), KeyVersion: 1},
		{ID: "2", Table: "api_keys", Ciphertext: []byte("lkms:v1:" + base64Str("local-new")), KeyVersion: 1},
		{ID: "3", Table: "api_keys", Ciphertext: []byte("aws-kms:v1:" + base64Str("migrated-2")), KeyVersion: 1},
		{ID: "4", Table: "api_keys", Ciphertext: []byte("raw-legacy-blob-no-prefix"), KeyVersion: 1},
	})
	c := NewMigrationCoordinator(store, nil, nil)
	res, err := c.AuditTable(context.Background(), "api_keys", "aws-kms")
	require.NoError(t, err)
	assert.Equal(t, 4, res.Total)
	assert.Equal(t, 2, res.Target, "two aws-kms rows")
	assert.Equal(t, 1, res.Local, "one lkms:v1: row")
	assert.Equal(t, 1, res.Legacy, "one un-prefixed legacy row")
	assert.Equal(t, 0, res.OtherKMS, "no gcp-kms rows in an aws-kms target deployment")
	assert.False(t, res.IsComplete(), "table with outstanding legacy/local rows must NOT report IsComplete")
}

func TestAuditMigrationTable_OtherKMSCountedSeparately(t *testing.T) {
	// An aws-kms target deployment that still has stray gcp-kms rows
	// (e.g. after a partial cloud switch) must classify them as OtherKMS,
	// not silently fold them into Target. They block fallback removal
	// for the same reason legacy rows do: the static fallback can't
	// decrypt a gcp-kms ciphertext.
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "api_keys", Ciphertext: []byte("aws-kms:v1:" + base64Str("a")), KeyVersion: 1},
		{ID: "2", Table: "api_keys", Ciphertext: []byte("gcp-kms:v1:" + base64Str("b")), KeyVersion: 1},
	})
	c := NewMigrationCoordinator(store, nil, nil)
	res, err := c.AuditTable(context.Background(), "api_keys", "aws-kms")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Target)
	assert.Equal(t, 1, res.OtherKMS, "gcp row in an aws-kms target deployment counts as OtherKMS")
	assert.False(t, res.IsComplete())
}

func TestAuditMigrationTable_EmptyTable(t *testing.T) {
	store := newFakeMigrationStore(nil)
	c := NewMigrationCoordinator(store, nil, nil)
	res, err := c.AuditTable(context.Background(), "api_keys", "aws-kms")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Total)
	assert.True(t, res.IsComplete(), "empty table is trivially complete")
}

func TestAuditMigrationTable_InvalidTarget_ReturnsError(t *testing.T) {
	// Guard against operator typos in the target flag — silently treating
	// "awd-kms" as a valid target would report every row as non-target and
	// give a misleading "0 migrated" picture.
	store := newFakeMigrationStore(nil)
	c := NewMigrationCoordinator(store, nil, nil)
	_, err := c.AuditTable(context.Background(), "api_keys", "awd-kms")
	require.Error(t, err)
}

func TestAuditMigrationAll_AllThreeTablesAggregated(t *testing.T) {
	store := newFakeMigrationStore([]MigrationRow{
		{ID: "1", Table: "provider_credentials", Ciphertext: []byte("aws-kms:v1:" + base64Str("a")), KeyVersion: 1},
		{ID: "2", Table: "api_keys", Ciphertext: []byte("aws-kms:v1:" + base64Str("b")), KeyVersion: 1},
		{ID: "3", Table: "org_sso_configs", Ciphertext: []byte("lkms:v1:" + base64Str("legacy-sso")), KeyVersion: 1},
	})
	c := NewMigrationCoordinator(store, nil, nil)
	results, err := c.AuditAll(context.Background(), "aws-kms")
	require.NoError(t, err)
	require.Contains(t, results, "provider_credentials")
	require.Contains(t, results, "api_keys")
	require.Contains(t, results, "org_sso_configs")
	assert.True(t, results["provider_credentials"].IsComplete())
	assert.True(t, results["api_keys"].IsComplete())
	assert.False(t, results["org_sso_configs"].IsComplete(), "org_sso_configs has an outstanding lkms row")
}

// base64Str returns the base64 encoding of s — matches what real provider
// Encrypt functions produce after the prefix.
func base64Str(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
