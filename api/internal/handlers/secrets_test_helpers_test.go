// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// --- Test mock implementations for handler tests ---

type testKeyStore struct {
	mu      sync.Mutex
	records map[string]*secrets.UserKeyRecord
}

func newTestKeyStore() *testKeyStore {
	return &testKeyStore{records: make(map[string]*secrets.UserKeyRecord)}
}

func (m *testKeyStore) GetUserKey(_ context.Context, userID string) (*secrets.UserKeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (m *testKeyStore) CreateUserKey(_ context.Context, record *secrets.UserKeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[record.UserID]; exists {
		return errors.New("user key already exists")
	}
	cp := *record
	m.records[record.UserID] = &cp
	return nil
}

func (m *testKeyStore) UpdateWrappedDEK(_ context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return errors.New("not found")
	}
	r.WrappedDEK = wrappedDEK
	r.Salt = salt
	r.KeyVersion = keyVersion
	return nil
}

type testDEKCache struct {
	mu    sync.Mutex
	store map[string][]byte
}

func newTestDEKCache() *testDEKCache {
	return &testDEKCache{store: make(map[string][]byte)}
}

func (m *testDEKCache) CacheDEK(_ context.Context, sessionID string, dek []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(dek))
	copy(cp, dek)
	m.store[sessionID] = cp
	return nil
}

func (m *testDEKCache) GetDEK(_ context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dek, ok := m.store[sessionID]
	if !ok {
		return nil, nil
	}
	return dek, nil
}

func (m *testDEKCache) EvictDEK(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, sessionID)
	return nil
}

type testSecretStore struct {
	mu        sync.Mutex
	secrets   map[string]*secrets.UserSecret
	bindings  map[string][]string
	autoBound map[string]map[string]struct{} // workspace -> bind_source=global_default ids
	audit     []*secrets.AuditEntry
}

func newTestSecretStore() *testSecretStore {
	return &testSecretStore{
		secrets:   make(map[string]*secrets.UserSecret),
		bindings:  make(map[string][]string),
		autoBound: make(map[string]map[string]struct{}),
	}
}

func (m *testSecretStore) CreateSecret(_ context.Context, secret *secrets.UserSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.secrets {
		if s.UserID == secret.UserID && s.Name == secret.Name {
			return fmt.Errorf("%w: %s", secrets.ErrDuplicateSecret, secret.Name)
		}
	}
	if secret.ID == "" {
		secret.ID = "sec-" + secret.Name
	}
	cp := *secret
	m.secrets[secret.ID] = &cp
	return nil
}

