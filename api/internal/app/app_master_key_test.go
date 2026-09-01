// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
)

// ---- deriveServerKey tests ----

func TestDeriveServerKey_AbsentEnv_ReturnsNil(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	if deriveServerKey("test") != nil {
		t.Error("expected nil when env var absent")
	}
}

func TestDeriveServerKey_EmptyEnv_ReturnsNil(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	if deriveServerKey("test") != nil {
		t.Error("expected nil for empty string")
	}
}

func TestDeriveServerKey_ShortRawBytes_ReturnsNil(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	// 31 non-hex chars → 31 raw bytes → below 32-byte minimum
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "abcdefghijklmnopqrstuvwxyz01234")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	if deriveServerKey("test") != nil {
		t.Error("expected nil for 31-char raw bytes")
	}
}

func TestDeriveServerKey_Exactly32RawBytes_Returns32ByteKey(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	// 32 non-hex chars → 32 raw bytes → meets minimum
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "abcdefghijklmnopqrstuvwxyz012345")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	key := deriveServerKey("test")
	if key == nil {
		t.Fatal("expected non-nil key for 32-char raw bytes")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(key))
	}
}

func TestDeriveServerKey_AlphanumericHelmFormat_Returns32ByteKey(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	// Helm randAlphaNum 64 — alphanumeric, not hex, 64 chars = 64 raw bytes
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "Abc123DefGhi456JklMno789PqrStu0VwxYz1Abc123DefGhi456JklMno789Pq")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	key := deriveServerKey("test")
	if key == nil {
		t.Fatal("expected non-nil key for 64-char alphanumeric (Helm format)")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_ValidHex64Chars_Returns32ByteKey(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	// 64 lowercase hex chars → 32 decoded bytes
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	key := deriveServerKey("test")
	if key == nil {
		t.Fatal("expected non-nil key for 64-char hex")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_ShortHex_ReturnsNil(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	// 60 hex chars → 30 decoded bytes → below 32-byte minimum
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	if deriveServerKey("test") != nil {
		t.Error("expected nil for 60-char hex (30 decoded bytes)")
	}
}

func TestDeriveServerKey_InvalidHexLongEnough_FallsBackToRawBytes(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	// Non-hex but 32+ chars → raw bytes path → should succeed
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ") // 32 uppercase Z — not valid hex
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	key := deriveServerKey("test")
	if key == nil {
		t.Fatal("expected non-nil key for 32-char non-hex string (raw bytes path)")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_LegacyEnvVar_Accepted(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "abcdefghijklmnopqrstuvwxyz012345")
	key := deriveServerKey("test")
	if key == nil {
		t.Fatal("expected non-nil key via legacy env var LLMSAFESPACES_DEK_MASTER_KEY")
	}
}

func TestDeriveServerKey_PrimaryEnvTakesPrecedence(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	primary := "abcdefghijklmnopqrstuvwxyz012345" // 32 chars
	legacy := "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"  // different 32 chars
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", primary)
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", legacy)

	primaryKey := deriveServerKey("test")

	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	legacyKey := deriveServerKey("test")

	if primaryKey == nil || legacyKey == nil {
		t.Fatal("both should produce non-nil keys")
	}
	for i := range primaryKey {
		if primaryKey[i] != legacyKey[i] {
			return // different keys — primary took precedence ✓
		}
	}
	t.Error("primary and legacy produced identical keys — primary should take precedence")
}

func TestDeriveServerKey_NoSideEffects(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "abcdefghijklmnopqrstuvwxyz012345")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	k1 := deriveServerKey("test")
	k2 := deriveServerKey("test")
	if len(k1) != len(k2) {
		t.Error("repeated calls produced different-length keys — side effect suspected")
	}
	for i := range k1 {
		if k1[i] != k2[i] {
			t.Error("repeated calls produced different keys — not deterministic")
			break
		}
	}
}

// ---- validateMasterSecret tests ----

func TestValidateMasterSecret_AbsentEnv_ReturnsError(t *testing.T) {
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, logs := logger.NewObserved()
	err := validateMasterSecret(log)
	if err == nil {
		t.Fatal("expected error when master secret absent")
	}
	if !strings.Contains(err.Error(), "LLMSAFESPACES_MASTER_SECRET") {
		t.Errorf("error should mention env var name, got: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("expected no Warn for absent secret (nothing to diagnose), got %d entries", logs.Len())
	}
}

func TestValidateMasterSecret_TooShort_LogsWarnAndReturnsError(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "shortkey") // 8 chars = 8 bytes
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, logs := logger.NewObserved()
	err := validateMasterSecret(log)
	if err == nil {
		t.Fatal("expected error for too-short secret")
	}
	if logs.Len() == 0 {
		t.Fatal("expected Warn log for present-but-short secret")
	}
	// US-50.1: the legacy-env path now also emits a deprecation Warn before the
	// too-short Warn, so find the too-short entry by message rather than by index.
	tooShort := logs.FilterMessageSnippet("too short for AES-256-GCM")
	if tooShort.Len() == 0 {
		t.Fatalf("expected a too-short Warn; got entries: %+v", logs.All())
	}
	entry := tooShort.All()[0]
	found := false
	for _, f := range entry.Context {
		if f.Key == "decoded_bytes" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Warn entry should include decoded_bytes field, fields: %v", entry.Context)
	}
}

func TestValidateMasterSecret_TooShort_DoesNotLogSecret(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	secret := "shortkey"
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", secret)
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, logs := logger.NewObserved()
	_ = validateMasterSecret(log)
	for _, entry := range logs.All() {
		if strings.Contains(entry.Message, secret) {
			t.Error("Warn message must not contain the secret value")
		}
		for _, f := range entry.Context {
			if strings.Contains(f.String, secret) {
				t.Errorf("Warn field %q must not contain the secret value", f.Key)
			}
		}
	}
}

func TestValidateMasterSecret_AlphanumericHelmFormat_Succeeds(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "Abc123DefGhi456JklMno789PqrStu0VwxYz1Abc123DefGhi456JklMno789Pq")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, _ := logger.NewObserved()
	if err := validateMasterSecret(log); err != nil {
		t.Errorf("64-char alphanumeric should succeed, got: %v", err)
	}
}

func TestValidateMasterSecret_HexFormat_Succeeds(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, _ := logger.NewObserved()
	if err := validateMasterSecret(log); err != nil {
		t.Errorf("64-char hex should succeed, got: %v", err)
	}
}

func TestValidateMasterSecret_LegacyEnvVar_Accepted(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "abcdefghijklmnopqrstuvwxyz012345") // 32 chars
	log, _ := logger.NewObserved()
	if err := validateMasterSecret(log); err != nil {
		t.Errorf("LLMSAFESPACES_DEK_MASTER_KEY should satisfy validation, got: %v", err)
	}
}

// ---- app.New wiring tests ----

// minimalCfg returns a *config.Config that makes kubernetes.New fail
// deterministically with a file-not-found error, without touching any real
// infrastructure. Used by app.New wiring tests.
func minimalCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Kubernetes.InCluster = false
	cfg.Kubernetes.ConfigPath = "/nonexistent/kubeconfig-epic34-test"
	return cfg
}

