// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// ServerKeyHKDFSalt is the HKDF domain-separation salt binding every
// purpose-scoped server key to the llmsafespaces-server domain. It is part
// of the at-rest key contract: changing it re-derives every purpose key and
// renders every existing ciphertext undecryptable without a rotation run.
const ServerKeyHKDFSalt = "llmsafespaces-server"

// DeriveServerKey derives the 32-byte purpose-scoped server key from master
// KEK material via HKDF-SHA256 with ServerKeyHKDFSalt. Each purpose string
// ("provider-credentials", "org-credentials", "master-kek", "dek-cache", …)
// yields a cryptographically independent key.
//
// This is the single derivation shared by the API server's per-purpose
// providers and the rotate-kek / migrate-kek CLIs (issue #832 consolidated
// three byte-identical copies into this export; the golden fixtures in
// testdata/derive_server_key_golden.txt prove the pre-consolidation outputs
// are preserved byte-for-byte).
//
// Returns nil — never a weak key — when master is shorter than the 32-byte
// AES-256-GCM minimum. Callers treat nil as a fail-closed configuration
// error.
func DeriveServerKey(master []byte, purpose string) []byte {
	if len(master) < 32 {
		return nil
	}
	key, err := DeriveKEKFromKey(master, []byte(ServerKeyHKDFSalt), purpose)
	if err != nil {
		return nil
	}
	return key
}
