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

func (m *testKeyStore) UpdateWrappedDEKAndSource(_ context.Context, userID string, wrappedDEK []byte, salt []byte, keyVersion int, dekSource string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return errors.New("not found")
	}
	r.WrappedDEK = wrappedDEK
	r.Salt = salt
	r.KeyVersion = keyVersion
	r.DEKSource = dekSource
	return nil
}

func (m *testKeyStore) UpdateWrappedDEKRecovery(_ context.Context, userID string, wrappedDEKRecovery []byte, recoverySalt []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[userID]
	if !ok {
		return errors.New("not found")
	}
	r.WrappedDEKRecovery = wrappedDEKRecovery
	r.RecoverySalt = recoverySalt
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
	mu       sync.Mutex
	secrets  map[string]*secrets.UserSecret
	bindings map[string][]string
	audit    []*secrets.AuditEntry
}

func newTestSecretStore() *testSecretStore {
	return &testSecretStore{
		secrets:  make(map[string]*secrets.UserSecret),
		bindings: make(map[string][]string),
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
	return nil
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
