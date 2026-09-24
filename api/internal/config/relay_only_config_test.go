// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRelayOnlyKeyDeliveryEnvBinding (US-72.4): the
// LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED env (rendered by the chart
// under relayOnlyKeyDelivery.enabled) reaches the flag the app wiring
// reads; unset stays false (flag off = byte-identical legacy batches).
func TestRelayOnlyKeyDeliveryEnvBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  host: localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RelayOnlyKeyDelivery.Enabled {
		t.Fatal("flag must default to false (byte-identical legacy batches)")
	}

	t.Setenv("LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED", "true")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load with env: %v", err)
	}
	if !cfg.RelayOnlyKeyDelivery.Enabled {
		t.Fatal("env LLMSAFESPACES_RELAYONLYKEYDELIVERY_ENABLED=true must set the flag")
	}
}

// M2 (design 0061 §4): the fallback mode env threads + the default is
// MIGRATION (the chart's merge-time default — the migration must be
// observable before it can be retired).
func TestRelayOnlyKeyDelivery_FallbackModeDefaultMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  jwtSecret: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RelayOnlyKeyDelivery.FallbackMode != "migration" {
		t.Fatalf("fallbackMode must default to migration, got %q", cfg.RelayOnlyKeyDelivery.FallbackMode)
	}
	t.Setenv("LLMSAFESPACES_RELAYONLYKEYDELIVERY_FALLBACK_MODE", "strict")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("load with env: %v", err)
	}
	if cfg.RelayOnlyKeyDelivery.FallbackMode != "strict" {
		t.Fatalf("fallbackMode env must thread, got %q", cfg.RelayOnlyKeyDelivery.FallbackMode)
	}
}

// M2: the enum is fail-loud — a typo must refuse boot, not silently
// arm the fail-open raw-key path.
func TestRelayOnlyKeyDelivery_FallbackModeEnumFailLoud(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  jwtSecret: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMSAFESPACES_RELAYONLYKEYDELIVERY_FALLBACK_MODE", "srtict")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `must be "migration" or "strict"`) {
		t.Fatalf("a typo must refuse boot, got: %v", err)
	}
}
