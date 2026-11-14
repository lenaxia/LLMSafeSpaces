// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package v1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetDefaults_Workspace applies all +kubebuilder:default annotations from
// WorkspaceSpec and its sub-structs in Go code. This is the unit-test seam
// for M5-b: fake-client does NOT apply kubebuilder defaults (only the real
// API server's admission webhook does), so tests that create a bare Workspace
// see zero values where production sees defaults. Calling this function on
// a test fixture mirrors what the admission webhook would have done.
//
// Idempotent: only sets a field when it is at its zero value. Explicitly-set
// values are preserved.
//
// Keep in sync with the +kubebuilder:default annotations in workspace_types.go.
// The crd_schema_test.go structural check catches missing fields, but does
// not check defaults — this function + its tests fill that gap.
func SetDefaults_Workspace(ws *Workspace) {
	if ws == nil {
		return
	}
	SetDefaults_WorkspaceSpec(&ws.Spec)
}

// SetDefaults_WorkspaceSpec applies WorkspaceSpec defaults.
func SetDefaults_WorkspaceSpec(spec *WorkspaceSpec) {
	if spec == nil {
		return
	}

	if spec.Harness == "" {
		spec.Harness = HarnessOpencode
	}
	if spec.Architecture == "" {
		spec.Architecture = "amd64"
	}
	if spec.SecurityLevel == "" {
		spec.SecurityLevel = "standard"
	}
	if spec.MaxActiveSessions == 0 {
		spec.MaxActiveSessions = 5
	}

	setDefaultsStorage(&spec.Storage)

	if spec.AutoSuspend == nil {
		spec.AutoSuspend = &WorkspaceAutoSuspend{}
	}
	setDefaultsAutoSuspend(spec.AutoSuspend)

	if spec.Resources == nil {
		spec.Resources = &ResourceRequirements{}
	}
	setDefaultsResources(spec.Resources)
}

func setDefaultsStorage(s *WorkspaceStorageConfig) {
	if s.AccessMode == "" {
		s.AccessMode = "ReadWriteOnce"
	}
}

func setDefaultsAutoSuspend(a *WorkspaceAutoSuspend) {
	// NOTE: Enabled defaults to true but its zero value is false — this is
	// the exact class of bug that bit PR #231 (default:false incident).
	// A nil-initialized AutoSuspend must get Enabled=true.
	if !a.Enabled && a.IdleTimeoutSeconds == 0 {
		a.Enabled = true
	}
	if a.IdleTimeoutSeconds == 0 {
		a.IdleTimeoutSeconds = 86400
	}
}

func setDefaultsResources(r *ResourceRequirements) {
	if r.CPU == "" {
		r.CPU = "500m"
	}
	if r.Memory == "" {
		r.Memory = "512Mi"
	}
}

// SetDefaults_InferenceRelay applies all +kubebuilder:default annotations
// from InferenceRelaySpec in Go code. See SetDefaults_Workspace for rationale.
func SetDefaults_InferenceRelay(relay *InferenceRelay) {
	if relay == nil {
		return
	}
	SetDefaults_InferenceRelaySpec(&relay.Spec)
}

// SetDefaults_InferenceRelaySpec applies InferenceRelaySpec defaults.
func SetDefaults_InferenceRelaySpec(spec *InferenceRelaySpec) {
	if spec == nil {
		return
	}

	if spec.UpstreamURL == "" {
		spec.UpstreamURL = "https://opencode.ai/zen/v1"
	}

	setDefaultsHealthCheck(&spec.HealthCheck)
	setDefaultsFallback(&spec.Fallback)
	setDefaultsRotation(&spec.Rotation)
}

func setDefaultsHealthCheck(h *HealthCheckConfig) {
	if h.Interval.Duration == 0 {
		h.Interval = metav1.Duration{Duration: 15 * time.Second}
	}
	if h.Timeout.Duration == 0 {
		h.Timeout = metav1.Duration{Duration: 5 * time.Second}
	}
	if h.UnhealthyThreshold == 0 {
		h.UnhealthyThreshold = 3
	}
	if h.ReplacementTimeout.Duration == 0 {
		h.ReplacementTimeout = metav1.Duration{Duration: 15 * time.Minute}
	}
}

func setDefaultsFallback(f *FallbackConfig) {
	// Enabled defaults to true but zero value is false — same class as
	// AutoSuspend.Enabled. Only set when both Enabled and Rate are at zero
	// (otherwise the caller explicitly configured fallback).
	if !f.Enabled && f.Rate == 0 && f.MaxConcurrent == 0 {
		f.Enabled = true
	}
	if f.Rate == 0 {
		f.Rate = 0.5
	}
	if f.MaxConcurrent == 0 {
		f.MaxConcurrent = 1
	}
}

func setDefaultsRotation(r *RotationConfig) {
	if !r.Enabled && r.Max429Rate == 0 && r.DetectionWindow.Duration == 0 {
		r.Enabled = true
	}
	if r.Max429Rate == 0 {
		r.Max429Rate = 0.5
	}
	if r.DetectionWindow.Duration == 0 {
		r.DetectionWindow = metav1.Duration{Duration: 5 * time.Minute}
	}
	if r.Cooldown.Duration == 0 {
		r.Cooldown = metav1.Duration{Duration: 30 * time.Minute}
	}
}
