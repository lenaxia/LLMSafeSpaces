package secrets

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaticKeyProvider_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	plaintext := []byte("lsp_deadbeef1234567890abcdef01234567890abcdef01234567890abcdef0123")
	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)
	require.NotEmpty(t, ct)

	decrypted, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestStaticKeyProvider_DifferentCiphertextEachEncrypt(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	plaintext := []byte("test-plaintext-data")
	ct1, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)
	ct2, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)
	assert.NotEqual(t, ct1, ct2, "random nonce should produce different ciphertext")
}

func TestStaticKeyProvider_WrongKeyFailsDecrypt(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	key2[0] = 1

	p1, err := NewStaticKeyProvider(key1)
	require.NoError(t, err)
	p2, err := NewStaticKeyProvider(key2)
	require.NoError(t, err)

	ct, err := p1.Encrypt(context.Background(), []byte("secret"))
	require.NoError(t, err)

	_, err = p2.Decrypt(context.Background(), ct)
	assert.ErrorIs(t, err, ErrDecryptionFailed)
}

func TestStaticKeyProvider_TamperedCiphertextFailsDecrypt(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	ct, err := p.Encrypt(context.Background(), []byte("secret"))
	require.NoError(t, err)

	ct[len(ct)-1] ^= 0xFF
	_, err = p.Decrypt(context.Background(), ct)
	assert.ErrorIs(t, err, ErrDecryptionFailed)
}

func TestStaticKeyProvider_TruncatedCiphertextFails(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	_, err = p.Decrypt(context.Background(), []byte{0x01, 0x02})
	assert.ErrorIs(t, err, ErrInvalidCiphertext)
}

func TestNewStaticKeyProvider_RejectsWrongSize(t *testing.T) {
	_, err := NewStaticKeyProvider(make([]byte, 16))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "32 bytes")

	_, err = NewStaticKeyProvider(make([]byte, 64))
	assert.Error(t, err)

	_, err = NewStaticKeyProvider(nil)
	assert.Error(t, err)
}

func TestStaticKeyProvider_CancelledContext(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	plaintext := []byte("test-data")
	ct, err := p.Encrypt(ctx, plaintext)
	require.NoError(t, err, "StaticKeyProvider ignores context cancellation (local AES)")

	decrypted, err := p.Decrypt(ctx, ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestStaticKeyProvider_LargePlaintext(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	plaintext := make([]byte, 4096)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)

	decrypted, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestSealedKeyProvider_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")
	passphrasePath := filepath.Join(tmpDir, "passphrase")

	passphrase := []byte("correct-horse-battery-staple")
	require.NoError(t, os.WriteFile(passphrasePath, passphrase, 0600))

	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	require.NoError(t, SealRootKey(sealedPath, passphrase, rootKey))

	p, err := NewSealedKeyProvider(sealedPath, passphrasePath)
	require.NoError(t, err)

	plaintext := []byte("lsp_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6")
	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)

	decrypted, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestSealedKeyProvider_WrongPassphraseFails(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")
	goodPassPath := filepath.Join(tmpDir, "good-pass")
	badPassPath := filepath.Join(tmpDir, "bad-pass")

	goodPass := []byte("correct-passphrase")
	badPass := []byte("wrong-passphrase")
	require.NoError(t, os.WriteFile(goodPassPath, goodPass, 0600))
	require.NoError(t, os.WriteFile(badPassPath, badPass, 0600))

	rootKey := make([]byte, 32)
	require.NoError(t, SealRootKey(sealedPath, goodPass, rootKey))

	_, err := NewSealedKeyProvider(sealedPath, badPassPath)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unseal")
}

func TestSealedKeyProvider_MissingSealedKeyFileFails(t *testing.T) {
	tmpDir := t.TempDir()
	passPath := filepath.Join(tmpDir, "passphrase")
	require.NoError(t, os.WriteFile(passPath, []byte("pass"), 0600))

	_, err := NewSealedKeyProvider(filepath.Join(tmpDir, "nonexistent"), passPath)
	assert.Error(t, err)
}

func TestSealedKeyProvider_MissingPassphraseFileFails(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")
	passPath := filepath.Join(tmpDir, "nonexistent")

	require.NoError(t, SealRootKey(sealedPath, []byte("pass"), make([]byte, 32)))

	_, err := NewSealedKeyProvider(sealedPath, passPath)
	assert.Error(t, err)
}

func TestSealedKeyProvider_CorruptedSealedKeyFails(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")
	passPath := filepath.Join(tmpDir, "passphrase")

	require.NoError(t, os.WriteFile(sealedPath, []byte("not-valid-sealed-data"), 0600))
	require.NoError(t, os.WriteFile(passPath, []byte("any-passphrase"), 0600))

	_, err := NewSealedKeyProvider(sealedPath, passPath)
	assert.Error(t, err)
}

func TestSealedKeyProvider_TruncatedSealedKeyFails(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")
	passPath := filepath.Join(tmpDir, "passphrase")

	require.NoError(t, SealRootKey(sealedPath, []byte("pass"), make([]byte, 32)))

	data, err := os.ReadFile(sealedPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sealedPath, data[:10], 0600))
	require.NoError(t, os.WriteFile(passPath, []byte("pass"), 0600))

	_, err = NewSealedKeyProvider(sealedPath, passPath)
	assert.Error(t, err)
}

