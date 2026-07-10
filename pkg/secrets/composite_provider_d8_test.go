// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingAudit is a test AuditWriter that records every LogAudit call
// behind a mutex so concurrent goroutines (AuditedProvider writes
// fire-and-forget) don't race with the test's read.
type recordingAudit struct {
	mu      sync.Mutex
	entries []*AuditEntry
}

func (r *recordingAudit) LogAudit(_ context.Context, entry *AuditEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
	return nil
}

func (r *recordingAudit) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// waitForAuditCount polls up to 1s for the fire-and-forget audit goroutines
// to land their entries. AuditedProvider writes via `go func()` so the test
// must allow time for the scheduler.
func waitForAuditCount(t *testing.T, r *recordingAudit, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if r.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equalf(t, want, r.count(), "expected %d audit entries within 1s; got %d", want, r.count())
}

// TestCompositeProvider_AuditedWrapperComposition_LegacyRowDecryptProducesExactlyOneAuditRow
// is the D8 regression test. Without the D8 construction order (AuditedProvider
// wraps the composite externally, not each member), a legacy-row decrypt in
// a KMS-primary deployment would audit twice: once when the KMS primary
// returns ErrNotMyCiphertext, once when the static fallback succeeds. This
// test pins the "one audit row per Decrypt call" invariant.
//
// The construction order itself is the responsibility of app.go wiring
// (PR3 of US-57.1). This test verifies the composition property: that
// wrapping CompositeProvider in AuditedProvider produces one audit row
// regardless of how many internal providers were tried. It does NOT
// verify that app.go constructs the wiring correctly — that's the
// integration test's job in PR3.
func TestCompositeProvider_AuditedWrapperComposition_LegacyRowDecryptProducesExactlyOneAuditRow(t *testing.T) {
	// Build a composite with KMS-primary + static-fallback. Use a real
	// StaticKeyProvider for the fallback so the legacy blob actually
	// decrypts (the fakeProvider doesn't do real AES-GCM).
	staticKey := make([]byte, 32)
	for i := range staticKey {
		staticKey[i] = byte(i + 1)
	}
	fallback, err := NewStaticKeyProvider(staticKey)
	require.NoError(t, err)

	// Primary is a fake KMS provider whose prefix doesn't match the
	// legacy blob, so Decrypt returns ErrNotMyCiphertext and the
	// composite moves on to the fallback.
	primary := &fakeProvider{prefix: "aws-kms:v1:"}

	composite, err := NewCompositeProvider(primary, fallback)
	require.NoError(t, err)

	// Wrap the COMPOSITE in AuditedProvider (the D8 construction order).
	audit := &recordingAudit{}
	audited := NewAuditedProvider(composite, audit, "test-purpose")

	// Encrypt a real lkms:v1:-prefixed ciphertext via the fallback so the
	// composite's Decrypt actually routes to it and succeeds.
	lkmsCT, err := fallback.Encrypt(context.Background(), []byte("legacy-row"))
	require.NoError(t, err)

	// Decrypt via the audited composite. Primary returns ErrNotMyCiphertext,
	// fallback returns success — but the audit should fire EXACTLY ONCE
	// because the AuditedProvider wraps the composite, not each member.
	_, err = audited.Decrypt(context.Background(), lkmsCT)
	require.NoError(t, err)

	waitForAuditCount(t, audit, 1)
	assert.Equal(t, 1, primary.decryptCalls, "primary tried first (returns ErrNotMyCiphertext)")
}

// TestCompositeProvider_AuditedWrapperComposition_PrimarySuccess_StillOneAuditRow
// is the symmetric case: when the primary itself succeeds, exactly one audit
// row is still produced (not zero, not multiple). Guards against a future
// refactor that skips audit on the fast path.
func TestCompositeProvider_AuditedWrapperComposition_PrimarySuccess_StillOneAuditRow(t *testing.T) {
	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	composite, err := NewCompositeProvider(primary)
	require.NoError(t, err)

	audit := &recordingAudit{}
	audited := NewAuditedProvider(composite, audit, "test-purpose")

	ct := []byte("aws-kms:v1:payload")
	_, err = audited.Decrypt(context.Background(), ct)
	require.NoError(t, err)

	waitForAuditCount(t, audit, 1)
}

// TestAuditedProvider_WrappingEachMember_ProducesDoubleAuditRows is the
// ANTI-test: it constructs the WRONG wiring order (AuditedProvider wraps
// each member, then the members are composed) and verifies it produces
// MORE than one audit row. This makes the D8 invariant concrete — if a
// future maintainer accidentally reintroduces per-member wrapping, this
// test's expectation (multiple rows) is what they'd see, and the
// regression test above (one row) would fail.
//
// Skipped from the normal test run via t.Skip pattern is NOT used — this
// is a real test that asserts the wrong-order behaviour so the diff
// between right and wrong is visible in the test file.
func TestAuditedProvider_WrappingEachMember_ProducesDoubleAuditRows(t *testing.T) {
	audit := &recordingAudit{}

	// Wrong order: wrap each member first.
	staticKey := make([]byte, 32)
	staticFallback, err := NewStaticKeyProvider(staticKey)
	require.NoError(t, err)
	auditedFallback := NewAuditedProvider(staticFallback, audit, "fallback")

	primary := &fakeProvider{prefix: "aws-kms:v1:"}
	auditedPrimary := NewAuditedProvider(primary, audit, "primary")

	// Then compose. The composite calls each member's Decrypt; each
	// member is independently audited.
	composite, err := NewCompositeProvider(auditedPrimary, auditedFallback)
	require.NoError(t, err)

	// A ciphertext that routes to the fallback (primary returns
	// ErrNotMyCiphertext, fallback succeeds). Both get audited.
	lkmsCT, err := staticFallback.Encrypt(context.Background(), []byte("legacy"))
	require.NoError(t, err)

	_, err = composite.Decrypt(context.Background(), lkmsCT)
	require.NoError(t, err)

	// Expect TWO audit entries — one from each member's AuditedProvider
	// wrapper. This is the bug D8 prevents.
	waitForAuditCount(t, audit, 2)
	assert.Equal(t, 2, audit.count(),
		"wrong-order wrapping produces one audit row per provider tried — this is the bug D8 prevents")
}
