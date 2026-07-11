// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NetworkPolicy drift: the chart's default blockedEgressCIDRs list must
// include CGNAT (100.64.0.0/10) because managed Kubernetes offerings
// (AKS default VNet, some EKS configs, k3s with default flannel) use
// 100.64/10 as the pod CIDR. Without it in the chart-side block list,
// workspace pods can reach internal pods/services in the CGNAT range.
//
// The controller-side list (controller/internal/workspace/network_policy.go)
// has had 100.64/10 since Epic 17 G16; the chart-side list was missed in
// that pass. This test pins parity.
func TestG16_DefaultRender_BlockedEgressIncludesCGNAT(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var foundPolicy bool
	for _, p := range policies {
		if name := metaName(p); !contains(name, "workspace-egress") {
			continue
		}

		// Walk every egress rule; find any ipBlock with `except:` and
		// assert CGNAT is in there.
		spec, _ := p["spec"].(map[string]any)
		egress, _ := spec["egress"].([]any)
		for _, ruleAny := range egress {
			rule, _ := ruleAny.(map[string]any)
			toList, _ := rule["to"].([]any)
			for _, toAny := range toList {
				to, _ := toAny.(map[string]any)
				ipBlock, _ := to["ipBlock"].(map[string]any)
				if ipBlock == nil {
					continue
				}
				excepts, _ := ipBlock["except"].([]any)
				for _, e := range excepts {
					if e == "100.64.0.0/10" {
						foundPolicy = true
					}
				}
			}
		}
	}
	require.True(t, foundPolicy,
		"default workspace-egress NetworkPolicy must list 100.64.0.0/10 (CGNAT) "+
			"in its blockedEgressCIDRs except: list — parity with the "+
			"controller-side privateOrInternalCIDRs list")
}

// TestG16_DefaultRender_BlockedEgressIncludesAllControllerSideCIDRs
// pins chart/controller parity for the full private-or-internal CIDR set.
// Drift here reopens paths to internal services (loopback, multicast,
// link-local, RFC1918, CGNAT) that the controller-side list covers.
func TestG16_DefaultRender_BlockedEgressIncludesAllControllerSideCIDRs(t *testing.T) {
	wantCIDRs := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
	}

	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	got := blockedEgressCIDRs(t, policies)
	for _, want := range wantCIDRs {
		assert.Contains(t, got, want,
			"chart default blockedEgressCIDRs must include %s for parity with controller-side list", want)
	}
}

func blockedEgressCIDRs(t *testing.T, policies []map[string]any) []string {
	t.Helper()
	for _, p := range policies {
		if name := metaName(p); !contains(name, "workspace-egress") {
			continue
		}
		spec, _ := p["spec"].(map[string]any)
		egress, _ := spec["egress"].([]any)
		for _, ruleAny := range egress {
			rule, _ := ruleAny.(map[string]any)
			toList, _ := rule["to"].([]any)
			for _, toAny := range toList {
				to, _ := toAny.(map[string]any)
				ipBlock, _ := to["ipBlock"].(map[string]any)
				if ipBlock == nil {
					continue
				}
				excepts, _ := ipBlock["except"].([]any)
				if len(excepts) > 0 {
					out := make([]string, 0, len(excepts))
					for _, e := range excepts {
						if s, ok := e.(string); ok {
							out = append(out, s)
						}
					}
					return out
				}
			}
		}
	}
	return nil
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