func TestSealedKeyProvider_EncryptDecryptWithRealAPIKeyData(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")
	passPath := filepath.Join(tmpDir, "passphrase")
	require.NoError(t, os.WriteFile(passPath, []byte("opensusame"), 0600))

	rootKey, err := GenerateDEK()
	require.NoError(t, err)
	require.NoError(t, SealRootKey(sealedPath, []byte("opensusame"), rootKey))

	p, err := NewSealedKeyProvider(sealedPath, passPath)
	require.NoError(t, err)

	rawAPIKey := "lsp_" + hex.EncodeToString(make([]byte, 32))

	ct, err := p.Encrypt(context.Background(), []byte(rawAPIKey))
	require.NoError(t, err)

	decrypted, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)

	require.Equal(t, 1, subtle.ConstantTimeCompare([]byte(rawAPIKey), decrypted))
}

func TestSealRootKey_DeterministicFormat(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed-key")

	passphrase := []byte("test-passphrase")
	rootKey := make([]byte, 32)

	require.NoError(t, SealRootKey(sealedPath, passphrase, rootKey))

	data, err := os.ReadFile(sealedPath)
	require.NoError(t, err)

	// V1 format (US-50.11): magic "LSKP-S"(6) || salt(32) || nonce(12) || ciphertext(32+16)
	// GCM adds a 16-byte tag to the 32-byte root key = 48 bytes ciphertext.
	// Total minimum: 6 + 32 + 12 + 48 = 98 bytes.
	assert.True(t, bytes.HasPrefix(data, []byte(sealedMagicV1)), "sealed key must start with the V1 magic prefix")
	assert.GreaterOrEqual(t, len(data), 98, "sealed key should contain magic + salt + nonce + ciphertext")
}

// writeLegacyV0SealedKey writes a sealed-key file in the pre-US-50.11 format
// (salt || ciphertext; KEK = Argon2id(passphrase, salt) with no HKDF info
// string). It exists only to prove the unseal path still reads old files.
func writeLegacyV0SealedKey(t *testing.T, path string, passphrase, rootKey []byte) {
	t.Helper()
	salt := make([]byte, sealedSaltSize)
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	kek, err := DeriveKEKFromPassword(passphrase, salt)
	require.NoError(t, err)
	ct, err := EncryptSecret(kek, rootKey)
	require.NoError(t, err)
	sealed := append(append([]byte{}, salt...), ct...)
	require.NoError(t, os.WriteFile(path, sealed, 0600))
}

func TestSealedKeyProvider_UnsealLegacyV0Format(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed")
	passPath := filepath.Join(tmpDir, "passphrase")
	passphrase := []byte("legacy-passphrase")
	require.NoError(t, os.WriteFile(passPath, passphrase, 0600))

	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	writeLegacyV0SealedKey(t, sealedPath, passphrase, rootKey)

	p, err := NewSealedKeyProvider(sealedPath, passPath)
	require.NoError(t, err, "legacy V0 sealed key must still unseal after US-50.11")

	plaintext := []byte("lsp_legacy_v0_roundtrip")
	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)
	dec, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

func TestSealedKeyProvider_RoundTrip_V1Format(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed")
	passPath := filepath.Join(tmpDir, "passphrase")
	passphrase := []byte("v1-passphrase")
	require.NoError(t, os.WriteFile(passPath, passphrase, 0600))

	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	require.NoError(t, SealRootKey(sealedPath, passphrase, rootKey))

	data, err := os.ReadFile(sealedPath)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(data, []byte(sealedMagicV1)), "newly sealed keys must use the V1 (LSKP-S) format")

	p, err := NewSealedKeyProvider(sealedPath, passPath)
	require.NoError(t, err)

	plaintext := []byte("lsp_v1_format_roundtrip")
	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)
	dec, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

// --- US-57.1: prefix-aware Encrypt/Decrypt for CompositeProvider dispatch ---

// TestStaticKeyProvider_Encrypt_WrapsWithLkmsPrefix verifies that new writes
// from a StaticKeyProvider carry the lkms:v1: prefix, so a future CompositeProvider
// can route Decrypt by prefix without trial-and-error on every row.
func TestStaticKeyProvider_Encrypt_WrapsWithLkmsPrefix(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	ct, err := p.Encrypt(context.Background(), []byte("prefixed-write-test"))
	require.NoError(t, err)

	previewLen := min(len(ct), 32)
	assert.True(t, bytes.HasPrefix(ct, []byte(staticCiphertextPrefix)),
		"new StaticKeyProvider ciphertexts must carry the %q prefix for composite dispatch; got prefix %q",
		staticCiphertextPrefix, ct[:previewLen])
}

