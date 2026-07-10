// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is a test double for RootKeyProvider used to exercise
// CompositeProvider dispatch without standing up a real KMS client.
// Each call to Encrypt/Decrypt is recorded so tests can assert routing
// order. Encrypt prepends the configured prefix; Decrypt strips it
// (or returns ErrNotMyCiphertext on prefix mismatch) so the composite's
// dispatch behaviour is exercised against realistic prefix handling.
type fakeProvider struct {
	prefix       string
	encryptErr   error
	decryptErr   error // returned AFTER recording the call; nil = success
	encryptCalls int
	decryptCalls int
}

func (f *fakeProvider) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	f.encryptCalls++
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	// Mimic what real providers do: prefix + plaintext as a stand-in for
	// the encrypted blob. Real providers base64-encode an AES-GCM blob;
	// for routing tests the inner content is irrelevant, only the prefix
	// and call count matter.
	return append([]byte(f.prefix), plaintext...), nil
}

func (f *fakeProvider) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	f.decryptCalls++
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	// Match the production routing logic: if the ciphertext starts with
	// some other known prefix, the fake should return ErrNotMyCiphertext.
	// For the simple test fake we just check our own prefix.
	if !hasPrefix(ciphertext, f.prefix) {
		// Check if it has any prefix-like shape (contains a colon early).
		// If so, treat as foreign → ErrNotMyCiphertext. If not, treat as
		// a legacy blob and fall through to "decryption" (strip prefix
		// and return the rest, simulating successful AES-GCM decrypt).
		if idx := indexOfByte(ciphertext, ':'); idx > 0 && idx < 16 {
			return nil, ErrNotMyCiphertext
		}
		// Legacy blob — fake-success by returning as-is.
		return ciphertext, nil
	}
	// Prefix matches — strip and return inner.
	return ciphertext[len(f.prefix):], nil
}

