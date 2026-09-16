// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Issue #821 (Epic 67): workspace egress had no destination allowlist —
// the chart-level policy allowed 0.0.0.0/0 minus RFC1918/CGNAT/private
// ranges, leaving arbitrary public IPs reachable from inside a sandbox
// (the exfiltration sink for SEC-1/SEC-3/SEC-6).
//
// These tests pin the destination-allowlist egress mode:
//
//   - networkPolicy.workspaceEgress.mode selects the posture:
//       "public"    (default, staged rollout): legacy 0.0.0.0/0-minus-
//                   blockedEgressCIDRs deny-list.
//       "allowlist": explicit destination allowlist — the general public
//                   internet rule is NOT rendered; only DNS, the in-
//                   namespace API relay path, the optional relay-router,
//                   and operator-supplied CIDR groups (data-plane LLM
//                   split from tooling registries) are permitted.
//   - Unknown modes fail the render loudly (a typo must not silently
//     fall back to the permissive posture).
//   - Allowlist group rules carry only TCP 80/443 (the public services
//     ports) and no `except:` list — the Kubernetes API rejects
//     ipBlock rules whose `except` entries are not subnets of `cidr`,
//     so the shared blockedEgressCIDRs list cannot be attached to
//     narrow public CIDRs.
//   - The legacy public-mode general rule keeps its `except:` list ONLY
//     on the 0.0.0.0/0 catch-all — a narrow custom allowedEgressCIDRs
//     entry with the shared except list is API-invalid (pre-existing
//     defect found while designing #821; pinned here).

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// egressAllowlistValues renders the chart in allowlist mode with
// TEST-NET CIDRs in both destination groups (data-plane + tooling).
const egressAllowlistValues = `
networkPolicy:
  workspaceEgress:
    mode: allowlist
    allowlist:
      llmCIDRs:
        - 203.0.113.0/24
      tooling:
        enabled: true
        cidrs:
          - 198.51.100.0/24
`

// findWorkspaceEgressPolicy returns the single workspace-egress
// NetworkPolicy from a rendered doc set.
func findWorkspaceEgressPolicy(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	policies := findByKind(docs, "NetworkPolicy")
	for _, p := range policies {
		if strings.Contains(metaName(p), "workspace-egress") {
			return p
		}
	}
	require.FailNow(t, "workspace-egress NetworkPolicy not found in render")
	return nil
}