// TestStaticKeyProvider_PrefixedDecrypt_RoundTrip verifies that a ciphertext
// produced by Encrypt (which now carries the lkms:v1: prefix) decrypts correctly.
func TestStaticKeyProvider_PrefixedDecrypt_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	plaintext := []byte("prefixed-roundtrip")
	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)

	dec, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

// TestStaticKeyProvider_LegacyUnprefixedCiphertext_StillDecrypts is the
// backward-compatibility guarantee: rows written before US-57.1 (raw
// 12-byte-nonce + AES-GCM blobs with no prefix) MUST continue to decrypt
// after the prefix-aware code ships, or every existing deployment breaks
// on upgrade.
//
// The legacy ciphertext is constructed by calling EncryptSecret directly
// (the pre-US-57.1 code path), bypassing the prefix wrapper.
func TestStaticKeyProvider_LegacyUnprefixedCiphertext_StillDecrypts(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	plaintext := []byte("legacy-row-from-pre-us57-deployment")
	// Construct a raw (un-prefixed) ciphertext exactly as the pre-US-57.1
	// code did: direct AES-GCM with random nonce.
	legacyCT, err := EncryptSecret(key, plaintext)
	require.NoError(t, err)
	// Sanity: legacy ciphertext must NOT have the prefix.
	require.False(t, bytes.HasPrefix(legacyCT, []byte(staticCiphertextPrefix)),
		"test setup error: legacy ciphertext unexpectedly has prefix")

	dec, err := p.Decrypt(context.Background(), legacyCT)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec,
		"legacy un-prefixed ciphertexts must decrypt via the fallback path")
}

// TestStaticKeyProvider_PrefixMismatch_ReturnsErrNotMyCiphertext verifies
// the routing signal: when Decrypt sees a prefixed ciphertext from a
// DIFFERENT provider (e.g. aws-kms:v1:), it must return ErrNotMyCiphertext
// so the composite can try the next provider. Returning ErrDecryptionFailed
// here would cause the composite to stop and report failure.
func TestStaticKeyProvider_PrefixMismatch_ReturnsErrNotMyCiphertext(t *testing.T) {
	key := make([]byte, 32)
	p, err := NewStaticKeyProvider(key)
	require.NoError(t, err)

	// A ciphertext with a foreign prefix — constructed to look like a KMS
	// provider's output. The StaticKeyProvider has never seen this prefix
	// and must not attempt trial-decrypt (which would always fail and
	// pollute logs with spurious "wrong key" errors).
	foreignCT := []byte("aws-kms:v1:" + base64.StdEncoding.EncodeToString([]byte("not-our-format")))
	_, err = p.Decrypt(context.Background(), foreignCT)
	require.ErrorIs(t, err, ErrNotMyCiphertext,
		"foreign-prefixed ciphertexts must return ErrNotMyCiphertext for composite routing")
}

// TestSealedKeyProvider_Encrypt_WrapsWithLkmsPrefix mirrors the static
// test for the sealed path: both local providers produce the same prefix
// so the composite's fallback handling is uniform.
func TestSealedKeyProvider_Encrypt_WrapsWithLkmsPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed")
	passPath := filepath.Join(tmpDir, "pass")
	require.NoError(t, os.WriteFile(passPath, []byte("passphrase-content-here"), 0600))

	rootKey := make([]byte, 32)
	require.NoError(t, SealRootKey(sealedPath, []byte("passphrase-content-here"), rootKey))

	p, err := NewSealedKeyProvider(sealedPath, passPath)
	require.NoError(t, err)

	ct, err := p.Encrypt(context.Background(), []byte("sealed-prefixed"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(ct, []byte(staticCiphertextPrefix)),
		"SealedKeyProvider must produce the same %q prefix as StaticKeyProvider for uniform composite routing",
		staticCiphertextPrefix)
}

// TestSealedKeyProvider_LegacyUnprefixedCiphertext_StillDecrypts mirrors
// the static backward-compat test for the sealed path.
func TestSealedKeyProvider_LegacyUnprefixedCiphertext_StillDecrypts(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed")
	passPath := filepath.Join(tmpDir, "pass")
	require.NoError(t, os.WriteFile(passPath, []byte("pass"), 0600))

	// Construct a sealed provider and read its raw key so we can build a
	// legacy un-prefixed ciphertext via direct EncryptSecret.
	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	require.NoError(t, SealRootKey(sealedPath, []byte("pass"), rootKey))
	p, err := NewSealedKeyProvider(sealedPath, passPath)
	require.NoError(t, err)

	plaintext := []byte("sealed-legacy-row")
	legacyCT, err := EncryptSecret(rootKey, plaintext)
	require.NoError(t, err)
	require.False(t, bytes.HasPrefix(legacyCT, []byte(staticCiphertextPrefix)))

	dec, err := p.Decrypt(context.Background(), legacyCT)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}
