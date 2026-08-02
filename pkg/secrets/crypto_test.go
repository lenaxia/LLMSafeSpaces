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

func TestGenerateRecoveryKey(t *testing.T) {
	key, err := GenerateRecoveryKey()
	if err != nil {
		t.Fatalf("GenerateRecoveryKey failed: %v", err)
	}
	if len(key) != 16 {
		t.Errorf("Recovery key should be 16 bytes, got %d", len(key))
	}
}

func TestWrapUnwrapDEK_RoundTrip(t *testing.T) {
	kek := make([]byte, 32)
	copy(kek, []byte("12345678901234567890123456789012"))

	dek, err := GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK failed: %v", err)
	}

	wrapped, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK failed: %v", err)
	}

	unwrapped, err := UnwrapDEK(kek, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK failed: %v", err)
	}

	if !bytes.Equal(dek, unwrapped) {
		t.Error("Unwrapped DEK should match original")
	}
}

func TestWrapDEK_DifferentCiphertexts(t *testing.T) {
	kek := make([]byte, 32)
	copy(kek, []byte("12345678901234567890123456789012"))
	dek := make([]byte, 32)
	copy(dek, []byte("abcdefghijklmnopqrstuvwxyz012345"))

	wrapped1, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK failed: %v", err)
	}
	wrapped2, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK failed: %v", err)
	}

	if bytes.Equal(wrapped1, wrapped2) {
		t.Error("Two wraps of same DEK should produce different ciphertexts (random nonce)")
	}
}

func TestUnwrapDEK_WrongKey(t *testing.T) {
	kek1 := make([]byte, 32)
	copy(kek1, []byte("12345678901234567890123456789012"))
	kek2 := make([]byte, 32)
	copy(kek2, []byte("abcdefghijklmnopqrstuvwxyz012345"))

	dek, _ := GenerateDEK()
	wrapped, _ := WrapDEK(kek1, dek)

	_, err := UnwrapDEK(kek2, wrapped)
	if err == nil {
		t.Error("UnwrapDEK with wrong key should fail")
	}
	if err != ErrDecryptionFailed {
		t.Errorf("Expected ErrDecryptionFailed, got: %v", err)
	}
}

func TestUnwrapDEK_TamperedCiphertext(t *testing.T) {
	kek := make([]byte, 32)
	copy(kek, []byte("12345678901234567890123456789012"))
	dek, _ := GenerateDEK()
	wrapped, _ := WrapDEK(kek, dek)

	// Tamper with the last byte
	wrapped[len(wrapped)-1] ^= 0xFF

	_, err := UnwrapDEK(kek, wrapped)
	if err == nil {
		t.Error("UnwrapDEK with tampered ciphertext should fail")
	}
}

func TestUnwrapDEK_TooShort(t *testing.T) {
	kek := make([]byte, 32)
	copy(kek, []byte("12345678901234567890123456789012"))

	_, err := UnwrapDEK(kek, []byte("short"))
	if err != ErrInvalidCiphertext {
		t.Errorf("Expected ErrInvalidCiphertext for short input, got: %v", err)
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

func TestFullKeyWrappingFlow(t *testing.T) {
	password := []byte("user-password-secure-123")
	salt, _ := GenerateSalt()
	dek, _ := GenerateDEK()

	kek, err := DeriveKEKFromPassword(password, salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword failed: %v", err)
	}

	wrappedDEK, err := WrapDEK(kek, dek)
	if err != nil {
		t.Fatalf("WrapDEK failed: %v", err)
	}

	kek2, _ := DeriveKEKFromPassword(password, salt)
	unwrappedDEK, err := UnwrapDEK(kek2, wrappedDEK)
	if err != nil {
		t.Fatalf("UnwrapDEK failed: %v", err)
	}

	if !bytes.Equal(dek, unwrappedDEK) {
		t.Error("Full flow: unwrapped DEK should match original")
	}

	secret := []byte("sk-anthropic-key-12345")
	ciphertext, _ := EncryptSecret(unwrappedDEK, secret)

	decrypted, err := DecryptSecret(unwrappedDEK, ciphertext)
	if err != nil {
		t.Fatalf("DecryptSecret failed: %v", err)
	}
	if !bytes.Equal(secret, decrypted) {
		t.Error("Full flow: decrypted secret should match original")
	}
}

func TestPasswordChangeFlow(t *testing.T) {
	password1 := []byte("old-password")
	salt1, _ := GenerateSalt()
	dek, _ := GenerateDEK()

	kek1, _ := DeriveKEKFromPassword(password1, salt1)
	wrappedDEK, _ := WrapDEK(kek1, dek)

	secret := []byte("my-secret")
	ciphertext, _ := EncryptSecret(dek, secret)

	unwrapped, _ := UnwrapDEK(kek1, wrappedDEK)

	password2 := []byte("new-password")
	salt2, _ := GenerateSalt()
	kek2, _ := DeriveKEKFromPassword(password2, salt2)
	newWrappedDEK, _ := WrapDEK(kek2, unwrapped)

	kek2Again, _ := DeriveKEKFromPassword(password2, salt2)
	dekAfterChange, err := UnwrapDEK(kek2Again, newWrappedDEK)
	if err != nil {
		t.Fatalf("UnwrapDEK after password change failed: %v", err)
	}

	decrypted, err := DecryptSecret(dekAfterChange, ciphertext)
	if err != nil {
		t.Fatalf("DecryptSecret after password change failed: %v", err)
	}
	if !bytes.Equal(secret, decrypted) {
		t.Error("Password change should not affect secret decryption")
	}
}

func TestRecoveryKeyFlow(t *testing.T) {
	password := []byte("original-password")
	salt, _ := GenerateSalt()
	recoverySalt, _ := GenerateSalt()
	dek, _ := GenerateDEK()
	recoveryKey, _ := GenerateRecoveryKey()

	kek, _ := DeriveKEKFromPassword(password, salt)
	_, _ = WrapDEK(kek, dek)

	recoveryKEK, _ := DeriveKEKFromKey(recoveryKey, recoverySalt, recInfo)
	wrappedDEKRecovery, _ := WrapDEK(recoveryKEK, dek)

	recoveryKEK2, _ := DeriveKEKFromKey(recoveryKey, recoverySalt, recInfo)
	recoveredDEK, err := UnwrapDEK(recoveryKEK2, wrappedDEKRecovery)
	if err != nil {
		t.Fatalf("Recovery unwrap failed: %v", err)
	}

	if !bytes.Equal(dek, recoveredDEK) {
		t.Error("Recovered DEK should match original")
	}

	newPassword := []byte("new-password-after-reset")
	newSalt, _ := GenerateSalt()
	newKEK, _ := DeriveKEKFromPassword(newPassword, newSalt)
	newWrappedDEK, _ := WrapDEK(newKEK, recoveredDEK)

	newKEK2, _ := DeriveKEKFromPassword(newPassword, newSalt)
	finalDEK, err := UnwrapDEK(newKEK2, newWrappedDEK)
	if err != nil {
		t.Fatalf("UnwrapDEK with new password after recovery failed: %v", err)
	}
	if !bytes.Equal(dek, finalDEK) {
		t.Error("DEK after recovery + re-wrap should match original")
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
