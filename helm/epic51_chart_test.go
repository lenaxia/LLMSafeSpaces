// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Epic 51 — Helm template integration tests.
//
// These tests verify that the Epic 51 features (gVisor RuntimeClass +
// per-tenant quota webhook) render correctly when enabled and are absent
// when disabled. They catch rendering bugs that unit tests cannot —
// e.g. the objectSelector matchLabels bug (PR #317 review) where the
// webhook config was syntactically valid but semantically broken.
//
// The objectSelector test (TestS51_QuotaWebhook_ObjectSelector) is
// specifically the regression for that bug: it verifies the selector
// uses matchExpressions:Exists only, with no matchLabels that would
// require an empty-string value (which sanitizeLabelValue never produces).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// =============================================================================
// S51.1 — gVisor RuntimeClass
// =============================================================================

func TestS51_1_RuntimeClass_AbsentByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	rc := findByKind(docs, "RuntimeClass")
	require.Empty(t, rc,
		"RuntimeClass must NOT render by default (gvisor.enabled=false); got %d RuntimeClass docs", len(rc))
}

func TestS51_1_RuntimeClass_RendersWhenEnabled(t *testing.T) {
	docs := helmTemplate(t, "gvisor:\n  enabled: true\n")
	rcs := findByKind(docs, "RuntimeClass")
	require.Len(t, rcs, 1, "exactly one RuntimeClass must render when gvisor.enabled=true")

	rc := rcs[0]
	require.Equal(t, "gvisor", metaName(rc),
		"RuntimeClass name must be 'gvisor' (or defaultRuntimeClass)")

	handler, _ := rc["handler"].(string)
	require.Equal(t, "runsc", handler,
		"RuntimeClass handler must be 'runsc'")
}

func TestS51_1_RuntimeClass_CustomName(t *testing.T) {
	docs := helmTemplate(t, "gvisor:\n  enabled: true\n  defaultRuntimeClass: \"custom-gvisor\"\n")
	rcs := findByKind(docs, "RuntimeClass")
	require.Len(t, rcs, 1)
	require.Equal(t, "custom-gvisor", metaName(rcs[0]),
		"RuntimeClass name must respect defaultRuntimeClass override")
}

func TestS51_1_ControllerFlag_DefaultRuntimeClass(t *testing.T) {
	docs := helmTemplate(t, "gvisor:\n  enabled: true\n")
	args := findControllerArgs(t, docs)

	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--default-runtime-class=") {
			found = a
			break
		}
	}
	require.Equal(t, "--default-runtime-class=gvisor", found,
		"controller must receive --default-runtime-class=gvisor when gvisor.enabled=true")
}

func TestS51_1_ControllerFlag_AbsentWhenDisabled(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)

	for _, a := range args {
		require.False(t, strings.HasPrefix(a, "--default-runtime-class="),
			"--default-runtime-class must NOT render when gvisor.enabled=false")
	}
}

// =============================================================================
// S51.2 — Per-tenant quota webhook
// =============================================================================

func TestS51_2_QuotaWebhook_AbsentByDefault(t *testing.T) {
	docs := helmTemplate(t, "")

	for _, d := range docs {
		if d["kind"] != "ValidatingWebhookConfiguration" {
			continue
		}
		webhooks, _ := d["webhooks"].([]any)
		for _, w := range webhooks {
			wm, _ := w.(map[string]any)
			name, _ := wm["name"].(string)
			require.NotContains(t, name, "tenantquota",
				"quota webhook must NOT render by default (all limits=0)")
		}
	}
}

