// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	_ "embed"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenDeriveServerKeyFixtures is the byte-compat golden matrix captured
// from the three pre-consolidation HKDF implementations (issue #832):
// cmd/rotate-kek deriveKey, cmd/migrate-kek deriveKey, and the API app
// package's deriveServerKey. Before consolidation, a throwaway generator in
// each package emitted its outputs for this exact master × purpose matrix;
// the three outputs were diffed and found byte-identical (28/28 records,
// including the 31-byte-master → nil edge). This file freezes those bytes so
// the consolidated DeriveServerKey can never drift from any of them.
//
//go:embed testdata/derive_server_key_golden.txt
var goldenDeriveServerKeyFixtures string

// goldenMasterMaterial rebuilds the fixture master keys. The IDs match the
// golden file's first column.
func goldenMasterMaterial(id string) []byte {
	switch id {
	case "m1-32B":
		b := make([]byte, 32)
		for i := range b {
			b[i] = byte(i + 1)
		}
		return b
	case "m2-48B":
		b := make([]byte, 48)
		for i := range b {
			b[i] = byte(i + 50)
		}
		return b
	case "m3-31B":
		b := make([]byte, 31)
		for i := range b {
			b[i] = byte(i + 90)
		}
		return b
	case "m4-z32":
		return make([]byte, 32)
	default:
		return nil
	}
}

// TestDeriveServerKey_GoldenByteCompat is the #832 gate: the consolidated
// export must reproduce the captured outputs byte-for-byte. An empty hex
// field encodes the nil return for the sub-32-byte master.
func TestDeriveServerKey_GoldenByteCompat(t *testing.T) {
	lines := strings.Split(strings.TrimSpace(goldenDeriveServerKeyFixtures), "\n")
	require.NotEmpty(t, lines, "golden fixture file must not be empty")
	for _, ln := range lines {
		parts := strings.Split(ln, "\t")
		require.Len(t, parts, 3, "golden line must be master\\tpurpose\\thex: %q", ln)
		masterID, purpose, wantHex := parts[0], parts[1], parts[2]

		master := goldenMasterMaterial(masterID)
		require.NotNil(t, master, "golden master %q must be known", masterID)

		got := DeriveServerKey(master, purpose)
		if wantHex == "" {
			assert.Nil(t, got, "master %s purpose %q: sub-32-byte master must derive nil", masterID, purpose)
			continue
		}
		want, err := hex.DecodeString(wantHex)
		require.NoError(t, err, "golden hex for %s/%q must decode", masterID, purpose)
		assert.Equal(t, want, got, "master %s purpose %q: consolidated derivation must be byte-identical to the captured implementations", masterID, purpose)
	}
}

// TestDeriveServerKey_MatchesDeriveKEKFromKeyServerPath pins the consolidated
// wrapper to the same primitive the API server's deriveServerKey called
// before consolidation (DeriveKEKFromKey with the llmsafespaces-server salt).
// If either side changes its HKDF parameters, this fails.
func TestDeriveServerKey_MatchesDeriveKEKFromKeyServerPath(t *testing.T) {
	master := goldenMasterMaterial("m1-32B")
	for _, purpose := range []string{"provider-credentials", "org-credentials", "master-kek", "dek-cache"} {
		viaWrapper := DeriveServerKey(master, purpose)
		viaPrimitive, err := DeriveKEKFromKey(master, []byte("llmsafespaces-server"), purpose)
		require.NoError(t, err)
		assert.Equal(t, viaPrimitive, viaWrapper, "purpose %q", purpose)
	}
}

// TestDeriveServerKey_Sub32ByteMasterReturnsNil guards the fail-closed edge
// shared by all three pre-consolidation implementations: master material
// shorter than the AES-256 minimum derives no key (nil), never a weak one.
func TestDeriveServerKey_Sub32ByteMasterReturnsNil(t *testing.T) {
	assert.Nil(t, DeriveServerKey(make([]byte, 31), "master-kek"))
	assert.Nil(t, DeriveServerKey(nil, "master-kek"))
	assert.Nil(t, DeriveServerKey([]byte{}, "dek-cache"))
}

// TestDeriveServerKey_PurposesAreIndependent verifies HKDF domain separation:
// distinct purpose strings must yield distinct keys from the same master.
func TestDeriveServerKey_PurposesAreIndependent(t *testing.T) {
	master := goldenMasterMaterial("m1-32B")
	seen := make(map[string]bool)
	for _, purpose := range []string{"provider-credentials", "org-credentials", "master-kek", "dek-cache", "oidc-state-cookie", "api-keys-audit"} {
		key := DeriveServerKey(master, purpose)
		require.NotNil(t, key)
		require.Len(t, key, 32)
		hexKey := hex.EncodeToString(key)
		assert.False(t, seen[hexKey], "purpose %q produced a repeated key", purpose)
		seen[hexKey] = true
	}
}
