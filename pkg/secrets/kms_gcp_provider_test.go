// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
)

// fakeGCPKMSClient is a test double for gcpKMSClient. It implements
// Encrypt/Decrypt with XOR "encryption" sufficient to verify the
// provider's prefix wrapping, prefix routing, and error propagation
// — same pattern as the AWS KMS fake server but using the GCP
// protobuf request/response types directly.
type fakeGCPKMSClient struct {
	encryptErr error
	decryptErr error
	keyBytes   []byte // 32-byte XOR key for fake crypto
}

func newFakeGCPKMSClient() *fakeGCPKMSClient {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return &fakeGCPKMSClient{keyBytes: key}
}

func (f *fakeGCPKMSClient) Encrypt(_ context.Context, req *kmspb.EncryptRequest, _ ...gax.CallOption) (*kmspb.EncryptResponse, error) {
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	ct := make([]byte, len(req.Plaintext))
	for i, b := range req.Plaintext {
		ct[i] = b ^ f.keyBytes[i%len(f.keyBytes)]
	}
	crc := crc32cChecksum(ct)
	return &kmspb.EncryptResponse{
		Ciphertext:              ct,
		CiphertextCrc32C:        wrapperspb.Int64(int64(crc)),
		VerifiedPlaintextCrc32C: true,
	}, nil
}

func (f *fakeGCPKMSClient) Decrypt(_ context.Context, req *kmspb.DecryptRequest, _ ...gax.CallOption) (*kmspb.DecryptResponse, error) {
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	pt := make([]byte, len(req.Ciphertext))
	for i, b := range req.Ciphertext {
		pt[i] = b ^ f.keyBytes[i%len(f.keyBytes)]
	}
	crc := crc32cChecksum(pt)
	return &kmspb.DecryptResponse{
		Plaintext:       pt,
		PlaintextCrc32C: wrapperspb.Int64(int64(crc)),
	}, nil
}

// --- GPCKMSProvider tests ---

func TestGPCKMSProvider_RoundTrip_EncryptThenDecrypt(t *testing.T) {
	client := newFakeGCPKMSClient()
	p := &GPCKMSProvider{client: client, keyName: "projects/test/locations/us/keyRings/r/cryptoKeys/k"}

	plaintext := []byte("the-llm-api-key-value")
	ct, err := p.Encrypt(context.Background(), plaintext)
	require.NoError(t, err)

	dec, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

func TestGPCKMSProvider_Encrypt_WrapsWithGcpKmsV1Prefix(t *testing.T) {
	client := newFakeGCPKMSClient()
	p := &GPCKMSProvider{client: client, keyName: "test-key"}

	ct, err := p.Encrypt(context.Background(), []byte("payload"))
	require.NoError(t, err)

	require.True(t, bytes.HasPrefix(ct, []byte(gcpKMSCiphertextPrefix)),
		"GPCKMSProvider.Encrypt output must start with %q; got %q",
		gcpKMSCiphertextPrefix, ct[:min(len(ct), 32)])

	body := ct[len(gcpKMSCiphertextPrefix):]
	_, err = base64.StdEncoding.DecodeString(string(body))
	require.NoError(t, err, "body after prefix must be valid base64")
}

func TestGPCKMSProvider_PrefixMismatch_ReturnsErrNotMyCiphertext(t *testing.T) {
	client := newFakeGCPKMSClient()
	p := &GPCKMSProvider{client: client, keyName: "test-key"}

	foreignCT := []byte(staticCiphertextPrefix + base64.StdEncoding.EncodeToString([]byte("not-ours")))
	_, err := p.Decrypt(context.Background(), foreignCT)
	require.ErrorIs(t, err, ErrNotMyCiphertext,
		"local-prefixed ciphertext must return ErrNotMyCiphertext for GCP KMS provider routing")
}

func TestGPCKMSProvider_Decrypt_KMSUnavailable_ReturnsError(t *testing.T) {
	client := &fakeGCPKMSClient{
		decryptErr: errors.New("rpc error: code = Unavailable"),
	}
	p := &GPCKMSProvider{client: client, keyName: "test-key"}

	ct := []byte(gcpKMSCiphertextPrefix + base64.StdEncoding.EncodeToString([]byte("anything")))
	_, err := p.Decrypt(context.Background(), ct)
	require.Error(t, err, "KMS unavailable must surface as an error")
	assert.NotErrorIs(t, err, ErrNotMyCiphertext,
		"KMS-unavailable must not be classified as a routing signal")
}

func TestGPCKMSProvider_Encrypt_KMSUnavailable_ReturnsError(t *testing.T) {
	client := &fakeGCPKMSClient{
		encryptErr: errors.New("rpc error: code = Unavailable"),
	}
	p := &GPCKMSProvider{client: client, keyName: "test-key"}

	_, err := p.Encrypt(context.Background(), []byte("payload"))
	require.Error(t, err, "KMS unavailable on Encrypt must surface as an error")
}

func TestGPCKMSProvider_Decrypt_CorruptBase64_ReturnsErrDecryptionFailed(t *testing.T) {
	client := newFakeGCPKMSClient()
	p := &GPCKMSProvider{client: client, keyName: "test-key"}

	corruptCT := []byte(gcpKMSCiphertextPrefix + "!@#$%^&*()")
	_, err := p.Decrypt(context.Background(), corruptCT)
	require.ErrorIs(t, err, ErrDecryptionFailed,
		"prefix matched but body corrupt must return ErrDecryptionFailed")
}