func TestApp_New_FailsWithoutMasterSecret(t *testing.T) {
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "")
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, _ := logger.NewObserved()
	_, err := New(minimalCfg(), log)
	if err == nil {
		t.Fatal("app.New should fail when master secret is absent")
	}
	if !strings.Contains(err.Error(), "LLMSAFESPACES_MASTER_SECRET") {
		t.Errorf("error should mention LLMSAFESPACES_MASTER_SECRET, got: %v", err)
	}
	// Must NOT contain kubernetes-related error — validateMasterSecret fires first
	if strings.Contains(err.Error(), "kubernetes") || strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("should not reach kubernetes step, but got: %v", err)
	}
}

func TestApp_New_FailsWithTooShortMasterSecret(t *testing.T) {
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "tooshort") // 8 chars
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, _ := logger.NewObserved()
	_, err := New(minimalCfg(), log)
	if err == nil {
		t.Fatal("app.New should fail when master secret is too short")
	}
	if strings.Contains(err.Error(), "kubernetes") || strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("should not reach kubernetes step, but got: %v", err)
	}
}

func TestApp_New_WithValidMasterSecret_FailsAtInfra(t *testing.T) {
	t.Setenv("LLMSAFESPACES_MASTER_SECRET", "abcdefghijklmnopqrstuvwxyz012345") // 32 raw bytes
	t.Setenv("LLMSAFESPACES_DEK_MASTER_KEY", "")
	log, _ := logger.NewObserved()
	_, err := New(minimalCfg(), log)
	if err == nil {
		t.Fatal("app.New should fail (no real infra available)")
	}
	// Must NOT be the master-secret error — validation passed and infra was attempted
	if strings.Contains(err.Error(), "LLMSAFESPACES_MASTER_SECRET") {
		t.Errorf("master secret validation should pass, but got master-secret error: %v", err)
	}
	// Must be a kubernetes/infra error — confirming validateMasterSecret passed
	if !strings.Contains(err.Error(), "kubernetes") && !strings.Contains(err.Error(), "kubeconfig") {
		t.Errorf("expected kubernetes/infra error after validation passes, got: %v", err)
	}
}

