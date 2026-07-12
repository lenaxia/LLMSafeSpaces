// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"
)

// CompositeProvider dispatches Encrypt/Decrypt across one primary and zero
// or more fallback RootKeyProvider instances. It exists to enable zero-
// downtime migration between provider types (local static/sealed ↔ cloud
// KMS) by routing Decrypt based on the ciphertext's self-identifying prefix.
//
// Design (Epic 57 US-57.1, D3):
//
//   - Encrypt always delegates to the primary. New writes carry the
//     primary's prefix; the composite does not transform the output.
//
//   - Decrypt iterates [primary, fallback...], calling each provider's
//     Decrypt. Dispatch is governed by the providers themselves: each
//     returns ErrNotMyCiphertext when the ciphertext's prefix doesn't
//     match (see unwrapPrefix in root_key.go), ErrDecryptionFailed when
//     the prefix matched but the key was wrong, or success. The composite
//     stops at the first non-ErrNotMyCiphertext result — whether success
//     or genuine decrypt failure — because continuing past a prefix match
//     would produce spurious calls against every fallback for every
//     corrupt row.
//
//   - The composite does NOT implement VersionedProvider. Versioning is
//     per-provider: Static has it; KMS providers track versions cloud-
//     side. Callers needing ActiveVersion() type-assert on the primary
//     via ActiveVersionOf, which returns 1 for non-VersionedProvider
//     primaries — the safe default for the key_version column.
//
// Thread safety: providers are constructed once at boot and never
// reassigned; the composite holds no mutable state. Safe for concurrent
// use after construction.
type CompositeProvider struct {
	primary   RootKeyProvider
	fallbacks []RootKeyProvider
}

// NewCompositeProvider constructs a composite from a required primary and
// zero or more optional fallbacks. Fallbacks are tried in the order
// supplied. Returns an error if primary is nil — a composite with no
// primary has no Encrypt target and would panic on the first write.
//
// Returns an error if any fallback in the variadic tail is nil. A nil
// fallback would panic on Decrypt the first time dispatch reached that
// slot (typically the first foreign-prefix ciphertext under traffic),
// turning a "remove the static mount after migration" operator action
// into an API-pod crash loop. Failing closed at construction surfaces
// the misconfiguration at boot.
//
// Callers that want a primary with no fallbacks should pass no variadic
// args at all: NewCompositeProvider(primary). The composite's Decrypt
// then routes only by the primary's prefix; foreign ciphertexts surface
// as ErrNotMyCiphertext — the same behavior the composite already
// exhibits when every fallback has been exhausted.
func NewCompositeProvider(primary RootKeyProvider, fallbacks ...RootKeyProvider) (*CompositeProvider, error) {
	if primary == nil {
		return nil, errors.New("CompositeProvider requires a non-nil primary provider")
	}
	// Defensive-copy the fallback slice so a caller appending to the
	// variadic input after construction cannot mutate our state. Reject
	// nil entries so dispatch can never dereference a nil provider.
	cp := make([]RootKeyProvider, len(fallbacks))
	for i, f := range fallbacks {
		if f == nil {
			return nil, fmt.Errorf("CompositeProvider fallback #%d is nil; pass no fallback args to construct a primary-only composite", i)
		}
		cp[i] = f
	}
	return &CompositeProvider{primary: primary, fallbacks: cp}, nil
}

// Encrypt delegates to the primary provider. The primary's output
// carries its self-identifying prefix (e.g. aws-kms:v1: or lkms:v1:);
// the composite does not transform it.
func (c *CompositeProvider) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	return c.primary.Encrypt(ctx, plaintext)
}

// Decrypt iterates primary first, then fallbacks in registration order.
// It returns the first non-ErrNotMyCiphertext result — whether success
// or a genuine decrypt failure. If every provider returns
// ErrNotMyCiphertext (no provider recognizes the ciphertext's prefix),
// the composite returns ErrNotMyCiphertext so the caller knows the row
// is unroutable rather than corrupt.
//
// The "stop on first non-routing result" invariant is load-bearing:
// a provider whose prefix matched but whose key didn't (ErrDecryptionFailed)
// must terminate dispatch, otherwise a single corrupt row would generate
// N-1 spurious decrypt calls (one per remaining provider) on every read.
// TestCompositeProvider_DecryptStopsOnPrefixMatch_NotErrNotMyCiphertext
// pins this behavior.
func (c *CompositeProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	pt, err := c.primary.Decrypt(ctx, ciphertext)
	if !errors.Is(err, ErrNotMyCiphertext) {
		return pt, err
	}
	for _, f := range c.fallbacks {
		pt, err = f.Decrypt(ctx, ciphertext)
		if !errors.Is(err, ErrNotMyCiphertext) {
			return pt, err
		}
	}
	return nil, fmt.Errorf("%w: no provider recognized the ciphertext prefix", ErrNotMyCiphertext)
}

// Compile-time interface check.
var _ RootKeyProvider = (*CompositeProvider)(nil)
