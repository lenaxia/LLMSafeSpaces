// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// upload_staging_env_test.go — design 0060 PR 2.5 pins: the flag
// round trip (typo'd keys rejected loudly), the env emission (set →
// the exact agentd-consumed names land; zero → NOTHING lands), and
// the pod wiring (both agentd-bearing containers carry the set env;
// zero config leaves both clean).

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
)

func TestUploadStagingFlag_RoundTrip(t *testing.T) {
	cfg, err := ParseUploadStagingFlag("budget=52428800,floor=25165824,concurrent=8,ttlMs=900000,applyTimeoutMs=120000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := UploadStagingConfig{BudgetBytes: 52428800, CredentialFloorByte: 25165824, MaxConcurrent: 8, TTLMilliseconds: 900000, ApplyTimeoutMilli: 120000, credentialFloorSet: true}
	if cfg != want {
		t.Fatalf("round trip: got %+v want %+v", cfg, want)
	}
	if got := want.FlagString(); got != "budget=52428800,floor=25165824,concurrent=8,ttlMs=900000,applyTimeoutMs=120000" {
		t.Fatalf("FlagString: %q", got)
	}
}

func TestUploadStagingFlag_EmptyIsDefault(t *testing.T) {
	cfg, err := ParseUploadStagingFlag("")
	if err != nil || cfg != (UploadStagingConfig{}) {
		t.Fatalf("empty flag must be the zero config, got %+v err %v", cfg, err)
	}
	if env := cfg.EnvVars(); len(env) != 0 {
		t.Fatalf("zero config must emit NO env, got %v", env)
	}
}

func TestUploadStagingFlag_UnknownKeyRejected(t *testing.T) {
	if _, err := ParseUploadStagingFlag("budgett=1"); err == nil {
		t.Fatal("a typo'd knob must fail loudly, never silently default")
	}
	if _, err := ParseUploadStagingFlag("budget=0"); err == nil {
		t.Fatal("non-positive budget must fail")
	}
	if _, err := ParseUploadStagingFlag("budget"); err == nil {
		t.Fatal("a bare key must fail")
	}
}

func TestUploadStagingEnv_SetLandsExactNames(t *testing.T) {
	cfg := UploadStagingConfig{BudgetBytes: 52428800, MaxConcurrent: 8, ApplyTimeoutMilli: 120000}
	env := cfg.EnvVars()
	byName := map[string]string{}
	for _, e := range env {
		byName[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"UPLOAD_STAGING_BUDGET":         "52428800",
		"UPLOAD_STAGING_MAX_CONCURRENT": "8",
		"UPLOAD_APPLY_TIMEOUT_MS":       "120000",
	} {
		if got, ok := byName[name]; !ok || got != want {
			t.Fatalf("%s: got %q (present=%v), want %q", name, got, ok, want)
		}
	}
	// Unset knobs emit nothing — agentd defaults stand.
	for _, absent := range []string{"UPLOAD_STAGING_CREDENTIAL_FLOOR", "UPLOAD_STAGING_TTL_MS"} {
		if _, ok := byName[absent]; ok {
			t.Fatalf("unset knob %s must not emit env", absent)
		}
	}
}

func TestUploadStagingEnv_NamesMatchAgentdParsers(t *testing.T) {
	// The contract: every emitted name is one cmd/workspace-agentd
	// parses (stagingConfigFromEnv / uploadApplyEngineFromEnv). Pinned
	// literally so a rename on either side fails here.
	cfg := UploadStagingConfig{BudgetBytes: 1, CredentialFloorByte: 1, MaxConcurrent: 1, TTLMilliseconds: 20001, ApplyTimeoutMilli: 1, credentialFloorSet: true}
	want := []string{
		"UPLOAD_STAGING_BUDGET",
		"UPLOAD_STAGING_CREDENTIAL_FLOOR",
		"UPLOAD_STAGING_MAX_CONCURRENT",
		"UPLOAD_STAGING_TTL_MS",
		"UPLOAD_APPLY_TIMEOUT_MS",
	}
	env := cfg.EnvVars()
	if len(env) != len(want) {
		t.Fatalf("expected %d env, got %d", len(want), len(env))
	}
	for i, e := range env {
		if e.Name != want[i] {
			t.Fatalf("env[%d]: got %q want %q", i, e.Name, want[i])
		}
	}
}

// findEnvIn scans a container's env for the upload-staging block.
func findUploadEnv(env []corev1.EnvVar) map[string]string {
	byName := map[string]string{}
	for _, e := range env {
		if strings.HasPrefix(e.Name, "UPLOAD_") {
			byName[e.Name] = e.Value
		}
	}
	return byName
}

// TestUploadStagingWiring_BothContainersCarrySetEnv pins the pod
// wiring both directions: SET config → the env lands on BOTH
// agentd-bearing containers (the sidecar: admission/staging; the
// workspace container: upload_apply + the destination scrub); ZERO
// config → NEITHER container carries any UPLOAD_* env (agentd
// defaults stand untouched).
func TestUploadStagingWiring_BothContainersCarrySetEnv(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerWithAgentd(t)
	r.AgentdSidecarEnabled = true
	r.UploadStaging = UploadStagingConfig{BudgetBytes: 52428800, TTLMilliseconds: 900000}
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	sc := sidecarInitContainer(pod, "agentd")
	got := findUploadEnv(sc.Env)
	if got["UPLOAD_STAGING_BUDGET"] != "52428800" || got["UPLOAD_STAGING_TTL_MS"] != "900000" {
		t.Fatalf("sidecar env: %v", got)
	}

	wc := &pod.Spec.Containers[0] // the workspace container (single-container default build order)
	gotW := findUploadEnv(wc.Env)
	if gotW["UPLOAD_STAGING_BUDGET"] != "52428800" || gotW["UPLOAD_STAGING_TTL_MS"] != "900000" {
		t.Fatalf("workspace-container env: %v", gotW)
	}
}

func TestUploadStagingWiring_ZeroConfigCarriesNothing(t *testing.T) {
	for _, sidecar := range []bool{true, false} {
		ws := newWorkspaceForSecurity(t)
		r := reconcilerWithAgentd(t)
		r.AgentdSidecarEnabled = sidecar
		pod, err := r.buildPod(context.Background(), ws)
		require.NoError(t, err)
		for _, c := range pod.Spec.Containers {
			if env := findUploadEnv(c.Env); len(env) != 0 {
				t.Fatalf("sidecar=%v container %q must carry no UPLOAD_* env, got %v", sidecar, c.Name, env)
			}
		}
		if sidecar {
			for _, c := range pod.Spec.InitContainers {
				if env := findUploadEnv(c.Env); len(env) != 0 {
					t.Fatalf("init container %q must carry no UPLOAD_* env, got %v", c.Name, env)
				}
			}
		}
	}
}

// TestUploadStagingFlag_FloorZeroIsExplicit pins the disposition: an
// EXPLICIT floor=0 (disable the reserve) parses, flag-strings, and
// emits env (agentd honors n ≥ 0) — never conflated with unset.
func TestUploadStagingFlag_FloorZeroIsExplicit(t *testing.T) {
	cfg, err := ParseUploadStagingFlag("floor=0,ttlMs=900000")
	if err != nil {
		t.Fatalf("floor=0 must parse: %v", err)
	}
	if !cfg.credentialFloorSet || cfg.CredentialFloorByte != 0 {
		t.Fatalf("explicit floor=0: %+v", cfg)
	}
	if got := cfg.FlagString(); got != "floor=0,ttlMs=900000" {
		t.Fatalf("FlagString must carry floor=0, got %q", got)
	}
	env := cfg.EnvVars()
	if len(env) != 2 || env[0].Name != "UPLOAD_STAGING_CREDENTIAL_FLOOR" || env[0].Value != "0" {
		t.Fatalf("floor=0 must emit env value 0, got %v", env)
	}
}

// TestUploadStagingFlag_TTLMustExceedApplyBound pins the cross-
// validation: the TTL sweeper must never be able to reclaim an
// in-flight apply's temp (ttl must exceed applyTimeout + its 5s slack).
func TestUploadStagingFlag_TTLMustExceedApplyBound(t *testing.T) {
	if _, err := ParseUploadStagingFlag("ttlMs=60000,applyTimeoutMs=600000"); err == nil {
		t.Fatal("a ttl inside the apply bound must reject (the sweeper-reclaims-in-flight class)")
	}
	if _, err := ParseUploadStagingFlag("ttlMs=66000,applyTimeoutMs=60000"); err != nil {
		t.Fatalf("ttl just past the bound must pass: %v", err)
	}
	// The r2 hole — single-knob configs: an unset applyTimeoutMs falls
	// back to agentd's compiled 60s default (the guard must use the
	// EFFECTIVE bound, not just the set one).
	if _, err := ParseUploadStagingFlag("ttlMs=60000"); err == nil {
		t.Fatal("ttlMs=60000 alone must reject (60s ≤ the effective default bound 60s+5s)")
	}
	if _, err := ParseUploadStagingFlag("ttlMs=66000"); err != nil {
		t.Fatalf("ttlMs=66000 alone must pass (66000 > the effective default bound 65000): %v", err)
	}
	if _, err := ParseUploadStagingFlag("applyTimeoutMs=900000"); err == nil {
		t.Fatal("applyTimeoutMs=900000 alone must reject (the default ttl 15m=900000 ≤ 905000)")
	}
	if _, err := ParseUploadStagingFlag("ttlMs=906000,applyTimeoutMs=900000"); err != nil {
		t.Fatalf("both knobs past the bound must pass: %v", err)
	}
}

// TestUploadStagingFlag_DuplicateKeysRejected pins the no-silent-last-
// win stance.
func TestUploadStagingFlag_DuplicateKeysRejected(t *testing.T) {
	if _, err := ParseUploadStagingFlag("budget=1,budget=2"); err == nil {
		t.Fatal("duplicate keys must reject (last-win is the silent-default class)")
	}
}
