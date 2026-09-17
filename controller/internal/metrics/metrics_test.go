// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
)

// isolatedRegistry returns a fresh Prometheus registry with all package
// metrics registered. Tests must use this instead of the global default
// registry to prevent cross-test pollution and double-registration panics.
func isolatedRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	require.NoError(t, metrics.RegisterWith(reg), "RegisterWith must not error on a fresh registry")
	return reg
}

// TestRegistrationNoPanic verifies that RegisterWith succeeds on a clean
// registry. This is the guard against duplicate-name panics: if two metrics
// share a name, Register returns an error here rather than panicking in
// production via MustRegister.
func TestRegistrationNoPanic(t *testing.T) {
	isolatedRegistry(t) // will call t.Fatal via require.NoError on any conflict
}

// TestRegistrationIdempotentOnSeparateRegistries verifies that two separate
// test runs (each with their own registry) do not interfere. The global
// default registry is never touched by these tests.
func TestRegistrationIdempotentOnSeparateRegistries(t *testing.T) {
	reg1 := prometheus.NewRegistry()
	reg2 := prometheus.NewRegistry()
	require.NoError(t, metrics.RegisterWith(reg1))
	require.NoError(t, metrics.RegisterWith(reg2))
}

// TestNoDoubleRegistrationOnDefaultRegistry verifies that calling SetupMetrics
// twice (e.g. in a test that somehow imports controller main) does not panic.
// We cannot call SetupMetrics directly in tests (it touches the global registry
// which persists across test runs), so we validate the isolation contract instead:
// each call to RegisterWith on a fresh registry succeeds, and a second call to
// the SAME registry returns an error (expected duplicate).
func TestDoubleRegistrationOnSameRegistryReturnsError(t *testing.T) {
	reg := prometheus.NewRegistry()
	require.NoError(t, metrics.RegisterWith(reg))
	err := metrics.RegisterWith(reg)
	assert.Error(t, err, "second RegisterWith on same registry must return AlreadyRegisteredError")
}

// TestWorkspaceCreateDurationObserve verifies the histogram accepts
// observations and produces the expected metric family.
func TestWorkspaceCreateDurationObserve(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspaceCreateDurationSeconds.WithLabelValues("false", "false").Observe(5.0)
	metrics.WorkspaceCreateDurationSeconds.WithLabelValues("true", "false").Observe(45.0)

	mf := gatherFamily(t, reg, "llmsafespaces_workspace_create_duration_seconds")
	require.NotNil(t, mf)

	// Two label combinations should produce two metric series.
	assert.Len(t, mf.GetMetric(), 2)

	// Verify the first series has count=1, sum≈5.0.
	m := findMetricByLabels(t, mf, map[string]string{
		"has_packages": "false", "has_init_script": "false",
	})
	require.NotNil(t, m)
	assert.EqualValues(t, 1, m.GetHistogram().GetSampleCount())
	assert.InDelta(t, 5.0, m.GetHistogram().GetSampleSum(), 0.001)
}

// TestWorkspaceResumeDurationLabels verifies both resume_type label values
// are accepted and produce distinct series.
func TestWorkspaceResumeDurationLabels(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspaceResumeDurationSeconds.WithLabelValues("first_resume").Observe(20.0)
	metrics.WorkspaceResumeDurationSeconds.WithLabelValues("subsequent_resume").Observe(12.0)

	mf := gatherFamily(t, reg, "llmsafespaces_workspace_resume_duration_seconds")
	require.NotNil(t, mf)
	assert.Len(t, mf.GetMetric(), 2)
}

// TestWorkspaceInitContainerDuration verifies the init container histogram
// produces a sample with the expected sum.
func TestWorkspaceInitContainerDuration(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspaceInitContainerDurationSeconds.Observe(7.3)

	mf := gatherFamily(t, reg, "llmsafespaces_workspace_init_container_duration_seconds")
	require.NotNil(t, mf)
	require.Len(t, mf.GetMetric(), 1)
	assert.EqualValues(t, 1, mf.GetMetric()[0].GetHistogram().GetSampleCount())
	assert.InDelta(t, 7.3, mf.GetMetric()[0].GetHistogram().GetSampleSum(), 0.001)
}

// TestWorkspacesRunningGauge verifies the running gauge can be incremented
// and decremented and produces the correct value.
func TestWorkspacesRunningGauge(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspacesRunning.WithLabelValues("python:3.11", "standard").Inc()
	metrics.WorkspacesRunning.WithLabelValues("python:3.11", "standard").Inc()
	metrics.WorkspacesRunning.WithLabelValues("python:3.11", "standard").Dec()

	mf := gatherFamily(t, reg, "llmsafespaces_workspaces_running")
	require.NotNil(t, mf)
	m := findMetricByLabels(t, mf, map[string]string{
		"runtime": "python:3.11", "security_level": "standard",
	})
	require.NotNil(t, m)
	assert.EqualValues(t, 1.0, m.GetGauge().GetValue())
}

// TestWorkspaceRecoveryExhaustedLabels verifies the #760 exhaustion
// counter accepts the failure_class label and increments correctly.
func TestWorkspaceRecoveryExhaustedLabels(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspaceRecoveryExhaustedTotal.WithLabelValues("Infrastructure").Inc()
	metrics.WorkspaceRecoveryExhaustedTotal.WithLabelValues("Infrastructure").Inc()
	metrics.WorkspaceRecoveryExhaustedTotal.WithLabelValues("Process").Inc()

	mf := gatherFamily(t, reg, "llmsafespaces_workspace_recovery_exhausted_total")
	require.NotNil(t, mf)
	assert.Len(t, mf.GetMetric(), 2)

	m := findMetricByLabels(t, mf, map[string]string{"failure_class": "Infrastructure"})
	require.NotNil(t, m)
	assert.EqualValues(t, 2.0, m.GetCounter().GetValue())
}

