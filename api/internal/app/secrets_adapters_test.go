// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// US-50.8: the static root-key-provider deprecation warning must fire on the
// Helm-empty default (""), not only on an explicit "static". Operators who
// accept the risk can suppress it via Security.SkipMasterKeyWarning.
//
// Tests for deriveServerKey/validateMasterSecret live in app_master_key_test.go.

const us508StaticWarnSnippet = "using static root key provider"

// setValidMasterSecretUS508 sets a 32-byte master secret so newRootKeyProvider's
// static path reaches provider construction and the warning (it returns nil
// before warning when the master secret is absent — app.New rejects that
// upstream via validateMasterSecret anyway).
func setValidMasterSecretUS508(t *testing.T) {
	t.Helper()
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "abcdefghijklmnopqrstuvwxyz012345")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
}

func TestNewRootKeyProvider_EmptyDefault_LogsWarning(t *testing.T) {
	setValidMasterSecretUS508(t)
	cfg := &config.Config{} // RootKeyProvider == "" is the Helm default
	log, logs := logger.NewObserved()

	p := newRootKeyProvider(cfg, log)
	require.NotNil(t, p, "static provider should be constructed with a valid master secret")
	assert.Equal(t, 1, logs.FilterMessageSnippet(us508StaticWarnSnippet).Len(),
		"US-50.8 M1: empty Helm default must emit the static deprecation warning")
}

func TestNewRootKeyProvider_ExplicitStatic_LogsWarning(t *testing.T) {
	setValidMasterSecretUS508(t)
	cfg := &config.Config{}
	cfg.Security.RootKeyProvider = "static"
	log, logs := logger.NewObserved()

	p := newRootKeyProvider(cfg, log)
	require.NotNil(t, p)
	assert.Equal(t, 1, logs.FilterMessageSnippet(us508StaticWarnSnippet).Len())
}

func TestNewRootKeyProvider_SkipWarning_Suppresses(t *testing.T) {
	setValidMasterSecretUS508(t)
	cfg := &config.Config{} // empty default would normally warn
	cfg.Security.SkipMasterKeyWarning = true
	log, logs := logger.NewObserved()

	p := newRootKeyProvider(cfg, log)
	require.NotNil(t, p)
	assert.Equal(t, 0, logs.FilterMessageSnippet(us508StaticWarnSnippet).Len(),
		"SkipMasterKeyWarning must suppress the static deprecation warning")
}

func TestNewRootKeyProvider_Sealed_NoWarning(t *testing.T) {
	tmpDir := t.TempDir()
	sealedPath := filepath.Join(tmpDir, "sealed")
	passPath := filepath.Join(tmpDir, "passphrase")
	passphrase := []byte("correct-horse-battery-staple")
	require.NoError(t, os.WriteFile(passPath, passphrase, 0600))

	rootKey := make([]byte, 32)
	for i := range rootKey {
		rootKey[i] = byte(i)
	}
	require.NoError(t, secrets.SealRootKey(sealedPath, passphrase, rootKey))

	cfg := &config.Config{}
	cfg.Security.RootKeyProvider = "sealed"
	cfg.Security.SealedKeyPath = sealedPath
	cfg.Security.PassphrasePath = passPath
	log, logs := logger.NewObserved()

	p := newRootKeyProvider(cfg, log)
	require.NotNil(t, p, "sealed provider should construct from valid sealed + passphrase files")
	assert.Equal(t, 0, logs.FilterMessageSnippet(us508StaticWarnSnippet).Len(),
		"sealed provider must not emit the static deprecation warning")
}

// TestEnsureFreeTierCredential_PlaintextHasKindAndSlug pins the free-tier
// credential seed plaintext to the post-Epic-55 shape: it must include
// kind="opencode" and slug="opencode-free-tier" so that, after decrypt at
// materialize time, LLMProviderData.Validate() succeeds and the credential
// reaches opencode as a real provider in agent-config.json.
//
// Regression: PR #430 (Epic 55 backend) updated UpsertFreeTierCredential's
// DAL to insert kind+slug columns, but the bootstrap caller's plaintext
// (this function) was missed — it still constructed the legacy
// {"provider":"opencode","apiKey":"public"} JSON. On live cluster, the
// materialize loop logged
//
//	`llm-provider/: skipped — invalid LLM provider data: kind is required`
//
// because the decrypted blob's Kind field was empty. This test ensures the
// plaintext shape stays in sync with LLMProviderData.Validate().
func TestEnsureFreeTierCredential_PlaintextHasKindAndSlug(t *testing.T) {
	// Capture the ciphertext that ensureFreeTierCredential generates by
	// supplying a recording fake seeder.
	var captured []byte
	seeder := &recordingFreeTierSeeder{
		onUpsert: func(_ context.Context, ct []byte) error { captured = ct; return nil },
	}

	// Use a deterministic static KEK so we can decrypt the captured ciphertext.
	kek := make([]byte, 32)
	for i := range kek {
		kek[i] = byte(i + 1)
	}
	prov, err := secrets.NewStaticKeyProvider(kek)
	require.NoError(t, err)

	logger, _ := logger.NewObserved()

	err = ensureFreeTierCredential(context.Background(), seeder, prov, logger)
	require.NoError(t, err)
	require.NotEmpty(t, captured, "ensureFreeTierCredential must have captured ciphertext")

	// Decrypt and inspect the plaintext. US-57.1: route through the
	// provider (not raw DecryptSecret) because provider output now
	// carries the lkms:v1: prefix for CompositeProvider dispatch.
	plain, err := prov.Decrypt(context.Background(), captured)
	require.NoError(t, err)

	var pd secrets.LLMProviderData
	require.NoError(t, json.Unmarshal(plain, &pd))

	// Validate the post-Epic-55 shape.
	require.NoError(t, pd.Validate(),
		"free-tier credential plaintext must satisfy LLMProviderData.Validate(); "+
			"materialize will skip it otherwise")
	assert.Equal(t, "opencode", pd.Kind,
		"free-tier credential must declare kind=opencode")
	assert.Equal(t, "opencode-free-tier", pd.Slug,
		"free-tier credential must declare slug=opencode-free-tier so it "+
			"appears in agent-config.json as the 'opencode-free-tier' provider key")
	assert.Equal(t, "public", pd.APIKey)
}

// recordingFreeTierSeeder is a credentialSeeder that records the ciphertext
// passed to UpsertFreeTierCredential without touching any DB.
type recordingFreeTierSeeder struct {
	onUpsert func(context.Context, []byte) error
}

func (r *recordingFreeTierSeeder) UpsertFreeTierCredential(ctx context.Context, ct []byte) error {
	if r.onUpsert != nil {
		return r.onUpsert(ctx, ct)
	}
	return nil
}
func (r *recordingFreeTierSeeder) BackfillFreeTierBindings(ctx context.Context) (int64, error) {
	return 0, nil
}
