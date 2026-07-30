// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRotationStore is an in-memory RotationStore for testing.
type mockRotationStore struct {
	mu      sync.Mutex
	rows    map[string][]RotationRow // table → rows
	updates map[string]int           // "table:rowID" → new key_version
	flushed bool
}

func newMockRotationStore() *mockRotationStore {
	return &mockRotationStore{
		rows:    make(map[string][]RotationRow),
		updates: make(map[string]int),
	}
}

func (s *mockRotationStore) addRow(table, id, ownerType string, ct []byte, ver int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[table] = append(s.rows[table], RotationRow{
		ID: id, Table: table, OwnerType: ownerType, Ciphertext: ct, KeyVersion: ver,
	})
}

func (s *mockRotationStore) ListRotationRows(_ context.Context, table, resumeFromID string, targetVersion, limit int) ([]RotationRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.rows[table]
	var out []RotationRow
	for _, r := range all {
		if resumeFromID != "" && r.ID <= resumeFromID {
			continue
		}
		if r.KeyVersion >= targetVersion {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *mockRotationStore) UpdateRotationRow(_ context.Context, table, rowID string, newCT []byte, newVer int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates[table+":"+rowID] = newVer
	// Update the in-memory row so idempotency works on re-run.
	for i, r := range s.rows[table] {
		if r.ID == rowID {
			s.rows[table][i].Ciphertext = newCT
			s.rows[table][i].KeyVersion = newVer
			break
		}
	}
	return nil
}

func (s *mockRotationStore) FlushDEKCache(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushed = true
	return nil
}

// buildProviderSets creates old/new providers from two different keys.
func buildProviderSets(t *testing.T) (oldProv, newProv map[string]RootKeyProvider) {
	t.Helper()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	newKey := make([]byte, 32)
	for i := range newKey {
		newKey[i] = byte(i + 50)
	}
	oldSP, err := NewStaticKeyProvider(oldKey)
	require.NoError(t, err)
	newSP, err := NewStaticKeyProvider(newKey)
	require.NoError(t, err)

	return map[string]RootKeyProvider{
			"provider-credentials": oldSP,
			"org-credentials":      oldSP,
			"master-kek":           oldSP,
			"dek-cache":            oldSP,
		}, map[string]RootKeyProvider{
			"provider-credentials": newSP,
			"org-credentials":      newSP,
			"master-kek":           newSP,
			"dek-cache":            newSP,
		}
}

// encryptWithKey encrypts plaintext with a raw key (test helper).
func encryptWithKey(t *testing.T, key []byte, plaintext []byte) []byte {
	t.Helper()
	ct, err := EncryptSecret(key, plaintext)
	require.NoError(t, err)
	return ct
}

func TestRotationCoordinator_ProviderCredentials_SelectsPurposeByOwnerType(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}

	adminCT := encryptWithKey(t, oldKey, []byte(`{"kind":"anthropic","slug":"anthropic","apiKey":"admin-key"}`))
	orgCT := encryptWithKey(t, oldKey, []byte(`{"kind":"openai","slug":"openai","apiKey":"org-key"}`))
	store.addRow("provider_credentials", "cred-1", "admin", adminCT, 1)
	store.addRow("provider_credentials", "cred-2", "org", orgCT, 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	result, err := coord.RotateTable(context.Background(), "provider_credentials", "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Processed)
	assert.Equal(t, 0, result.Failed)

	// Both rows should now be at version 2 and decrypt with the new provider.
	newProvPC := newProv["provider-credentials"]
	adminRow := store.rows["provider_credentials"][0]
	assert.Equal(t, 2, adminRow.KeyVersion)
	dec, err := newProvPC.Decrypt(context.Background(), adminRow.Ciphertext)
	require.NoError(t, err)
	assert.Contains(t, string(dec), "admin-key")

	// org row uses org-credentials provider — verify it decrypts too.
	newProvOrg := newProv["org-credentials"]
	orgRow := store.rows["provider_credentials"][1]
	dec2, err := newProvOrg.Decrypt(context.Background(), orgRow.Ciphertext)
	require.NoError(t, err)
	assert.Contains(t, string(dec2), "org-key")
}

func TestRotationCoordinator_DryRun_DoesNotMutate(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	ct := encryptWithKey(t, oldKey, []byte("secret"))
	originalCT := make([]byte, len(ct))
	copy(originalCT, ct)
	store.addRow("api_keys", "key-1", "", ct, 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	result, err := coord.RotateTable(context.Background(), "api_keys", "", 2, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Processed)

	// Row must be unchanged.
	row := store.rows["api_keys"][0]
	assert.Equal(t, 1, row.KeyVersion, "dry-run must not change key_version")
	assert.Equal(t, originalCT, row.Ciphertext, "dry-run must not change ciphertext")
}

func TestRotationCoordinator_Idempotent_SecondRunNoOp(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	ct := encryptWithKey(t, oldKey, []byte("secret"))
	store.addRow("org_sso_configs", "org-1", "", ct, 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	// First run: rotates.
	r1, err := coord.RotateTable(context.Background(), "org_sso_configs", "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r1.Processed)

	// Second run: no-op (row already at version 2).
	r2, err := coord.RotateTable(context.Background(), "org_sso_configs", "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 0, r2.Processed)
	assert.Equal(t, 0, r2.Skipped, "rows at target version are filtered out by ListRotationRows")
}

func TestRotationCoordinator_DecryptFailure_ReportsRowID(t *testing.T) {
	store := newMockRotationStore()
	rogueKey := make([]byte, 32)
	for i := range rogueKey {
		rogueKey[i] = byte(i + 99)
	}
	// Ciphertext encrypted with a key NOT in the old provider set.
	ct := encryptWithKey(t, rogueKey, []byte("rogue"))
	store.addRow("api_keys", "key-bad", "", ct, 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	result, err := coord.RotateTable(context.Background(), "api_keys", "", 2, false)
	require.NoError(t, err) // per-row failures don't error the whole call
	assert.Equal(t, 1, result.Failed)
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "key-bad", result.Errors[0].RowID)
}

func TestRotationCoordinator_FlushesRedisDEKCache(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	store.addRow("api_keys", "key-1", "", encryptWithKey(t, oldKey, []byte("s")), 1)
	store.addRow("provider_credentials", "cred-1", "admin", encryptWithKey(t, oldKey, []byte("s")), 1)
	store.addRow("org_sso_configs", "org-1", "", encryptWithKey(t, oldKey, []byte("s")), 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	_, err := coord.RotateAll(context.Background(), 2, false)
	require.NoError(t, err)
	assert.True(t, store.flushed, "RotateAll must flush the Redis DEK cache on success")
}

func TestRotationCoordinator_AllTables_HappyPath(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}

	store.addRow("provider_credentials", "cred-1", "admin", encryptWithKey(t, oldKey, []byte("admin-secret")), 1)
	store.addRow("provider_credentials", "cred-2", "org", encryptWithKey(t, oldKey, []byte("org-secret")), 1)
	store.addRow("api_keys", "key-1", "", encryptWithKey(t, oldKey, []byte("api-key-secret")), 1)
	store.addRow("org_sso_configs", "org-1", "", encryptWithKey(t, oldKey, []byte("sso-secret")), 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	results, err := coord.RotateAll(context.Background(), 2, false)
	require.NoError(t, err)
	assert.Equal(t, 2, results["provider_credentials"].Processed)
	assert.Equal(t, 1, results["api_keys"].Processed)
	assert.Equal(t, 1, results["org_sso_configs"].Processed)
	// user_keys has no seeded rows → no-op, no error (RotateAll must tolerate it).
	assert.Equal(t, 0, results["user_keys"].Processed)
	assert.True(t, store.flushed)
}

// TestRotationCoordinator_UserKeys_RotatesServerKEKRows verifies the Epic 58
// user_keys walk: server_kek rows re-wrap under the master-kek purpose. The
// store contract is that password-tier rows are excluded upstream.
func TestRotationCoordinator_UserKeys_RotatesServerKEKRows(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}

	// Server-KEK-wrapped user DEK — rotatable.
	store.addRow("user_keys", "user-sso", "", encryptWithKey(t, oldKey, []byte("sso-user-dek")), 1)
	row := store.rows["user_keys"][0]
	row.DEKSource = "server_kek"
	store.rows["user_keys"][0] = row

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	res, err := coord.RotateTable(context.Background(), "user_keys", "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Processed)
	assert.Equal(t, 2, store.rows["user_keys"][0].KeyVersion, "server_kek row must be re-wrapped to the target version")
}

// TestRotationCoordinator_UserKeys_PurposeIsMasterKEK guards that a user_keys
// row routed under the WRONG purpose (no master-kek provider) fails rather than
// silently skipping — proving the purpose dispatch selects master-kek.
func TestRotationCoordinator_UserKeys_PurposeIsMasterKEK(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	store.addRow("user_keys", "u1", "", encryptWithKey(t, oldKey, []byte("dek")), 1)
	store.rows["user_keys"][0].DEKSource = "server_kek"

	// Old/new providers MISSING the master-kek purpose.
	oldSP, err := NewStaticKeyProvider(oldKey)
	require.NoError(t, err)
	coord := NewRotationCoordinator(store,
		map[string]RootKeyProvider{"provider-credentials": oldSP},
		map[string]RootKeyProvider{"provider-credentials": oldSP})

	res, err := coord.RotateTable(context.Background(), "user_keys", "", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Failed, "user_keys rotation must require the master-kek purpose provider")
}

func TestRotationCoordinator_ResumeFromCursor_ContinuesFromLastRow(t *testing.T) {
	store := newMockRotationStore()
	oldKey := make([]byte, 32)
	for i := range oldKey {
		oldKey[i] = byte(i + 1)
	}
	store.addRow("api_keys", "key-1", "", encryptWithKey(t, oldKey, []byte("s1")), 1)
	store.addRow("api_keys", "key-2", "", encryptWithKey(t, oldKey, []byte("s2")), 1)
	store.addRow("api_keys", "key-3", "", encryptWithKey(t, oldKey, []byte("s3")), 1)

	oldProv, newProv := buildProviderSets(t)
	coord := NewRotationCoordinator(store, oldProv, newProv)

	// Simulate an interrupted run: key-1 was processed (bumped to v2),
	// key-2 was the resume point.
	store.rows["api_keys"][0].KeyVersion = 2

	result, err := coord.RotateTable(context.Background(), "api_keys", "key-1", 2, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Processed, "should rotate key-2 and key-3 (skipping key-1 which is already v2)")
	assert.Equal(t, "key-2", store.rows["api_keys"][1].ID)
	assert.Equal(t, 2, store.rows["api_keys"][1].KeyVersion)
	assert.Equal(t, 2, store.rows["api_keys"][2].KeyVersion)
}

var _ = fmt.Sprintf // keep import if unused