func (m *testSecretStore) GetSecret(_ context.Context, userID, secretID string) (*secrets.UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[secretID]
	if !ok || s.UserID != userID {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (m *testSecretStore) GetSecretByName(_ context.Context, userID, name string) (*secrets.UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.secrets {
		if s.UserID == userID && s.Name == name {
			cp := *s
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *testSecretStore) ListSecrets(_ context.Context, userID string) ([]*secrets.UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*secrets.UserSecret
	for _, s := range m.secrets {
		if s.UserID == userID {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *testSecretStore) ListGlobalDefaultSecrets(_ context.Context, userID string) ([]*secrets.UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*secrets.UserSecret
	for _, s := range m.secrets {
		if s.UserID == userID && s.GlobalDefault {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *testSecretStore) UpdateSecret(_ context.Context, secret *secrets.UserSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secrets[secret.ID]; !ok {
		return errors.New("not found: " + secret.ID)
	}
	cp := *secret
	m.secrets[secret.ID] = &cp
	return nil
}

func (m *testSecretStore) ReEncryptUserSecrets(ctx context.Context, userID string, newKeyVersion int, transform func([]byte) ([]byte, error), commit func(context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	updates := make(map[string][]byte)
	for id, s := range m.secrets {
		if s.UserID != userID {
			continue
		}
		newCT, err := transform(s.Ciphertext)
		if err != nil {
			return err
		}
		updates[id] = newCT
	}
	if commit != nil {
		if err := commit(ctx); err != nil {
			return err
		}
	}
	for id, newCT := range updates {
		s := m.secrets[id]
		s.Ciphertext = newCT
		s.KeyVersion = newKeyVersion
	}
	return nil
}

func (m *testSecretStore) DeleteSecret(_ context.Context, userID, secretID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[secretID]
	if !ok || s.UserID != userID {
		return errors.New("not found: " + secretID)
	}
	delete(m.secrets, secretID)
	for wsID, sids := range m.bindings {
		var filtered []string
		for _, sid := range sids {
			if sid != secretID {
				filtered = append(filtered, sid)
			}
		}
		m.bindings[wsID] = filtered
	}
	return nil
}

func (m *testSecretStore) SetBindings(_ context.Context, workspaceID string, secretIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bindings[workspaceID] = secretIDs
	// A manual replace-set is the user's explicit claim over the whole
	// set: any prior auto rows for this workspace are gone with it
	// (mirrors the adapter; the reconciler re-asserts current policy
	// on its next pass).
	delete(m.autoBound, workspaceID)
	return nil
}

// SyncGlobalDefaultBindings mirrors the PgSecretStore contract: insert
// missing auto rows, remove auto rows absent from secretIDs, never
// touch manual rows.
func (m *testSecretStore) SyncGlobalDefaultBindings(_ context.Context, workspaceID string, secretIDs []string) ([]string, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := make(map[string]struct{}, len(secretIDs))
	for _, sid := range secretIDs {
		want[sid] = struct{}{}
	}
	auto := m.autoBound[workspaceID]
	if auto == nil {
		auto = make(map[string]struct{})
	}
	existing := m.bindings[workspaceID]
	inList := make(map[string]struct{}, len(existing))
	for _, sid := range existing {
		inList[sid] = struct{}{}
	}
	var added, removed []string
	next := existing[:0:0]
	for _, sid := range existing {
		if _, isAuto := auto[sid]; isAuto {
			if _, isWant := want[sid]; !isWant {
				delete(auto, sid)
				removed = append(removed, sid)
				continue
			}
		}
		next = append(next, sid)
	}
	for sid := range want {
		if _, ok := inList[sid]; !ok {
			// Only rows the sync itself inserts become auto — a
			// pre-existing manual row is the stronger claim and
			// shadows the policy (matches PgSecretStore ON CONFLICT).
			next = append(next, sid)
			auto[sid] = struct{}{}
			added = append(added, sid)
		}
	}
	if len(next) > 0 {
		m.bindings[workspaceID] = next
	} else {
		delete(m.bindings, workspaceID)
	}
	if len(auto) > 0 {
		m.autoBound[workspaceID] = auto
	} else {
		delete(m.autoBound, workspaceID)
	}
	return added, removed, nil
}

func (m *testSecretStore) AddBindings(_ context.Context, workspaceID string, secretIDs []string) error {
	if len(secretIDs) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing := m.bindings[workspaceID]
	seen := make(map[string]struct{}, len(existing)+len(secretIDs))
	for _, id := range existing {
		seen[id] = struct{}{}
	}
	for _, id := range secretIDs {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		existing = append(existing, id)
	}
	m.bindings[workspaceID] = existing
	return nil
}

func (m *testSecretStore) GetBindings(_ context.Context, workspaceID string) ([]*secrets.UserSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sids := m.bindings[workspaceID]
	var result []*secrets.UserSecret
	for _, sid := range sids {
		if s, ok := m.secrets[sid]; ok {
			cp := *s
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *testSecretStore) GetBindingsForSecret(_ context.Context, secretID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var workspaces []string
	for wsID, sids := range m.bindings {
		for _, sid := range sids {
			if sid == secretID {
				workspaces = append(workspaces, wsID)
			}
		}
	}
	return workspaces, nil
}

func (m *testSecretStore) LogAudit(_ context.Context, entry *secrets.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, entry)
	return nil
}

func (m *testSecretStore) QueryAudit(_ context.Context, userID string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*secrets.AuditEntry
	for _, e := range m.audit {
		if e.UserID == userID {
			result = append(result, e)
		}
	}
	return result, nil
}

func (m *testSecretStore) CurrentRevision(context.Context, string) (int64, string, bool, error) {
	return 0, "", false, nil
}

func (m *testSecretStore) EnsureRevision(context.Context, string, string) (int64, error) {
	return 1, nil
}

func (m *testSecretStore) GetWorkspaceCredentials(_ context.Context, workspaceID string) ([]secrets.CredentialBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sids := m.bindings[workspaceID]
	var result []secrets.CredentialBinding
	for _, sid := range sids {
		s, ok := m.secrets[sid]
		if !ok || s.Type != secrets.SecretTypeLLMProvider {
			continue
		}
		result = append(result, secrets.CredentialBinding{
			ID:        s.ID,
			OwnerType: "user",
			OwnerID:   s.UserID,
			Kind:      s.Name, Slug: s.Name, // use name as provider key for dedup; decryptBinding resolves the real provider
			Ciphertext: s.Ciphertext,
			Version:    1,
		})
	}
	return result, nil
}

func (m *testSecretStore) UpsertFreeTierCredential(_ context.Context, _ []byte) error { return nil }

func (m *testSecretStore) SeedWorkspaceCredentials(_ context.Context, _, _ string, _ *string) error {
	return nil
}

func (m *testSecretStore) BindCredentialToAllUserWorkspaces(_ context.Context, _, _ string) error {
	return nil
}

func (m *testSecretStore) HasUserProviderCredential(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (m *testSecretStore) GetWorkspaceMCPServers(_ context.Context, _ string) ([]secrets.MCPServerBindingRow, error) {
	return nil, nil
}