func hasPrefix(b []byte, p string) bool {
	if len(b) < len(p) {
		return false
	}
	for i := 0; i < len(p); i++ {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

func indexOfByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// --- CompositeProvider tests ---

func TestCompositeProvider_PrimaryOnly_PrefixedWrites(t *testing.T) {
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	c, err := NewCompositeProvider(primary)
	require.NoError(t, err)

	ct, err := c.Encrypt(context.Background(), []byte("payload"))
	require.NoError(t, err)
	assert.Equal(t, "aws-kms:v1:payload", string(ct), "Encrypt must delegate to primary verbatim")
	assert.Equal(t, 1, primary.encryptCalls, "primary must receive the Encrypt call")
}

func TestCompositeProvider_PrimaryOnly_LegacyFallbackDecrypt(t *testing.T) {
	// A composite with only a primary whose prefix is "aws-kms:v1:".
	// A legacy un-prefixed blob (no colon early) should still decrypt via
	// the primary's legacy path (the fake returns the blob as-is).
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	c, err := NewCompositeProvider(primary)
	require.NoError(t, err)

	legacyBlob := []byte{0xDE, 0xAD, 0xBE, 0xEF} // no prefix, no early colon
	pt, err := c.Decrypt(context.Background(), legacyBlob)
	require.NoError(t, err)
	assert.Equal(t, legacyBlob, pt, "primary must handle its own legacy format")
	assert.Equal(t, 1, primary.decryptCalls)
}

func TestCompositeProvider_KMSPrimary_StaticFallback_RoutesByPrefix(t *testing.T) {
	// Composite with KMS primary (prefix "aws-kms:v1:") and a Static
	// fallback. The Static provider's Decrypt handles its own lkms:v1:
	// prefix and legacy blobs; the composite must dispatch an lkms:v1:
	// ciphertext to the fallback, not the primary.
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	staticKey := make([]byte, 32)
	fallback, err := NewStaticKeyProvider(staticKey)
	require.NoError(t, err)

	c, err := NewCompositeProvider(primary, fallback)
	require.NoError(t, err)

	// Encrypt via fallback to produce a real lkms:v1:-prefixed ciphertext.
	lkmsCT, err := fallback.Encrypt(context.Background(), []byte("legacy-row"))
	require.NoError(t, err)

	// Decrypt via composite — must route to fallback, not primary.
	pt, err := c.Decrypt(context.Background(), lkmsCT)
	require.NoError(t, err)
	assert.Equal(t, []byte("legacy-row"), pt)
	assert.Equal(t, 1, primary.decryptCalls, "primary must be tried first (returns ErrNotMyCiphertext)")
}

func TestCompositeProvider_NoMatchingProvider_ReturnsErrNotMyCiphertext(t *testing.T) {
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	c, err := NewCompositeProvider(primary)
	require.NoError(t, err)

	// A ciphertext with a third prefix nobody handles.
	unknownCT := []byte("gcp-kms:v1:something")
	_, err = c.Decrypt(context.Background(), unknownCT)
	require.ErrorIs(t, err, ErrNotMyCiphertext,
		"unknown-prefix ciphertext with no matching provider must surface ErrNotMyCiphertext")
}

func TestCompositeProvider_EncryptUsesPrimary_DecryptTriesAllInOrder(t *testing.T) {
	// Verify dispatch order: primary first, then fallbacks in registration
	// order. The first provider that doesn't return ErrNotMyCiphertext
	// wins (whether it succeeds or returns ErrDecryptionFailed — the
	// composite stops on a prefix match).
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	fallback1 := &fakeProvider{prefix: "lkms:v1:"}
	fallback2 := &fakeProvider{prefix: "gcp-kms:v1:"}

	c, err := NewCompositeProvider(primary, fallback1, fallback2)
	require.NoError(t, err)

	// Ciphertext that matches fallback1's prefix.
	ct := []byte("lkms:v1:payload")
	_, err = c.Decrypt(context.Background(), ct)
	require.NoError(t, err)

	assert.Equal(t, 1, primary.decryptCalls, "primary tried first")
	assert.Equal(t, 1, fallback1.decryptCalls, "fallback1 tried second and matched")
	assert.Equal(t, 0, fallback2.decryptCalls, "fallback2 not tried once a provider matched")
}

// TestCompositeProvider_DecryptStopsOnPrefixMatch_NotErrNotMyCiphertext
// verifies the dispatch invariant: a provider returning ErrDecryptionFailed
// (prefix matched but key wrong) must STOP the dispatch loop, not continue
// to the next provider. Otherwise a corrupt row would produce spurious
// KMS calls against every fallback.
func TestCompositeProvider_DecryptStopsOnPrefixMatch_NotErrNotMyCiphertext(t *testing.T) {
	primary := &fakeProvider{
		prefix:     "aws-kms:v1:",
		decryptErr: ErrDecryptionFailed, // prefix matches, decrypt fails
	}
	fallback := &fakeProvider{prefix: "lkms:v1:"}

	c, err := NewCompositeProvider(primary, fallback)
	require.NoError(t, err)

	// A ciphertext with primary's prefix — primary returns ErrDecryptionFailed,
	// composite must surface that error and NOT try fallback.
	ct := []byte("aws-kms:v1:corrupt")
	_, err = c.Decrypt(context.Background(), ct)
	require.ErrorIs(t, err, ErrDecryptionFailed,
		"ErrDecryptionFailed from a prefix-matching provider must surface, not continue dispatch")
	assert.Equal(t, 1, primary.decryptCalls)
	assert.Equal(t, 0, fallback.decryptCalls, "fallback must not be tried when primary's prefix matched")
}

// TestCompositeProvider_NilPrimary_ReturnsError verifies the constructor
// guards against a nil primary (which would panic on Encrypt).
func TestCompositeProvider_NilPrimary_ReturnsError(t *testing.T) {
	_, err := NewCompositeProvider(nil)
	require.Error(t, err, "constructor must refuse nil primary")
}

// TestNewCompositeProvider_NoFallbacks_OK verifies that a composite with
// only a primary is valid — fallbacks are optional.
func TestNewCompositeProvider_NoFallbacks_OK(t *testing.T) {
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	c, err := NewCompositeProvider(primary)
	require.NoError(t, err)
	assert.NotNil(t, c)
}

// Ensure errors package is exercised even when no test directly imports it
// (some build configurations prune unused imports).
var _ = errors.Is
