// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// recordingProvider is a RootKeyProvider test fake that records calls and
// implements a reversible transform (XOR marker) so Encrypt/Decrypt round-trip.
type recordingProvider struct {
	encCalls    [][]byte
	decCalls    [][]byte
	failDecrypt bool
}

func (p *recordingProvider) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	cp := make([]byte, len(plaintext))
	copy(cp, plaintext)
	// invert bits so it's a real transform (not identity); Decrypt inverts back.
	for i := range cp {
		cp[i] ^= 0x5A
	}
	p.encCalls = append(p.encCalls, cp)
	return cp, nil
}

func (p *recordingProvider) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	p.decCalls = append(p.decCalls, ciphertext)
	if p.failDecrypt {
		return nil, ErrDecryptionFailed
	}
	cp := make([]byte, len(ciphertext))
	copy(cp, ciphertext)
	for i := range cp {
		cp[i] ^= 0x5A
	}
	return cp, nil
}

func TestInitializeUserKeysServerKEK_GeneratesServerKEKWrappedDEK(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	prov := &recordingProvider{}
	svc.SetAPIKeyStore(nil, prov) // wires rootKeyProvider

	if err := svc.InitializeUserKeysServerKEK(context.Background(), "user-sso"); err != nil {
		t.Fatalf("InitializeUserKeysServerKEK: %v", err)
	}

	rec, _ := store.GetUserKey(context.Background(), "user-sso")
	if rec == nil {
		t.Fatal("expected a user_keys row")
	}
	if rec.DEKSource != "server_kek" {
		t.Errorf("DEKSource = %q, want server_kek", rec.DEKSource)
	}
	if rec.Salt != nil {
		t.Errorf("Salt must be nil for server_kek row, got %d bytes", len(rec.Salt))
	}
	if rec.WrappedDEKRecovery != nil {
		t.Error("server_kek rows have no recovery wrap")
	}
	if len(rec.WrappedDEK) == 0 || len(prov.encCalls) != 1 {
		t.Errorf("provider.Encrypt must be called exactly once (got %d calls)", len(prov.encCalls))
	}
}

func TestInitializeUserKeysServerKEK_NoProvider_Fails(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	// rootKeyProvider intentionally NOT wired.

	err := svc.InitializeUserKeysServerKEK(context.Background(), "user-sso")
	if !errors.Is(err, ErrServerKEKUnavailable) {
		t.Errorf("expected ErrServerKEKUnavailable, got %v", err)
	}
	if rec, _ := store.GetUserKey(context.Background(), "user-sso"); rec != nil {
		t.Error("no user_keys row should be written when provider is missing")
	}
}

func TestUnlockDEKWithSigningKey_ServerKEK_UsesRootKeyProvider(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	prov := &recordingProvider{}
	svc.SetAPIKeyStore(nil, prov)

	_ = svc.InitializeUserKeysServerKEK(context.Background(), "u1")

	// Unlock with an arbitrary "password" — must be ignored for server_kek.
	if err := svc.UnlockDEKWithSigningKey(context.Background(), "u1", []byte("ignored-password"), "sess-1", time.Hour, nil); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if len(prov.decCalls) != 1 {
		t.Fatalf("provider.Decrypt must be called once, got %d", len(prov.decCalls))
	}
	got, _ := cache.GetDEK(context.Background(), "sess-1")
	if got == nil {
		t.Fatal("DEK should be cached under sess-1")
	}
	// The cached DEK must equal the plaintext the provider wrapped at provisioning.
	rec, _ := store.GetUserKey(context.Background(), "u1")
	expected, _ := prov.Decrypt(context.Background(), rec.WrappedDEK)
	if !bytes.Equal(got, expected) {
		t.Error("cached DEK does not match the provisioned DEK")
	}
}

func TestUnlockDEKWithSigningKey_ServerKEK_NoProvider_FailsClosed(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	// Seed a server_kek record directly.
	store.records["u1"] = &UserKeyRecord{
		UserID:     "u1",
		WrappedDEK: []byte("ciphertext"),
		DEKSource:  "server_kek",
	}
	svc := NewKeyService(store, cache)
	// rootKeyProvider NOT wired.

	err := svc.UnlockDEKWithSigningKey(context.Background(), "u1", nil, "sess", time.Hour, nil)
	if !errors.Is(err, ErrServerKEKUnavailable) {
		t.Errorf("expected ErrServerKEKUnavailable, got %v", err)
	}
	cached, _ := cache.GetDEK(context.Background(), "sess")
	if cached != nil {
		t.Error("nothing should be cached on fail-closed")
	}
}

