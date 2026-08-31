// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// secret_size_test.go — W8: bind-time per-secret value validation. A
// plaintext larger than MaxSecretValueBytes can never deliver through the
// staged-files budget; the author gets a 400 at create/update time, not a
// spawn-time degrade (the materializer's per-entry/whole-batch checks are
// the bypass defense).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateValue_SizeCap(t *testing.T) {
	atCap := strings.Repeat("x", MaxSecretValueBytes)
	overCap := strings.Repeat("x", MaxSecretValueBytes+1)

	require.NoError(t, validateValue(SecretTypeSecretFile, atCap),
		"a value exactly at the cap is legal")

	err := validateValue(SecretTypeSecretFile, overCap)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidMetadata, "bind-time size rejection maps to 400")
	assert.Contains(t, err.Error(), "size_exceeded", "machine-readable reason (W8)")

	for _, typ := range []SecretType{SecretTypeEnvSecret, SecretTypeSSHKey, SecretTypeGitCredential, SecretTypeLLMProvider} {
		err := validateValue(typ, overCap)
		assert.Error(t, err, "%s values are capped too", typ)
	}
}