// egressRulesOf returns the policy's spec.egress rules.
func egressRulesOf(t *testing.T, policy map[string]any) []map[string]any {
	t.Helper()
	spec, ok := policy["spec"].(map[string]any)
	require.True(t, ok, "policy spec must be a map")
	rules, ok := spec["egress"].([]any)
	require.True(t, ok, "policy spec.egress must be a list")
	out := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

// ruleIPBlockCIDRs returns the ipBlock cidrs of a single egress rule.
func ruleIPBlockCIDRs(t *testing.T, rule map[string]any) []string {
	t.Helper()
	toList, _ := rule["to"].([]any)
	out := []string{}
	for _, toAny := range toList {
		to, _ := toAny.(map[string]any)
		ipBlock, _ := to["ipBlock"].(map[string]any)
		if ipBlock == nil {
			continue
		}
		cidr, _ := ipBlock["cidr"].(string)
		out = append(out, cidr)
	}
	return out
}

// allIPBlockCIDRs returns every ipBlock cidr across the policy's rules.
func allIPBlockCIDRs(t *testing.T, policy map[string]any) []string {
	t.Helper()
	var out []string
	for _, rule := range egressRulesOf(t, policy) {
		out = append(out, ruleIPBlockCIDRs(t, rule)...)
	}
	return out
}

// rulePorts returns the rule's ports list as parsed maps (nil when the
// rule is all-ports).
func rulePorts(rule map[string]any) []map[string]any {
	ports, _ := rule["ports"].([]any)
	out := make([]map[string]any, 0, len(ports))
	for _, pAny := range ports {
		p, _ := pAny.(map[string]any)
		out = append(out, p)
	}
	return out
}

// rulePortNumbers returns the port numbers of a rule's ports list.
func rulePortNumbers(rule map[string]any) []int {
	out := []int{}
	for _, p := range rulePorts(rule) {
		out = append(out, toInt(p["port"]))
	}
	return out
}

// TestEgress_Allowlist_DefaultModeIsPublic pins the staged-rollout
// default (#821): an unmodified render keeps the legacy public posture.
// Flipping the default silently would break every upgrading deployment
// whose agents rely on direct-to-Zen + arbitrary package installs; the
// migration to allowlist-by-default is deliberately operator-driven.
func TestEgress_Allowlist_DefaultModeIsPublic(t *testing.T) {
	docs := helmTemplate(t, "")
	policy := findWorkspaceEgressPolicy(t, docs)
	assert.Contains(t, allIPBlockCIDRs(t, policy), "0.0.0.0/0",
		"default render must keep the legacy public-egress rule (staged rollout)")
}

// TestEgress_Allowlist_ModeDropsPublicInternetRule verifies the core
// #821 remediation: in allowlist mode the 0.0.0.0/0 general rule is not
// rendered at all, and with empty groups the policy permits nothing
// beyond the selector-based rules (DNS / API relay / router) — no
// ipBlock destination exists, so no arbitrary public IP is reachable.
func TestEgress_Allowlist_ModeDropsPublicInternetRule(t *testing.T) {
	values := `
networkPolicy:
  workspaceEgress:
    mode: allowlist
`
	docs := helmTemplate(t, values)
	policy := findWorkspaceEgressPolicy(t, docs)
	assert.NotContains(t, allIPBlockCIDRs(t, policy), "0.0.0.0/0",
		"allowlist mode must not render the general public-internet rule")
	assert.Empty(t, allIPBlockCIDRs(t, policy),
		"allowlist mode with empty groups must permit zero IP destinations "+
			"(egress limited to DNS + in-namespace API relay + optional router)")
}

// TestEgress_Allowlist_RendersGroupsOnHTTPSPortsOnly verifies that both
// destination groups render as ipBlock rules restricted to TCP 443/80 —
// the ports the default surface (LLM APIs, registries, git hosts)
// actually needs. An unrestricted-port allow entry would widen the
// exfiltration channel to arbitrary protocols on allowed CIDRs.
func TestEgress_Allowlist_RendersGroupsOnHTTPSPortsOnly(t *testing.T) {
	docs := helmTemplate(t, egressAllowlistValues)
	policy := findWorkspaceEgressPolicy(t, docs)
	got := allIPBlockCIDRs(t, policy)
	assert.Contains(t, got, "203.0.113.0/24", "llmCIDRs entry must render")
	assert.Contains(t, got, "198.51.100.0/24", "tooling.cidrs entry must render")

	for _, rule := range egressRulesOf(t, policy) {
		cidrs := ruleIPBlockCIDRs(t, rule)
		if len(cidrs) == 0 {
			continue
		}
		ports := rulePortNumbers(rule)
		assert.ElementsMatch(t, []int{443, 80}, ports,
			"allowlist ipBlock rule for %v must be restricted to TCP 443/80", cidrs)
		for _, p := range rulePorts(rule) {
			assert.Equal(t, "TCP", p["protocol"],
				"allowlist ipBlock ports must be TCP-only for %v", cidrs)
		}
	}
}

// TestEgress_Allowlist_ToolingSplit pins the #821 data-plane/tooling
// split: tooling.enabled=false removes registry/git egress while the
// LLM data-plane group remains — the posture a relay-only future
// (#820) tightens further by pointing the data-plane in-cluster.
func TestEgress_Allowlist_ToolingSplit(t *testing.T) {
	values := `
networkPolicy:
  workspaceEgress:
    mode: allowlist
    allowlist:
      llmCIDRs:
        - 203.0.113.0/24
      tooling:
        enabled: false
        cidrs:
          - 198.51.100.0/24
`
	docs := helmTemplate(t, values)
	policy := findWorkspaceEgressPolicy(t, docs)
	got := allIPBlockCIDRs(t, policy)
	assert.Contains(t, got, "203.0.113.0/24",
		"data-plane (llmCIDRs) entries must render regardless of tooling toggle")
	assert.NotContains(t, got, "198.51.100.0/24",
		"tooling entries must NOT render when allowlist.tooling.enabled=false")
}

// TestEgress_Allowlist_KeepsDNSAPIAndRouterRules verifies the in-band
// paths survive the posture switch: DNS (the policy is useless without
// it), the in-namespace API relay WebSocket path, and — when the relay
// fleet is enabled — the relay-router pod-selector rule. These are
// selector-based rules, unaffected by the ipBlock groups.
func TestEgress_Allowlist_KeepsDNSAPIAndRouterRules(t *testing.T) {
	values := relayEnabledValues + `
networkPolicy:
  workspaceEgress:
    mode: allowlist
`
	docs := helmTemplate(t, values)
	policy := findWorkspaceEgressPolicy(t, docs)

	var sawDNSUDP, sawDNSTCP, sawAPISelector, sawRouter bool
	for _, rule := range egressRulesOf(t, policy) {
		ports := rulePortNumbers(rule)
		for _, p := range rulePorts(rule) {
			switch {
			case toInt(p["port"]) == 53 && p["protocol"] == "UDP":
				sawDNSUDP = true
			case toInt(p["port"]) == 53 && p["protocol"] == "TCP":
				sawDNSTCP = true
			}
		}
		for _, toAny := range rule["to"].([]any) {
			to, _ := toAny.(map[string]any)
			podSel, _ := to["podSelector"].(map[string]any)
			if podSel == nil {
				continue
			}
			labels, _ := podSel["matchLabels"].(map[string]any)
			if labels["app.kubernetes.io/component"] == "api" {
				sawAPISelector = true
			}
			if labels["app.kubernetes.io/component"] == "relay-router" && containsInt(ports, 8080) {
				sawRouter = true
			}
		}
	}
	assert.True(t, sawDNSUDP && sawDNSTCP, "allowlist mode must keep UDP+TCP 53 DNS rule")
	assert.True(t, sawAPISelector, "allowlist mode must keep the in-namespace API relay rule")
	assert.True(t, sawRouter, "allowlist mode must keep the relay-router rule when the fleet is enabled")
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestEgress_Allowlist_GroupRulesCarryNoExcept pins the Kubernetes API
// constraint that makes the shared blockedEgressCIDRs `except:` list
// impossible on group rules: `except` entries must be subnets of the
// ipBlock `cidr`, and the blocked ranges (10/8, 172.16/12, ...) are not
// subnets of a narrow public CIDR. A group rule with the shared except
// list would make the whole NetworkPolicy API-invalid — on a fresh
// install that means NO egress policy at all (fail-open).
func TestEgress_Allowlist_GroupRulesCarryNoExcept(t *testing.T) {
	docs := helmTemplate(t, egressAllowlistValues)
	policy := findWorkspaceEgressPolicy(t, docs)
	for _, rule := range egressRulesOf(t, policy) {
		toList, _ := rule["to"].([]any)
		for _, toAny := range toList {
			to, _ := toAny.(map[string]any)
			ipBlock, _ := to["ipBlock"].(map[string]any)
			if ipBlock == nil {
				continue
			}
			_, hasExcept := ipBlock["except"]
			assert.False(t, hasExcept,
				"allowlist group ipBlock %v must not carry an except: list — "+
					"Kubernetes rejects except entries that are not subnets of cidr",
				ipBlock["cidr"])
		}
	}
}

// TestEgress_Allowlist_ExtraEgressCIDRsEscapeHatch verifies the
// operator escape hatch keeps working in allowlist mode:
// networkPolicy.extraEgressCIDRs renders plain (all-ports) ipBlock
// entries for operator-accepted internal destinations — deliberately
// WITHOUT the blockedEgressCIDRs subtraction (that is its purpose:
// carving specific internal destinations out of the blanket block).
func TestEgress_Allowlist_ExtraEgressCIDRsEscapeHatch(t *testing.T) {
	values := `
networkPolicy:
  workspaceEgress:
    mode: allowlist
  extraEgressCIDRs:
    - 192.0.2.0/24
`
	docs := helmTemplate(t, values)
	policy := findWorkspaceEgressPolicy(t, docs)

	var sawPlainExtra bool
	for _, rule := range egressRulesOf(t, policy) {
		if !containsStr(ruleIPBlockCIDRs(t, rule), "192.0.2.0/24") {
			continue
		}
		sawPlainExtra = true
		_, hasPorts := rule["ports"]
		assert.False(t, hasPorts,
			"extraEgressCIDRs entries must keep their all-ports semantics (documented escape hatch)")
	}
	assert.True(t, sawPlainExtra, "extraEgressCIDRs must render in allowlist mode")
}

func containsStr(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestEgress_Allowlist_InvalidModeFailsRender verifies a typo'd mode
// value fails the render loudly instead of silently falling back to
// the permissive public posture (fail-closed on operator error).
func TestEgress_Allowlist_InvalidModeFailsRender(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	values := "networkPolicy:\n  workspaceEgress:\n    mode: open-sesame\n"
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, writeFile(valuesPath, values))

	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns",
		"--kube-version", testKubeVersion, "-f", valuesPath,
		"--set", "controller.agentdDelivery.image="+testAgentdPin,
		"--set", "controller.opencodeDelivery.image="+testOpencodePin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	require.Error(t, err, "helm template must fail on an unknown workspaceEgress.mode")
	assert.Contains(t, stderr.String(), "workspaceEgress.mode",
		"error message must name the invalid value key")
}

// TestEgress_PublicMode_NarrowAllowedCIDRRendersWithoutExcept pins the
// fix for a pre-existing defect exposed while designing #821: the
// legacy general rule attached the shared blockedEgressCIDRs except:
// list to EVERY allowedEgressCIDRs entry. For any cidr other than
// 0.0.0.0/0 those except entries are not subnets of the cidr, so the
// API server rejects the whole NetworkPolicy — on a fresh install that
// leaves workspace pods with NO egress policy (fully open). The fix
// attaches the except list only to the 0.0.0.0/0 catch-all, where
// containment is guaranteed.
func TestEgress_PublicMode_NarrowAllowedCIDRRendersWithoutExcept(t *testing.T) {
	values := `
networkPolicy:
  allowedEgressCIDRs:
    - 203.0.113.0/24
`
	docs := helmTemplate(t, values)
	policy := findWorkspaceEgressPolicy(t, docs)

	var sawNarrow bool
	for _, rule := range egressRulesOf(t, policy) {
		toList, _ := rule["to"].([]any)
		for _, toAny := range toList {
			to, _ := toAny.(map[string]any)
			ipBlock, _ := to["ipBlock"].(map[string]any)
			if ipBlock == nil {
				continue
			}
			if ipBlock["cidr"] != "203.0.113.0/24" {
				continue
			}
			sawNarrow = true
			_, hasExcept := ipBlock["except"]
			assert.False(t, hasExcept,
				"narrow allowedEgressCIDRs entries must not carry the shared except list "+
					"(Kubernetes rejects except entries that are not subnets of cidr)")
		}
	}
	assert.True(t, sawNarrow, "custom allowedEgressCIDRs entry must render in public mode")
}