// ---- US-50.1: master KEK file mount ----

// writeFileHelper writes content to path with mode 0600; test helper.
func writeFileHelper(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// clearMasterSecretSources unsets every master-KEK source env var so a test
// starts from a known-empty state (t.Setenv would otherwise leak between
// sub-tests within one test function).
func clearMasterSecretSources(t *testing.T) {
	t.Helper()
	t.Setenv(masterSecretFileEnv, "")
	t.Setenv(masterSecretValueEnv, "")
	t.Setenv(masterSecretLegacyEnv, "")
}

func TestDeriveServerKey_FromFile(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := tmpDir + "/master-secret"
	writeFileHelper(t, keyFile, "abcdefghijklmnopqrstuvwxyz012345") // 32 raw bytes

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, keyFile)

	key := deriveServerKey("test-purpose")
	if key == nil {
		t.Fatal("expected non-nil key derived from file mount")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_FromFile_HexContent(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := tmpDir + "/master-secret"
	// 64 hex chars = 32 decoded bytes, with trailing newline (as a Secret mount adds)
	writeFileHelper(t, keyFile, "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20\n")

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, keyFile)

	key := deriveServerKey("test-purpose")
	if key == nil {
		t.Fatal("expected non-nil key for hex file content")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_FromFile_MissingFile_FallsBackToEnv(t *testing.T) {
	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, "/nonexistent/master-secret-501-test")
	t.Setenv(masterSecretValueEnv, "abcdefghijklmnopqrstuvwxyz012345") // 32 raw bytes

	// File path configured but file missing -> fall back to the env value.
	key := deriveServerKey("test-purpose")
	if key == nil {
		t.Fatal("expected non-nil key via env fallback when file missing")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_FilePathEmpty_UsesEnv(t *testing.T) {
	clearMasterSecretSources(t)
	t.Setenv(masterSecretValueEnv, "abcdefghijklmnopqrstuvwxyz012345") // 32 raw bytes

	key := deriveServerKey("test-purpose")
	if key == nil {
		t.Fatal("expected non-nil key via env when file path unset")
	}
	if len(key) != 32 {
		t.Errorf("expected 32-byte derived key, got %d", len(key))
	}
}

func TestDeriveServerKey_MultiFile_RotationWindow(t *testing.T) {
	tmpDir := t.TempDir()
	oldFile := tmpDir + "/master-old"
	newFile := tmpDir + "/master-new"
	oldRaw := "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ" // 32 raw bytes (old key)
	newRaw := "abcdefghijklmnopqrstuvwxyz012345" // 32 raw bytes (new/active key)
	writeFileHelper(t, oldFile, oldRaw)
	writeFileHelper(t, newFile, newRaw)

	clearMasterSecretSources(t)
	// Colon-separated: old first, new last (last = highest/active version per US-50.4 rotation window).
	t.Setenv(masterSecretFileEnv, oldFile+":"+newFile)

	// Both files are loaded as distinct materials.
	materials := loadMasterSecretMaterials()
	if len(materials) != 2 {
		t.Fatalf("expected 2 loaded materials (rotation window), got %d", len(materials))
	}

	// deriveServerKey derives from the ACTIVE (last/highest-version) material.
	key := deriveServerKey("test-purpose")
	if key == nil {
		t.Fatal("expected non-nil key during rotation window")
	}
	// The key derived from the new material must differ from one derived purely from the old.
	clearMasterSecretSources(t)
	t.Setenv(masterSecretValueEnv, oldRaw)
	oldKey := deriveServerKey("test-purpose")
	if oldKey == nil {
		t.Fatal("expected non-nil old key")
	}
	if string(key) == string(oldKey) {
		t.Error("deriveServerKey should use the active (new) material, not the old one")
	}

	// And match the new-only derivation.
	clearMasterSecretSources(t)
	t.Setenv(masterSecretValueEnv, newRaw)
	newKey := deriveServerKey("test-purpose")
	if string(key) != string(newKey) {
		t.Error("deriveServerKey in rotation window should equal the new-material derivation")
	}
}

func TestDeriveServerKey_FilePreferredOverEnv(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := tmpDir + "/master-secret"
	fileRaw := "abcdefghijklmnopqrstuvwxyz012345" // 32 raw bytes
	envRaw := "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"  // different 32 raw bytes
	writeFileHelper(t, keyFile, fileRaw)

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, keyFile)
	t.Setenv(masterSecretValueEnv, envRaw) // should be ignored

	fileKey := deriveServerKey("test-purpose")

	clearMasterSecretSources(t)
	t.Setenv(masterSecretValueEnv, fileRaw)
	expectKey := deriveServerKey("test-purpose")

	if fileKey == nil || expectKey == nil {
		t.Fatal("expected non-nil keys")
	}
	if string(fileKey) != string(expectKey) {
		t.Error("file source should take precedence over env source")
	}
}

func TestValidateMasterSecret_FromFile_TooShort_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := tmpDir + "/master-secret"
	writeFileHelper(t, keyFile, "shortkey") // 8 bytes

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, keyFile)

	log, logs := logger.NewObserved()
	err := validateMasterSecret(log)
	if err == nil {
		t.Fatal("expected error for too-short file material")
	}
	// The precise "too short" diagnostic must fire (regression: the loader must
	// surface present-but-short files rather than dropping them as "no readable file").
	if !strings.Contains(err.Error(), "too short") && !strings.Contains(err.Error(), "minimum is 32") {
		t.Errorf("error should report the too-short file material, got: %v", err)
	}
	if logs.FilterMessageSnippet("too short for AES-256-GCM").Len() == 0 {
		t.Error("expected a too-short Warn entry for the file material")
	}
	// Must NOT fall through to the legacy env path (file path is configured).
	if logs.FilterMessageSnippet("deprecated").Len() != 0 {
		t.Error("too-short file material must not emit the env deprecation warning")
	}
}

// TestValidateMasterSecret_FileAndEnvBothSet_WarnsEnvStillExposed (US-50.1):
// when the file mount is healthy AND a legacy value env var is also set, the
// env is unused at runtime but still leaks the KEK value — warn the operator.
func TestValidateMasterSecret_FileAndEnvBothSet_WarnsEnvStillExposed(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := tmpDir + "/master-secret"
	writeFileHelper(t, keyFile, "abcdefghijklmnopqrstuvwxyz012345") // valid 32 raw bytes

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, keyFile)
	t.Setenv(masterSecretValueEnv, "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ") // redundant legacy env, still exposes KEK

	log, logs := logger.NewObserved()
	if err := validateMasterSecret(log); err != nil {
		t.Fatalf("file source should validate, got: %v", err)
	}
	if logs.FilterMessageSnippet("ignored because the file mount takes precedence").Len() == 0 {
		t.Error("expected a Warn that the redundant env var still exposes the KEK")
	}
}

