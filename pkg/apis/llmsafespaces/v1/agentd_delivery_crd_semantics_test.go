// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package v1

// agentd_delivery_crd_semantics_test.go — #1746 r2 (review Finding 1):
// spec.agentdDelivery.updatePolicy must carry NO structural-schema
// default. The documented resolution chain is
//
//	spec.agentdDelivery.updatePolicy → --agentd-update-policy → OnRefresh
//
// and "empty = inherit the instance default" must hold not only for an
// ABSENT agentdDelivery object but for a PRESENT one written without an
// explicit policy — e.g. the operator's documented break-glass patch
//
//	kubectl patch workspace my-ws --type=merge \
//	  '{"spec":{"agentdDelivery":{"forceRoll":true}}}'
//
// Kubernetes defaulting fills defaults for properties of a PRESENT
// object: `default: OnRefresh` would stamp updatePolicy=OnRefresh
// server-side on exactly that patch, permanently pinning the workspace
// away from an instance-level `--agentd-update-policy=Rolling` —
// silently, surviving the forceRoll clear, invisible to fake-client
// tests (they apply no CRD defaults). This pin reads the chart CRD
// directly and refuses the default; empty is resolved in code
// (resolveAgentdUpdatePolicy), the one place the whole chain is visible.
//
// forceRoll's `default: false` is deliberately tolerated: false is the
// zero value — a materialized false is behaviorally identical to an
// absent field (no inheritance semantics ride it).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCRDAgentdDeliveryUpdatePolicyHasNoDefault(t *testing.T) {
	repoRoot, err := findRepoRootForCRD()
	require.NoError(t, err, "repo root")

	raw, err := os.ReadFile(filepath.Join(repoRoot, "helm", "crds", "workspace.yaml"))
	require.NoError(t, err, "reading the chart CRD")

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc), "the chart CRD must parse")

	spec := doc["spec"].(map[string]any)
	versions := spec["versions"].([]any)
	version := versions[0].(map[string]any)
	schema := version["schema"].(map[string]any)
	openAPI := schema["openAPIV3Schema"].(map[string]any)
	props := openAPI["properties"].(map[string]any)
	specProps := props["spec"].(map[string]any)["properties"].(map[string]any)
	delivery, ok := specProps["agentdDelivery"].(map[string]any)
	require.True(t, ok, "spec.agentdDelivery must exist in the chart CRD")
	deliveryProps := delivery["properties"].(map[string]any)
	policy, ok := deliveryProps["updatePolicy"].(map[string]any)
	require.True(t, ok, "spec.agentdDelivery.updatePolicy must exist in the chart CRD")

	require.NotContains(t, policy, "default",
		"updatePolicy must carry NO schema default: a default materializes on partial writes of agentdDelivery "+
			"(e.g. the forceRoll break-glass patch) and silently pins the workspace to OnRefresh, defeating the "+
			"documented spec → --agentd-update-policy → OnRefresh inheritance chain (#1746 r2 Finding 1). "+
			"Empty means inherit — resolved in resolveAgentdUpdatePolicy, the one place the whole chain is visible.")
}
