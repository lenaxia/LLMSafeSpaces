// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// InspectStagingEnvelope returns the envelope's algorithm discriminator and
// plaintext keyID without any cryptographic work. The BYO resolve router
// (US-72.2) uses it to route each cached envelope to the right resolver:
// `aes-256-gcm` → the KMS-mode provider (one per deployment), `hpke` → the
// keypair-generation resolver keyed by keyID. The values remain
// integrity-bound at resolve time — a tampered discriminator fails AEAD
// authentication in Resolve; this call only reads the routing metadata.
func InspectStagingEnvelope(envelope string) (alg, keyID string, err error) {
	env, err := parseStagingEnvelope(envelope)
	if err != nil {
		return "", "", err
	}
	return env.alg, env.keyID, nil
}
