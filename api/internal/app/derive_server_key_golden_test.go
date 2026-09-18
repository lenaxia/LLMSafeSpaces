// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeriveServerKey_GoldenByteCompat pins the server's purpose-key
// derivation to the #832 golden fixtures (pkg/secrets/testdata/
// derive_server_key_golden.txt). Those bytes were captured from this
// function's pre-consolidation body, the two CLI deriveKey copies, and the
// primitive (DeriveKEKFromKey + llmsafespaces-server salt) — all three
// agreed byte-for-byte. deriveServerKey now delegates to
// secrets.DeriveServerKey; this test fails if that delegation ever changes
// the derived bytes.
func TestDeriveServerKey_GoldenByteCompat(t *testing.T) {
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i + 1)
	}
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv(masterSecretValueEnv, hex.EncodeToString(master))
	t.Setenv(masterSecretLegacyEnv, "")

	golden := map[string]string{
		"provider-credentials": "fe448d678443b728ecd2edb73c616777d287f849fbcce7f6ed9cf17fc6678bdb",
		"org-credentials":      "36c46c7a97d1cf00391adf7ca2a4b1a7a5e3eb59cf72ec83634f54d66b2cf86a",
		"master-kek":           "fec3e30fc3b7c93acac52d47cdafcbdc1d1900cbcb310f6aeec23210fb89fbb9",
		"dek-cache":            "5f4bd58e6c9bc0598261af063c434079a8400ee463df7df46225995b64a238d2",
	}
	for purpose, wantHex := range golden {
		want, err := hex.DecodeString(wantHex)
		require.NoError(t, err)
		got := deriveServerKey(purpose)
		require.NotNil(t, got, "purpose %s", purpose)
		assert.Equal(t, want, got, "purpose %s: deriveServerKey must reproduce the #832 golden bytes", purpose)
	}
}
