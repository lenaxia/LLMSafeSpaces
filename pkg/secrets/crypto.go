// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

const (
	dekSize  = 32
	saltSize = 32

	kekInfo = "llmsafespaces-kek"
	recInfo = "llmsafespaces-recovery"

	argon2Time    = 3
	argon2Memory  = 64 * 1024
	argon2Threads = 4
	argon2KeyLen  = 32
)

var (
	ErrDecryptionFailed  = errors.New("decryption failed: ciphertext tampered or wrong key")
	ErrInvalidCiphertext = errors.New("ciphertext too short")
	ErrInvalidSaltLength = errors.New("salt must be 32 bytes")
)

// DeriveKEKFromPassword derives a 32-byte KEK from a passphrase and salt via
// Argon2id. Despite the name it is a generic KDF primitive — the sealed root-key
// provider uses it to unseal the master key (unsealKeyV0), independent of any
// user password tier. The user-password DEK tier was removed; this function stays
// because the sealed provider depends on it.
func DeriveKEKFromPassword(password, salt []byte) ([]byte, error) {
	if len(salt) != saltSize {
		return nil, ErrInvalidSaltLength
	}
	return argon2.IDKey(password, salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen), nil
}

func DeriveKEKFromKey(keyMaterial, salt []byte, info string) ([]byte, error) {
	hkdfReader := hkdf.New(sha256.New, keyMaterial, salt, []byte(info))
	kek := make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, kek); err != nil {
		return nil, err
	}
	return kek, nil
}

// DeriveSealedKEK derives the KEK used to wrap the sealed root-key file's
// root key. It domain-separates via HKDF: Argon2id has no native info/context
// parameter, so HKDF derives a 32-byte sub-salt from the stored salt bound to
// info, and that sub-salt feeds Argon2id's salt input. Different info values
// therefore produce cryptographically independent KEKs for an identical
// passphrase + salt, while retaining Argon2id's memory-hardness against the
// (typically low-entropy) passphrase. See US-50.11.
func DeriveSealedKEK(password, salt []byte, info string) ([]byte, error) {
	if len(salt) != saltSize {
		return nil, ErrInvalidSaltLength
	}
	subSalt, err := DeriveKEKFromKey(salt, nil, info)
	if err != nil {
		return nil, err
	}
	return argon2.IDKey(password, subSalt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen), nil
}

func GenerateDEK() ([]byte, error) {
	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	return dek, nil
}

func GenerateSalt() ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

func EncryptSecret(dek, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func DecryptSecret(dek, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, ErrInvalidCiphertext
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}
