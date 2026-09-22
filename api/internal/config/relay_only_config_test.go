// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
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