func TestValidateMasterSecret_FromFile_MissingFile_ReturnsError(t *testing.T) {
	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, "/nonexistent/master-secret-501-missing")

	log, _ := logger.NewObserved()
	err := validateMasterSecret(log)
	if err == nil {
		t.Fatal("expected error when file path configured but no readable file")
	}
	if !strings.Contains(err.Error(), masterSecretFileEnv) {
		t.Errorf("error should reference the file env var, got: %v", err)
	}
}

func TestValidateMasterSecret_LegacyEnv_DeprecationWarning(t *testing.T) {
	clearMasterSecretSources(t)
	t.Setenv(masterSecretValueEnv, "abcdefghijklmnopqrstuvwxyz012345") // valid 32 raw bytes

	log, logs := logger.NewObserved()
	if err := validateMasterSecret(log); err != nil {
		t.Fatalf("legacy env should still validate, got: %v", err)
	}
	if logs.FilterMessageSnippet("deprecated").Len() == 0 {
		t.Error("legacy env delivery should emit a deprecation Warn")
	}
}

func TestValidateMasterSecret_FileSource_NoDeprecationWarning(t *testing.T) {
	tmpDir := t.TempDir()
	keyFile := tmpDir + "/master-secret"
	writeFileHelper(t, keyFile, "abcdefghijklmnopqrstuvwxyz012345")

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, keyFile)

	log, logs := logger.NewObserved()
	if err := validateMasterSecret(log); err != nil {
		t.Fatalf("file source should validate, got: %v", err)
	}
	if logs.FilterMessageSnippet("deprecated").Len() != 0 {
		t.Error("file source must not emit the env deprecation warning")
	}
}

