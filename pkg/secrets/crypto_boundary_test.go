// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestDeriveKEKFromPassword_EmptyPassword(t *testing.T) {
	salt := make([]byte, 32)
	kek, err := DeriveKEKFromPassword([]byte{}, salt)
	if err != nil {
		t.Fatalf("DeriveKEKFromPassword with empty password should succeed: %v", err)
	}
	if len(kek) != 32 {
		t.Errorf("KEK should still be 32 bytes, got %d", len(kek))
	}
}

func TestDeriveKEKFromPassword_EmptySalt(t *testing.T) {
	_, err := DeriveKEKFromPassword([]byte("password"), []byte{})
	if err == nil {
		t.Error("DeriveKEKFromPassword with empty salt should fail")
	}
}

func TestDeriveKEKFromKey_EmptyInfo(t *testing.T) {
	salt := make([]byte, 32)
	kek, err := DeriveKEKFromKey([]byte("password"), salt, "")
	if err != nil {
		t.Fatalf("DeriveKEKFromKey with empty info should succeed: %v", err)
	}
	if len(kek) != 32 {
		t.Errorf("KEK should be 32 bytes, got %d", len(kek))
	}
}

func TestEncryptSecret_InvalidKeySize(t *testing.T) {
	_, err := EncryptSecret([]byte("short"), []byte("plaintext"))
	if err == nil {
		t.Error("EncryptSecret with invalid key size should fail")
	}
}

func TestDecryptSecret_NilCiphertext(t *testing.T) {
	dek := make([]byte, 32)
	_, err := DecryptSecret(dek, nil)
	if err != ErrInvalidCiphertext {
		t.Errorf("Expected ErrInvalidCiphertext for nil, got: %v", err)
	}
}

func TestDecryptSecret_ExactlyNonceSize(t *testing.T) {
	dek := make([]byte, 32)
	block, _ := aes.NewCipher(dek)
	gcm, _ := cipher.NewGCM(block)
	// Input exactly nonce size — no ciphertext to decrypt
	nonce := make([]byte, gcm.NonceSize())
	_, err := DecryptSecret(dek, nonce)
	if err == nil {
		t.Error("Decrypting nonce-only input should fail")
	}
}

func TestEncryptDecryptSecret_BinaryData(t *testing.T) {
	dek, _ := GenerateDEK()
	// Binary data with null bytes
	plaintext := []byte{0x00, 0x01, 0xFF, 0xFE, 0x00, 0x00, 0xAB}

	ct, err := EncryptSecret(dek, plaintext)
	if err != nil {
		t.Fatalf("Encrypt binary data failed: %v", err)
	}
	pt, err := DecryptSecret(dek, ct)
	if err != nil {
		t.Fatalf("Decrypt binary data failed: %v", err)
	}
	if !bytes.Equal(plaintext, pt) {
		t.Error("Binary data round-trip failed")
	}
}

func TestEncryptDecryptSecret_UnicodeData(t *testing.T) {
	dek, _ := GenerateDEK()
	plaintext := []byte("密码是：🔑 très sécurisé 日本語テスト")

	ct, _ := EncryptSecret(dek, plaintext)
	pt, err := DecryptSecret(dek, ct)
	if err != nil {
		t.Fatalf("Decrypt unicode failed: %v", err)
	}
	if !bytes.Equal(plaintext, pt) {
		t.Error("Unicode round-trip failed")
	}
}
