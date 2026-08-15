// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// #863: agentdDelivery values gating.
//
//   - Default (empty image): no agentd flags on the controller Deployment.
//   - Configured: all three flags render, exactly as pinned.
//   - Half-configured (image set, hashes missing): the render FAILS —
//     the required guards in controller-deployment.yaml turn operator
//     error into an install-time error instead of a runtime one
//     (entrypoint exit-81 storm).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func agentdFlags(t *testing.T, valuesYAML string) []string {
	t.Helper()
	docs := helmTemplate(t, valuesYAML)
	var flags []string
	for _, doc := range docs {
		if doc["kind"] != "Deployment" {
			continue
		}
		name := doc["metadata"].(map[string]any)["name"]
		if !strings.Contains(name.(string), "controller") {
			continue
		}
		spec := doc["spec"].(map[string]any)
		pod := spec["template"].(map[string]any)["spec"].(map[string]any)
		for _, c := range pod["containers"].([]any) {
			cm := c.(map[string]any)
			if cm["name"] != "controller" {
				continue
			}
			for _, a := range cm["args"].([]any) {
				if s, ok := a.(string); ok && strings.HasPrefix(s, "--agentd-") {
					flags = append(flags, s)
				}
			}
		}
	}
	return flags
}

func TestAgentdDelivery_DefaultRendersNoFlags(t *testing.T) {
	flags := agentdFlags(t, "")
	require.Empty(t, flags, "legacy mode must render no agentd flags")
}

func TestAgentdDelivery_ConfiguredRendersAllFlags(t *testing.T) {
	flags := agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    binarySHA256Amd64: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    binarySHA256Arm64: bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`)
	require.Len(t, flags, 3, "image + both binary pins must render: %v", flags)
	require.Contains(t, flags, "--agentd-image=ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	require.Contains(t, flags, "--agentd-binary-sha256-amd64=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.Contains(t, flags, "--agentd-binary-sha256-arm64=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
}

func TestAgentdDelivery_HalfConfiguredFailsRender(t *testing.T) {
	// helmTemplate require-fails on a bad render; recover the failure and
	// assert it carries the required-guard message for the missing pins.
	var renderErr string
	func() {
		defer func() {
			if r := recover(); r != nil {
				if s, ok := r.(string); ok {
					renderErr = s
				} else {
					renderErr = "non-string failure"
				}
			}
		}()
		agentdFlags(t, `controller:
  agentdDelivery:
    image: ghcr.io/lenaxia/llmsafespaces/agentd@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
`)
	}()
	require.NotEmpty(t, renderErr,
		"half-configured agentdDelivery must fail the render — install-time error beats an entrypoint exit-81 storm")
}