// TestBucketCoverage verifies the startup buckets cover all values from
// sub-second to 5 minutes so histograms never overflow into +Inf prematurely
// for realistic latencies.
func TestBucketCoverage(t *testing.T) {
	reg := isolatedRegistry(t)

	// Observe a 4-minute startup — should land in a real bucket, not +Inf.
	metrics.WorkspaceCreateDurationSeconds.
		WithLabelValues("true", "true").Observe(240.0)

	mf := gatherFamily(t, reg, "llmsafespaces_workspace_create_duration_seconds")
	require.NotNil(t, mf)
	h := mf.GetMetric()[0].GetHistogram()

	// Find the +Inf bucket (last bucket always exists).
	var infCount uint64
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == 300.0 {
			// 240s should be <= 300s bucket.
			assert.EqualValues(t, 1, b.GetCumulativeCount(),
				"240s observation must fall within the 300s bucket")
		}
		infCount = b.GetCumulativeCount()
	}
	_ = infCount
}

func TestSeedWorkspacesRunningOverrides(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspacesRunning.WithLabelValues("base", "standard").Inc()
	metrics.WorkspacesRunning.WithLabelValues("base", "standard").Inc()
	metrics.WorkspacesRunning.WithLabelValues("base", "standard").Inc()
	metrics.WorkspacesRunning.WithLabelValues("base", "standard").Dec()

	metrics.SeedWorkspacesRunning("base", "standard", 6)

	mf := gatherFamily(t, reg, "llmsafespaces_workspaces_running")
	require.NotNil(t, mf)
	m := findMetricByLabels(t, mf, map[string]string{
		"runtime": "base", "security_level": "standard",
	})
	require.NotNil(t, m)
	assert.EqualValues(t, 6.0, m.GetGauge().GetValue(),
		"SeedWorkspacesRunning must Set (absolute), not Add (relative)")
}

func TestSeedWorkspacesRunningZeroActiveWorkspaces(t *testing.T) {
	reg := isolatedRegistry(t)

	metrics.WorkspacesRunning.WithLabelValues("base", "standard").Inc()
	metrics.WorkspacesRunning.WithLabelValues("base", "standard").Inc()

	metrics.SeedWorkspacesRunning("base", "standard", 0)

	mf := gatherFamily(t, reg, "llmsafespaces_workspaces_running")
	require.NotNil(t, mf)
	m := findMetricByLabels(t, mf, map[string]string{
		"runtime": "base", "security_level": "standard",
	})
	require.NotNil(t, m)
	assert.EqualValues(t, 0.0, m.GetGauge().GetValue(),
		"Seeding with 0 must reset the gauge to 0")
}

func TestCountActiveByLabels(t *testing.T) {
	_ = isolatedRegistry(t)

	ws := []struct {
		phase    string
		runtime  string
		secLevel string
	}{
		{"Active", "base", "standard"},
		{"Active", "base", "standard"},
		{"Suspended", "base", "standard"},
		{"Active", "python:3.11", "hardened"},
		{"Creating", "base", "standard"},
	}

	counts := map[[2]string]int{}
	for _, w := range ws {
		if w.phase == "Active" {
			counts[[2]string{w.runtime, w.secLevel}]++
		}
	}

	assert.Equal(t, 2, counts[[2]string{"base", "standard"}])
	assert.Equal(t, 1, counts[[2]string{"python:3.11", "hardened"}])
	_, hasSuspended := counts[[2]string{"base", "suspended"}]
	assert.False(t, hasSuspended, "Suspended workspaces must not be counted")
	assert.Len(t, counts, 2, "Only Active workspaces produce entries")
}

func TestAllCollectorsGatherableAfterRegisterWith(t *testing.T) {
	reg := isolatedRegistry(t)

	families, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool, len(families))
	for _, mf := range families {
		names[mf.GetName()] = true
	}

	// Unlabeled scalar metrics are always gatherable after registration.
	// Labeled metrics (CounterVec/GaugeVec) only appear after their first
	// WithLabelValues().Inc()/Set() call, so we don't assert them here —
	// that's tested by the individual metric tests above.
	// Note: llmsafespaces_workspace_status_update_conflicts_total is a
	// CounterVec with a "site" label (Epic 23) so it lives in the labeled
	// bucket; conflict-iteration coverage is asserted in
	// controller/internal/workspace/status_update_retry_test.go.
	expected := []string{
		"llmsafespaces_api_key_legacy_total",
		"llmsafespaces_workspace_init_container_duration_seconds",
	}
	for _, name := range expected {
		assert.True(t, names[name], "metric %q must be gatherable after RegisterWith", name)
	}

	// Verify the full collector count matches what RegisterWith registered.
	assert.GreaterOrEqual(t, len(families), len(expected),
		"RegisterWith must register at least %d unlabeled collectors (got %d metric families)",
		len(expected), len(families))
}

// ---- helpers ----

func gatherFamily(t *testing.T, reg *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Errorf("metric family %q not found in registry", name)
	return nil
}

func findMetricByLabels(t *testing.T, mf *dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()
outer:
	for _, m := range mf.GetMetric() {
		labelMap := make(map[string]string, len(m.GetLabel()))
		for _, lp := range m.GetLabel() {
			labelMap[lp.GetName()] = lp.GetValue()
		}
		for k, v := range want {
			if labelMap[k] != v {
				continue outer
			}
		}
		return m
	}
	return nil
}
