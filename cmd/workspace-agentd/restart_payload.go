// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// restart_payload.go — content gate for secrets-driven opencode restarts.
//
// shouldRestart is type-gated only: any env-secret/api-key entry in an
// applied batch schedules a restart, even when the batch is byte-identical
// to what was applied before. The secrets-reconcile notify cadence can
// re-apply identical batches in quick succession, and every such apply
// killed the running opencode child (in-flight chat turns, reconnect
// storms). The gate here hashes the restart-relevant payload and persists
// it next to the secrets-env file (same tmpfs lifecycle as the rev
// anchor: pod death wipes both, so the first post-boot apply always
// restarts). A byte-identical subsequent apply is suppressed; any change
// to the type, name, or value of a restart-relevant entry restarts.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

const restartPayloadSuffix = ".restart-payload"

// restartPayloadPath extends the secrets-env path into the payload-hash
// sibling (same pattern as revAnchorPath).
func restartPayloadPath(secretsEnvPath string) string {
	return secretsEnvPath + restartPayloadSuffix
}

// restartPayloadHash hashes the restart-relevant slice of the batch:
// canonical `type|name|value` lines (sorted, newline-joined) over
// env-secret and api-key entries — exactly the classes shouldRestart
// gates on. The env var name (Metadata["var_name"]) is the identity for
// env-secrets (mirror classifySecretRestartReason); the secret Name for
// api-keys. A batch with no restart-relevant entries hashes to "" so it
// can never be persisted or compared meaningfully.
func restartPayloadHash(batch []secrets.Secret) string {
	lines := make([]string, 0, len(batch))
	for _, s := range batch {
		if s.Type != "env-secret" && s.Type != "api-key" {
			continue
		}
		name := s.Name
		if vn := s.Metadata["var_name"]; vn != "" {
			name = vn
		}
		lines = append(lines, s.Type+"|"+name+"|"+s.Plaintext)
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// readRestartPayloadHash returns the persisted hash; absent or unreadable
// is "" (unknown → gate open, restart proceeds).
func readRestartPayloadHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeRestartPayloadHash persists the applied payload hash atomically.
func writeRestartPayloadHash(path, hash string) error {
	return atomicWriteAny(path, []byte(hash), 0o600)
}