func TestS51_2_QuotaWebhook_RendersWhenConfigured(t *testing.T) {
	docs := helmTemplate(t, `webhooks:
  tenantQuota:
    maxWorkspacesPerTenant: 10
`)

	var found bool
	for _, d := range docs {
		if d["kind"] != "ValidatingWebhookConfiguration" {
			continue
		}
		webhooks, _ := d["webhooks"].([]any)
		for _, w := range webhooks {
			wm, _ := w.(map[string]any)
			name, _ := wm["name"].(string)
			if strings.Contains(name, "tenantquota") {
				found = true

				// Verify the webhook targets pods on CREATE.
				rules, _ := wm["rules"].([]any)
				require.NotEmpty(t, rules)
				rule, _ := rules[0].(map[string]any)
				resources, _ := rule["resources"].([]any)
				require.Contains(t, resources, "pods")
				ops, _ := rule["operations"].([]any)
				require.Contains(t, ops, "CREATE")

				// Verify the clientConfig path.
				cc, _ := wm["clientConfig"].(map[string]any)
				svc, _ := cc["service"].(map[string]any)
				path, _ := svc["path"].(string)
				require.Equal(t, "/validate-pod-tenant-quota", path)
			}
		}
	}
	require.True(t, found, "quota webhook must render when maxWorkspacesPerTenant > 0")
}

func TestS51_2_QuotaWebhook_ObjectSelector(t *testing.T) {
	// Regression for PR #317 review bug: matchLabels with "" value
	// made the webhook never fire on real pods. The selector must
	// use matchExpressions:Exists only.
	docs := helmTemplate(t, `webhooks:
  tenantQuota:
    maxWorkspacesPerTenant: 10
`)

	for _, d := range docs {
		if d["kind"] != "ValidatingWebhookConfiguration" {
			continue
		}
		webhooks, _ := d["webhooks"].([]any)
		for _, w := range webhooks {
			wm, _ := w.(map[string]any)
			name, _ := wm["name"].(string)
			if !strings.Contains(name, "tenantquota") {
				continue
			}

			objSel, _ := wm["objectSelector"].(map[string]any)

			// matchLabels must be absent or empty — it would require a
			// specific value, but sanitizeLabelValue never produces "".
			matchLabels, hasML := objSel["matchLabels"]
			if hasML {
				ml, _ := matchLabels.(map[string]any)
				require.Empty(t, ml,
					"objectSelector.matchLabels must be empty — a non-empty matchLabels "+
						"with \"\" value makes the webhook never fire (PR #317 bug)")
			}

			// matchExpressions must include Exists on the tenant label.
			matchExprs, _ := objSel["matchExpressions"].([]any)
			var sawExists bool
			for _, me := range matchExprs {
				mem, _ := me.(map[string]any)
				key, _ := mem["key"].(string)
				op, _ := mem["operator"].(string)
				if key == "llmsafespaces.dev/tenant" && op == "Exists" {
					sawExists = true
				}
			}
			require.True(t, sawExists,
				"objectSelector must have matchExpressions: "+
					"{key: llmsafespaces.dev/tenant, operator: Exists}")
			return
		}
	}
	t.Fatal("quota webhook not found in rendered output")
}

func TestS51_2_ControllerFlag_QuotaFlags(t *testing.T) {
	docs := helmTemplate(t, `webhooks:
  tenantQuota:
    maxWorkspacesPerTenant: 10
    maxCPUMillisPerTenant: 8000
    maxMemoryMiPerTenant: 16384
`)
	args := findControllerArgs(t, docs)

	var foundWS, foundCPU, foundMem bool
	for _, a := range args {
		switch a {
		case "--max-workspaces-per-tenant=10":
			foundWS = true
		case "--max-cpu-millis-per-tenant=8000":
			foundCPU = true
		case "--max-memory-mi-per-tenant=16384":
			foundMem = true
		}
	}
	require.True(t, foundWS, "--max-workspaces-per-tenant=10 must render")
	require.True(t, foundCPU, "--max-cpu-millis-per-tenant=8000 must render")
	require.True(t, foundMem, "--max-memory-mi-per-tenant=16384 must render")
}

func TestS51_2_ControllerFlag_QuotaFlagsAbsentByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)

	for _, a := range args {
		for _, flag := range []string{
			"--max-workspaces-per-tenant",
			"--max-cpu-millis-per-tenant",
			"--max-memory-mi-per-tenant",
		} {
			require.False(t, strings.HasPrefix(a, flag),
				"%s must NOT render when all quota limits are 0", flag)
		}
	}
}
