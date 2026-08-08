// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"testing"
)

func TestResolvePodNamespace_Default(t *testing.T) {
	os.Unsetenv("POD_NAMESPACE")
	ns := resolvePodNamespace()
	if ns != "llmsafespaces" {
		t.Errorf("expected default 'llmsafespaces', got %q", ns)
	}
}

func TestResolvePodNamespace_EnvSet(t *testing.T) {
	os.Setenv("POD_NAMESPACE", "custom-ns")
	defer os.Unsetenv("POD_NAMESPACE")
	ns := resolvePodNamespace()
	if ns != "custom-ns" {
		t.Errorf("expected 'custom-ns', got %q", ns)
	}
}

func TestResolvePodNamespace_EmptyString(t *testing.T) {
	os.Setenv("POD_NAMESPACE", "")
	defer os.Unsetenv("POD_NAMESPACE")
	ns := resolvePodNamespace()
	if ns != "llmsafespaces" {
		t.Errorf("expected default 'llmsafespaces' for empty env, got %q", ns)
	}
}
