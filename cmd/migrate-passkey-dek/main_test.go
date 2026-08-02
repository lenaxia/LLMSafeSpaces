// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// memStore is a minimal KeyStore for testing the migration core. It mirrors the
// mockKeyStore in pkg/secrets/key_service_test.go but lives here to keep the
// test self-contained.
type memStore struct {
	mu       sync.Mutex
	record   *secrets.UserKeyRecord
	getErr   error
	updErr   error
	updCalls int
}

func (s *memStore) GetUserKey(_ context.Context, _ string) (*secrets.UserKeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.record == nil {
		return nil, nil
	}
	cp := *s.record
	return &cp, nil
}

func (s *memStore) UpdateWrappedDEKAndSource(_ context.Context, _ string, wrappedDEK, salt []byte, keyVersion int, dekSource string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updCalls++
	if s.updErr != nil {
		return s.updErr
	}
	if s.record == nil {
		return errors.New("no record")
	}
	s.record.WrappedDEK = append([]byte(nil), wrappedDEK...)
	s.record.Salt = append([]byte(nil), salt...)
	s.record.KeyVersion = keyVersion
	s.record.DEKSource = dekSource
	return nil
}

// The KeyStore interface requires a few more methods that migrateUser never
// calls. They are stubbed here to satisfy the interface.

func (s *memStore) CreateUserKey(_ context.Context, record *secrets.UserKeyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *record
	s.record = &cp
	return nil
}

func (s *memStore) UpdateWrappedDEK(_ context.Context, _ string, wrappedDEK []byte, salt []byte, keyVersion int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return errors.New("no record")
	}
	s.record.WrappedDEK = append([]byte(nil), wrappedDEK...)
	s.record.Salt = append([]byte(nil), salt...)
	s.record.KeyVersion = keyVersion
	return nil
}

func (s *memStore) UpdateWrappedDEKRecovery(_ context.Context, _ string, wrappedDEKRecovery []byte, recoverySalt []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return errors.New("no record")
	}
	s.record.WrappedDEKRecovery = append([]byte(nil), wrappedDEKRecovery...)
	s.record.RecoverySalt = append([]byte(nil), recoverySalt...)
	return nil
}

// setupPasswordTierUser provisions a user exactly as InitializeUserKeys would:
// generates a DEK, derives a KEK from the password, wraps the DEK, and stores a
// dek_source="password" record. Returns the plaintext DEK for assertion.
func setupPasswordTierUser(t *testing.T, password string) (*secrets.UserKeyRecord, []byte) {
	t.Helper()
	dek, err := secrets.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	salt, err := secrets.GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt: %v", err)
	}
	kek, err := secrets.DeriveKEKFromPassword([]byte(password), salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword: %v", err)
	}
	wrapped, err := secrets.WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	return &secrets.UserKeyRecord{
		UserID:     "u-1",
		KeyVersion: 1,
		WrappedDEK: wrapped,
		Salt:       salt,
		DEKSource:  "password",
	}, dek
}

func TestMigrateUser_RoundTripsDEK(t *testing.T) {
	const password = "correct horse battery staple"
	record, originalDEK := setupPasswordTierUser(t, password)
	store := &memStore{record: record}

	provider, err := secrets.NewStaticKeyProvider(testStaticKey(t))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}

	if err := migrateUser(context.Background(), store, "u-1", []byte(password), provider, false); err != nil {
		t.Fatalf("migrateUser: %v", err)
	}

	// dek_source flipped.
	if got := store.record.DEKSource; got != "server_kek" {
		t.Fatalf("dek_source = %q, want server_kek", got)
	}
	// Salt cleared (server_kek rows have no Argon2 salt).
	if len(store.record.Salt) != 0 {
		t.Fatalf("salt not cleared: %d bytes", len(store.record.Salt))
	}
	// The re-wrapped DEK must decrypt back to the ORIGINAL plaintext DEK via the
	// master provider — proving no data loss across the tier transition.
	decryptedDEK, err := provider.Decrypt(context.Background(), store.record.WrappedDEK)
	if err != nil {
		t.Fatalf("provider.Decrypt after migration: %v", err)
	}
	if !bytes.Equal(decryptedDEK, originalDEK) {
		t.Fatalf("DEK changed across migration: got %x, want %x", decryptedDEK, originalDEK)
	}
	// Exactly one commit.
	if store.updCalls != 1 {
		t.Fatalf("UpdateWrappedDEKAndSource calls = %d, want 1", store.updCalls)
	}
}

func TestMigrateUser_WrongPassword_FailsClosed(t *testing.T) {
	record, _ := setupPasswordTierUser(t, "the-real-password")
	store := &memStore{record: record}
	provider, err := secrets.NewStaticKeyProvider(testStaticKey(t))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}

	err = migrateUser(context.Background(), store, "u-1", []byte("wrong-password"), provider, false)
	if err == nil {
		t.Fatal("migrateUser with wrong password: expected error, got nil")
	}
	// No commit should have occurred (fail closed).
	if store.updCalls != 0 {
		t.Fatalf("UpdateWrappedDEKAndSource calls = %d, want 0 (fail closed)", store.updCalls)
	}
	// dek_source must remain unchanged.
	if got := store.record.DEKSource; got != "password" {
		t.Fatalf("dek_source changed despite failure: %q", got)
	}
}

func TestMigrateUser_AlreadyServerKEK_IsIdempotent(t *testing.T) {
	provider, err := secrets.NewStaticKeyProvider(testStaticKey(t))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	dek, _ := secrets.GenerateDEK()
	wrapped, err := provider.Encrypt(context.Background(), dek)
	if err != nil {
		t.Fatalf("provider.Encrypt: %v", err)
	}
	store := &memStore{record: &secrets.UserKeyRecord{
		UserID:     "u-1",
		WrappedDEK: wrapped,
		DEKSource:  "server_kek",
	}}

	if err := migrateUser(context.Background(), store, "u-1", []byte("anything"), provider, false); err != nil {
		t.Fatalf("idempotent skip returned error: %v", err)
	}
	if store.updCalls != 0 {
		t.Fatalf("UpdateWrappedDEKAndSource calls = %d, want 0 (idempotent skip)", store.updCalls)
	}
}

func TestMigrateUser_DryRun_NoWrite(t *testing.T) {
	record, _ := setupPasswordTierUser(t, "pw")
	store := &memStore{record: record}
	provider, err := secrets.NewStaticKeyProvider(testStaticKey(t))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}

	if err := migrateUser(context.Background(), store, "u-1", []byte("pw"), provider, true); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if store.updCalls != 0 {
		t.Fatalf("dry run wrote: %d commits", store.updCalls)
	}
	if got := store.record.DEKSource; got != "password" {
		t.Fatalf("dry run changed dek_source: %q", got)
	}
}

func TestMigrateUser_NoKeyMaterial(t *testing.T) {
	store := &memStore{record: nil}
	provider, err := secrets.NewStaticKeyProvider(testStaticKey(t))
	if err != nil {
		t.Fatalf("NewStaticKeyProvider: %v", err)
	}
	err = migrateUser(context.Background(), store, "u-1", []byte("pw"), provider, false)
	if err == nil {
		t.Fatal("expected error for user with no key material")
	}
}

// testStaticKey returns a deterministic 32-byte key for tests.
func testStaticKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}
