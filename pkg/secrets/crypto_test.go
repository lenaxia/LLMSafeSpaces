// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveKEKFromPasswordProduces32Bytes(t *testing.T) {
	password := []byte("test-password-123")
	salt := bytes.Repeat([]byte{0x01}, 32)

	kek, err := DeriveKEKFromPassword(password, salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}
	if len(kek) != 32 {
		t.Errorf("KEK should be 32 bytes, got %d", len(kek))
	}
}

func TestDeriveKEKFromPasswordDeterministic(t *testing.T) {
	password := []byte("test-password-123")
	salt := bytes.Repeat([]byte{0x01}, 32)

	kek1, err := DeriveKEKFromPassword(password, salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}
	kek2, err := DeriveKEKFromPassword(password, salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}

	if !bytes.Equal(kek1, kek2) {
		t.Error("DeriveKEKFromPassword should be deterministic for same inputs")
	}
}

func TestDeriveKEKFromPasswordDifferentPasswords(t *testing.T) {
	salt := bytes.Repeat([]byte{0x01}, 32)

	kek1, err := DeriveKEKFromPassword([]byte("password-1"), salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}
	kek2, err := DeriveKEKFromPassword([]byte("password-2"), salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}

	if bytes.Equal(kek1, kek2) {
		t.Error("Different passwords should produce different KEKs")
	}
}

func TestDeriveKEKFromPasswordDifferentSalts(t *testing.T) {
	password := []byte("test-password-123")
	salt1 := bytes.Repeat([]byte{0x01}, 32)
	salt2 := bytes.Repeat([]byte{0x02}, 32)

	kek1, err := DeriveKEKFromPassword(password, salt1)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}
	kek2, err := DeriveKEKFromPassword(password, salt2)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}

	if bytes.Equal(kek1, kek2) {
		t.Error("Different salts should produce different KEKs")
	}
}

func TestDeriveKEKFromPasswordRejectsWrongSaltLength(t *testing.T) {
	tests := []struct {
		name string
		salt []byte
	}{
		{"nil salt", nil},
		{"empty salt", []byte{}},
		{"31-byte salt", make([]byte, 31)},
		{"33-byte salt", make([]byte, 33)},
		{"16-byte salt", make([]byte, 16)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DeriveKEKFromPassword([]byte("password"), tt.salt)
			if err == nil {
				t.Error("expected error for wrong salt length")
			}
		})
	}
}

func TestDeriveKEKFromKeyDeterministic(t *testing.T) {
	key := []byte("test-key-material-1234567890123456")
	salt := []byte("0123456789abcdef0123456789abcdef")

	kek1, err := DeriveKEKFromKey(key, salt, kekInfo)
	if err != nil {
		t.Fatalf("DeriveKEKFromKey failed: %v", err)
	}
	kek2, err := DeriveKEKFromKey(key, salt, kekInfo)
	if err != nil {
		t.Fatalf("DeriveKEKFromKey failed: %v", err)
	}

	if !bytes.Equal(kek1, kek2) {
		t.Error("DeriveKEKFromKey should be deterministic for same inputs")
	}
	if len(kek1) != 32 {
		t.Errorf("KEK should be 32 bytes, got %d", len(kek1))
	}
}

func TestDeriveKEKFromKeyDifferentSalts(t *testing.T) {
	key := []byte("test-key-material-1234567890123456")
	salt1 := []byte("0123456789abcdef0123456789abcdef")
	salt2 := []byte("fedcba9876543210fedcba9876543210")

	kek1, err := DeriveKEKFromKey(key, salt1, kekInfo)
	if err != nil {
		t.Fatalf("DeriveKEKFromKey failed: %v", err)
	}
	kek2, err := DeriveKEKFromKey(key, salt2, kekInfo)
	if err != nil {
		t.Fatalf("DeriveKEKFromKey failed: %v", err)
	}

	if bytes.Equal(kek1, kek2) {
		t.Error("Different salts should produce different KEKs")
	}
}

func TestDeriveKEKFromKeyDifferentInfo(t *testing.T) {
	key := []byte("test-key-material")
	salt := []byte("0123456789abcdef0123456789abcdef")

	kek1, err := DeriveKEKFromKey(key, salt, kekInfo)
	if err != nil {
		t.Fatalf("DeriveKEKFromKey failed: %v", err)
	}
	kek2, err := DeriveKEKFromKey(key, salt, recInfo)
	if err != nil {
		t.Fatalf("DeriveKEKFromKey failed: %v", err)
	}

	if bytes.Equal(kek1, kek2) {
		t.Error("Different info strings should produce different KEKs")
	}
}

func TestGenerateDEK(t *testing.T) {
	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK failed: %v", err)
	}
	if len(dek) != 32 {
		t.Errorf("DEK should be 32 bytes, got %d", len(dek))
	}

	// Two calls should produce different DEKs
	dek2, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK failed: %v", err)
	}
	if bytes.Equal(dek, dek2) {
		t.Error("Two GenerateDEK calls should produce different keys")
	}
}

func TestGenerateSalt(t *testing.T) {
	salt, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt failed: %v", err)
	}
	if len(salt) != 32 {
		t.Errorf("Salt should be 32 bytes, got %d", len(salt))
	}
}

func TestEncryptDecryptSecret_RoundTrip(t *testing.T) {
	dek, _ := GenerateDEK()
	plaintext := []byte("my-secret-api-key-sk-1234567890")

	ciphertext, err := EncryptSecret(dek, plaintext)
	if err != nil {
		t.Fatalf("EncryptSecret failed: %v", err)
	}

	decrypted, err := DecryptSecret(dek, ciphertext)
	if err != nil {
		t.Fatalf("DecryptSecret failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Decrypted text should match original plaintext")
	}
}