// TestValidateMasterSecret_MultiFile_BotchedRotation_ActiveShortFails (US-50.1):
// in the rotation window, if the active (last) file is too short while an
// earlier file is valid, validateMasterSecret must fail closed on the active
// material rather than silently using the earlier one.
func TestValidateMasterSecret_MultiFile_BotchedRotation_ActiveShortFails(t *testing.T) {
	tmpDir := t.TempDir()
	goodFile := tmpDir + "/master-old"
	badFile := tmpDir + "/master-new"
	writeFileHelper(t, goodFile, "abcdefghijklmnopqrstuvwxyz012345") // valid 32 raw bytes (earlier/old)
	writeFileHelper(t, badFile, "shortkey")                          // 8 bytes (active/last — botched)

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, goodFile+":"+badFile)

	log, logs := logger.NewObserved()
	err := validateMasterSecret(log)
	if err == nil {
		t.Fatal("expected error when the active (last) rotation file is too short")
	}
	if !strings.Contains(err.Error(), "too short") && !strings.Contains(err.Error(), "minimum is 32") {
		t.Errorf("error should report the too-short active material, got: %v", err)
	}
	if logs.FilterMessageSnippet("too short for AES-256-GCM").Len() == 0 {
		t.Error("expected a too-short Warn for the active rotation file")
	}
}

// TestDeriveServerKey_MultiFile_ActiveShortFailsClosed (US-50.1): when the
// active (last) file is too short, deriveServerKey returns nil (fails closed)
// rather than deriving from a weak key or silently using an earlier file.
func TestDeriveServerKey_MultiFile_ActiveShortFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	goodFile := tmpDir + "/master-old"
	badFile := tmpDir + "/master-new"
	writeFileHelper(t, goodFile, "abcdefghijklmnopqrstuvwxyz012345")
	writeFileHelper(t, badFile, "shortkey")

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, goodFile+":"+badFile)

	if key := deriveServerKey("test-purpose"); key != nil {
		t.Errorf("deriveServerKey must return nil when the active file is too short, got %d bytes", len(key))
	}
}

// TestValidateMasterSecret_MultiFile_BotchedRotation_ActiveEmptyFails (US-50.1):
// the active (last) file is empty (e.g. mis-mounted Secret key) while an
// earlier file is valid. Must fail closed rather than silently using the old
// key — the loader preserves presence so validation rejects the empty material.
func TestValidateMasterSecret_MultiFile_BotchedRotation_ActiveEmptyFails(t *testing.T) {
	tmpDir := t.TempDir()
	goodFile := tmpDir + "/master-old"
	emptyFile := tmpDir + "/master-new"
	writeFileHelper(t, goodFile, "abcdefghijklmnopqrstuvwxyz012345")   // valid (earlier/old)
	require.NoError(t, os.WriteFile(emptyFile, []byte("   \n"), 0600)) // empty/whitespace (active/last — botched mount)

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, goodFile+":"+emptyFile)

	log, _ := logger.NewObserved()
	err := validateMasterSecret(log)
	if err == nil {
		t.Fatal("expected error when the active (last) rotation file is empty")
	}
}

// TestDeriveServerKey_MultiFile_ActiveEmptyFailsClosed (US-50.1): an empty
// active file must NOT silently fall back to an earlier key; deriveServerKey
// returns nil (fails closed).
func TestDeriveServerKey_MultiFile_ActiveEmptyFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	goodFile := tmpDir + "/master-old"
	emptyFile := tmpDir + "/master-new"
	writeFileHelper(t, goodFile, "abcdefghijklmnopqrstuvwxyz012345")
	require.NoError(t, os.WriteFile(emptyFile, nil, 0600))

	clearMasterSecretSources(t)
	t.Setenv(masterSecretFileEnv, goodFile+":"+emptyFile)

	if key := deriveServerKey("test-purpose"); key != nil {
		t.Errorf("deriveServerKey must return nil when the active file is empty, got %d bytes", len(key))
	}
}
