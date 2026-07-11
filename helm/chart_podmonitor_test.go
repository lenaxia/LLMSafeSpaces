// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Regression tests for the agentd PodMonitor template (issue #425).
//
// The PodMonitor CRD from prometheus-operator defines its per-port scrape
// list under spec.podMetricsEndpoints. spec.endpoints is the ServiceMonitor
// analog. Writing `endpoints` on a PodMonitor is rejected by Prometheus
// Operator CRD validation under strict structural-schema enforcement
// (every current release), surfacing as:
//
//	failed to create typed patch object ... .spec.endpoints: field not declared in schema
//
// and failing the helm install/upgrade entirely. The chart's own CI missed
// this because `helm template` and `helm lint` do not validate against the
// installed cluster CRDs.
//
// These tests render the template and assert the rendered PodMonitor uses
// the correct field and remains toggle-gated. They fail against the buggy
// template (which emits spec.endpoints).

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentdPodMonitor_UsesPodMetricsEndpointsNotEndpoints verifies the
// rendered PodMonitor uses the PodMonitor-native `podMetricsEndpoints`
// field, not the ServiceMonitor-only `endpoints` field.
func TestAgentdPodMonitor_UsesPodMetricsEndpointsNotEndpoints(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	pms := findByKind(docs, "PodMonitor")
	require.NotEmpty(t, pms,
		"agentd PodMonitor must render when monitoring.enabled=true (agentdPodMonitor.enabled defaults to true)")

	var pm map[string]any
	for _, d := range pms {
		if metaName(d) == "test-release-llmsafespaces-agentd" {
			pm = d
			break
		}
	}
	require.NotNil(t, pm, "agentd PodMonitor (test-release-llmsafespaces-agentd) not rendered")

	spec, ok := pm["spec"].(map[string]any)
	require.True(t, ok, "PodMonitor must have a spec")

	// Correct field for PodMonitor.
	require.Contains(t, spec, "podMetricsEndpoints",
		"PodMonitor must use spec.podMetricsEndpoints (the PodMonitor field)")
	pme, ok := spec["podMetricsEndpoints"].([]any)
	require.True(t, ok && len(pme) > 0, "spec.podMetricsEndpoints must be a non-empty list")
	first, ok := pme[0].(map[string]any)
	require.True(t, ok, "first podMetricsEndpoints entry must be a map")
	assert.Equal(t, "agentd-admin", first["port"],
		"podMetricsEndpoints[0].port must be agentd-admin (workspace agentd /metrics on :4098)")
	assert.Equal(t, "/metrics", first["path"],
		"podMetricsEndpoints[0].path must be /metrics")

	// ServiceMonitor-only field must NOT be present.
	_, hasEndpoints := spec["endpoints"]
	require.False(t, hasEndpoints,
		"PodMonitor must NOT use spec.endpoints (that is a ServiceMonitor field; "+
			"Prometheus Operator CRD validation rejects it — see issue #425)")
}

// TestAgentdPodMonitor_DefaultNotRendered verifies the PodMonitor is opt-in
// via the monitoring.enabled master toggle (defaults to false), so
// clusters without prometheus-operator don't get an unresolvable CRD.
func TestAgentdPodMonitor_DefaultNotRendered(t *testing.T) {
	docs := helmTemplate(t, "")
	assert.Empty(t, findByKind(docs, "PodMonitor"),
		"no PodMonitor should render with default values (monitoring.enabled=false)")
}

// TestAgentdPodMonitor_DisabledViaAgentdPodMonitorToggle verifies the
// PodMonitor can be turned off independently of the master monitoring
// toggle for clusters that run ServiceMonitors but not PodMonitors.
func TestAgentdPodMonitor_DisabledViaAgentdPodMonitorToggle(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n  serviceMonitors:\n    agentdPodMonitor:\n      enabled: false\n")
	assert.Empty(t, findByKind(docs, "PodMonitor"),
		"no PodMonitor should render when agentdPodMonitor.enabled=false")
}
