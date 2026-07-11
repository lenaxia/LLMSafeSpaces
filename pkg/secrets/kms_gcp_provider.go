// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
	"hash/crc32"

	kms "cloud.google.com/go/kms/apiv1"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/protobuf/types/known/wrapperspb"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
)

// gcpKMSCiphertextPrefix is the self-identifying prefix for every ciphertext
// produced by GPCKMSProvider. CompositeProvider routes Decrypt by this prefix.
const gcpKMSCiphertextPrefix = "gcp-kms:v1:"

// gcpKMSClient is the subset of kms.KeyManagementClient used by
// GPCKMSProvider. Defined as an interface so tests can inject a fake
// without standing up a gRPC server.
type gcpKMSClient interface {
	Encrypt(ctx context.Context, req *kmspb.EncryptRequest, opts ...gax.CallOption) (*kmspb.EncryptResponse, error)
	Decrypt(ctx context.Context, req *kmspb.DecryptRequest, opts ...gax.CallOption) (*kmspb.DecryptResponse, error)
}

// GPCKMSProvider implements RootKeyProvider using Google Cloud KMS for
// encrypt/decrypt operations. Parallel to AWSKMSProvider — same threat-model
// properties (KEK never leaves Google's HSM), different SDK and auth.
//
// Auth is via file-mounted service-account JSON (D2: file-mount, not
// Workload Identity Federation — narrower trust surface).
//
// One provider instance holds one KMS key resource name. Per-purpose
// domain separation (D4) is achieved by constructing multiple instances,
// one per purpose.
type GPCKMSProvider struct {
	client  gcpKMSClient
	keyName string
}

// NewGPCKMSProvider constructs a provider from an SDK client and KMS key
// resource name (e.g. "projects/my-project/locations/us-east1/keyRings/
// my-ring/cryptoKeys/my-key").
func NewGPCKMSProvider(client *kms.KeyManagementClient, keyName string) *GPCKMSProvider {
	return &GPCKMSProvider{client: client, keyName: keyName}
}

func (p *GPCKMSProvider) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	out, err := p.client.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:            p.keyName,
		Plaintext:       plaintext,
		PlaintextCrc32C: wrapperspb.Int64(int64(crc32cChecksum(plaintext))),
	})
	if err != nil {
		return nil, fmt.Errorf("gcp kms encrypt: %w", err)
	}
	// Verify GCP received the correct CRC32C — catches in-transit corruption.
	if !out.GetVerifiedPlaintextCrc32C() {
		return nil, fmt.Errorf("gcp kms encrypt: server did not verify plaintext CRC32C — request corrupted in transit")
	}
	// Verify response ciphertext CRC32C — catches response-side corruption.
	if out.CiphertextCrc32C != nil && int64(crc32cChecksum(out.Ciphertext)) != out.CiphertextCrc32C.Value {
		return nil, fmt.Errorf("gcp kms encrypt: response CRC32C mismatch — response corrupted in transit")
	}
	return wrapWithPrefix(gcpKMSCiphertextPrefix, out.Ciphertext), nil
}

func (p *GPCKMSProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	rawCT, err := unwrapKMSCiphertext(gcpKMSCiphertextPrefix, ciphertext)
	if err != nil {
		return nil, err
	}
	out, err := p.client.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:             p.keyName,
		Ciphertext:       rawCT,
		CiphertextCrc32C: wrapperspb.Int64(int64(crc32cChecksum(rawCT))),
	})
	if err != nil {
		return nil, fmt.Errorf("gcp kms decrypt: %w", err)
	}
	// Verify response plaintext CRC32C — catches response-side corruption.
	if out.PlaintextCrc32C != nil && int64(crc32cChecksum(out.Plaintext)) != out.PlaintextCrc32C.Value {
		return nil, fmt.Errorf("gcp kms decrypt: response CRC32C mismatch — response corrupted in transit")
	}
	return out.Plaintext, nil
}

// crc32cChecksum computes the Castagnoli CRC32 of data. GCP KMS
// recommends sending CRC32C values with every request so the server
// can detect in-transit corruption.
func crc32cChecksum(data []byte) uint32 {
	t := crc32.MakeTable(crc32.Castagnoli)
	return crc32.Checksum(data, t)
}

// Compile-time interface check.
var _ RootKeyProvider = (*GPCKMSProvider)(nil)
