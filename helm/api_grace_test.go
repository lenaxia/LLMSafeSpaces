// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// #1019 option C: the API deployment's termination grace period must
// cover a worst-case in-flight outbox delivery. SIGTERM triggers the
// graceful path (Run joins workers, lock released) — but k8s SIGKILLs
// after the grace period, and a killed-mid-delivery worker leaves the
// per-session lock to its 12-minute TTL (the incident's freeze window).
// A grace ≥ DeliveryTimeout (10 min) + margin means every graceful
// deploy lets deliveries finish naturally; it costs nothing when idle
// because the container exits as soon as the process does.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func apiGraceDeploymentDoc(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range findByKind(docs, "Deployment") {
		if metaName(d) == "test-release-llmsafespaces-api" {
			return d
		}
	}
	t.Fatal("api Deployment not rendered")
	return nil
}

func TestAPI_TerminationGrace_DefaultCoversDeliveryWindow(t *testing.T) {
	docs := helmTemplate(t, "")
	dep := apiGraceDeploymentDoc(t, docs)
	spec := dep["spec"].(map[string]any)
	tmpl := spec["template"].(map[string]any)
	pod := tmpl["spec"].(map[string]any)

	grace, ok := pod["terminationGracePeriodSeconds"].(float64)
	require.True(t, ok, "terminationGracePeriodSeconds must be rendered on the API pod")
	assert.GreaterOrEqual(t, grace, float64(660),
		"default grace must cover DeliveryTimeout (10m) + verify margin — a lower value reintroduces the #1019 SIGKILL freeze on rolling deploys")
}

func TestAPI_TerminationGrace_OperatorOverride(t *testing.T) {
	docs := helmTemplate(t, "api:\n  terminationGracePeriodSeconds: 90\n")
	dep := apiGraceDeploymentDoc(t, docs)
	spec := dep["spec"].(map[string]any)
	tmpl := spec["template"].(map[string]any)
	pod := tmpl["spec"].(map[string]any)

	grace, ok := pod["terminationGracePeriodSeconds"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(90), grace, "operator override must win")
}
