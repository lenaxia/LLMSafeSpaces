// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
)

// awsKMSCiphertextPrefix is the self-identifying prefix for every ciphertext
// produced by AWSKMSProvider. CompositeProvider routes Decrypt by this prefix.
// The base64 layer wraps the opaque KMS ciphertext blob so the prefix boundary
// is byte-aligned (KMS ciphertexts can contain any byte including ':').
const awsKMSCiphertextPrefix = "aws-kms:v1:"

// AWSKMSProvider implements RootKeyProvider using AWS KMS for encrypt/decrypt
// operations. The key material never leaves AWS — every Encrypt and Decrypt
// call is a network round-trip to the KMS API. This converts an API-pod RCE
// from "permanent KEK exfiltration" to "ephemeral compromise bounded by the
// RCE window" (Epic 57 US-57.1, threat-model row 2.4).
//
// Auth is via file-mounted static AWS credentials (D2), not IRSA — narrower
// trust surface per US-50.1's file-mount pattern.
//
// One provider instance holds one KMS key ID. Per-purpose domain separation
// (D4) is achieved by constructing multiple instances, one per purpose, each
// with its own key ARN. The chart exposes per-purpose key ARN configuration.
type AWSKMSProvider struct {
	client interface {
		Encrypt(ctx context.Context, params *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
		Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
	}
	keyID string
}

// NewAWSKMSProvider constructs a provider from an SDK client and KMS key ID
// (full ARN, e.g. "arn:aws:kms:us-east-1:123:key/abc-def"). The client must
// be pre-configured with credentials, region, and (optionally) a custom
// endpoint for testing.
func NewAWSKMSProvider(client *kms.Client, keyID string) *AWSKMSProvider {
	return &AWSKMSProvider{client: client, keyID: keyID}
}

// Encrypt calls KMS Encrypt and wraps the result with aws-kms:v1: prefix.
func (p *AWSKMSProvider) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	out, err := p.client.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     aws.String(p.keyID),
		Plaintext: plaintext,
	})
	if err != nil {
		return nil, fmt.Errorf("kms encrypt: %w", err)
	}
	return wrapWithPrefix(awsKMSCiphertextPrefix, out.CiphertextBlob), nil
}

// Decrypt strips the aws-kms:v1: prefix and calls KMS Decrypt. A foreign
// prefix returns ErrNotMyCiphertext so CompositeProvider can route to the
// next provider. A matching prefix with a corrupt base64 body returns
// ErrDecryptionFailed (same semantics as the local providers).
//
// Unlike the local providers, there is NO legacy un-prefixed fallback path.
// KMS ciphertexts always carry the aws-kms:v1: prefix from day one — there
// are no pre-US-57.1 KMS rows to be backward-compatible with.
func (p *AWSKMSProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	kmsCT, err := unwrapKMSCiphertext(awsKMSCiphertextPrefix, ciphertext)
	if err != nil {
		return nil, err
	}
	out, err := p.client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: kmsCT,
	})
	if err != nil {
		return nil, fmt.Errorf("kms decrypt: %w", err)
	}
	return out.Plaintext, nil
}

// unwrapKMSCiphertext strips the provider's prefix and base64-decodes the
// body. Unlike the local providers' unwrapPrefix, there is no legacy
// passthrough — a KMS provider has no pre-prefix-format rows. A foreign
// prefix returns ErrNotMyCiphertext; no prefix at all also returns
// ErrNotMyCiphertext (this ciphertext was never ours).
func unwrapKMSCiphertext(prefix string, ciphertext []byte) ([]byte, error) {
	if !bytes.HasPrefix(ciphertext, []byte(prefix)) {
		// Check for any known provider prefix (local or otherwise).
		for _, fp := range knownForeignPrefixes {
			if bytes.HasPrefix(ciphertext, []byte(fp)) {
				return nil, ErrNotMyCiphertext
			}
		}
		// Check for the local provider's prefix specifically — the local
		// provider has a legacy un-prefixed fallback that means "no prefix"
		// might be a local-provider row. From the KMS provider's perspective
		// it's still not ours.
		if bytes.HasPrefix(ciphertext, []byte(staticCiphertextPrefix)) {
			return nil, ErrNotMyCiphertext
		}
		// No recognized prefix and not ours → ErrNotMyCiphertext. This is
		// the correct signal for the composite to try the next provider.
		return nil, ErrNotMyCiphertext
	}
	body := ciphertext[len(prefix):]
	raw, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, fmt.Errorf("%w: base64 decode of %q-prefixed ciphertext: %v",
			ErrDecryptionFailed, prefix, err)
	}
	return raw, nil
}

// Compile-time interface check.
var _ RootKeyProvider = (*AWSKMSProvider)(nil)