func TestUnlockDEKWithSigningKey_ServerKEK_DecryptFailure(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	store.records["u1"] = &UserKeyRecord{UserID: "u1", WrappedDEK: []byte("ct"), DEKSource: "server_kek"}
	svc := NewKeyService(store, cache)
	prov := &recordingProvider{failDecrypt: true}
	svc.SetAPIKeyStore(nil, prov)

	err := svc.UnlockDEKWithSigningKey(context.Background(), "u1", nil, "sess", time.Hour, nil)
	if err == nil {
		t.Fatal("decrypt failure must surface as an error")
	}
}

// TestUnlockDEKWithSigningKey_PasswordPath_Unchanged guards the regression that
// password-tier users still unwrap via Argon2id(password, salt).
func TestUnlockDEKWithSigningKey_PasswordPath_Unchanged(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	_, err := svc.InitializeUserKeys(context.Background(), "u1", []byte("correct-horse"))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	rec, _ := store.GetUserKey(context.Background(), "u1")
	if rec.DEKSource != "password" {
		t.Errorf("InitializeUserKeys must set DEKSource=password, got %q", rec.DEKSource)
	}
	if err := svc.UnlockDEKWithSigningKey(context.Background(), "u1", []byte("correct-horse"), "sess", time.Hour, nil); err != nil {
		t.Fatalf("unlock password path: %v", err)
	}
	if err := svc.UnlockDEKWithSigningKey(context.Background(), "u1", []byte("wrong"), "sess2", time.Hour, nil); err == nil {
		t.Error("wrong password must fail on the password path")
	}
}

func TestChangePassword_TransitionsServerKEKToPassword(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)
	prov := &recordingProvider{}
	svc.SetAPIKeyStore(nil, prov)

	_ = svc.InitializeUserKeysServerKEK(context.Background(), "u1")
	origRec, _ := store.GetUserKey(context.Background(), "u1")
	origDEK, _ := prov.Decrypt(context.Background(), origRec.WrappedDEK)

	// oldPassword is meaningless for a server_kek user; pass anything.
	if err := svc.ChangePassword(context.Background(), "u1", "", []byte("anything"), []byte("new-password")); err != nil {
		t.Fatalf("ChangePassword transition: %v", err)
	}

	after, _ := store.GetUserKey(context.Background(), "u1")
	if after.DEKSource != "password" {
		t.Errorf("DEKSource after ChangePassword = %q, want password", after.DEKSource)
	}
	if after.Salt == nil {
		t.Error("Salt must be populated after re-wrap with a password")
	}
	if !store.updateWithSourceCalled {
		t.Error("ChangePassword must use the atomic UpdateWrappedDEKAndSource for the server_kek→password transition")
	}
	// The new wrap must unwrap with the NEW password and decrypt to the SAME DEK.
	newKEK, _ := DeriveKEKFromPassword([]byte("new-password"), after.Salt)
	recoveredDEK, _ := UnwrapDEK(newKEK, after.WrappedDEK)
	if !bytes.Equal(recoveredDEK, origDEK) {
		t.Fatal("re-wrap must preserve the underlying DEK across the transition")
	}
}

// TestChangePassword_PasswordPath_Unchanged guards that password→password does
// NOT route through the atomic-with-source path (no transition needed).
func TestChangePassword_PasswordPath_Unchanged(t *testing.T) {
	store := newMockKeyStore()
	cache := newMockDEKCache()
	svc := NewKeyService(store, cache)

	_, _ = svc.InitializeUserKeys(context.Background(), "u1", []byte("oldpw"))
	store.updateWithSourceCalled = false

	if err := svc.ChangePassword(context.Background(), "u1", "", []byte("oldpw"), []byte("newpw")); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if store.updateWithSourceCalled {
		t.Error("password→password must use plain UpdateWrappedDEK, not the with-source variant")
	}
	after, _ := store.GetUserKey(context.Background(), "u1")
	if after.DEKSource != "password" {
		t.Errorf("DEKSource = %q, want password", after.DEKSource)
	}
}
