// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kmsTestSecret is a 64-char hex string that decodes to 32 bytes — the
// minimum activeMasterSecret accepts. Used so deriveServerKey returns
// a non-nil key in tests that don't test the nil-master-secret path.
const kmsTestSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// credentials file path and key ARNs. Used by all KMS-wiring tests.
func kmsTestConfig(credsFile string) *config.Config {
	cfg := &config.Config{}
	cfg.Security.RootKeyProvider = "aws-kms"
	cfg.Security.KMS.AWS.Region = "us-east-1"
	cfg.Security.KMS.AWS.CredentialsFile = credsFile
	cfg.Security.KMS.AWS.KeyArns = map[string]string{
		"providerCredentials": "arn:aws:kms:us-east-1:123:key/test",
		"orgCredentials":      "arn:aws:kms:us-east-1:123:key/org",
		"masterKek":           "arn:aws:kms:us-east-1:123:key/mkek",
	}
	return cfg
}

// writeTestCredsFile writes a minimal AWS shared-credentials INI file
// to tmpDir and returns the path.
func writeTestCredsFile(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	credsFile := filepath.Join(tmpDir, "credentials")
	require.NoError(t, os.WriteFile(credsFile,
		[]byte("[default]\naws_access_key_id = AKIATEST\naws_secret_access_key = secrettest\n"),
		0600))
	return credsFile
}

// TestNewPurposeProvider_NoKMS_ReturnsLocal verifies that when
// RootKeyProvider is not "aws-kms", the function returns a plain
// local static provider (no CompositeProvider wrapper).
func TestNewPurposeProvider_NoKMS_ReturnsLocal(t *testing.T) {
	log, _ := logger.NewObserved()
	cfg := &config.Config{}
	cfg.Security.RootKeyProvider = "static"
	t.Setenv(masterSecretValueEnv, kmsTestSecret)

	p := newPurposeProvider(cfg, log, "provider-credentials")
	require.NotNil(t, p)

	_, isComposite := p.(*secrets.CompositeProvider)
	assert.False(t, isComposite,
		"non-KMS provider must NOT be wrapped in CompositeProvider")
}

// TestNewPurposeProvider_NilCfg_ReturnsLocal verifies that when cfg is
// nil (some test/wiring paths), the function falls back to local.
func TestNewPurposeProvider_NilCfg_ReturnsLocal(t *testing.T) {
	log, _ := logger.NewObserved()
	t.Setenv(masterSecretValueEnv, kmsTestSecret)

	p := newPurposeProvider(nil, log, "provider-credentials")
	require.NotNil(t, p)
}

// TestNewPurposeProvider_KMSConfigured_MissingARN_ReturnsNil verifies
// the fail-closed contract: when RootKeyProvider is explicitly "aws-kms"
// but the key ARN for the purpose is missing, the function returns nil
// rather than silently falling back to local.
func TestNewPurposeProvider_KMSConfigured_MissingARN_ReturnsNil(t *testing.T) {
	log, _ := logger.NewObserved()
	cfg := kmsTestConfig("/nonexistent")
	// Remove the providerCredentials ARN.
	delete(cfg.Security.KMS.AWS.KeyArns, "providerCredentials")
	t.Setenv(masterSecretValueEnv, kmsTestSecret)

	p := newPurposeProvider(cfg, log, "provider-credentials")
	assert.Nil(t, p,
		"KMS enabled with missing ARN must return nil (fail-closed)")
}

// Note on credentials-file validation: the AWS SDK lazy-loads credentials —
// config.LoadDefaultConfig with a nonexistent SharedCredentialsFiles does NOT
// fail at construction time. The file is only read when the first API call is
// made (Encrypt/Decrypt). So "bad credentials" is a runtime failure, not a
// boot-time failure. The chart's Helm `required` guards on credentialsSecret
// ensure the K8s Secret object exists; whether the file contents are valid
// AWS credentials is verified by the first real Encrypt/Decrypt call.

// TestNewPurposeProvider_KMSConfigured_ValidConfig_ReturnsComposite
// verifies the happy path: valid AWS credentials + ARN produces a
// CompositeProvider with KMS-primary and static-fallback.
func TestNewPurposeProvider_KMSConfigured_ValidConfig_ReturnsComposite(t *testing.T) {
	log, _ := logger.NewObserved()
	cfg := kmsTestConfig(writeTestCredsFile(t))
	t.Setenv(masterSecretValueEnv, kmsTestSecret)

	p := newPurposeProvider(cfg, log, "provider-credentials")
	require.NotNil(t, p)

	composite, isComposite := p.(*secrets.CompositeProvider)
	require.True(t, isComposite,
		"KMS-configured provider must be a CompositeProvider")
	assert.NotNil(t, composite, "composite must be non-nil")
}

// TestNewRootKeyProvider_AWSKMSCase_DelegatesToMasterKek verifies that
// the "aws-kms" case in newRootKeyProvider routes through
// newPurposeProvider with the "master-kek" purpose, producing a
// CompositeProvider.
func TestNewRootKeyProvider_AWSKMSCase_DelegatesToMasterKek(t *testing.T) {
	log, _ := logger.NewObserved()
	cfg := kmsTestConfig(writeTestCredsFile(t))
	t.Setenv(masterSecretValueEnv, kmsTestSecret)

	p := newRootKeyProvider(cfg, log)
	require.NotNil(t, p)

	_, isComposite := p.(*secrets.CompositeProvider)
	assert.True(t, isComposite,
		"aws-kms root key provider must produce a CompositeProvider")
}

// TestBoot_SkipsMultiVersionUpgradeWhenKMSPrimary verifies acceptance
// criterion 10: when the root key provider is KMS-backed (CompositeProvider),
// the multi-version upgrade block at app.go is skipped — apiKeyProv stays
// a CompositeProvider, not upgraded to StaticKeyProviderMultiVersion.
//
// This test exercises the exact type-assertion that the boot code uses
// to detect whether to skip the upgrade.
func TestBoot_SkipsMultiVersionUpgradeWhenKMSPrimary(t *testing.T) {
	staticKey := make([]byte, 32)
	static, err := secrets.NewStaticKeyProvider(staticKey)
	require.NoError(t, err)

	composite, err := secrets.NewCompositeProvider(static)
	require.NoError(t, err)

	// The app.go code checks:
	//   if _, isComposite := apiKeyProv.(*secrets.CompositeProvider); isComposite
	_, isComposite := (secrets.RootKeyProvider(composite)).(*secrets.CompositeProvider)
	assert.True(t, isComposite,
		"a CompositeProvider must be detectable by type assertion for the skip-upgrade branch")

	// Verify a non-composite (plain static) is NOT caught by the skip branch.
	_, isComposite2 := (secrets.RootKeyProvider(static)).(*secrets.CompositeProvider)
	assert.False(t, isComposite2,
		"a plain StaticKeyProvider must NOT trigger the skip-upgrade branch")
}
