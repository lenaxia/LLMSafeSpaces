// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkResolveIsLocalAESGCM is the US-72.1 AC pin: steady-state resolve
// performs ZERO KMS calls. The counting fake KMS fails the benchmark if any
// resolve path touches the provider after construction (D2: KMS is O(boot),
// never O(request)). Run with: go test -bench BenchmarkResolveIsLocalAESGCM ./pkg/secrets/
func BenchmarkResolveIsLocalAESGCM(b *testing.B) {
	kek, err := GenerateStagingKEK()
	require.NoError(b, err)
	kms := newCountingKMS(b)
	secretData, err := BuildKEKSecretData(context.Background(), kms, "llm-relay-kek-v1", kek)
	require.NoError(b, err)

	provider, err := NewKMSStagingProvider(context.Background(), kms, secretData, nil)
	require.NoError(b, err)
	_, decrypts := kms.counts()
	if decrypts != 1 {
		b.Fatalf("construction must unwrap the KEK exactly once, got %d KMS decrypts", decrypts)
	}

	key := []byte("benchmark-provider-key-0123456789")
	envelope, err := provider.Seal(context.Background(), key)
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := provider.Resolve(context.Background(), envelope)
		if err != nil {
			b.Fatal(err)
		}
		if string(got) != string(key) {
			b.Fatalf("resolve roundtrip mismatch at iteration %d", i)
		}
	}
	b.StopTimer()

	_, decrypts = kms.counts()
	if decrypts != 1 {
		b.Fatalf("%d resolves must add zero KMS calls; decrypt count is %d", b.N, decrypts)
	}
	encrypts, _ := kms.counts()
	if encrypts != 1 {
		b.Fatalf("steady-state must not call KMS Encrypt; count is %d", encrypts)
	}
}

func BenchmarkSealIsLocalAESGCM(b *testing.B) {
	kek, err := GenerateStagingKEK()
	require.NoError(b, err)
	kms := newCountingKMS(b)
	secretData, err := BuildKEKSecretData(context.Background(), kms, "llm-relay-kek-v1", kek)
	require.NoError(b, err)

	provider, err := NewKMSStagingProvider(context.Background(), kms, secretData, nil)
	require.NoError(b, err)
	key := []byte(fmt.Sprintf("seal-bench-key-%06d", 0))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := provider.Seal(context.Background(), key); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()

	encrypts, decrypts := kms.counts()
	if decrypts != 1 || encrypts != 1 {
		b.Fatalf("steady-state seal must add zero KMS calls; encrypts=%d decrypts=%d", encrypts, decrypts)
	}
}