func TestEncryptSecret_DifferentCiphertexts(t *testing.T) {
	dek, _ := GenerateDEK()
	plaintext := []byte("same-plaintext")

	ct1, _ := EncryptSecret(dek, plaintext)
	ct2, _ := EncryptSecret(dek, plaintext)

	if bytes.Equal(ct1, ct2) {
		t.Error("Two encryptions of same plaintext should differ (random nonce)")
	}
}

func TestDecryptSecret_WrongKey(t *testing.T) {
	dek1, _ := GenerateDEK()
	dek2, _ := GenerateDEK()
	plaintext := []byte("secret-data")

	ciphertext, _ := EncryptSecret(dek1, plaintext)

	_, err := DecryptSecret(dek2, ciphertext)
	if err == nil {
		t.Error("DecryptSecret with wrong key should fail")
	}
	if err != ErrDecryptionFailed {
		t.Errorf("Expected ErrDecryptionFailed, got: %v", err)
	}
}

func TestDecryptSecret_TamperedCiphertext(t *testing.T) {
	dek, _ := GenerateDEK()
	plaintext := []byte("secret-data")

	ciphertext, _ := EncryptSecret(dek, plaintext)
	ciphertext[len(ciphertext)-1] ^= 0xFF

	_, err := DecryptSecret(dek, ciphertext)
	if err == nil {
		t.Error("DecryptSecret with tampered ciphertext should fail")
	}
}

func TestDecryptSecret_TooShort(t *testing.T) {
	dek, _ := GenerateDEK()

	_, err := DecryptSecret(dek, []byte("x"))
	if err != ErrInvalidCiphertext {
		t.Errorf("Expected ErrInvalidCiphertext, got: %v", err)
	}
}

func TestEncryptDecryptSecret_EmptyPlaintext(t *testing.T) {
	dek, _ := GenerateDEK()
	plaintext := []byte("")

	ciphertext, err := EncryptSecret(dek, plaintext)
	if err != nil {
		t.Fatalf("EncryptSecret with empty plaintext failed: %v", err)
	}

	decrypted, err := DecryptSecret(dek, ciphertext)
	if err != nil {
		t.Fatalf("DecryptSecret failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Empty plaintext round-trip failed")
	}
}

func TestEncryptDecryptSecret_LargePlaintext(t *testing.T) {
	dek, _ := GenerateDEK()
	plaintext := make([]byte, 1024*1024) // 1MB
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	ciphertext, err := EncryptSecret(dek, plaintext)
	if err != nil {
		t.Fatalf("EncryptSecret with large plaintext failed: %v", err)
	}

	decrypted, err := DecryptSecret(dek, ciphertext)
	if err != nil {
		t.Fatalf("DecryptSecret failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("Large plaintext round-trip failed")
	}
}

// ---- DeriveSealedKEK (US-50.11) ----

func TestDeriveSealedKEKProduces32Bytes(t *testing.T) {
	password := []byte("correct-horse-battery-staple")
	salt := make([]byte, 32)
	for i := range salt {
		salt[i] = byte(i)
	}
	kek, err := DeriveSealedKEK(password, salt, sealedKeyInfoStr)
	require.NoError(t, err)
	require.Len(t, kek, 32)
}

func TestDeriveSealedKEK_DifferentInfoProducesDifferentKeys(t *testing.T) {
	password := []byte("correct-horse-battery-staple")
	salt := make([]byte, 32)
	for i := range salt {
		salt[i] = byte(i)
	}

	kekA, err := DeriveSealedKEK(password, salt, sealedKeyInfoStr)
	require.NoError(t, err)
	kekB, err := DeriveSealedKEK(password, salt, "other-purpose")
	require.NoError(t, err)

	require.Len(t, kekA, 32)
	require.Len(t, kekB, 32)
	require.NotEqual(t, kekA, kekB, "different HKDF info strings must produce independent KEKs")

	// Deterministic for identical inputs.
	kekA2, err := DeriveSealedKEK(password, salt, sealedKeyInfoStr)
	require.NoError(t, err)
	require.Equal(t, kekA, kekA2)
}

func TestDeriveSealedKEK_DistinctFromPlainArgon(t *testing.T) {
	// The info-mixed sub-salt must yield a KEK different from the legacy
	// Argon2id-without-info derivation, proving domain separation.
	password := []byte("correct-horse-battery-staple")
	salt := make([]byte, 32)
	for i := range salt {
		salt[i] = byte(i)
	}

	plain, err := DeriveKEKFromPassword(password, salt)
	require.NoError(t, err)
	seal, err := DeriveSealedKEK(password, salt, sealedKeyInfoStr)
	require.NoError(t, err)
	require.NotEqual(t, plain, seal, "info-mixed KEK must differ from plain Argon2id KEK")
}

func TestDeriveSealedKEK_DifferentPasswords(t *testing.T) {
	salt := make([]byte, 32)
	kek1, err := DeriveSealedKEK([]byte("password-1"), salt, sealedKeyInfoStr)
	require.NoError(t, err)
	kek2, err := DeriveSealedKEK([]byte("password-2"), salt, sealedKeyInfoStr)
	require.NoError(t, err)
	require.NotEqual(t, kek1, kek2, "different passwords must produce different KEKs")
}

func TestDeriveSealedKEKRejectsWrongSaltLength(t *testing.T) {
	shortSalts := [][]byte{nil, make([]byte, 16), make([]byte, 64)}
	for _, s := range shortSalts {
		_, err := DeriveSealedKEK([]byte("password"), s, sealedKeyInfoStr)
		require.ErrorIs(t, err, ErrInvalidSaltLength, "salt len %d must be rejected", len(s))
	}
}
