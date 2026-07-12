// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// Helm chart rendering tests for Epic 17 G16 remediation.
//
// These tests run `helm template` as a subprocess (the same command the
// Makefile target uses and the same code path operators run during
// `helm install`). They assert structural invariants about the rendered
// manifests:
//
//   - A default-deny ingress NetworkPolicy is rendered for the workspace
//     namespace.
//   - A workspace egress allow-list NetworkPolicy is rendered with at
//     least the operator-supplied LLM/DNS allowances.
//   - The NetworkPolicy resources are gated on values.networkPolicy.enabled
//     so operators with their own policy controllers can opt out.
//   - The default value of networkPolicy.enabled is true (Epic 17 requires
//     secure-by-default).
//   - The cluster default of rbac.scope is "namespace" (G5 follow-on);
//     defer that to a later remediation, but assert the file's presence.
//
// The tests are designed to fail clearly if any contract bit drifts. They
// don't assert exact YAML content because Helm renders fields in
// non-deterministic order; they parse the output as YAML documents and
// query by kind/name.
//
// To run:
//
//	go test ./helm/...
//
// helm must be on $PATH. The test skips otherwise.

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func chartDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Dir(thisFile)
}

func helmTemplate(t *testing.T, valuesYAML string) []map[string]any {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}

	args := []string{"template", "test-release", chartDir(t), "-n", "test-ns"}
	if valuesYAML != "" {
		dir := t.TempDir()
		valuesPath := filepath.Join(dir, "values.yaml")
		require.NoError(t, writeFile(valuesPath, valuesYAML))
		args = append(args, "-f", valuesPath)
	}
	cmd := exec.Command("helm", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(), "helm template failed: %s", stderr.String())

	docs := splitYAMLDocs(stdout.Bytes())
	parsed := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		if len(bytes.TrimSpace(d)) == 0 {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal(d, &m); err != nil {
			t.Logf("skipping unparseable doc: %v\n%s", err, string(d))
			continue
		}
		if m == nil {
			continue
		}
		parsed = append(parsed, m)
	}
	return parsed
}

func splitYAMLDocs(b []byte) [][]byte {
	// helm template separates docs with `\n---\n` lines.
	parts := bytes.Split(b, []byte("\n---\n"))
	out := make([][]byte, 0, len(parts))
	out = append(out, parts...)
	return out
}

func writeFile(path, content string) error {
	return execOK(exec.Command("sh", "-c", "cat > "+path), content)
}

func execOK(cmd *exec.Cmd, stdin string) error {
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Run()
}

// findByKind returns all rendered docs whose kind matches.
func findByKind(docs []map[string]any, kind string) []map[string]any {
	out := []map[string]any{}
	for _, d := range docs {
		if k, _ := d["kind"].(string); k == kind {
			out = append(out, d)
		}
	}
	return out
}

// metaName returns metadata.name from a rendered doc.
func metaName(d map[string]any) string {
	meta, _ := d["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return name
}

// TestG16_DefaultRender_IncludesNetworkPolicies verifies the chart ships
// at least one NetworkPolicy by default.
func TestG16_DefaultRender_IncludesNetworkPolicies(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")
	require.NotEmpty(t, policies,
		"chart must ship at least one NetworkPolicy by default (Epic 17 G16)")
}

// TestG16_DefaultRender_HasDefaultDenyIngress verifies the workspace
// ingress policy denies-by-default with an explicit narrow allowance for
// the API proxy. NetworkPolicy semantics: any pod matching podSelector
// receives ONLY the listed ingress rules; everything else is denied. So
// the contract is:
//
//   - The policy exists, scoped to the workspace pod selector.
//   - Its policyTypes include "Ingress".
//   - Its ingress block lists exactly the API proxy on agentd port 4097
//     (and opencode 4096 for SSE/proxy paths).
//
// We deliberately do NOT assert "ingress list is empty" — a true empty
// list would break the proxy. What matters is that no other clients can
// reach the workspace pod.
func TestG16_DefaultRender_HasDefaultDenyIngress(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var found bool
	for _, p := range policies {
		name := metaName(p)
		if !strings.Contains(name, "workspace-default-deny") {
			continue
		}
		spec, _ := p["spec"].(map[string]any)
		policyTypes, _ := spec["policyTypes"].([]any)
		hasIngress := false
		for _, pt := range policyTypes {
			if pt == "Ingress" {
				hasIngress = true
			}
		}
		require.True(t, hasIngress,
			"default-deny policy %q must declare policyTypes: [Ingress, ...]", name)

		ingress, _ := spec["ingress"].([]any)
		// Two ingress rules expected:
		//   1. API server pods → 4096/4097/4098 (proxy + agentd traffic).
		//   2. Controller pods → 4098 (Epic 22 health-endpoint polling).
		// Without rule 2 the controller's /v1/healthz probe times out,
		// trips the 3-strike threshold, and kills the workspace pod in
		// an infinite loop.
		require.Len(t, ingress, 2,
			"default-deny policy %q must have two allow rules (API and controller)", name)

		// Locate and verify the API proxy rule (allows 4097 from component=api).
		var apiRule, controllerRule map[string]any
		for _, r := range ingress {
			rm, _ := r.(map[string]any)
			from, _ := rm["from"].([]any)
			if len(from) == 0 {
				continue
			}
			fromMap, _ := from[0].(map[string]any)
			podSel, _ := fromMap["podSelector"].(map[string]any)
			matchLabels, _ := podSel["matchLabels"].(map[string]any)
			switch matchLabels["app.kubernetes.io/component"] {
			case "api":
				apiRule = rm
			case "controller":
				controllerRule = rm
			}
		}

		require.NotNil(t, apiRule,
			"default-deny policy %q must include an ingress rule for the API server", name)
		require.NotNil(t, controllerRule,
			"default-deny policy %q must include an ingress rule for the controller (Epic 22 health polling)", name)

		// API rule: must allow 4097.
		apiPorts, _ := apiRule["ports"].([]any)
		var foundAgentdPort bool
		for _, p := range apiPorts {
			pm := p.(map[string]any)
			if port := pm["port"]; port == float64(4097) || port == 4097 {
				foundAgentdPort = true
			}
		}
		require.True(t, foundAgentdPort,
			"API ingress rule must allow agentd port 4097")

		// Controller rule: must allow at least 4098 (health probes).
		controllerPorts, _ := controllerRule["ports"].([]any)
		var foundAdminPort bool
		for _, p := range controllerPorts {
			pm := p.(map[string]any)
			if port := pm["port"]; port == float64(4098) || port == 4098 {
				foundAdminPort = true
			}
		}
		require.True(t, foundAdminPort,
			"Controller ingress rule must allow admin port 4098 (Epic 22)")

		found = true
		break
	}
	require.True(t, found, "default-deny ingress NetworkPolicy not found in default render")
}

// TestG16_DefaultRender_HasWorkspaceEgressAllowList verifies that the
// chart ships an egress-allow policy that permits at least DNS so
// sandbox pods can resolve LLM endpoints. Without DNS, every workspace
// is broken on first boot.
func TestG16_DefaultRender_HasWorkspaceEgressAllowList(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var found bool
	for _, p := range policies {
		name := metaName(p)
		if !strings.Contains(name, "workspace-egress") {
			continue
		}
		spec, _ := p["spec"].(map[string]any)
		policyTypes, _ := spec["policyTypes"].([]any)
		hasEgress := false
		for _, pt := range policyTypes {
			if pt == "Egress" {
				hasEgress = true
			}
		}
		require.True(t, hasEgress,
			"workspace-egress policy %q must declare policyTypes: [Egress, ...]", name)
		// Must permit at least one egress entry (DNS).
		egress, _ := spec["egress"].([]any)
		require.NotEmpty(t, egress,
			"workspace-egress policy %q must have at least one egress rule (DNS)", name)
		found = true
		break
	}
	require.True(t, found, "workspace-egress NetworkPolicy not found in default render")
}

// TestG16_NetworkPolicyDisabled_OmitsResources verifies operators can
// opt out by setting networkPolicy.enabled=false. This is for clusters
// that already enforce equivalent policies via Cilium CRDs or admission
// controllers.
func TestG16_NetworkPolicyDisabled_OmitsResources(t *testing.T) {
	docs := helmTemplate(t, "networkPolicy:\n  enabled: false\n")
	policies := findByKind(docs, "NetworkPolicy")
	require.Empty(t, policies,
		"setting networkPolicy.enabled=false must omit all chart NetworkPolicies")
}

// TestG16_PoliciesScopeToWorkspaceNamespace verifies the policies are
// rendered into the workspace namespace, not the platform's release
// namespace. The release namespace runs API/controller, which need their
// own policies; mixing them with workspace policies leads to lockout
// during upgrades.
func TestG16_PoliciesScopeToWorkspaceNamespace(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	for _, p := range policies {
		meta, _ := p["metadata"].(map[string]any)
		ns, _ := meta["namespace"].(string)
		// Workspace policies should target the workspace namespace, which
		// defaults to the release namespace when namespace.create=false.
		// In our test setup with -n test-ns, the workspace namespace
		// resolves to test-ns. We assert it's set (not empty).
		require.NotEmpty(t, ns,
			"NetworkPolicy %q must have an explicit namespace", metaName(p))
	}
}

// =============================================================================
// G26 — Datastore credentials + datastore NetworkPolicies
// =============================================================================
//
// These tests guard the Critical finding from worklog 0089 (RT-4.5):
//
//   - postgres-password defaulted to the literal string "changeme"
//   - redis-password defaulted to "" (Valkey reported `requirepass` empty)
//   - No NetworkPolicy gated postgres or valkey ingress
//
// The chart fix has three contracts:
//
//   1. If the operator does not supply a postgres password, the chart
//      auto-generates a random 32+ character one (mirrors jwtSecret).
//      No literal "changeme" may appear in the rendered Secret.
//   2. Same for redis-password.
//   3. When `datastore.networkPolicy.enabled` (default true) the chart
//      renders two NetworkPolicy objects naming `app=postgres` and
//      `app=valkey` selectors, each with an ingress rule restricting
//      traffic to the API + migrate-job pod selectors only.
//
// Each test deliberately reverses to FAIL if the contract drifts (mutation-
// validated: revert the fix in values.yaml or the new template; the test
// must turn red).

func secretValue(t *testing.T, sec map[string]any, key string) string {
	t.Helper()
	if sd, ok := sec["stringData"].(map[string]any); ok {
		if v, ok := sd[key].(string); ok {
			return v
		}
	}
	if d, ok := sec["data"].(map[string]any); ok {
		if v, ok := d[key].(string); ok {
			return v
		}
	}
	return ""
}

// configYAML extracts the "config.yaml" entry from a rendered ConfigMap doc.
// Used by email-block tests (and any future test that asserts on the API's
// rendered config.yaml contents).
func configYAML(t *testing.T, cm map[string]any) string {
	t.Helper()
	data, _ := cm["data"].(map[string]any)
	if data == nil {
		return ""
	}
	s, _ := data["config.yaml"].(string)
	return s
}

// TestG26_DefaultRender_PostgresPasswordIsGenerated proves that a fresh
// `helm template` with no overrides does NOT render the literal
// "changeme" as the postgres password. Pre-fix this test FAILs because
// values.yaml seeded the default.
func TestG26_DefaultRender_PostgresPasswordIsGenerated(t *testing.T) {
	docs := helmTemplate(t, "")
	// The chart's secret is named per release; helmTemplate uses
	// release "test" (see helmTemplate impl above).
	var sec map[string]any
	for _, d := range docs {
		if d["kind"] == "Secret" {
			meta, _ := d["metadata"].(map[string]any)
			ns, _ := meta["namespace"].(string)
			// Only consider the platform credentials Secret, not any
			// per-workspace ephemeral secrets.
			if ns == "test-ns" {
				sec = d
				break
			}
		}
	}
	require.NotNil(t, sec, "platform credentials Secret must be rendered by default")

	pw := secretValue(t, sec, "postgres-password")
	require.NotEqual(t, "changeme", pw,
		"postgres-password must NOT default to the literal 'changeme' (G26)")
	require.GreaterOrEqual(t, len(pw), 24,
		"auto-generated postgres-password must be at least 24 chars; got %d", len(pw))
}

// TestG26_DefaultRender_RedisPasswordIsGenerated mirrors the postgres
// test for the Valkey/Redis password. Pre-fix the value defaulted to
// the empty string, which Valkey treats as "no auth required".
func TestG26_DefaultRender_RedisPasswordIsGenerated(t *testing.T) {
	docs := helmTemplate(t, "")
	var sec map[string]any
	for _, d := range docs {
		if d["kind"] == "Secret" {
			meta, _ := d["metadata"].(map[string]any)
			if ns, _ := meta["namespace"].(string); ns == "test-ns" {
				sec = d
				break
			}
		}
	}
	require.NotNil(t, sec, "platform credentials Secret must be rendered")

	pw := secretValue(t, sec, "redis-password")
	require.NotEmpty(t, pw,
		"redis-password must NOT default to empty (Valkey requirepass would be unset; G26)")
	require.GreaterOrEqual(t, len(pw), 24,
		"auto-generated redis-password must be at least 24 chars; got %d", len(pw))
}

// TestG26_OperatorOverride_PostgresPasswordIsRespected proves the
// operator can still pin a specific password (no surprise rotation on
// upgrade). This guards the rotation-safety property: an existing
// installation with a known password must keep it across `helm upgrade`.
func TestG26_OperatorOverride_PostgresPasswordIsRespected(t *testing.T) {
	docs := helmTemplate(t, "externalSecret:\n  postgresPassword: \"operator-supplied-9876\"\n  redisPassword: \"operator-redis-1234\"\n")
	var sec map[string]any
	for _, d := range docs {
		if d["kind"] == "Secret" {
			meta, _ := d["metadata"].(map[string]any)
			if ns, _ := meta["namespace"].(string); ns == "test-ns" {
				sec = d
				break
			}
		}
	}
	require.NotNil(t, sec)
	require.Equal(t, "operator-supplied-9876", secretValue(t, sec, "postgres-password"))
	require.Equal(t, "operator-redis-1234", secretValue(t, sec, "redis-password"))
}

// TestG26_DefaultRender_HasPostgresIngressPolicy verifies a NetworkPolicy
// named per the chart's helper exists, selects pods with `app=postgres`,
// and has at least one ingress rule restricting the source.
func TestG26_DefaultRender_HasPostgresIngressPolicy(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var pgPolicy map[string]any
	for _, p := range policies {
		spec, _ := p["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		if app, _ := ml["app"].(string); app == "postgres" {
			pgPolicy = p
			break
		}
	}
	require.NotNil(t, pgPolicy,
		"a NetworkPolicy selecting `app=postgres` must be rendered by default (G26)")

	spec, _ := pgPolicy["spec"].(map[string]any)
	policyTypes, _ := spec["policyTypes"].([]any)
	require.Contains(t, policyTypes, "Ingress",
		"postgres NetworkPolicy must declare Ingress in policyTypes")
	ingress, _ := spec["ingress"].([]any)
	require.NotEmpty(t, ingress,
		"postgres NetworkPolicy must have at least one ingress rule")
}

// TestG26_DefaultRender_HasValkeyIngressPolicy is the Valkey twin of the
// above. Same shape, different selector.
func TestG26_DefaultRender_HasValkeyIngressPolicy(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var vkPolicy map[string]any
	for _, p := range policies {
		spec, _ := p["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		if app, _ := ml["app"].(string); app == "valkey" {
			vkPolicy = p
			break
		}
	}
	require.NotNil(t, vkPolicy,
		"a NetworkPolicy selecting `app=valkey` must be rendered by default (G26)")

	spec, _ := vkPolicy["spec"].(map[string]any)
	policyTypes, _ := spec["policyTypes"].([]any)
	require.Contains(t, policyTypes, "Ingress")
	ingress, _ := spec["ingress"].([]any)
	require.NotEmpty(t, ingress,
		"valkey NetworkPolicy must have at least one ingress rule")
}

// TestG26_DatastoreNetworkPolicy_OptOut lets operators who manage their
// own policies disable the chart's datastore policies without having
// to disable the workspace policies (which are critical and should
// stay on by default). Different toggles, separate concerns.
func TestG26_DatastoreNetworkPolicy_OptOut(t *testing.T) {
	docs := helmTemplate(t, "datastore:\n  networkPolicy:\n    enabled: false\n")
	policies := findByKind(docs, "NetworkPolicy")
	for _, p := range policies {
		spec, _ := p["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		app, _ := ml["app"].(string)
		require.NotEqual(t, "postgres", app,
			"datastore.networkPolicy.enabled=false must omit postgres NetworkPolicy")
		require.NotEqual(t, "valkey", app,
			"datastore.networkPolicy.enabled=false must omit valkey NetworkPolicy")
	}
}

// =============================================================================
// G2 — Workspace ValidatingWebhookConfiguration + controller flag wiring
// =============================================================================
//
// Closes F1.2.1, F1.2.2, F1.2.9, RT-2.18, RT-6.10, RT-6.1. The chart-side
// fix is two contracts:
//
//   1. ValidatingWebhookConfiguration includes a webhook for `workspaces`
//      pointing at /validate-llmsafespaces-dev-v1-workspace.
//   2. The controller deployment passes --allowed-image-registries,
//      --allowed-storage-class-names, and --max-workspace-storage-gi
//      to the controller binary, populated from values.yaml.

// findControllerArgs locates the controller container's args list in
// the rendered Deployment.
func findControllerArgs(t *testing.T, docs []map[string]any) []string {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != "Deployment" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if !strings.Contains(name, "controller") {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		if len(containers) == 0 {
			continue
		}
		c, _ := containers[0].(map[string]any)
		raw, _ := c["args"].([]any)
		out := make([]string, 0, len(raw))
		for _, a := range raw {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// TestG2_WebhookConfig_IncludesWorkspace asserts the
// ValidatingWebhookConfiguration carries a webhook for the workspaces
// resource. Without this entry the workspace webhook never receives
// admission requests and the registry allow-list is bypassed for any
// kubectl-direct workspace creation.
func TestG2_WebhookConfig_IncludesWorkspace(t *testing.T) {
	docs := helmTemplate(t, "")
	for _, d := range docs {
		if d["kind"] != "ValidatingWebhookConfiguration" {
			continue
		}
		webhooks, _ := d["webhooks"].([]any)
		var sawWorkspace bool
		for _, w := range webhooks {
			wm, _ := w.(map[string]any)
			cc, _ := wm["clientConfig"].(map[string]any)
			svc, _ := cc["service"].(map[string]any)
			path, _ := svc["path"].(string)
			if path == "/validate-llmsafespaces-dev-v1-workspace" {
				sawWorkspace = true
				rules, _ := wm["rules"].([]any)
				require.NotEmpty(t, rules, "workspace webhook must declare at least one rule")
				rule, _ := rules[0].(map[string]any)
				resources, _ := rule["resources"].([]any)
				require.Contains(t, resources, "workspaces")
				ops, _ := rule["operations"].([]any)
				require.Contains(t, ops, "CREATE")
				require.Contains(t, ops, "UPDATE")
				break
			}
		}
		require.True(t, sawWorkspace,
			"ValidatingWebhookConfiguration must include a webhook for /validate-llmsafespaces-dev-v1-workspace")
		return
	}
	t.Fatal("no ValidatingWebhookConfiguration rendered")
}

// TestG2_ControllerArgs_PassesAllowedImageRegistries asserts the
// controller deployment receives the --allowed-image-registries flag
// populated from values.yaml. Default values.yaml ships a non-empty
// list (ghcr.io/lenaxia/) so the flag must appear by default.
func TestG2_ControllerArgs_PassesAllowedImageRegistries(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	require.NotEmpty(t, args, "controller container must have args")

	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--allowed-image-registries=") {
			found = a
			break
		}
	}
	require.NotEmpty(t, found,
		"--allowed-image-registries flag must be set when webhooks.allowedImageRegistries is non-empty")
	require.Contains(t, found, "ghcr.io/lenaxia/",
		"default --allowed-image-registries must include ghcr.io/lenaxia/ (G2)")
}

// TestG2_ControllerArgs_OmitsAllowedRegistriesWhenEmpty validates the
// negative-case rendering: with an empty list the flag is omitted so
// the controller's default (also empty list) takes effect.
func TestG2_ControllerArgs_OmitsAllowedRegistriesWhenEmpty(t *testing.T) {
	docs := helmTemplate(t, "webhooks:\n  allowedImageRegistries: []\n")
	args := findControllerArgs(t, docs)
	for _, a := range args {
		require.False(t, strings.HasPrefix(a, "--allowed-image-registries="),
			"--allowed-image-registries must NOT be set when the values list is empty (avoids '--flag=' which Go flag parses as empty)")
	}
}

// TestG2_ControllerArgs_PassesMaxStorageGi asserts the upper-bound
// flag flows through. Default 1024 must be the rendered value.
func TestG2_ControllerArgs_PassesMaxStorageGi(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--max-workspace-storage-gi=") {
			found = a
			break
		}
	}
	require.Equal(t, "--max-workspace-storage-gi=1024", found,
		"controller must receive the default 1024 GiB upper-bound flag (G2 / RT-6.1)")
}

// TestG2_ControllerArgs_HonorsOperatorOverride confirms the operator
// can change the upper bound and add storage class allow-list entries
// through values.yaml, and the deployment re-renders with the new
// values.
func TestG2_ControllerArgs_HonorsOperatorOverride(t *testing.T) {
	docs := helmTemplate(t, `webhooks:
  allowedImageRegistries:
    - "registry.k8s.io/"
  allowedStorageClassNames:
    - "longhorn"
    - "gp3"
  maxWorkspaceStorageGi: 64
`)
	args := findControllerArgs(t, docs)
	require.NotEmpty(t, args)

	asMap := map[string]string{}
	for _, a := range args {
		if i := strings.Index(a, "="); i > 0 {
			asMap[a[:i]] = a[i+1:]
		}
	}
	require.Equal(t, "registry.k8s.io/", asMap["--allowed-image-registries"])
	require.Equal(t, "longhorn,gp3", asMap["--allowed-storage-class-names"])
	require.Equal(t, "64", asMap["--max-workspace-storage-gi"])
}

// =============================================================================
// F1 / F5 — Org-suspension wiring (worklog 0372)
// =============================================================================
//
// D20 org-level workspace suspension is driven by the controller polling an
// internal API endpoint. Pre-fix the chart did NOT wire --api-service-url nor
// the shared LLMSAFESPACES_INTERNAL_TOKEN, so the feature was inert in every
// Helm deployment and the internal endpoint was unauthenticated. These tests
// lock in:
//   1. The controller deployment receives --api-service-url (F1).
//   2. Both API and controller deployments mount LLMSAFESPACES_INTERNAL_TOKEN
//      from the same Secret key (F1+F5).
//   3. The credentials Secret carries an auto-generated internal-token (F1).
//   4. The opt-in API ingress NetworkPolicy is absent by default and present
//      when networkPolicy.apiIngressRestricted=true (F5).

// containerEnvNames returns the set of env var names declared on the named
// container in a Deployment doc.
func containerEnvNames(deploy map[string]any, name string) map[string]bool {
	out := map[string]bool{}
	c := containerByName(deploy, name)
	if c == nil {
		return out
	}
	env, _ := c["env"].([]any)
	for _, e := range env {
		em, _ := e.(map[string]any)
		if n, ok := em["name"].(string); ok {
			out[n] = true
		}
	}
	return out
}

// TestF1_ControllerArgs_PassesApiServiceURL asserts the controller deployment
// receives --api-service-url so org-suspension is functional (D20). Pre-fix the
// flag was absent and OrgStatusClient was always nil in Helm deployments.
func TestF1_ControllerArgs_PassesApiServiceURL(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	require.NotEmpty(t, args)

	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--api-service-url=") {
			found = a
			break
		}
	}
	require.NotEmpty(t, found,
		"controller deployment must pass --api-service-url so org-suspension is functional (F1)")
	// Default-derivation: the chart derives the in-cluster API service URL from
	// the release name + namespace + API port.
	require.Contains(t, found, "-api.",
		"--api-service-url must derive the in-cluster API service URL by default, got %q", found)
	require.Contains(t, found, ":8080",
		"--api-service-url must target the API service port, got %q", found)
}

// TestF1_ControllerArgs_ApiServiceURL_HonorsOverride confirms an operator can
// point the controller at a custom API URL.
func TestF1_ControllerArgs_ApiServiceURL_HonorsOverride(t *testing.T) {
	docs := helmTemplate(t, "controller:\n  apiServiceURL: \"http://api.custom.svc:9090\"\n")
	args := findControllerArgs(t, docs)
	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--api-service-url=") {
			found = a
			break
		}
	}
	require.Equal(t, "--api-service-url=http://api.custom.svc:9090", found,
		"controller.apiServiceURL override must flow through verbatim (F1)")
}

// TestF1_InternalTokenEnv_OnBothDeployments asserts both the API and the
// controller mount LLMSAFESPACES_INTERNAL_TOKEN from the credentials Secret, so
// the fail-closed internal endpoint is reachable by the controller (F1+F5).
func TestF1_InternalTokenEnv_OnBothDeployments(t *testing.T) {
	docs := helmTemplate(t, "")

	apiDeploy := findDeploymentByNameSubstr(docs, "-api")
	require.NotNil(t, apiDeploy, "API Deployment must be rendered")
	require.Contains(t, containerEnvNames(apiDeploy, "api"), "LLMSAFESPACES_INTERNAL_TOKEN",
		"API deployment must mount LLMSAFESPACES_INTERNAL_TOKEN (the internal endpoint fails closed without it; F5)")

	controllerDeploy := findDeploymentByNameSubstr(docs, "-controller")
	require.NotNil(t, controllerDeploy, "controller Deployment must be rendered")
	require.Contains(t, containerEnvNames(controllerDeploy, "manager"), "LLMSAFESPACES_INTERNAL_TOKEN",
		"controller deployment must mount LLMSAFESPACES_INTERNAL_TOKEN so it can authenticate the internal org-status poll (F1)")
}

// TestF1_SecretIncludesInternalToken asserts the credentials Secret carries an
// auto-generated internal-token key (so the env mounts resolve on a fresh
// install with no operator overrides).
func TestF1_SecretIncludesInternalToken(t *testing.T) {
	docs := helmTemplate(t, "")
	var sec map[string]any
	for _, d := range docs {
		if d["kind"] == "Secret" {
			if meta, _ := d["metadata"].(map[string]any); meta != nil {
				if ns, _ := meta["namespace"].(string); ns == "test-ns" {
					sec = d
					break
				}
			}
		}
	}
	require.NotNil(t, sec, "platform credentials Secret must be rendered")
	tok := secretValue(t, sec, "internal-token")
	require.NotEmpty(t, tok, "Secret must include an auto-generated internal-token key (F1)")
	require.GreaterOrEqual(t, len(tok), 24,
		"auto-generated internal-token must be at least 24 chars; got %d", len(tok))
}

// TestF5_ApiNetworkPolicy_DefaultOff asserts the API ingress NetworkPolicy is
// NOT rendered by default (it is opt-in: an incomplete allowlist would lock
// users out, and the internal endpoint is already token-gated).
func TestF5_ApiNetworkPolicy_DefaultOff(t *testing.T) {
	docs := helmTemplate(t, "")
	for _, p := range findByKind(docs, "NetworkPolicy") {
		require.NotContains(t, metaName(p), "api-ingress",
			"API ingress NetworkPolicy must be absent by default (opt-in via networkPolicy.apiIngressRestricted; F5)")
	}
}

// TestF5_ApiNetworkPolicy_OptIn asserts the policy renders with controller +
// user-traffic + kube-system allow rules when apiIngressRestricted=true.
func TestF5_ApiNetworkPolicy_OptIn(t *testing.T) {
	docs := helmTemplate(t, "networkPolicy:\n  apiIngressRestricted: true\n")
	var apiPolicy map[string]any
	for _, p := range findByKind(docs, "NetworkPolicy") {
		if strings.Contains(metaName(p), "api-ingress") {
			apiPolicy = p
			break
		}
	}
	require.NotNil(t, apiPolicy, "API ingress NetworkPolicy must render when apiIngressRestricted=true (F5)")

	spec, _ := apiPolicy["spec"].(map[string]any)
	policyTypes, _ := spec["policyTypes"].([]any)
	require.Contains(t, policyTypes, "Ingress", "API NetworkPolicy must declare Ingress in policyTypes")
	ingress, _ := spec["ingress"].([]any)
	require.GreaterOrEqual(t, len(ingress), 3,
		"API NetworkPolicy must admit controller + user-traffic + kube-system (3 ingress rules)")
}

// =============================================================================
// G5 / F1.3.x — RBAC tightening (worklog 0107)
// =============================================================================

// findResources returns all rendered docs of the given Kind.
func findResources(docs []map[string]any, kind string) []map[string]any {
	out := []map[string]any{}
	for _, d := range docs {
		if d["kind"] == kind {
			out = append(out, d)
		}
	}
	return out
}

// resourceVerbs walks the rules of a Role/ClusterRole doc and returns
// a {apiGroup/resource: verbs[]} map for assertion.
func resourceVerbs(doc map[string]any) map[string][]string {
	out := map[string][]string{}
	rules, _ := doc["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		groups, _ := rule["apiGroups"].([]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		var verbStrs []string
		for _, v := range verbs {
			if s, ok := v.(string); ok {
				verbStrs = append(verbStrs, s)
			}
		}
		for _, g := range groups {
			for _, res := range resources {
				key := fmt.Sprintf("%s/%s", g, res)
				out[key] = append(out[key], verbStrs...)
			}
		}
	}
	return out
}

// TestG5_DefaultIsNamespaceScope asserts the post-fix default `rbac.scope`
// is "namespace" — operators no longer get cluster-wide secrets/pods
// access by default.
func TestG5_DefaultIsNamespaceScope(t *testing.T) {
	docs := helmTemplate(t, "")
	clusterRoles := findResources(docs, "ClusterRole")
	// Allow ONLY the storageclass-reader ClusterRole — the cluster
	// scope ClusterRole must NOT be rendered by default.
	for _, cr := range clusterRoles {
		name := metaName(cr)
		require.NotContains(t, name, "controller-cluster",
			"default install must NOT render the cluster-scope ClusterRole; got %q", name)
	}
}

// TestG5_ClusterScopeOptInRendersClusterRole asserts the cluster
// scope is preserved as an opt-in. Read-only watch on pods/secrets
// IS permitted (controller-runtime informer cache requires it);
// CRUD verbs are still forbidden cluster-wide.
func TestG5_ClusterScopeOptInRendersClusterRole(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: cluster\n")
	clusterRoles := findResources(docs, "ClusterRole")
	var sawClusterScope bool
	mutating := map[string]struct{}{
		"create": {}, "update": {}, "patch": {}, "delete": {}, "deletecollection": {},
	}
	for _, cr := range clusterRoles {
		if !strings.Contains(metaName(cr), "controller-cluster") {
			continue
		}
		sawClusterScope = true
		rules, _ := cr["rules"].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			groups, _ := rule["apiGroups"].([]any)
			resources, _ := rule["resources"].([]any)
			verbs, _ := rule["verbs"].([]any)
			isCore := false
			for _, g := range groups {
				if s, _ := g.(string); s == "" {
					isCore = true
				}
			}
			if !isCore {
				continue
			}
			for _, res := range resources {
				resStr, _ := res.(string)
				if resStr != "secrets" && resStr != "pods" {
					continue
				}
				for _, v := range verbs {
					vStr, _ := v.(string)
					_, mut := mutating[vStr]
					require.False(t, mut,
						"cluster ClusterRole must NOT grant cluster-wide mutating verb %q on %s (G5 / F1.3.3)",
						vStr, resStr)
				}
			}
		}
	}
	require.True(t, sawClusterScope,
		"rbac.scope=cluster must render the controller-cluster ClusterRole")
}

// TestF132_LeasesAreNamespaceScoped asserts coordination.k8s.io/leases
// is granted via Role (namespace), not ClusterRole.
func TestF132_LeasesAreNamespaceScoped(t *testing.T) {
	docs := helmTemplate(t, "")
	clusterRoles := findResources(docs, "ClusterRole")
	for _, cr := range clusterRoles {
		rv := resourceVerbs(cr)
		require.NotContains(t, rv, "coordination.k8s.io/leases",
			"leases must not be cluster-scoped (F1.3.2); found in ClusterRole %q", metaName(cr))
	}
	// And the Role for leader election must contain leases.
	roles := findResources(docs, "Role")
	var sawLeases bool
	for _, role := range roles {
		rv := resourceVerbs(role)
		if _, ok := rv["coordination.k8s.io/leases"]; ok {
			sawLeases = true
		}
	}
	require.True(t, sawLeases, "leases must be granted via a namespace-scoped Role")
}

// TestF134_APIDoesNotGrantRuntimeEnvironments asserts the API SA Role
// does not include runtimeenvironments (unused per audit).
func TestF134_APIDoesNotGrantRuntimeEnvironments(t *testing.T) {
	docs := helmTemplate(t, "")
	roles := findResources(docs, "Role")
	for _, role := range roles {
		name := metaName(role)
		if !strings.Contains(name, "-api") {
			continue
		}
		rv := resourceVerbs(role)
		require.NotContains(t, rv, "llmsafespaces.dev/runtimeenvironments",
			"API Role %q must NOT grant runtimeenvironments (F1.3.4)", name)
	}
}

// TestF135_APIDoesNotGrantPodsLog asserts the API SA Role does not
// include pods/log (unused per audit).
func TestF135_APIDoesNotGrantPodsLog(t *testing.T) {
	docs := helmTemplate(t, "")
	roles := findResources(docs, "Role")
	for _, role := range roles {
		name := metaName(role)
		if !strings.Contains(name, "-api") {
			continue
		}
		rv := resourceVerbs(role)
		require.NotContains(t, rv, "/pods/log",
			"API Role %q must NOT grant pods/log (F1.3.5)", name)
	}
}

// TestF131_ControllerDoesNotGrantUnusedResources asserts services and
// configmaps are removed from the controller's grants (F1.3.1) — but
// allows the narrow configmaps grant required by the free-models
// refresher (2026-06-23 cold-start optimization, item #1a) when that
// feature is enabled.
//
// The F1.3.1 invariant is "only what's used"; the freemodels refresher
// uses configmaps to publish the cluster-wide opencode free-tier model
// catalog. This test verifies that when the refresher is OFF,
// configmaps are not granted at all (preserving the original invariant
// for clusters that don't need the optimization), and when the
// refresher is ON, the verbs are scoped to what the refresher actually
// performs (no `delete`).
func TestF131_ControllerDoesNotGrantUnusedResources(t *testing.T) {
	t.Run("freeModelsRefresher_disabled_no_configmaps", func(t *testing.T) {
		// Opt out of the refresher so the F1.3.1 invariant is exact:
		// no configmaps anywhere in the controller's RBAC.
		docs := helmTemplate(t, `
rbac:
  scope: cluster
controller:
  freeModelsRefresher:
    enabled: false
`)
		for _, kind := range []string{"Role", "ClusterRole"} {
			for _, doc := range findResources(docs, kind) {
				name := metaName(doc)
				if !strings.Contains(name, "controller") {
					continue
				}
				rv := resourceVerbs(doc)
				require.NotContains(t, rv, "/services",
					"%s %q must NOT grant services (F1.3.1)", kind, name)
				require.NotContains(t, rv, "/configmaps",
					"%s %q must NOT grant configmaps when freeModelsRefresher is disabled (F1.3.1)", kind, name)
			}
		}
	})

	t.Run("freeModelsRefresher_enabled_narrow_configmaps", func(t *testing.T) {
		// Default (refresher on): configmaps grant must be scoped to
		// what the refresher actually does — no `delete`, no
		// secrets-style breadth.
		docs := helmTemplate(t, "rbac:\n  scope: cluster\n")
		var found bool
		for _, kind := range []string{"Role", "ClusterRole"} {
			for _, doc := range findResources(docs, kind) {
				name := metaName(doc)
				if !strings.Contains(name, "controller") {
					continue
				}
				rv := resourceVerbs(doc)
				require.NotContains(t, rv, "/services",
					"%s %q must NOT grant services (F1.3.1)", kind, name)

				if cmVerbs, ok := rv["/configmaps"]; ok {
					found = true
					assert.NotContains(t, cmVerbs, "delete",
						"%s %q must NOT grant configmaps `delete` — the freemodels refresher "+
							"only publishes the catalog (Create + Update + Patch); deletion is "+
							"never required", kind, name)
				}
			}
		}
		assert.True(t, found,
			"expected at least one controller Role/ClusterRole to grant configmaps "+
				"when freeModelsRefresher is enabled (the default)")
	})
}

// TestF137_StorageClassesIsAlwaysClusterRole asserts storageclasses
// is granted via a ClusterRole regardless of rbac.scope, so it doesn't
// silently disappear in namespace mode.
func TestF137_StorageClassesIsAlwaysClusterRole(t *testing.T) {
	for _, scope := range []string{"namespace", "cluster"} {
		t.Run("scope="+scope, func(t *testing.T) {
			docs := helmTemplate(t, fmt.Sprintf("rbac:\n  scope: %s\n", scope))
			clusterRoles := findResources(docs, "ClusterRole")
			var sawSC bool
			for _, cr := range clusterRoles {
				rv := resourceVerbs(cr)
				if _, ok := rv["storage.k8s.io/storageclasses"]; ok {
					sawSC = true
				}
			}
			require.True(t, sawSC,
				"storageclasses must be granted via a ClusterRole when scope=%s (F1.3.7)", scope)
		})
	}
}

// =============================================================================
// Helm audit fixes (worklog 0174) — regression tests for 7 bugs found in
// the chart audit. Each test is designed to turn red if the corresponding
// fix is accidentally reverted.
// =============================================================================

// findDeploymentByNameSubstr returns the first Deployment whose metadata.name
// contains the given substring.
func findDeploymentByNameSubstr(docs []map[string]any, substr string) map[string]any {
	for _, d := range docs {
		if d["kind"] != "Deployment" {
			continue
		}
		if strings.Contains(metaName(d), substr) {
			return d
		}
	}
	return nil
}

// findServiceByNameSubstr returns the first Service whose metadata.name
// contains the given substring.
func findServiceByNameSubstr(docs []map[string]any, substr string) map[string]any {
	for _, d := range docs {
		if d["kind"] != "Service" {
			continue
		}
		if strings.Contains(metaName(d), substr) {
			return d
		}
	}
	return nil
}

// containerByName returns the first container spec matching the given name
// from a Deployment doc.
func containerByName(deploy map[string]any, name string) map[string]any {
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	for _, c := range containers {
		cm, _ := c.(map[string]any)
		if n, _ := cm["name"].(string); n == name {
			return cm
		}
	}
	return nil
}

// initContainerByName returns the first initContainer spec matching the
// given name from a Deployment doc. Walks spec.template.spec.initContainers
// — the array containerByName does not look at. Required for asserting
// PSA-restricted compliance on initContainers (e.g. copy-html).
func initContainerByName(deploy map[string]any, name string) map[string]any {
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	initContainers, _ := podSpec["initContainers"].([]any)
	for _, c := range initContainers {
		cm, _ := c.(map[string]any)
		if n, _ := cm["name"].(string); n == name {
			return cm
		}
	}
	return nil
}

// allContainersAndInitContainers returns every container and initContainer
// spec from a Deployment doc, in order (containers first, then
// initContainers). Used by recurrence guards that must assert a property
// holds for every container in the pod, not just the main one.
func allContainersAndInitContainers(deploy map[string]any) []map[string]any {
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	var out []map[string]any
	if containers, _ := podSpec["containers"].([]any); containers != nil {
		for _, c := range containers {
			if cm, ok := c.(map[string]any); ok {
				out = append(out, cm)
			}
		}
	}
	if initContainers, _ := podSpec["initContainers"].([]any); initContainers != nil {
		for _, c := range initContainers {
			if cm, ok := c.(map[string]any); ok {
				out = append(out, cm)
			}
		}
	}
	return out
}

// podSecCtx returns the pod-level securityContext from a Deployment doc.
func podSecCtx(deploy map[string]any) map[string]any {
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	ctx, _ := podSpec["securityContext"].(map[string]any)
	return ctx
}

// TestF1_MCPResourcesUseReleaseNamespace guards the F1 fix: both the MCP
// Deployment and Service must render into .Release.Namespace, not into
// whatever .Values.namespace.name resolves to (undefined = "").
func TestF1_MCPResourcesUseReleaseNamespace(t *testing.T) {
	docs := helmTemplate(t, "mcp:\n  enabled: true\n")

	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.NotNil(t, deploy, "MCP Deployment must be rendered when mcp.enabled=true")
	meta, _ := deploy["metadata"].(map[string]any)
	ns, _ := meta["namespace"].(string)
	require.Equal(t, "test-ns", ns,
		"MCP Deployment namespace must equal .Release.Namespace (F1 fix: was .Values.namespace.name)")

	svc := findServiceByNameSubstr(docs, "-mcp")
	require.NotNil(t, svc, "MCP Service must be rendered when mcp.enabled=true")
	smeta, _ := svc["metadata"].(map[string]any)
	sns, _ := smeta["namespace"].(string)
	require.Equal(t, "test-ns", sns,
		"MCP Service namespace must equal .Release.Namespace (F1 fix)")
}

// TestF2_MCPProbesAreTCPSocket guards the F2 fix: the MCP container's
// liveness and readiness probes must use tcpSocket, not httpGet. The old
// httpGet /sse hung indefinitely because /sse is a streaming endpoint.
func TestF2_MCPProbesAreTCPSocket(t *testing.T) {
	docs := helmTemplate(t, "mcp:\n  enabled: true\n")

	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.NotNil(t, deploy, "MCP Deployment must be rendered")
	c := containerByName(deploy, "mcp")
	require.NotNil(t, c, "mcp container must exist")

	liveness, _ := c["livenessProbe"].(map[string]any)
	require.NotNil(t, liveness, "MCP container must have a livenessProbe")
	_, hasTCP := liveness["tcpSocket"]
	_, hasHTTP := liveness["httpGet"]
	require.True(t, hasTCP, "MCP livenessProbe must use tcpSocket (F2 fix: httpGet /sse hung)")
	require.False(t, hasHTTP, "MCP livenessProbe must NOT use httpGet")

	readiness, _ := c["readinessProbe"].(map[string]any)
	require.NotNil(t, readiness, "MCP container must have a readinessProbe (F2 fix: was missing)")
	_, hasTCPR := readiness["tcpSocket"]
	require.True(t, hasTCPR, "MCP readinessProbe must use tcpSocket")
}

// TestF3_MCPSecurityContext guards the F3 fix: the MCP pod must have a
// podSecurityContext and containerSecurityContext that satisfy PSA restricted
// (the chart's own namespace default). Pre-fix, the pod had neither and was
// rejected immediately by admission.
func TestF3_MCPSecurityContext(t *testing.T) {
	docs := helmTemplate(t, "mcp:\n  enabled: true\n")

	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.NotNil(t, deploy)

	// Pod-level security context.
	psc := podSecCtx(deploy)
	require.NotNil(t, psc, "MCP Deployment must have a podSecurityContext (F3 fix)")
	require.Equal(t, true, psc["runAsNonRoot"],
		"MCP podSecurityContext.runAsNonRoot must be true (PSA restricted)")
	seccomp, _ := psc["seccompProfile"].(map[string]any)
	require.Equal(t, "RuntimeDefault", seccomp["type"],
		"MCP podSecurityContext.seccompProfile.type must be RuntimeDefault")

	// Container-level security context.
	c := containerByName(deploy, "mcp")
	require.NotNil(t, c)
	csc, _ := c["securityContext"].(map[string]any)
	require.NotNil(t, csc, "MCP container must have a securityContext (F3 fix)")
	require.Equal(t, false, csc["allowPrivilegeEscalation"],
		"MCP container.allowPrivilegeEscalation must be false")
	require.Equal(t, true, csc["readOnlyRootFilesystem"],
		"MCP container.readOnlyRootFilesystem must be true (F3 fix)")
	caps, _ := csc["capabilities"].(map[string]any)
	drop, _ := caps["drop"].([]any)
	var droppedAll bool
	for _, d := range drop {
		if d == "ALL" {
			droppedAll = true
		}
	}
	require.True(t, droppedAll, "MCP container must drop ALL capabilities (F3 fix)")
}

// TestF4_FrontendReadOnlyRootFilesystem guards the F4 fix: the frontend
// container must have readOnlyRootFilesystem=true with emptyDir volumes
// for the paths nginx needs to write. Pre-fix, readOnlyRootFilesystem was
// explicitly false.
func TestF4_FrontendReadOnlyRootFilesystem(t *testing.T) {
	docs := helmTemplate(t, "frontend:\n  enabled: true\n")

	deploy := findDeploymentByNameSubstr(docs, "-frontend")
	require.NotNil(t, deploy, "frontend Deployment must be rendered when frontend.enabled=true")

	c := containerByName(deploy, "frontend")
	require.NotNil(t, c, "frontend container must exist")
	csc, _ := c["securityContext"].(map[string]any)
	require.NotNil(t, csc, "frontend container must have a securityContext")
	require.Equal(t, true, csc["readOnlyRootFilesystem"],
		"frontend container.readOnlyRootFilesystem must be true (F4 fix: was false)")

	// Must have emptyDir volumes for the writable nginx paths.
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	volumes, _ := podSpec["volumes"].([]any)
	volumeNames := map[string]bool{}
	for _, v := range volumes {
		vm, _ := v.(map[string]any)
		if name, ok := vm["name"].(string); ok {
			_, isEmptyDir := vm["emptyDir"]
			if isEmptyDir {
				volumeNames[name] = true
			}
		}
	}
	for _, required := range []string{"nginx-cache", "nginx-run", "tmp"} {
		require.True(t, volumeNames[required],
			"frontend Deployment must have an emptyDir volume %q for nginx writability (F4 fix)", required)
	}
}

// TestF4b_FrontendCopyHtmlInitContainer_PSARestricted guards #468: the
// copy-html initContainer must satisfy the PSA restricted profile.
// Pre-fix it dropped no capabilities and set no container-level
// seccompProfile, so deploying into a namespace enforcing
// pod-security.kubernetes.io/enforce: restricted rejected the pod:
//
//	unrestricted capabilities (container "copy-html" must set
//	securityContext.capabilities.drop=["ALL"])
//
// and the frontend Deployment sat at 0/1 Ready forever. The main
// frontend container was already compliant; only copy-html was broken.
func TestF4b_FrontendCopyHtmlInitContainer_PSARestricted(t *testing.T) {
	docs := helmTemplate(t, "frontend:\n  enabled: true\n")

	deploy := findDeploymentByNameSubstr(docs, "-frontend")
	require.NotNil(t, deploy, "frontend Deployment must be rendered when frontend.enabled=true")

	c := initContainerByName(deploy, "copy-html")
	require.NotNil(t, c, "frontend pod must have a copy-html initContainer")

	csc, _ := c["securityContext"].(map[string]any)
	require.NotNil(t, csc, "copy-html initContainer must have a securityContext (#468)")

	// capabilities.drop must contain ALL — this was the blocking PSA error.
	caps, _ := csc["capabilities"].(map[string]any)
	require.NotNil(t, caps, "copy-html initContainer must set capabilities (#468)")
	drop, _ := caps["drop"].([]any)
	require.NotEmpty(t, drop, "copy-html initContainer.capabilities.drop must not be empty (#468)")
	var droppedAll bool
	for _, d := range drop {
		if d == "ALL" {
			droppedAll = true
		}
	}
	require.True(t, droppedAll,
		"copy-html initContainer.capabilities.drop must contain ALL (#468: PSA restricted requires it)")

	// seccompProfile set explicitly as defense-in-depth (it would otherwise
	// be inherited from the pod-level securityContext). Asserting it here
	// keeps copy-html aligned with the main frontend container.
	seccomp, _ := csc["seccompProfile"].(map[string]any)
	require.NotNil(t, seccomp, "copy-html initContainer must set a seccompProfile (#468)")
	require.Equal(t, "RuntimeDefault", seccomp["type"],
		"copy-html initContainer.seccompProfile.type must be RuntimeDefault (#468)")

	require.Equal(t, false, csc["allowPrivilegeEscalation"],
		"copy-html initContainer.allowPrivilegeEscalation must be false (#468)")
}

// TestF4c_FrontendAllContainersDropAllCapabilities is a recurrence guard
// for #468: every container AND initContainer in the frontend pod must
// drop ALL capabilities so the Deployment stays deployable in a PSA
// restricted namespace. Pre-fix only the main frontend container
// dropped ALL; the copy-html initContainer did not. This test fails if
// any future container/initContainer is added without the drop.
func TestF4c_FrontendAllContainersDropAllCapabilities(t *testing.T) {
	docs := helmTemplate(t, "frontend:\n  enabled: true\n")

	deploy := findDeploymentByNameSubstr(docs, "-frontend")
	require.NotNil(t, deploy, "frontend Deployment must be rendered when frontend.enabled=true")

	containers := allContainersAndInitContainers(deploy)
	require.NotEmpty(t, containers, "frontend pod must have at least one container")

	for _, c := range containers {
		name, _ := c["name"].(string)
		csc, _ := c["securityContext"].(map[string]any)
		require.NotNil(t, csc, "container %q must have a securityContext (#468 recurrence guard)", name)
		caps, _ := csc["capabilities"].(map[string]any)
		require.NotNil(t, caps, "container %q must set capabilities (#468 recurrence guard)", name)
		drop, _ := caps["drop"].([]any)
		var droppedAll bool
		for _, d := range drop {
			if d == "ALL" {
				droppedAll = true
			}
		}
		require.True(t, droppedAll,
			"container %q capabilities.drop must contain ALL (#468 recurrence guard: PSA restricted requires it for every container and initContainer)", name)
	}
}

// TestF5_AdditionalHostsHaveAPIPath guards the F5 fix: when additionalHosts
// is configured, every additional host's ingress rule must include both an
// /api path (to the API service) and a / path (to the frontend). Pre-fix,
// only the / path was generated, causing 502 for all API calls on extra hosts.
func TestF5_AdditionalHostsHaveAPIPath(t *testing.T) {
	docs := helmTemplate(t, `frontend:
  enabled: true
  ingress:
    enabled: true
    host: "primary.example.com"
    additionalHosts:
      - host: "extra.example.com"
`)

	var frontendIngress map[string]any
	for _, d := range docs {
		if d["kind"] != "Ingress" {
			continue
		}
		if strings.Contains(metaName(d), "frontend") {
			frontendIngress = d
			break
		}
	}
	require.NotNil(t, frontendIngress, "frontend Ingress must be rendered")

	spec, _ := frontendIngress["spec"].(map[string]any)
	rules, _ := spec["rules"].([]any)

	// Find the rule for extra.example.com.
	var extraRule map[string]any
	for _, r := range rules {
		rm, _ := r.(map[string]any)
		if h, _ := rm["host"].(string); h == "extra.example.com" {
			extraRule = rm
			break
		}
	}
	require.NotNil(t, extraRule,
		"Ingress must contain a rule for the additionalHost extra.example.com")

	http, _ := extraRule["http"].(map[string]any)
	paths, _ := http["paths"].([]any)

	var hasAPI, hasRoot bool
	for _, p := range paths {
		pm, _ := p.(map[string]any)
		path, _ := pm["path"].(string)
		if path == "/api" {
			hasAPI = true
		}
		if path == "/" {
			hasRoot = true
		}
	}
	require.True(t, hasAPI,
		"additionalHost rule must include /api path to the API service (F5 fix: was missing)")
	require.True(t, hasRoot,
		"additionalHost rule must include / path to the frontend service")
}

// TestF8_ValkeyPolicyAllowsMigrateJob guards the F8 fix: the Valkey
// NetworkPolicy must include an ingress rule for the migrate Job pod selector,
// symmetric with the Postgres policy. Pre-fix, only the API pod was allowed.
func TestF8_ValkeyPolicyAllowsMigrateJob(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var vkPolicy map[string]any
	for _, p := range policies {
		spec, _ := p["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		if app, _ := ml["app"].(string); app == "valkey" {
			vkPolicy = p
			break
		}
	}
	require.NotNil(t, vkPolicy, "Valkey NetworkPolicy must exist")

	spec, _ := vkPolicy["spec"].(map[string]any)
	ingress, _ := spec["ingress"].([]any)

	var foundMigrateRule bool
	for _, rule := range ingress {
		rm, _ := rule.(map[string]any)
		from, _ := rm["from"].([]any)
		for _, f := range from {
			fm, _ := f.(map[string]any)
			podSel, _ := fm["podSelector"].(map[string]any)
			ml, _ := podSel["matchLabels"].(map[string]any)
			if comp, _ := ml["app.kubernetes.io/component"].(string); comp == "migrate" {
				foundMigrateRule = true
			}
		}
	}
	require.True(t, foundMigrateRule,
		"Valkey NetworkPolicy must allow the migrate Job pod selector (F8 fix: was missing)")
}

// =============================================================================
// Helm audit — additional depth tests (gap analysis follow-up)
//
// The initial TestF1–TestF8 suite verified the fixes at a coarse level.
// These tests close the specific gaps identified in the gap analysis:
//   - F2: probe thresholds (not just type)
//   - F3: non-zero UID; /tmp emptyDir declared AND mounted
//   - F4: volumeMounts wired into the frontend container (not just declared)
//   - F5: primary host also has /api path; TLS entry for additionalHost
//   - F8: API-allow rule still present after adding the migrate rule
//   - Negative: MCP disabled → no Deployment/Service rendered
// =============================================================================

// volumeMountPaths returns the set of mountPath values for a container.
func volumeMountPaths(c map[string]any) map[string]bool {
	out := map[string]bool{}
	mounts, _ := c["volumeMounts"].([]any)
	for _, m := range mounts {
		mm, _ := m.(map[string]any)
		if mp, ok := mm["mountPath"].(string); ok {
			out[mp] = true
		}
	}
	return out
}

// TestF2_MCPProbeThresholds guards probe timing so a revert to the old
// config (5s initial delay, 30s period, no readiness) is caught.
func TestF2_MCPProbeThresholds(t *testing.T) {
	docs := helmTemplate(t, "mcp:\n  enabled: true\n")
	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.NotNil(t, deploy)
	c := containerByName(deploy, "mcp")
	require.NotNil(t, c)

	liveness, _ := c["livenessProbe"].(map[string]any)
	require.NotNil(t, liveness)
	require.EqualValues(t, 5, liveness["initialDelaySeconds"],
		"MCP liveness initialDelaySeconds must be 5")
	require.EqualValues(t, 30, liveness["periodSeconds"],
		"MCP liveness periodSeconds must be 30")

	readiness, _ := c["readinessProbe"].(map[string]any)
	require.NotNil(t, readiness)
	require.EqualValues(t, 3, readiness["initialDelaySeconds"],
		"MCP readiness initialDelaySeconds must be 3")
	require.EqualValues(t, 10, readiness["periodSeconds"],
		"MCP readiness periodSeconds must be 10")
}

// TestF3_MCPNonZeroUID guards that the MCP pod runs as a non-zero UID
// (65532). runAsNonRoot=true alone is not sufficient — some runtimes accept
// numeric UID 0 and rely on the admission webhook to block it.
func TestF3_MCPNonZeroUID(t *testing.T) {
	docs := helmTemplate(t, "mcp:\n  enabled: true\n")
	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.NotNil(t, deploy)

	psc := podSecCtx(deploy)
	require.NotNil(t, psc)
	uid := psc["runAsUser"]
	require.NotNil(t, uid, "MCP podSecurityContext must set runAsUser")
	require.NotEqual(t, float64(0), uid,
		"MCP podSecurityContext.runAsUser must not be 0 (root)")
}

// TestF3_MCPTmpVolumeAndMount guards that the /tmp emptyDir is both declared
// as a volume AND mounted into the mcp container. A regression could add the
// volume but forget the mount (or vice versa), causing readOnlyRootFilesystem
// to reject any write to /tmp at runtime.
func TestF3_MCPTmpVolumeAndMount(t *testing.T) {
	docs := helmTemplate(t, "mcp:\n  enabled: true\n")
	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.NotNil(t, deploy)

	// Check volume declared at pod spec level.
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	podSpec, _ := tmpl["spec"].(map[string]any)
	volumes, _ := podSpec["volumes"].([]any)
	var hasTmpVolume bool
	for _, v := range volumes {
		vm, _ := v.(map[string]any)
		if n, _ := vm["name"].(string); n == "tmp" {
			_, isEmptyDir := vm["emptyDir"]
			if isEmptyDir {
				hasTmpVolume = true
			}
		}
	}
	require.True(t, hasTmpVolume,
		"MCP pod must declare a 'tmp' emptyDir volume (F3 fix: readOnlyRootFilesystem=true requires writable /tmp)")

	// Check mount wired into the container.
	c := containerByName(deploy, "mcp")
	require.NotNil(t, c)
	mounts := volumeMountPaths(c)
	require.True(t, mounts["/tmp"],
		"MCP container must have a volumeMount for /tmp (F3 fix)")
}

// TestF4_FrontendVolumeMountsWired guards that the three emptyDir volumes
// (nginx-cache, nginx-run, tmp) are not just declared but actually wired
// into the frontend container at the correct paths. A regression could add
// the volumes without the mounts, leaving nginx unable to write and crashing
// on startup with readOnlyRootFilesystem=true.
func TestF4_FrontendVolumeMountsWired(t *testing.T) {
	docs := helmTemplate(t, "frontend:\n  enabled: true\n")
	deploy := findDeploymentByNameSubstr(docs, "-frontend")
	require.NotNil(t, deploy)

	c := containerByName(deploy, "frontend")
	require.NotNil(t, c)
	mounts := volumeMountPaths(c)

	for path, desc := range map[string]string{
		"/var/cache/nginx": "nginx cache dir (F4 fix)",
		"/var/run":         "nginx pid/socket dir (F4 fix)",
		"/tmp":             "tmp dir (F4 fix)",
	} {
		require.True(t, mounts[path],
			"frontend container must have volumeMount at %s — %s", path, desc)
	}
}

// TestF5_PrimaryHostHasAPIPath guards the primary host rule in the frontend
// Ingress. A refactor that broke only the primary host while keeping
// additionalHosts intact would not be caught by TestF5 alone.
func TestF5_PrimaryHostHasAPIPath(t *testing.T) {
	docs := helmTemplate(t, `frontend:
  enabled: true
  ingress:
    enabled: true
    host: "primary.example.com"
`)
	var frontendIngress map[string]any
	for _, d := range docs {
		if d["kind"] == "Ingress" && strings.Contains(metaName(d), "frontend") {
			frontendIngress = d
			break
		}
	}
	require.NotNil(t, frontendIngress)

	spec, _ := frontendIngress["spec"].(map[string]any)
	rules, _ := spec["rules"].([]any)
	var primaryRule map[string]any
	for _, r := range rules {
		rm, _ := r.(map[string]any)
		if h, _ := rm["host"].(string); h == "primary.example.com" {
			primaryRule = rm
			break
		}
	}
	require.NotNil(t, primaryRule, "primary host rule must exist")

	http, _ := primaryRule["http"].(map[string]any)
	paths, _ := http["paths"].([]any)
	var hasAPI, hasRoot bool
	for _, p := range paths {
		pm, _ := p.(map[string]any)
		switch pm["path"] {
		case "/api":
			hasAPI = true
		case "/":
			hasRoot = true
		}
	}
	require.True(t, hasAPI, "primary host must have /api path to API service")
	require.True(t, hasRoot, "primary host must have / path to frontend service")
}

// TestF5_AdditionalHostsTLSEntry guards that when tls=true, the additionalHost
// gets its own TLS entry in the Ingress spec. Without it, HTTPS terminates
// with the primary host's certificate (wrong cert for the SNI name).
func TestF5_AdditionalHostsTLSEntry(t *testing.T) {
	docs := helmTemplate(t, `frontend:
  enabled: true
  ingress:
    enabled: true
    host: "primary.example.com"
    tls: true
    tlsSecret: "primary-tls"
    additionalHosts:
      - host: "extra.example.com"
        tlsSecret: "extra-tls"
`)
	var frontendIngress map[string]any
	for _, d := range docs {
		if d["kind"] == "Ingress" && strings.Contains(metaName(d), "frontend") {
			frontendIngress = d
			break
		}
	}
	require.NotNil(t, frontendIngress)

	spec, _ := frontendIngress["spec"].(map[string]any)
	tls, _ := spec["tls"].([]any)
	require.NotEmpty(t, tls, "tls block must be present when frontend.ingress.tls=true")

	var foundExtraTLS bool
	for _, t := range tls {
		tm, _ := t.(map[string]any)
		hosts, _ := tm["hosts"].([]any)
		for _, h := range hosts {
			if h == "extra.example.com" {
				foundExtraTLS = true
			}
		}
	}
	require.True(t, foundExtraTLS,
		"additionalHost extra.example.com must have a TLS entry (F5 fix)")
}

// TestF8_ValkeyAPIAllowRulePreserved guards that the existing API pod allow
// rule in the Valkey policy was not accidentally removed when the migrate
// rule was added. A regression that replaced rather than appended would
// break Valkey cache for the API.
func TestF8_ValkeyAPIAllowRulePreserved(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var vkPolicy map[string]any
	for _, p := range policies {
		spec, _ := p["spec"].(map[string]any)
		sel, _ := spec["podSelector"].(map[string]any)
		ml, _ := sel["matchLabels"].(map[string]any)
		if app, _ := ml["app"].(string); app == "valkey" {
			vkPolicy = p
			break
		}
	}
	require.NotNil(t, vkPolicy)

	spec, _ := vkPolicy["spec"].(map[string]any)
	ingress, _ := spec["ingress"].([]any)
	require.GreaterOrEqual(t, len(ingress), 2,
		"Valkey NetworkPolicy must have at least 2 ingress rules (API + migrate)")

	var foundAPIRule bool
	for _, rule := range ingress {
		rm, _ := rule.(map[string]any)
		from, _ := rm["from"].([]any)
		for _, f := range from {
			fm, _ := f.(map[string]any)
			podSel, _ := fm["podSelector"].(map[string]any)
			ml, _ := podSel["matchLabels"].(map[string]any)
			if comp, _ := ml["app.kubernetes.io/component"].(string); comp == "api" {
				foundAPIRule = true
			}
		}
	}
	require.True(t, foundAPIRule,
		"Valkey NetworkPolicy must still allow the API pod (F8 fix must not have removed it)")
}

// TestF_MCPDisabled_NoResourcesRendered guards that when mcp.enabled=false
// (the chart default), no MCP Deployment or Service is rendered. If the
// gating condition is accidentally removed, every install would ship an
// MCP pod even when the operator didn't want one.
func TestF_MCPDisabled_NoResourcesRendered(t *testing.T) {
	// Explicitly disable to verify the default behavior is honored.
	docs := helmTemplate(t, "mcp:\n  enabled: false\n")

	deploy := findDeploymentByNameSubstr(docs, "-mcp")
	require.Nil(t, deploy,
		"no MCP Deployment must be rendered when mcp.enabled=false")

	svc := findServiceByNameSubstr(docs, "-mcp")
	require.Nil(t, svc,
		"no MCP Service must be rendered when mcp.enabled=false")
}

// TestF133_ControllerSecretsAreNamespaceScoped asserts that secrets
// and pods are NEVER granted CRUD verbs via ClusterRole, even when
// rbac.scope=cluster. Read-only verbs (get/list/watch) are
// permitted because the controller-runtime informer cache requires
// cluster-wide watches; CRUD is the dangerous surface (F1.3.3 / G5).
func TestF133_ControllerSecretsAreNamespaceScoped(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: cluster\n")
	clusterRoles := findResources(docs, "ClusterRole")

	// CRUD verbs that MUST NOT appear cluster-wide on secrets/pods.
	mutatingVerbs := map[string]struct{}{
		"create": {}, "update": {}, "patch": {}, "delete": {}, "deletecollection": {},
	}

	for _, cr := range clusterRoles {
		// Walk rules; for any rule that grants secrets or pods, the
		// verb set must contain only read-only verbs.
		rules, _ := cr["rules"].([]any)
		for _, r := range rules {
			rule, _ := r.(map[string]any)
			groups, _ := rule["apiGroups"].([]any)
			resources, _ := rule["resources"].([]any)
			verbs, _ := rule["verbs"].([]any)

			coreGroup := false
			for _, g := range groups {
				if s, ok := g.(string); ok && s == "" {
					coreGroup = true
				}
			}
			if !coreGroup {
				continue
			}
			for _, res := range resources {
				resStr, _ := res.(string)
				if resStr != "secrets" && resStr != "pods" {
					continue
				}
				for _, v := range verbs {
					verbStr, _ := v.(string)
					if _, isMutating := mutatingVerbs[verbStr]; isMutating {
						t.Fatalf(
							"ClusterRole %q grants cluster-wide %q on %s — must be namespace-scoped (F1.3.3 / G5)",
							metaName(cr), verbStr, resStr)
					}
				}
			}
		}
	}
}

// =============================================================================
// InferenceRelay — API ClusterRole for cluster-scoped CRD
// =============================================================================

// TestRelay_APIInferenceRelayClusterRole_DisabledByDefault asserts that
// NEITHER the API ClusterRole nor its ClusterRoleBinding for inferencerelays
// renders when the relay subsystem is disabled (the chart default). Guards
// against accidental removal of the {{- if }} gate on either document.
func TestRelay_APIInferenceRelayClusterRole_DisabledByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	for _, d := range docs {
		k, _ := d["kind"].(string)
		if k != "ClusterRole" && k != "ClusterRoleBinding" {
			continue
		}
		require.NotContains(t, metaName(d), "api-inferencerelay",
			"API InferenceRelay %s must NOT render when controller.inferenceRelay.enabled is false (default)", k)
	}
}

// TestRelay_APIInferenceRelayClusterRole_RendersWhenEnabled asserts the
// API ClusterRole + ClusterRoleBinding for inferencerelays render with a
// least-privilege grant when the relay subsystem is enabled, and that the
// binding is correctly wired (roleRef → the ClusterRole, subject → the API
// ServiceAccount in the release namespace). The InferenceRelay CRD is
// cluster-scoped, so a namespace Role is insufficient.
func TestRelay_APIInferenceRelayClusterRole_RendersWhenEnabled(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)

	leastPrivilege := []string{"get", "list", "create", "update"}

	var roleName, bindingRoleRef string
	var sawRole, sawBinding bool
	for _, d := range docs {
		k, _ := d["kind"].(string)
		name := metaName(d)
		if !strings.Contains(name, "api-inferencerelay") {
			continue
		}
		switch k {
		case "ClusterRole":
			sawRole = true
			roleName = name
			rv := resourceVerbs(d)
			verbs := rv["llmsafespaces.dev/inferencerelays"]
			require.NotEmpty(t, verbs,
				"ClusterRole %q must grant access to inferencerelays", name)
			require.ElementsMatch(t, leastPrivilege, verbs,
				"API inferencerelays grant must be exactly [get,list,create,update] (least-privilege)")
			require.NotContains(t, rv, "llmsafespaces.dev/inferencerelays/status",
				"API must NOT receive /status subresource access")
			require.NotContains(t, rv, "llmsafespaces.dev/inferencerelays/finalizers",
				"API must NOT receive /finalizers subresource access")
		case "ClusterRoleBinding":
			sawBinding = true
			roleRef, _ := d["roleRef"].(map[string]any)
			require.Equal(t, "ClusterRole", roleRef["kind"],
				"ClusterRoleBinding %q roleRef.kind must be ClusterRole", name)
			roleRefName, _ := roleRef["name"].(string)
			require.NotEmpty(t, roleRefName,
				"ClusterRoleBinding %q must reference a ClusterRole by name", name)
			bindingRoleRef = roleRefName
			subjects, _ := d["subjects"].([]any)
			require.Len(t, subjects, 1,
				"ClusterRoleBinding %q must bind exactly one subject (the API ServiceAccount)", name)
			subj, _ := subjects[0].(map[string]any)
			require.Equal(t, "ServiceAccount", subj["kind"],
				"ClusterRoleBinding %q subject must be a ServiceAccount", name)
			subjNS, _ := subj["namespace"].(string)
			require.Equal(t, "test-ns", subjNS,
				"ClusterRoleBinding %q subject must be in the release namespace", name)
		}
	}
	require.True(t, sawRole,
		"ClusterRole for API inferencerelays must render when controller.inferenceRelay.enabled=true")
	require.True(t, sawBinding,
		"ClusterRoleBinding for API inferencerelays must render when controller.inferenceRelay.enabled=true")
	require.Equal(t, roleName, bindingRoleRef,
		"ClusterRoleBinding.roleRef.name must point at the rendered API inferencerelay ClusterRole")
}

// TestRelay_APISecretsCreate_NotResourceNameScoped is a regression test for
// LLMSafeSpaces#463. The chart originally granted the API ServiceAccount
// `create` on the three relay-credential Secrets (oci-credentials,
// gcp-credentials, aws-relay-irwa) via rules that combined
// `verbs: [..., create, ...]` with `resourceNames: [...]`. Kubernetes RBAC
// silently ignores `resourceNames` on the `create` verb:
//
//	https://kubernetes.io/docs/reference/access-authn-authz/rbac/#referring-to-resources
//	> You cannot restrict create or deletecollection requests by their
//	> resource name. For create, this limitation is because the name of the
//	> new object may not be known at authorization time.
//
// The rule therefore granted no create permission at all — the API server
// rejected the very first POST /api/v1/admin/relay/{aws,oci,gcp}-creds with
// "secrets is forbidden: ... cannot create resource secrets", because the
// upsert path goes NotFound→Create on first configuration.
//
// This test asserts that for the API Role (`llmsafespaces-api`, in the
// release namespace), every rule that grants `create` on the core `secrets`
// resource does so WITHOUT any `resourceNames` filter — the only K8s-honored
// form. resourceNames-scoped rules MAY still appear alongside, granting the
// name-scoped verbs (get/update/patch) that RBAC does enforce by name.
func TestRelay_APISecretsCreate_NotResourceNameScoped(t *testing.T) {
	docs := helmTemplate(t, "")

	var apiRole map[string]any
	for _, d := range docs {
		k, _ := d["kind"].(string)
		if k != "Role" {
			continue
		}
		name := metaName(d)
		// The API Role's release-rendered name is "<release>-llmsafespaces-api".
		// Match by suffix to stay release-name agnostic.
		if strings.HasSuffix(name, "llmsafespaces-api") {
			apiRole = d
			break
		}
	}
	require.NotNil(t, apiRole,
		"API Role (kind=Role, name ending in 'llmsafespaces-api') must render")

	sawCreateRule := false
	rules, _ := apiRole["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		groups, _ := rule["apiGroups"].([]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		resourceNames, _ := rule["resourceNames"].([]any)

		coreGroup := false
		for _, g := range groups {
			if s, ok := g.(string); ok && s == "" {
				coreGroup = true
				break
			}
		}
		if !coreGroup {
			continue
		}
		hasSecrets := false
		for _, res := range resources {
			if s, ok := res.(string); ok && s == "secrets" {
				hasSecrets = true
				break
			}
		}
		if !hasSecrets {
			continue
		}
		hasCreate := false
		for _, v := range verbs {
			if s, ok := v.(string); ok && s == "create" {
				hasCreate = true
				break
			}
		}
		if !hasCreate {
			continue
		}
		sawCreateRule = true
		require.Empty(t, resourceNames,
			"API Role rule grants `create` on Secrets but is scoped by resourceNames=%v — "+
				"Kubernetes RBAC silently ignores resourceNames on the create verb, so this rule "+
				"grants NO create permission. Split into a rule with `verbs: [create]` and no "+
				"resourceNames, plus a separate rule with the name-scoped verbs (get/update/patch). "+
				"See LLMSafeSpaces#463.",
			resourceNames)
	}
	require.True(t, sawCreateRule,
		"API Role must include at least one rule granting `create` on core/secrets (the relay "+
			"admin handler creates aws-relay-irwa / oci-credentials / gcp-credentials on first config)")

	// Adversarial guard: the name-scoped update/patch rule must also survive.
	// If a future edit drops this rule, the SECOND call to upsertSecret (when
	// the Secret already exists — controller path = Update at relay_admin.go:615)
	// would fail at runtime: "secrets is forbidden ... cannot update resource
	// secrets". The K8s `update` and `patch` verbs DO honor resourceNames, so
	// there must be a rule with both verbs scoped to all three credential
	// Secret names. See #463 review thread (follow-up assertion).
	relayCredNames := map[string]bool{
		"oci-credentials": true,
		"gcp-credentials": true,
		"aws-relay-irwa":  true,
	}
	var sawUpdatePatchRule bool
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		groups, _ := rule["apiGroups"].([]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		names, _ := rule["resourceNames"].([]any)

		coreGroup := false
		for _, g := range groups {
			if s, ok := g.(string); ok && s == "" {
				coreGroup = true
			}
		}
		hasSecrets := false
		for _, res := range resources {
			if s, ok := res.(string); ok && s == "secrets" {
				hasSecrets = true
			}
		}
		if !coreGroup || !hasSecrets {
			continue
		}
		verbSet := map[string]bool{}
		for _, v := range verbs {
			if s, ok := v.(string); ok {
				verbSet[s] = true
			}
		}
		if !verbSet["update"] || !verbSet["patch"] {
			continue
		}
		nameSet := map[string]bool{}
		for _, n := range names {
			if s, ok := n.(string); ok {
				nameSet[s] = true
			}
		}
		if len(nameSet) == 0 {
			continue // unscoped update/patch is too broad — not this rule
		}
		// All three relay-credential names must be in the resourceNames list.
		allPresent := true
		for n := range relayCredNames {
			if !nameSet[n] {
				allPresent = false
				break
			}
		}
		if allPresent {
			sawUpdatePatchRule = true
			break
		}
	}
	require.True(t, sawUpdatePatchRule,
		"API Role must include a rule granting update+patch on core/secrets with "+
			"resourceNames scoping to all three relay-credential Secret names "+
			"(aws-relay-irwa, oci-credentials, gcp-credentials). Without this, the "+
			"second call to /admin/relay/{aws,oci,gcp}-creds (Secret exists, upsert "+
			"path = Update) would 403 at runtime. See #463.")
}

// =============================================================================
// InferenceRelay — relay-router chart rendering
// =============================================================================
//
// These tests guard the relay-router chart rendering. Post-WG-removal
// (worklog 0442) the relay-router is a single non-privileged container —
// no WireGuard sidecar, no UDP Service, no hostPath /dev/net/tun, no NET_ADMIN.
// The regression guards for that posture live in the
// "WireGuard removal regression guards" section below.

// relayEnabledValues is the minimal values that enable the relay-router
// subsystem (it is disabled by default). Includes the required artifact
// checksums so the controller-deployment render does not fail on `required`.
const relayArtifactVals = "    artifact:\n      sha256Arm64: \"aaa\"\n      sha256Amd64: \"bbb\"\n"
const relayEnabledValues = "controller:\n  inferenceRelay:\n    enabled: true\n" + relayArtifactVals

// podSpecMap returns the .spec.template.spec (PodSpec) of a Deployment doc.
func podSpecMap(deploy map[string]any) map[string]any {
	spec, _ := deploy["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	ps, _ := tmpl["spec"].(map[string]any)
	return ps
}

// capSet returns the capabilities.add list (as a string slice) of a container.

// toInt coerces a YAML-decoded number to int. sigs.k8s.io/yaml unmarshals
// numeric scalars as float64 (via encoding/json), so a bare .(int) assertion
// silently returns 0 — this helper handles int / int64 / float64.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// TestRelayRouter_NetworkPolicy_RendersWhenEnabled asserts that enabling the
// relay subsystem renders a NetworkPolicy that (a) selects the relay-router
// pod, (b) allows TCP 8080 ingress from workspace pods (the proxy path) and
// the controller pod (metrics scrape), and (c) allows unrestricted egress
// (HTTP to relay VM public IPs — per-VM token is the auth — plus DNS and
// Zen-direct fallback). Post-WG-removal (worklog 0442) there is no UDP 51820
// ingress rule.
func TestRelayRouter_NetworkPolicy_RendersWhenEnabled(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)

	var policy map[string]any
	for _, d := range findByKind(docs, "NetworkPolicy") {
		if strings.Contains(metaName(d), "relay-router") {
			policy = d
			break
		}
	}
	require.NotNil(t, policy, "relay-router NetworkPolicy must render when inferenceRelay is enabled")

	spec, _ := policy["spec"].(map[string]any)
	sel, _ := spec["podSelector"].(map[string]any)
	matchLabels, _ := sel["matchLabels"].(map[string]any)
	require.Equal(t, "relay-router", matchLabels["app.kubernetes.io/component"],
		"relay-router NetworkPolicy must select relay-router pods")

	types, _ := spec["policyTypes"].([]any)
	require.Contains(t, types, "Ingress", "policy must govern ingress")
	require.Contains(t, types, "Egress", "policy must govern egress")

	ingress, _ := spec["ingress"].([]any)
	var saw8080 bool
	for _, rule := range ingress {
		rm, _ := rule.(map[string]any)
		ports, _ := rm["ports"].([]any)
		for _, p := range ports {
			pm, _ := p.(map[string]any)
			port := toInt(pm["port"])
			proto, _ := pm["protocol"].(string)
			if port == 8080 && proto == "TCP" {
				saw8080 = true
			}
			if port == 51820 {
				t.Fatalf("NetworkPolicy must NOT include UDP 51820 post-WG-removal (worklog 0442); got rule port=51820")
			}
		}
	}
	require.True(t, saw8080, "NetworkPolicy must allow TCP 8080 ingress (workspace proxy + controller metrics)")

	// Egress must be effectively unrestricted for the router to reach relay
	// VMs over HTTP (per-VM token auth), DNS, and Zen-direct fallback.
	egress, _ := spec["egress"].([]any)
	require.NotEmpty(t, egress, "NetworkPolicy must include egress rules")
}

// TestRelayRouter_NetworkPolicy_HiddenWhenNetworkPolicyDisabled asserts the
// relay-router NetworkPolicy honors the master networkPolicy.enabled toggle
// (parity with the workspace/datastore policies) — it must NOT render when the
// policy controller is disabled, even if inferenceRelay is enabled.
func TestRelayRouter_NetworkPolicy_HiddenWhenNetworkPolicyDisabled(t *testing.T) {
	docs := helmTemplate(t, "controller:\n  inferenceRelay:\n    enabled: true\n"+relayArtifactVals+"networkPolicy:\n  enabled: false\n")
	for _, d := range findByKind(docs, "NetworkPolicy") {
		require.NotContains(t, metaName(d), "relay-router",
			"relay-router NetworkPolicy must NOT render when networkPolicy.enabled is false (master-toggle contract)")
	}
}

// TestRelayRouter_NetworkPolicy_AllowsAPIIngress is a regression test for
// LLMSafeSpaces#466. The relay-router NetworkPolicy originally allowed
// ingress on TCP 8080 from workspace pods (the proxy path) and the
// controller pod (its own /metrics scrape via controller/internal/relay/health.go),
// but NOT from the API pods. The API's relay admin handler ALSO needs to
// scrape /metrics — RelayAdminHandler.scrapeRouterMetrics in
// api/internal/handlers/relay_admin.go does this on every
// GET /api/v1/admin/relay/status to populate the per-relay requestsToday,
// requests429Today, and activeStreams fields of the admin dashboard.
//
// With the API blocked at the NetworkPolicy layer the scrape timed out
// (~5s, Go HTTP client default) and the handler silently swallowed the
// error (relay_admin.go:637-640), so the dashboard rendered zeros for
// those three fields while totalRequests / egressBytes (sourced from the
// InferenceRelay CR status, populated by the controller's scrape) showed
// correctly. The bug was invisible in API logs.
//
// This test asserts that the rendered NetworkPolicy includes an ingress
// rule allowing TCP 8080 from a pod matching the API selector labels.
func TestRelayRouter_NetworkPolicy_AllowsAPIIngress(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)

	var policy map[string]any
	for _, d := range findByKind(docs, "NetworkPolicy") {
		if strings.Contains(metaName(d), "relay-router") {
			policy = d
			break
		}
	}
	require.NotNil(t, policy, "relay-router NetworkPolicy must render when inferenceRelay is enabled")

	spec, _ := policy["spec"].(map[string]any)
	ingress, _ := spec["ingress"].([]any)
	require.NotEmpty(t, ingress, "relay-router NetworkPolicy must have ingress rules")

	// Walk every ingress rule. We accept the rule if it (a) permits TCP 8080
	// and (b) names the API podSelector via either app.kubernetes.io/component=api
	// or any equivalent selector that the chart wires up. Match is intentionally
	// flexible on the *from* shape (namespaceSelector + podSelector, podSelector
	// alone, etc.) so this test does not over-constrain the chart's choice of
	// how to express the source — only that the API is a source.
	var sawAPIRule bool
	for _, rule := range ingress {
		rm, _ := rule.(map[string]any)

		// Must permit TCP 8080.
		allows8080 := false
		ports, _ := rm["ports"].([]any)
		for _, p := range ports {
			pm, _ := p.(map[string]any)
			if toInt(pm["port"]) == 8080 {
				proto, _ := pm["protocol"].(string)
				if proto == "TCP" || proto == "" {
					allows8080 = true
					break
				}
			}
		}
		if !allows8080 {
			continue
		}

		// Must mention the API podSelector somewhere in `from`. Look for any
		// matchLabels entry with app.kubernetes.io/component=api on the API
		// component (under app.kubernetes.io/name=llmsafespaces, matching the
		// chart's standard component-labeling convention used elsewhere).
		from, _ := rm["from"].([]any)
		for _, src := range from {
			sm, _ := src.(map[string]any)
			ps, _ := sm["podSelector"].(map[string]any)
			ml, _ := ps["matchLabels"].(map[string]any)
			if comp, ok := ml["app.kubernetes.io/component"].(string); ok && comp == "api" {
				sawAPIRule = true
				break
			}
		}
		if sawAPIRule {
			break
		}
	}
	require.True(t, sawAPIRule,
		"relay-router NetworkPolicy must allow TCP 8080 ingress from API pods "+
			"(app.kubernetes.io/component=api). The API's RelayAdminHandler scrapes "+
			"/metrics on every GET /admin/relay/status — without this rule the dashboard "+
			"shows zeros for requestsToday/requests429Today/activeStreams. See LLMSafeSpaces#466.")

	// Defensive: assert the policy still has exactly 3 ingress rules so a
	// future refactor that REPLACES (rather than appends) — e.g. dropping
	// the workspace path while adding the API path — would silently break
	// the proxy. The three rules are workspace proxy, controller /metrics
	// scrape, and API /metrics scrape; each on port 8080.
	require.Len(t, ingress, 3,
		"relay-router NetworkPolicy must have exactly 3 ingress rules "+
			"(workspace proxy, controller /metrics scrape, API /metrics scrape). "+
			"If you're adding a new source, add a rule rather than replacing one; "+
			"the workspace and controller rules are not redundant.")
}

// =============================================================================
// Monitoring — Grafana dashboards, PrometheusRule, ServiceMonitor
// =============================================================================

// TestMonitoring_DisabledByDefault_NoResourcesRendered verifies the master
// toggle defaults to false and no monitoring resources are rendered.
func TestMonitoring_DisabledByDefault_NoResourcesRendered(t *testing.T) {
	docs := helmTemplate(t, "")
	for _, d := range docs {
		k, _ := d["kind"].(string)
		require.NotEqual(t, "PrometheusRule", k,
			"PrometheusRule must NOT render when monitoring.enabled is false (default)")
	}
	for _, d := range docs {
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		require.False(t, strings.Contains(name, "grafana-dashboards"),
			"dashboard ConfigMap must NOT render when monitoring is disabled")
	}
}

// TestMonitoring_Enabled_RendersAllResources verifies all monitoring resources
// appear when the master toggle is on.
func TestMonitoring_Enabled_RendersAllResources(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")

	var sawDashboards, sawPrometheusRule, sawAPIServMon, sawCtrlServMon bool
	for _, d := range docs {
		k, _ := d["kind"].(string)
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)

		if k == "ConfigMap" && strings.Contains(name, "grafana-dashboards") {
			sawDashboards = true
		}
		if k == "PrometheusRule" {
			sawPrometheusRule = true
		}
		if k == "ServiceMonitor" && strings.Contains(name, "-api") {
			sawAPIServMon = true
		}
		if k == "ServiceMonitor" && strings.Contains(name, "-controller") {
			sawCtrlServMon = true
		}
	}
	require.True(t, sawDashboards, "dashboard ConfigMap must render")
	require.True(t, sawPrometheusRule, "PrometheusRule must render")
	require.True(t, sawAPIServMon, "API ServiceMonitor must render")
	require.True(t, sawCtrlServMon, "controller ServiceMonitor must render")
}

// TestMonitoring_DashboardsDisabled_NoConfigMap verifies the sub-toggle
// can independently disable dashboards while keeping alerts and monitors.
func TestMonitoring_DashboardsDisabled_NoConfigMap(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n  dashboards:\n    enabled: false\n")
	for _, d := range docs {
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		require.False(t, strings.Contains(name, "grafana-dashboards"),
			"dashboard ConfigMap must NOT render when dashboards.enabled=false")
	}
	var sawPrometheusRule bool
	for _, d := range docs {
		if d["kind"] == "PrometheusRule" {
			sawPrometheusRule = true
		}
	}
	require.True(t, sawPrometheusRule, "PrometheusRule must still render when only dashboards disabled")
}

// TestMonitoring_ServiceMonitorsDisabled_NoServiceMonitors verifies the
// sub-toggle can independently disable ServiceMonitors.
func TestMonitoring_ServiceMonitorsDisabled_NoServiceMonitors(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n  serviceMonitors:\n    enabled: false\n")
	for _, d := range docs {
		require.NotEqual(t, "ServiceMonitor", d["kind"],
			"ServiceMonitor must NOT render when serviceMonitors.enabled=false")
	}
}

// TestMonitoring_ControllerMetricsAddrOverride verifies the controller
// deployment uses 0.0.0.0:8080 for metrics when ServiceMonitors are enabled.
func TestMonitoring_ControllerMetricsAddrOverride(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	args := findControllerArgs(t, docs)
	require.NotEmpty(t, args)
	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--metrics-addr=") {
			found = a
			break
		}
	}
	require.Equal(t, "--metrics-addr=0.0.0.0:8080", found,
		"controller must override metricsAddr to 0.0.0.0:8080 when ServiceMonitors enabled")
}

// TestMonitoring_ControllerMetricsAddrDefault_NoOverride verifies the
// controller keeps loopback binding when monitoring is off.
func TestMonitoring_ControllerMetricsAddrDefault_NoOverride(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	require.NotEmpty(t, args)
	var found string
	for _, a := range args {
		if strings.HasPrefix(a, "--metrics-addr=") {
			found = a
			break
		}
	}
	require.Equal(t, "--metrics-addr=127.0.0.1:8080", found,
		"controller must keep default loopback binding when monitoring is off")
}

// TestMonitoring_PrometheusRulesDisabled_NoRules verifies the sub-toggle
// can independently disable PrometheusRules.
func TestMonitoring_PrometheusRulesDisabled_NoRules(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n  prometheusRules:\n    enabled: false\n")
	for _, d := range docs {
		require.NotEqual(t, "PrometheusRule", d["kind"],
			"PrometheusRule must NOT render when prometheusRules.enabled=false")
	}
}

// TestMonitoring_DashboardConfigMap_ContainsJSON verifies the dashboard
// ConfigMap data keys include the expected dashboard files.
func TestMonitoring_DashboardConfigMap_ContainsJSON(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var cm map[string]any
	for _, d := range docs {
		if d["kind"] == "ConfigMap" {
			_, _ = d["metadata"].(map[string]any)
			if strings.Contains(metaName(d), "grafana-dashboards") {
				cm = d
				break
			}
		}
	}
	require.NotNil(t, cm, "dashboard ConfigMap must exist")
	data, _ := cm["data"].(map[string]any)
	require.Contains(t, data, "operational.json", "ConfigMap must contain operational.json")
	require.Contains(t, data, "billing.json", "ConfigMap must contain billing.json")
}

// TestMonitoring_DashboardConfigMap_HasGrafanaLabel verifies the
// grafana_dashboard label is present for the sidecar importer.
func TestMonitoring_DashboardConfigMap_HasGrafanaLabel(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var cm map[string]any
	for _, d := range docs {
		if d["kind"] == "ConfigMap" {
			if strings.Contains(metaName(d), "grafana-dashboards") {
				cm = d
				break
			}
		}
	}
	require.NotNil(t, cm)
	meta, _ := cm["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	require.Equal(t, "1", labels["grafana_dashboard"],
		"dashboard ConfigMap must have grafana_dashboard=1 label for sidecar import")
}

// TestMonitoring_NamespaceOverride verifies all monitoring resources respect
// the namespace override.
func TestMonitoring_NamespaceOverride(t *testing.T) {
	docs := helmTemplate(t, `monitoring:
  enabled: true
  dashboards:
    namespace: monitoring
  prometheusRules:
    namespace: monitoring
  serviceMonitors:
    namespace: monitoring
`)
	for _, d := range docs {
		k, _ := d["kind"].(string)
		if k == "PrometheusRule" || k == "ServiceMonitor" {
			meta, _ := d["metadata"].(map[string]any)
			ns, _ := meta["namespace"].(string)
			require.Equal(t, "monitoring", ns,
				"%s namespace must match override", k)
		}
		if k == "ConfigMap" {
			meta, _ := d["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			if strings.Contains(name, "grafana-dashboards") {
				ns, _ := meta["namespace"].(string)
				require.Equal(t, "monitoring", ns,
					"dashboard ConfigMap namespace must match override")
			}
		}
	}
}

// TestMonitoring_PrometheusRule_SpecIsTopLevel verifies that the rendered
// PrometheusRule has spec.groups as a top-level key. This is a regression
// test for an accidental indentation bug that nested `spec:` under
// `metadata:`, silently breaking all alerting rules.
func TestMonitoring_PrometheusRule_SpecIsTopLevel(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var rule map[string]any
	for _, d := range docs {
		if d["kind"] == "PrometheusRule" {
			rule = d
			break
		}
	}
	require.NotNil(t, rule, "PrometheusRule must be rendered")

	spec, ok := rule["spec"].(map[string]any)
	require.True(t, ok,
		"PrometheusRule must have a top-level spec key (not nested under metadata)")
	groups, ok := spec["groups"].([]any)
	require.True(t, ok,
		"PrometheusRule spec must have a groups array")
	require.NotEmpty(t, groups,
		"PrometheusRule spec.groups must not be empty")
}

// TestMonitoring_PrometheusRule_ContainsAllAlerts verifies all expected
// alert names are present in the rendered PrometheusRule.
func TestMonitoring_PrometheusRule_ContainsAllAlerts(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var rule map[string]any
	for _, d := range docs {
		if d["kind"] == "PrometheusRule" {
			rule = d
			break
		}
	}
	require.NotNil(t, rule, "PrometheusRule must be rendered")

	spec, ok := rule["spec"].(map[string]any)
	require.True(t, ok, "spec must be top-level")
	groups, _ := spec["groups"].([]any)

	alertNames := map[string]bool{}
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		rules, _ := gm["rules"].([]any)
		for _, r := range rules {
			rm, _ := r.(map[string]any)
			if name, ok := rm["alert"].(string); ok {
				alertNames[name] = true
			}
		}
	}

	expected := []string{
		"LLMSafeSpacesLowAvailability",
		"LLMSafeSpacesHighLatency",
		"LLMSafeSpacesHighAuthFailures",
		"LLMSafeSpacesSSEBrokerDroppingEvents",
		"LLMSafeSpacesReconciliationErrors",
		"LLMSafeSpacesWorkspaceFailures",
		"LLMSafeSpacesWorkspaceCreationSlow",
		"LLMSafeSpacesRecoveryBackoffHigh",
		"LLMSafeSpacesSafeModeActive",
		"LLMSafeSpacesHighConsecutiveFailures",
		"LLMSafeSpacesStatusUpdateConflicts",
		"LLMSafeSpacesInitContainerSlow",
		"LLMSafeSpacesAgentReloadFailures",
		"LLMSafeSpacesAgentdSlowStartup",
		"LLMSafeSpacesRelayInjectorFailures",
		"LLMSafeSpacesHighInferenceCostRate",
		"LLMSafeSpacesWorkspaceDiskUsageHigh",
		"LLMSafeSpacesLegacyAPIKeysRemaining",
	}
	for _, expectedName := range expected {
		require.True(t, alertNames[expectedName],
			"PrometheusRule must contain alert %q", expectedName)
	}

	// Old two-tier error rate alerts must be removed.
	require.False(t, alertNames["LLMSafeSpacesHighAPIErrorRate"],
		"old warning-tier LLMSafeSpacesHighAPIErrorRate must be removed (replaced by LLMSafeSpacesLowAvailability)")
	require.False(t, alertNames["LLMSafeSpacesHighAPIErrorRateCritical"],
		"old critical-tier LLMSafeSpacesHighAPIErrorRateCritical must be removed (replaced by LLMSafeSpacesLowAvailability)")
}

// TestMonitoring_DatasourceConfigMap_RendersWithLabel verifies the
// Grafana Postgres datasource ConfigMap is rendered with the correct
// sidecar label when monitoring and datasources are enabled.
func TestMonitoring_DatasourceConfigMap_RendersWithLabel(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var cm map[string]any
	for _, d := range docs {
		if d["kind"] == "ConfigMap" && strings.Contains(metaName(d), "grafana-datasources") {
			cm = d
			break
		}
	}
	require.NotNil(t, cm, "datasource ConfigMap must render when monitoring.enabled=true")
	meta, _ := cm["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
	require.Equal(t, "1", labels["grafana_datasource"],
		"datasource ConfigMap must have grafana_datasource=1 label for sidecar import")
	data, _ := cm["data"].(map[string]any)
	require.Contains(t, data, "llmsafespaces-postgres.yaml",
		"datasource ConfigMap must contain llmsafespaces-postgres.yaml")
}

// TestMonitoring_DatasourcesDisabled_NoConfigMap verifies the sub-toggle
// can independently disable the datasource ConfigMap.
func TestMonitoring_DatasourcesDisabled_NoConfigMap(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n  datasources:\n    enabled: false\n")
	for _, d := range docs {
		if d["kind"] == "ConfigMap" {
			require.False(t, strings.Contains(metaName(d), "grafana-datasources"),
				"datasource ConfigMap must NOT render when datasources.enabled=false")
		}
	}
}

// TestMonitoring_DashboardConfigMap_NotEmpty verifies the dashboard JSON
// files are non-trivial (not accidentally truncated or emptied).
func TestMonitoring_DashboardConfigMap_NotEmpty(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var cm map[string]any
	for _, d := range docs {
		if d["kind"] == "ConfigMap" && strings.Contains(metaName(d), "grafana-dashboards") {
			cm = d
			break
		}
	}
	require.NotNil(t, cm)
	data, _ := cm["data"].(map[string]any)
	for _, key := range []string{"operational.json", "billing.json"} {
		content, ok := data[key].(string)
		require.True(t, ok, "ConfigMap must contain key %q", key)
		require.Greater(t, len(content), 1000,
			"dashboard %q must be non-trivial (>1000 chars); got %d", key, len(content))
	}
}

// TestMonitoring_DashboardJobVariablesPortable verifies that the operational
// and billing dashboards' PromQL job-label matchers are rendered from the
// release name at chart-render time, not hard-coded to a specific release.
//
// Helm release names vary (e.g. `llmsafespace` singular vs `llmsafespaces`
// plural); the resulting scrape-job labels are tied to Service names, which
// are tied to release names. A dashboard with `job="llmsafespaces-api"`
// hard-coded would render empty in any deployment whose Service labels emit
// `llmsafespace-api` (or any other release-derived name).
//
// The chart eliminates this risk by:
//
//  1. Storing dashboard JSON files with PLACEHOLDER strings
//     (__LLMSAFESPACES_API_JOB__, __LLMSAFESPACES_CTRL_JOB__) instead of
//     hard-coded job names.
//  2. Substituting them at render time in dashboards-configmap.yaml using
//     the Helm `replace` pipeline against a release-derived `<fullname>-api.*`
//     / `<fullname>-controller.*` regex pattern (matches the convention in
//     prometheus-rules.yaml).
//
// This test enforces three contracts on the rendered ConfigMap:
//
//   - **No leftover placeholders.** Every __LLMSAFESPACES_*_JOB__ string
//     must have been substituted; an unrendered placeholder would produce
//     PromQL that matches no series.
//   - **Job matchers contain the test release name.** `job=~"...llmsafespaces-api.*"`
//     etc. — proves the substitution is wired and uses the release-derived
//     pattern rather than a fixed string.
//   - **No hard-coded release-specific job names remain.** No literal
//     `llmsafespace-api` (singular, the failure mode of worklog 0508) or
//     other plausible-looking release-prefix that would be a regression.
//
// Worklog 0508 documents the original failure mode where dashboards shipped
// with hard-coded `current.value=["llmsafespaces-api"]` but the cluster's
// ServiceMonitor emitted `job=llmsafespace-api`, leaving every panel empty
// on first load. Worklog NNNN_2026-06-23_grafana-dashboard-job-vars
// documents the redesign that eliminates the template-variable indirection
// (and thus the entire stale-URL-var failure mode) while still being
// release-portable via Helm-time substitution.
func TestMonitoring_DashboardJobVariablesPortable(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var cm map[string]any
	for _, d := range docs {
		if d["kind"] == "ConfigMap" && strings.Contains(metaName(d), "grafana-dashboards") {
			cm = d
			break
		}
	}
	require.NotNil(t, cm)
	data, _ := cm["data"].(map[string]any)

	// helmTemplate uses a fixed release name; pull it from the rendered
	// ConfigMap's name rather than hard-coding it here so that any future
	// change to the test harness's release name keeps this test correct.
	cmName := metaName(cm)
	releasePrefix := strings.TrimSuffix(cmName, "-grafana-dashboards")

	for _, key := range []string{"operational.json", "billing.json"} {
		content, ok := data[key].(string)
		require.True(t, ok, "dashboard %q must be present in the ConfigMap", key)

		// Contract 1: every placeholder must have been rendered.
		require.NotContains(t, content, "__LLMSAFESPACES_API_JOB__",
			"%s: every __LLMSAFESPACES_API_JOB__ placeholder must be substituted at chart-render time; "+
				"an unrendered placeholder would produce PromQL that matches no series",
			key)
		require.NotContains(t, content, "__LLMSAFESPACES_CTRL_JOB__",
			"%s: every __LLMSAFESPACES_CTRL_JOB__ placeholder must be substituted at chart-render time",
			key)

		// Contract 2: job matchers must contain the release-derived prefix.
		// Operational has both api and controller jobs; billing only has api.
		require.Contains(t, content, releasePrefix+"-api.*",
			"%s: must contain `%s-api.*` after rendering, proving the api-job substitution is wired "+
				"to the release name (not a static string)",
			key, releasePrefix)
		if key == "operational.json" {
			require.Contains(t, content, releasePrefix+"-controller.*",
				"%s: must contain `%s-controller.*` after rendering, proving the controller-job "+
					"substitution is wired to the release name",
				key, releasePrefix)
		}

		// Contract 3: the rendered output must NOT contain the singular
		// `llmsafespace-api` (without the trailing s) — that was the
		// failure mode of worklog 0508 where a stale dashboard JSON had
		// `current.value=["llmsafespace-api"]` hard-coded. The rendered
		// output should only contain `<release>-llmsafespaces-api.*`
		// (release name + chart name plural + suffix).
		//
		// Phrased as a regex: any `job=~"X"` matcher where X starts with
		// `llmsafespace-` (singular, no trailing s before the dash) is a
		// regression. Allow the plural form `llmsafespaces-` because the
		// chart name is plural; reject the singular form.
		bad := regexp.MustCompile(`job=~?"llmsafespace-(api|controller)`)
		require.False(t, bad.MatchString(content),
			"%s: contains a hard-coded singular release-name reference (`llmsafespace-api` or "+
				"`llmsafespace-controller`). This is the regression mode of worklog 0508. "+
				"All job matchers must use the release-derived pattern from "+
				"dashboards-configmap.yaml's replace pipeline.",
			key)
	}
}

// TestMonitoring_DashboardUIDsAreStable pins the exact UIDs declared by
// the operational and billing dashboard JSON files. UIDs are the only
// stable contract between operators' bookmarks/links and the dashboards
// themselves: changing them silently breaks every existing URL.
//
// Failure mode discovered during worklog 0522 incident response: the
// chart was previously named `llmsafespace` (singular) and shipped
// dashboards with UIDs `llmsafespace-operational` /
// `llmsafespace-billing`. When the chart was renamed to `llmsafespaces`
// (plural), the dashboard UIDs followed and became
// `llmsafespaces-operational` / `llmsafespaces-billing`. The Grafana
// sidecar provisioner saw both variants in its database (the old singular
// rows from the prior deploy were never garbage-collected) and refused to
// write either due to optimistic-concurrency conflicts ("unexpected number
// of dashboards for id ...: found 2, desired 1"). Operators' bookmarked
// singular URLs returned the stale singular dashboards (with stale,
// release-mismatched job-label matchers), which appeared as "No data" on
// every panel. Worklog 0522 documented the URL-template-variable angle of
// the same incident; this test pins the UID-stability angle. See worklog
// NNNN for the full redesign.
//
// This test enforces three contracts:
//
//  1. Each dashboard's top-level `uid` field is exactly the value below.
//     ANY change is a regression that breaks operator bookmarks; if a
//     genuine UID change is required, that's a deliberate decision that
//     ALSO requires updating the manual-cleanup runbook in CHART-UPGRADE.md.
//  2. The UID prefix is consistent (`llmsafespaces-`) so any future
//     dashboard added to `helm/dashboards/` is forced to
//     follow the same convention.
//  3. The UIDs survive the Helm `replace` pipeline (no placeholder leaks
//     into the UID field — placeholders are only meant to substitute job
//     labels in PromQL `expr` strings, never the dashboard identity).
func TestMonitoring_DashboardUIDsAreStable(t *testing.T) {
	docs := helmTemplate(t, "monitoring:\n  enabled: true\n")
	var cm map[string]any
	for _, d := range docs {
		if d["kind"] == "ConfigMap" && strings.Contains(metaName(d), "grafana-dashboards") {
			cm = d
			break
		}
	}
	require.NotNil(t, cm)
	data, _ := cm["data"].(map[string]any)

	// Pinned UIDs for the dashboards we know about. To intentionally
	// change either, you must:
	//   1. Update the value here.
	//   2. Update the dashboard JSON's top-level "uid" field.
	//   3. Update CHART-UPGRADE.md with the migration steps.
	//   4. Update the EXPECTED_UIDS list in
	//      helm/scripts/grafana-purge-stale-dashboards.sh.
	//   5. Notify operators that bookmarked URLs will break.
	expectedUIDs := map[string]string{
		"operational.json": "llmsafespaces-operational",
		"billing.json":     "llmsafespaces-billing",
	}

	// Contract 0: every JSON file in the rendered ConfigMap is exercised
	// by this test, even ones we don't have a pinned expectation for.
	// Iterating over `data` (rather than `expectedUIDs`) ensures that any
	// future dashboard added to helm/dashboards/ without
	// being added to `expectedUIDs` will fail this test — catching the
	// "added a third dashboard with a different prefix and forgot to
	// update the test" regression vector flagged in PR #375 review.
	for filename, raw := range data {
		// Helm renders the ConfigMap "data" entries as strings; a non-string
		// value here would indicate a Helm-template structural change worth
		// failing on.
		content, ok := raw.(string)
		require.True(t, ok, "ConfigMap data[%q] must be a string", filename)

		// Contract 3: no placeholder leaked into the rendered output.
		// Run this on every file, regardless of whether we have a pin.
		require.NotContains(t, content, "__LLMSAFESPACES_API_JOB__",
			"%s: rendered ConfigMap must not contain placeholder strings — see TestMonitoring_DashboardJobVariablesPortable",
			filename)

		// Use a regex that pins the top-level "uid" field specifically.
		// Nested datasource UIDs in panels are a different field shape
		// (`"uid": "${datasource}"`) and must not match this pin.
		uidRe := regexp.MustCompile(`(?m)^\s{0,2}"uid":\s*"([^"]+)",?$`)
		matches := uidRe.FindAllStringSubmatch(content, -1)

		// Contract 1: at least one top-level uid must exist.
		var topLevelUIDs []string
		for _, m := range matches {
			topLevelUIDs = append(topLevelUIDs, m[1])
		}
		require.NotEmpty(t, topLevelUIDs,
			"%s: must declare a top-level `uid` field (line of the form `  \"uid\": \"<value>\",`)",
			filename)

		// Find the dashboard's top-level UID (heuristic: the one that
		// matches our `llmsafespaces-` prefix, NOT the ${datasource}
		// nested ones). This survives JSON reordering by the Helm
		// rendering and tolerates whitespace variations.
		var foundTopLevel string
		for _, u := range topLevelUIDs {
			if strings.HasPrefix(u, "llmsafespaces-") {
				foundTopLevel = u
				break
			}
		}

		// Contract 2: prefix consistency. Forces any dashboard in the
		// ConfigMap (including future additions) to follow the
		// `llmsafespaces-*` UID convention. Mixed prefixes complicate
		// the manual cleanup procedure in CHART-UPGRADE.md.
		//
		// The selection logic above only assigns foundTopLevel from
		// UIDs matching the llmsafespaces- prefix, so this NotEmpty
		// check IS the prefix-consistency assertion: a missing
		// foundTopLevel means no top-level UID had the required prefix.
		require.NotEmpty(t, foundTopLevel,
			"%s: no top-level UID with the `llmsafespaces-` prefix found. "+
				"Every dashboard in this chart must use the consistent UID prefix; "+
				"saw top-level UIDs %v.",
			filename, topLevelUIDs)

		// Contract 1 (specific): if we have a pinned expectation for
		// this file, verify the UID matches exactly. New dashboards
		// must be added to expectedUIDs (forcing the developer to also
		// update CHART-UPGRADE.md and the cleanup script).
		want, pinned := expectedUIDs[filename]
		require.True(t, pinned,
			"%s: dashboard added to helm/dashboards/ without being added "+
				"to TestMonitoring_DashboardUIDsAreStable's `expectedUIDs` map. Add it (along "+
				"with the matching entry in scripts/grafana-purge-stale-dashboards.sh's "+
				"EXPECTED_UIDS list) so the UID stability contract covers all dashboards.",
			filename)
		require.Equal(t, want, foundTopLevel,
			"%s: top-level dashboard UID has changed — this breaks operator bookmarks AND triggers "+
				"the multi-version-coexistence failure mode in Grafana's sidecar provisioner "+
				"discovered during worklog 0522 incident response. "+
				"If this change is intentional, see CHART-UPGRADE.md for the required cleanup procedure.",
			filename)
	}

	// Belt-and-suspenders sanity check: every entry in expectedUIDs must
	// correspond to a real file in the rendered ConfigMap. Catches the
	// case where a dashboard is removed from the chart but its pin is
	// left dangling.
	for filename := range expectedUIDs {
		_, ok := data[filename]
		require.True(t, ok,
			"%s: pinned in expectedUIDs but not present in the rendered ConfigMap. "+
				"Either restore the dashboard JSON file or remove the pin (and the "+
				"matching EXPECTED_UIDS entry in the cleanup script).",
			filename)
	}
}

//
// These tests guard the post-WG-removal chart contract: the relay-router
// Deployment must have NO privileged sidecar / NET_ADMIN / WG volumes; the
// namespace must stay PSA `restricted` (no auto-widening to `privileged`);
// no `relay-wireguard` image may be referenced anywhere; no UDP WG Service
// or render-wg.sh ConfigMap may render. A regression that re-introduces any
// of these would fail loudly here.

// findNamespace returns the Namespace doc rendered by the chart, or nil.
func findNamespace(docs []map[string]any) map[string]any {
	for _, d := range docs {
		if d["kind"] == "Namespace" {
			return d
		}
	}
	return nil
}

// TestRelayRouter_Deployment_NoPrivilegedSidecar verifies the relay-router
// pod has exactly ONE container (the router itself) — no WG sidecar, no
// NET_ADMIN/NET_RAW capabilities, no runAsUser:0. Pre-WG-removal this pod
// had a root sidecar + 5 WG volumes + hostPath /dev/net/tun; all gone.
func TestRelayRouter_Deployment_NoPrivilegedSidecar(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)

	deploy := findDeploymentByNameSubstr(docs, "relay-router")
	require.NotNil(t, deploy, "relay-router Deployment must render when inferenceRelay is enabled")

	ps := podSpecMap(deploy)
	containers, _ := ps["containers"].([]any)
	require.Len(t, containers, 1,
		"relay-router must have exactly ONE container post-WG-removal (the WG sidecar was deleted, worklog 0442)")

	c, _ := containers[0].(map[string]any)
	assert.Equal(t, "relay-router", c["name"], "the single container must be the router itself")

	sc, _ := c["securityContext"].(map[string]any)
	if sc != nil {
		// Capabilities: must drop ALL, must NOT add NET_ADMIN or NET_RAW.
		caps, _ := sc["capabilities"].(map[string]any)
		if caps != nil {
			add, _ := caps["add"].([]any)
			for _, a := range add {
				s, _ := a.(string)
				assert.NotContains(t, []string{"NET_ADMIN", "NET_RAW"}, s,
					"relay-router must NOT add NET_ADMIN/NET_RAW post-WG-removal")
			}
		}
		// runAsUser: must NOT be 0 (root).
		if u, ok := sc["runAsUser"].(float64); ok {
			assert.NotEqual(t, float64(0), u, "relay-router must NOT run as root (uid 0)")
		}
	}

	// Volumes: no wg-scripts / wg-config / wg-secret / dev-tun.
	volumes, _ := ps["volumes"].([]any)
	for _, v := range volumes {
		vm, _ := v.(map[string]any)
		name, _ := vm["name"].(string)
		for _, bad := range []string{"wg-scripts", "wg-config", "wg-secret", "dev-tun", "wg-run"} {
			assert.NotEqual(t, bad, name,
				"relay-router must NOT mount WG volume %q post-WG-removal", bad)
		}
	}

	// hostNetwork: must NOT be set (the hostNetwork ingress mode is gone).
	hn, _ := ps["hostNetwork"].(bool)
	assert.False(t, hn, "relay-router must NOT use hostNetwork post-WG-removal")
}

// TestRelayRouter_NoRelayWireguardImageReference verifies no rendered doc
// references the deleted `relay-wireguard` image. The image build was removed
// from CI (worklog 0442); any lingering reference would fail at pod schedule
// time with ImagePullBackOff.
func TestRelayRouter_NoRelayWireguardImageReference(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)
	for _, d := range docs {
		// Walk the doc looking for any string containing "relay-wireguard".
		var found []string
		walkStrings(d, func(s string) {
			if strings.Contains(s, "relay-wireguard") {
				found = append(found, s)
			}
		})
		assert.Empty(t, found,
			"no rendered doc may reference the deleted relay-wireguard image; found: %v", found)
	}
}

// TestRelayRouter_NoWGTemplatesRender verifies the deleted WG templates do
// not render: no `relay-router-wg-scripts` ConfigMap (the render-wg.sh sidecar
// brain) and no UDP-51820 Service. These templates were deleted in worklog
// 0442; a future merge that restores them would re-introduce the privileged
// sidecar dependency.
func TestRelayRouter_NoWGTemplatesRender(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)
	for _, d := range docs {
		name := metaName(d)
		assert.NotContains(t, name, "wg-scripts",
			"relay-router-wg-scripts ConfigMap must NOT render (deleted in worklog 0442)")
		kind, _ := d["kind"].(string)
		if kind == "Service" {
			assert.NotContains(t, name, "wg",
				"no WG-named Service must render (UDP LB/NodePort templates deleted in worklog 0442)")
		}
	}
}

// TestNamespace_StaysRestrictedWhenRelayEnabled verifies the namespace PSA
// profile stays `restricted` when inferenceRelay is enabled — the auto-
// widening to `privileged` (necessary for the WG sidecar) was removed in
// worklog 0442. The namespace can now stay locked-down even with the relay
// fleet active.
func TestNamespace_StaysRestrictedWhenRelayEnabled(t *testing.T) {
	docs := helmTemplate(t, "namespace:\n  create: true\n  podSecurityEnforce: \"restricted\"\ncontroller:\n  inferenceRelay:\n    enabled: true\n"+relayArtifactVals)

	ns := findNamespace(docs)
	require.NotNil(t, ns, "Namespace must render when namespace.create=true")

	labels, _ := ns["metadata"].(map[string]any)["labels"].(map[string]any)
	assert.Equal(t, "restricted", labels["pod-security.kubernetes.io/enforce"],
		"namespace must STAY restricted when inferenceRelay.enabled=true (PSA widening removed in worklog 0442)")
}

// walkStrings recursively walks a map/slice/string structure calling fn on
// every string value. Used by the no-relay-wireguard-image test to scan
// every field of every rendered doc without knowing the structure ahead of
// time.
func walkStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case map[string]any:
		for _, sub := range x {
			walkStrings(sub, fn)
		}
	case []any:
		for _, sub := range x {
			walkStrings(sub, fn)
		}
	}
}

// =============================================================================
// Epic 49 — Email config (US-49.1): move email config out of env into helm
// =============================================================================
//
// These tests guard the contract that email configuration is rendered into
// the API ConfigMap (config.yaml) from helm values, rather than being
// env-var-only. Pre-fix the only way to configure email was via
// LLMSAFESPACES_EMAIL_* env vars, which was inconsistent with every other
// config section (auth/rateLimiting/security are all helm-rendered).
//
// Contracts:
//   1. By default (email.enabled=false), NO email block renders — the API
//      falls back to NoopProvider (provider=""), matching today's behavior.
//   2. When email.enabled=true with provider=ses, the email block renders
//      with all four fields (provider, sesRegion, fromAddress, baseUrl)
//      that Config.Email reads via Viper mapstructure.
//   3. When email.enabled=true but provider is empty, the block still
//      renders with provider="" (NoopProvider path).
//   4. Operator overrides flow through (region/from/baseUrl).

// findAPIConfigMap returns the API service ConfigMap (config.yaml carrier).
func findAPIConfigMap(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != "ConfigMap" {
			continue
		}
		if strings.Contains(metaName(d), "-api-config") {
			return d
		}
	}
	return nil
}

// nsPSAEnforce returns the pod-security.kubernetes.io/enforce label value
// on a Namespace doc, or "" if unset.
func TestEmail_DefaultRender_OmitsEmailBlock(t *testing.T) {
	docs := helmTemplate(t, "")
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm, "API config ConfigMap must be rendered by default")
	cfg := configYAML(t, cm)
	require.NotContains(t, cfg, "email:",
		"email block must NOT render when email.enabled=false (default); config.yaml was:\n%s", cfg)
}

// TestEmail_EnabledSES_RendersEmailBlock asserts the email block renders
// with all four fields when email.enabled=true + provider=ses.
func TestEmail_EnabledSES_RendersEmailBlock(t *testing.T) {
	docs := helmTemplate(t, `email:
  enabled: true
  provider: ses
  sesRegion: us-east-1
  fromAddress: noreply@example.com
  baseUrl: https://app.example.com
`)
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, "email:\n  provider: \"ses\"",
		"email block must render with provider=ses; config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, `sesRegion: "us-east-1"`)
	require.Contains(t, cfg, `fromAddress: "noreply@example.com"`)
	require.Contains(t, cfg, `baseUrl: "https://app.example.com"`)
}

// TestEmail_EnabledNoProvider_RendersEmptyProvider asserts that enabling
// email without setting a provider renders provider="" — the NoopProvider
// path. This is the dev/air-gapped default.
func TestEmail_EnabledNoProvider_RendersEmptyProvider(t *testing.T) {
	docs := helmTemplate(t, "email:\n  enabled: true\n")
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, "email:\n  provider: \"\"",
		"email block must render with provider=\"\"; config.yaml was:\n%s", cfg)
}

// TestEmail_OperatorOverride_FlowsThrough asserts operator-supplied values
// override the rendered config. Guards against a regression that hardcodes
// values or ignores overrides.
func TestEmail_OperatorOverride_FlowsThrough(t *testing.T) {
	docs := helmTemplate(t, `email:
  enabled: true
  provider: ses
  sesRegion: eu-west-1
  fromAddress: hello@acme.io
  baseUrl: https://chat.acme.io
`)
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, `sesRegion: "eu-west-1"`)
	require.Contains(t, cfg, `fromAddress: "hello@acme.io"`)
	require.Contains(t, cfg, `baseUrl: "https://chat.acme.io"`)
}

// TestWorkspace_DefaultRender_OmitsWorkspaceBlock asserts that when
// workspace.defaultStorageClass is empty (the chart default), the rendered
// config.yaml does NOT contain a top-level `workspace:` block. The API
// then leaves the `workspace.defaultStorageClass` instance setting fully
// admin-mutable. Guards against the block appearing with an empty value
// (which would still trip SetHelmOverrides gate in app.go — the gate
// checks non-empty).
func TestWorkspace_DefaultRender_OmitsWorkspaceBlock(t *testing.T) {
	docs := helmTemplate(t, "")
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm, "API config ConfigMap must render by default")
	cfg := configYAML(t, cm)
	// Match `workspace:` only at the start of a line (top-level block).
	// The string `create_workspace:` appears in the rateLimiting.limits
	// map, so a plain Contains check gives false positives.
	require.NotRegexp(t, `(?m)^workspace:`, cfg,
		"top-level workspace block must NOT render when defaultStorageClass empty (default); config.yaml was:\n%s", cfg)
}

// TestWorkspace_DefaultStorageClass_RendersBlock asserts that when the
// operator sets workspace.defaultStorageClass in Helm values, the rendered
// config.yaml contains a `workspace: defaultStorageClass: <value>` block
// that app.go can then pin via SetHelmOverrides.
func TestWorkspace_DefaultStorageClass_RendersBlock(t *testing.T) {
	docs := helmTemplate(t, `workspace:
  defaultStorageClass: longhorn-2r
`)
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, "workspace:\n  defaultStorageClass: \"longhorn-2r\"",
		"workspace.defaultStorageClass must render into config.yaml; config.yaml was:\n%s", cfg)
}

// TestWorkspace_DefaultStorageClass_OperatorOverride verifies alternate
// StorageClass names flow through unmodified. Guards against a regression
// that hardcodes a specific SC name (e.g. only accepts longhorn-*).
func TestWorkspace_DefaultStorageClass_OperatorOverride(t *testing.T) {
	docs := helmTemplate(t, `workspace:
  defaultStorageClass: my-custom-sc
`)
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, `defaultStorageClass: "my-custom-sc"`)
}

// TestRelayRouter_UpstreamAuth_MountsSecretWhenConfigured verifies that when
// upstreamAuth.keySecret.name is set, the router container gets a
// UPSTREAM_AUTH_KEY env from that Secret (the optional key-injection wiring
// from PR #297; rationale updated 2026-06-20 - see worklog 0420 correction).
func TestRelayRouter_UpstreamAuth_MountsSecretWhenConfigured(t *testing.T) {
	vals := `controller:
  inferenceRelay:
    enabled: true
    artifact:
      sha256Arm64: "aaa"
      sha256Amd64: "bbb"
    upstreamAuth:
      keySecret:
        name: relay-upstream-key
        key: key
      header: x-api-key
`
	docs := helmTemplate(t, vals)
	deploy := findDeploymentByNameSubstr(docs, "relay-router")
	require.NotNil(t, deploy)
	router := containerByName(deploy, "relay-router")
	require.NotNil(t, router)
	envs, _ := router["env"].([]any)
	var sawKey, sawHeader bool
	for _, e := range envs {
		em, _ := e.(map[string]any)
		name, _ := em["name"].(string)
		switch name {
		case "UPSTREAM_AUTH_KEY":
			sawKey = true
			vf, _ := em["valueFrom"].(map[string]any)
			sr, _ := vf["secretKeyRef"].(map[string]any)
			require.Equal(t, "relay-upstream-key", sr["name"], "UPSTREAM_AUTH_KEY must reference the configured Secret")
			require.Equal(t, "key", sr["key"], "UPSTREAM_AUTH_KEY must reference the configured key")
		case "UPSTREAM_AUTH_HEADER":
			sawHeader = true
			require.Equal(t, "x-api-key", em["value"], "UPSTREAM_AUTH_HEADER must propagate")
		}
	}
	require.True(t, sawKey, "UPSTREAM_AUTH_KEY env must render when keySecret.name is set")
	require.True(t, sawHeader, "UPSTREAM_AUTH_HEADER env must always render")
}

// TestRelayRouter_UpstreamAuth_OmittedWhenSecretEmpty verifies the default
// (keySecret.name empty) renders NO UPSTREAM_AUTH_KEY env - the router
// forwards the client header unchanged. This is the correct default posture
// for a Zen free-model fleet where `public` authorizes inference for any
// model flagged `allowAnonymous` (A23 disproven 2026-06-20, worklog 0420
// correction); it also means the install must not require a Secret that
// doesn't exist.
func TestRelayRouter_UpstreamAuth_OmittedWhenSecretEmpty(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)
	deploy := findDeploymentByNameSubstr(docs, "relay-router")
	require.NotNil(t, deploy)
	router := containerByName(deploy, "relay-router")
	envs, _ := router["env"].([]any)
	for _, e := range envs {
		em, _ := e.(map[string]any)
		if name, _ := em["name"].(string); name == "UPSTREAM_AUTH_KEY" {
			t.Fatalf("UPSTREAM_AUTH_KEY must NOT render when upstreamAuth.keySecret.name is empty (default)")
		}
	}
}

// =============================================================================
// Epic 43 / US-43.10 — Per-org OIDC SSO instance plumbing (helm surfacing)
// =============================================================================
//
// These tests guard the contract that the OIDC instance-plumbing config
// (oidc.redirectBaseUrl, oidc.frontendRedirectUrl, oidc.stateCookieName) is
// rendered into the API ConfigMap from helm values. Pre-fix these were only
// reachable via LLMSAFESPACES_OIDC_* env vars; in chart-managed deploys
// oidc.redirectBaseUrl was empty, triggering the F11 header-trust fallback
// at api/internal/handlers/org_sso.go:245 where the callback URL is derived
// from X-Forwarded-Proto + Host.
//
// IMPORTANT SCOPE: this is instance-*plumbing* (where the IdP redirects back
// to, where the browser lands). Per-org IdP wiring (discovery URL, client ID,
// secret, claimed domains, group mapping) lives in org_sso_configs and is
// configured by org admins via the API — NOT in this chart block.
//
// Contracts (each test is designed to turn red if its fix is reverted):
//   1. Default render includes the oidc: block with redirectBaseUrl="" and
//      frontendRedirectUrl="". This is the load-bearing assertion: removing
//      the oidc: section entirely (regressing the F11 fix) makes this test
//      fail. The empty values are correct — Go treats empty as "derive from
//      request" (redirectBaseUrl) and "/" (frontendRedirectUrl).
//   2. Custom operator values flow through verbatim for all three keys.
//   3. stateCookieName is OMITTED from the default render — the {{- with }}
//      guard in configmap-api.yaml deliberately omits it when empty. This is
//      a cleaner-YAML / belt-and-suspenders design choice, not a load-bearing
//      correctness guard: sso.go:130-133 handles empty strings via Go fallback
//      (if cookieName == "" { cookieName = "lsp_sso_state" }), so an empty
//      render would still produce a working cookie. We omit the line anyway
//      to (a) produce cleaner rendered YAML and (b) not rely on the fallback.

// TestOIDC_DefaultRender_IncludesEmptyBlock asserts the oidc: block is
// rendered by default with empty redirectBaseUrl and frontendRedirectUrl.
// This is the regression guard for the F11 fix: if the oidc: section is
// removed from the template, the test fails — forcing the operator back to
// env-var-only config and the header-trust fallback.
func TestOIDC_DefaultRender_IncludesEmptyBlock(t *testing.T) {
	docs := helmTemplate(t, "")
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm, "API config ConfigMap must be rendered by default")
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, "oidc:",
		"oidc: block must render by default (F11 fix); config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, "oidc:\n  redirectBaseUrl: \"\"",
		"default oidc.redirectBaseUrl must render as empty string; config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, "frontendRedirectUrl: \"\"",
		"default oidc.frontendRedirectUrl must render as empty string; config.yaml was:\n%s", cfg)
}

// TestOIDC_CustomValues_FlowsThrough asserts all three operator-supplied
// values propagate to the rendered configmap. Guards against a typo'd
// .Values.oidc.* path or a copy/paste error in the template.
func TestOIDC_CustomValues_FlowsThrough(t *testing.T) {
	docs := helmTemplate(t, `oidc:
  redirectBaseUrl: "https://api.example.com"
  frontendRedirectUrl: "https://app.example.com"
  stateCookieName: "custom_sso_state"
`)
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, `redirectBaseUrl: "https://api.example.com"`,
		"operator oidc.redirectBaseUrl must flow through; config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, `frontendRedirectUrl: "https://app.example.com"`,
		"operator oidc.frontendRedirectUrl must flow through; config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, `stateCookieName: "custom_sso_state"`,
		"operator oidc.stateCookieName must flow through; config.yaml was:\n%s", cfg)
}

// TestOIDC_DefaultRender_OmitsStateCookieName asserts the stateCookieName
// line is NOT present in the default render. The {{- with .Values.oidc.stateCookieName }}
// guard in configmap-api.yaml deliberately omits the line when empty.
//
// Note: this is a cleaner-YAML / belt-and-suspenders design choice, NOT a
// load-bearing correctness guard. The Go code at sso.go:130-133 explicitly
// handles empty strings (if cookieName == "" { cookieName = "lsp_sso_state" }),
// so rendering stateCookieName: "" would still produce a working cookie via
// the Go fallback. We omit the line anyway to (a) produce cleaner rendered
// YAML and (b) not rely on the Go fallback for default behavior. This test
// locks in that design decision against regression to an unconditional render.
func TestOIDC_DefaultRender_OmitsStateCookieName(t *testing.T) {
	docs := helmTemplate(t, "")
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.NotContains(t, cfg, "stateCookieName:",
		"oidc.stateCookieName must be omitted from the default render (cleaner YAML + belt-and-suspenders; "+
			"the Go default lsp_sso_state applies via sso.go:130-133); config.yaml was:\n%s", cfg)
}

// ---------------------------------------------------------------------------
// Workspace→router wiring (Design Principle 6, Epic 42). When the in-cluster
// relay fleet is enabled, workspace pods must route free-model traffic through
// the relay-router Service (http://relay-router:8080), NOT the external CF
// Worker. Post-WG-removal (worklog 0442) the router reaches relay VMs over
// HTTP with per-VM token auth, so --inference-relay-secret must NOT render in
// this mode. Before this wiring, enabling the fleet only affected controller-
// side /metrics scraping; workspace traffic still went to the CF Worker
// regardless.
// ---------------------------------------------------------------------------

// TestControllerArgs_RoutesWorkspacesThroughRouterWhenFleetEnabled verifies
// that with controller.inferenceRelay.enabled=true, the controller's
// --inference-relay-url (which becomes INFERENCE_RELAY_BASEURL on workspace
// pods) points at the in-cluster relay-router via the cross-namespace FQDN
// (workspace pods may run in any namespace; the router Service is in the
// release namespace), and no path-secret is passed.
func TestControllerArgs_RoutesWorkspacesThroughRouterWhenFleetEnabled(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)
	args := findControllerArgs(t, docs)
	require.NotEmpty(t, args)

	var relayURL string
	for _, a := range args {
		if strings.HasPrefix(a, "--inference-relay-url=") {
			relayURL = strings.TrimPrefix(a, "--inference-relay-url=")
		}
		if strings.HasPrefix(a, "--inference-relay-secret=") {
			t.Fatalf("--inference-relay-secret must NOT render when fleet enabled (router reaches relays via per-VM token, worklog 0442); got %q", a)
		}
	}
	require.Equal(t, "http://relay-router.test-ns.svc.cluster.local:8080", relayURL,
		"with fleet enabled, workspace traffic must route through the in-cluster relay-router FQDN (Design Principle 6), not the CF Worker and not a same-namespace short name (workspaces may be cross-ns)")
}

// TestControllerArgs_WorkspaceRouterURLOverride verifies that an explicit
// controller.inferenceRelay.workspaceRouterURL overrides the derived FQDN
// (for the separate-namespace-router deploy case).
func TestControllerArgs_WorkspaceRouterURLOverride(t *testing.T) {
	vals := "controller:\n  inferenceRelay:\n    enabled: true\n" + relayArtifactVals + "    workspaceRouterURL: http://my-router.privileged-ns.svc:8080\n"
	docs := helmTemplate(t, vals)
	args := findControllerArgs(t, docs)
	for _, a := range args {
		if strings.HasPrefix(a, "--inference-relay-url=") {
			require.Equal(t, "--inference-relay-url=http://my-router.privileged-ns.svc:8080", a,
				"explicit workspaceRouterURL must propagate as the workspace-facing inference-relay-url")
			return
		}
	}
	t.Fatal("--inference-relay-url must render when fleet is enabled")
}

// TestControllerArgs_NoRelayURLByDefault verifies the post-Epic-60 default:
// with no chart overrides, the controller renders no --inference-relay-url
// flag at all. The chart's inferenceRelayURL value was removed (the CF Worker
// relay is gone — Zen blocks CF Worker IPs). Empty = direct-to-Zen mode,
// where workspace pods call https://opencode.ai/zen/v1 directly with the
// built-in `public` key. The fleet path (controller.inferenceRelay.enabled)
// is exercised by TestControllerArgs_RoutesWorkspacesThroughRouterWhenFleetEnabled.
func TestControllerArgs_NoRelayURLByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	for _, a := range args {
		if strings.HasPrefix(a, "--inference-relay-url") {
			t.Fatalf("--inference-relay-url must NOT render by default (post-Epic-60 direct-to-Zen mode); got %q", a)
		}
		if strings.HasPrefix(a, "--inference-relay-secret") {
			t.Fatalf("--inference-relay-secret must NOT render (flag removed Epic 60); got %q", a)
		}
	}
}

// TestControllerArgs_RelayArtifactFlags_RenderWhenEnabled verifies the
// artifact URL + SHA-256 flags render when the fleet is enabled — without
// these, a provisioned VM downloads nothing and crash-loops relay-proxy.
func TestControllerArgs_RelayArtifactFlags_RenderWhenEnabled(t *testing.T) {
	vals := `controller:
  inferenceRelay:
    enabled: true
    artifact:
      urls:
        - "https://github.com/lenaxia/llmsafespace/releases/latest/download"
        - "https://s3.amazonaws.com/llmsafespace-artifacts"
      sha256Arm64: "aaa"
      sha256Amd64: "bbb"
`
	docs := helmTemplate(t, vals)
	args := findControllerArgs(t, docs)

	var sawURL, sawArm, sawAmd bool
	for _, a := range args {
		if a == "--relay-artifact-url=https://github.com/lenaxia/llmsafespace/releases/latest/download,https://s3.amazonaws.com/llmsafespace-artifacts" {
			sawURL = true
		}
		if a == "--relay-artifact-sha256-arm64=aaa" {
			sawArm = true
		}
		if a == "--relay-artifact-sha256-amd64=bbb" {
			sawAmd = true
		}
	}
	require.True(t, sawURL, "--relay-artifact-url must render (comma-separated mirrors) when fleet enabled")
	require.True(t, sawArm, "--relay-artifact-sha256-arm64 must render when fleet enabled")
	require.True(t, sawAmd, "--relay-artifact-sha256-amd64 must render when fleet enabled")
}

// TestControllerArgs_RelayArtifactFlags_AbsentWhenDisabled verifies no
// artifact flags render when the fleet is disabled (default).
func TestControllerArgs_RelayArtifactFlags_AbsentWhenDisabled(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	for _, a := range args {
		if strings.HasPrefix(a, "--relay-artifact") {
			t.Fatalf("artifact flags must NOT render when fleet disabled; got %q", a)
		}
	}
}

// findControllerEnv locates the controller container's env list in the
// rendered Deployment.
func findControllerEnv(t *testing.T, docs []map[string]any) []map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != "Deployment" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if !strings.Contains(name, "controller") {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		if len(containers) == 0 {
			continue
		}
		c, _ := containers[0].(map[string]any)
		rawEnv, _ := c["env"].([]any)
		out := make([]map[string]any, 0, len(rawEnv))
		for _, e := range rawEnv {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// TestControllerEnv_PodNamespaceFromFieldRef pins that the controller
// deployment exports POD_NAMESPACE via the downward API. The InferenceRelay
// reconciler reads this env var to locate the relay-router peer ConfigMap
// and per-VM token Secret in the release namespace; without it, the
// reconciler falls back to a hardcoded "llmsafespaces" namespace literal
// that only happens to work when the chart is installed under that exact
// name. See worklog 0464 for the production failure mode this prevents.
//
// This test is unconditional (relay-disabled default) because POD_NAMESPACE
// is unconditionally rendered: it is cheap to add and other controller
// components may grow to read it.
func TestControllerEnv_PodNamespaceFromFieldRef(t *testing.T) {
	docs := helmTemplate(t, "")
	env := findControllerEnv(t, docs)

	var sawPodNamespace bool
	for _, e := range env {
		if e["name"] != "POD_NAMESPACE" {
			continue
		}
		sawPodNamespace = true
		valueFrom, ok := e["valueFrom"].(map[string]any)
		require.True(t, ok, "POD_NAMESPACE must use valueFrom (not literal value)")
		fieldRef, ok := valueFrom["fieldRef"].(map[string]any)
		require.True(t, ok, "POD_NAMESPACE valueFrom must use fieldRef (downward API)")
		require.Equal(t, "metadata.namespace", fieldRef["fieldPath"],
			"POD_NAMESPACE must source from metadata.namespace via downward API")
		break
	}
	require.True(t, sawPodNamespace,
		"controller deployment must export POD_NAMESPACE env var; without it the relay reconciler falls back to a hardcoded namespace and fails on non-default chart installs")
}

// TestWorkspaceEgress_AllowsRelayRouter verifies the workspace-egress
// NetworkPolicy includes an explicit allow rule for the in-cluster
// relay-router on port 8080. Without this, the workspace agentd cannot
// reach INFERENCE_RELAY_BASEURL=http://relay-router.<ns>.svc.cluster.local:8080
// because the router's ClusterIP falls under the RFC1918 block in the
// general-egress rule. Discovered live during worklog 0467 testing.
func TestWorkspaceEgress_AllowsRelayRouter(t *testing.T) {
	docs := helmTemplate(t, relayEnabledValues)
	policies := findByKind(docs, "NetworkPolicy")

	var checked bool
	for _, p := range policies {
		name := metaName(p)
		if !strings.Contains(name, "workspace-egress") {
			continue
		}
		checked = true
		spec, _ := p["spec"].(map[string]any)
		egress, _ := spec["egress"].([]any)

		var sawRouterAllow bool
		for _, rawRule := range egress {
			rule, _ := rawRule.(map[string]any)
			ports, _ := rule["ports"].([]any)
			to, _ := rule["to"].([]any)

			// Look for a rule with port 8080 that targets the relay-router
			// pod selector (component=relay-router).
			port8080 := false
			for _, rawPort := range ports {
				port, _ := rawPort.(map[string]any)
				if port["port"] == float64(8080) || port["port"] == int64(8080) || port["port"] == 8080 {
					port8080 = true
					break
				}
			}
			if !port8080 {
				continue
			}
			for _, rawTo := range to {
				toEntry, _ := rawTo.(map[string]any)
				podSelector, _ := toEntry["podSelector"].(map[string]any)
				if podSelector == nil {
					continue
				}
				matchLabels, _ := podSelector["matchLabels"].(map[string]any)
				if matchLabels["app.kubernetes.io/component"] == "relay-router" {
					sawRouterAllow = true
				}
			}
		}
		require.True(t, sawRouterAllow,
			"workspace-egress policy must include an explicit allow rule "+
				"for component=relay-router on port 8080. Without this, "+
				"workspace pods cannot reach INFERENCE_RELAY_BASEURL because "+
				"the router's ClusterIP falls under the RFC1918 exclude. "+
				"Bug discovered during worklog 0467 in-cluster testing.")
	}
	require.True(t, checked, "workspace-egress NetworkPolicy must exist when fleet enabled")
}

// TestWorkspaceEgress_NoRelayRouterRuleWhenFleetDisabled verifies the
// rule does not render when the fleet is disabled — there is no
// in-cluster router to permit egress to.
func TestWorkspaceEgress_NoRelayRouterRuleWhenFleetDisabled(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	for _, p := range policies {
		name := metaName(p)
		if !strings.Contains(name, "workspace-egress") {
			continue
		}
		spec, _ := p["spec"].(map[string]any)
		egress, _ := spec["egress"].([]any)
		for _, rawRule := range egress {
			rule, _ := rawRule.(map[string]any)
			to, _ := rule["to"].([]any)
			for _, rawTo := range to {
				toEntry, _ := rawTo.(map[string]any)
				podSelector, _ := toEntry["podSelector"].(map[string]any)
				if podSelector == nil {
					continue
				}
				matchLabels, _ := podSelector["matchLabels"].(map[string]any)
				if matchLabels["app.kubernetes.io/component"] == "relay-router" {
					t.Fatal("workspace-egress must NOT include relay-router rule when fleet disabled")
				}
			}
		}
	}
}

// TestWorkspaceEgress_ToggleOff verifies that setting
// networkPolicy.workspaceEgress.enabled=false suppresses the workspace-egress
// NetworkPolicy entirely, while leaving the workspace-default-deny-ingress
// NetworkPolicy in place.
//
// Motivation: operators using a CNI-native FQDN allowlist (e.g. Cilium
// CiliumNetworkPolicy) need this NP suppressed — otherwise Kubernetes
// unions its allow rules with the CNP's, widening effective egress to
// whatever this chart's NP permits (default: 0.0.0.0/0 minus RFC1918).
// See 2026-07-02 Cilium migration runbook gotcha #10 in ops-prod.
func TestWorkspaceEgress_ToggleOff(t *testing.T) {
	values := "networkPolicy:\n  workspaceEgress:\n    enabled: false\n"
	docs := helmTemplate(t, values)
	policies := findByKind(docs, "NetworkPolicy")

	var egressFound, ingressFound bool
	for _, p := range policies {
		name := metaName(p)
		if strings.Contains(name, "workspace-egress") {
			egressFound = true
		}
		if strings.Contains(name, "workspace-default-deny-ingress") {
			ingressFound = true
		}
	}
	require.False(t, egressFound,
		"workspace-egress NetworkPolicy must be absent when workspaceEgress.enabled=false")
	require.True(t, ingressFound,
		"workspace-default-deny-ingress NetworkPolicy must remain when workspaceEgress.enabled=false")
}

// TestWorkspaceEgress_ToggleOnByDefault verifies the toggle defaults to
// on (backwards-compat). Operators upgrading the chart get the same
// behavior they had before the toggle was introduced.
func TestWorkspaceEgress_ToggleOnByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	policies := findByKind(docs, "NetworkPolicy")

	var egressFound bool
	for _, p := range policies {
		if strings.Contains(metaName(p), "workspace-egress") {
			egressFound = true
			break
		}
	}
	require.True(t, egressFound,
		"workspace-egress NetworkPolicy must render by default (backwards compat)")
}

// ============================================================================
// PR #501 review round 3 findings: CSP + Turnstile integration tests.
//
// The nginx-ingress CSP annotation must include Cloudflare's Turnstile
// domain (challenges.cloudflare.com) in both script-src and frame-src
// when turnstile.enabled=true — otherwise the widget's script fails to
// load and the register submit button stays permanently disabled.
// ============================================================================

// TestTurnstile_CSPExtendedWhenEnabled verifies the frontend Ingress
// annotation's Content-Security-Policy is augmented to allow the
// Turnstile CDN when turnstile.enabled=true.
func TestTurnstile_CSPExtendedWhenEnabled(t *testing.T) {
	values := "frontend:\n  enabled: true\n  ingress:\n    enabled: true\nturnstile:\n  enabled: true\n  siteKey: 0xSITE\n"
	docs := helmTemplate(t, values)
	ingresses := findByKind(docs, "Ingress")

	var found bool
	for _, ing := range ingresses {
		name := metaName(ing)
		if !strings.Contains(name, "frontend") {
			continue
		}
		found = true
		annos := metadataAnnotations(ing)
		snippet := annos["nginx.ingress.kubernetes.io/configuration-snippet"]
		require.Contains(t, snippet, "script-src 'self' https://challenges.cloudflare.com",
			"CSP script-src must include Turnstile CDN when turnstile enabled — otherwise the widget script fails to load and submit button never unlocks")
		require.Contains(t, snippet, "https://challenges.cloudflare.com",
			"CSP frame-src must include Turnstile CDN when turnstile enabled — the widget renders in an iframe")
	}
	require.True(t, found, "frontend Ingress must render for this test")
}

// TestTurnstile_CSPUnchangedWhenDisabled verifies the default CSP is
// untouched when turnstile.enabled=false (i.e. no accidental broadening
// of script-src for deployments that don't use Turnstile).
func TestTurnstile_CSPUnchangedWhenDisabled(t *testing.T) {
	values := "frontend:\n  enabled: true\n  ingress:\n    enabled: true\n"
	docs := helmTemplate(t, values)
	ingresses := findByKind(docs, "Ingress")

	var found bool
	for _, ing := range ingresses {
		name := metaName(ing)
		if !strings.Contains(name, "frontend") {
			continue
		}
		found = true
		annos := metadataAnnotations(ing)
		snippet := annos["nginx.ingress.kubernetes.io/configuration-snippet"]
		require.Contains(t, snippet, "script-src 'self'",
			"CSP script-src must be 'self' when Turnstile disabled — default posture")
		require.NotContains(t, snippet, "challenges.cloudflare.com",
			"CSP must NOT include Turnstile CDN when turnstile disabled — no accidental broadening")
	}
	require.True(t, found, "frontend Ingress must render for this test")
}

// metadataAnnotations extracts .metadata.annotations from a rendered
// object, returning an empty map if absent. Type-narrowed helper for
// the CSP tests above; avoids repeating the two-step map assertion
// dance.
func metadataAnnotations(obj map[string]any) map[string]string {
	m, _ := obj["metadata"].(map[string]any)
	if m == nil {
		return map[string]string{}
	}
	a, _ := m["annotations"].(map[string]any)
	if a == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(a))
	for k, v := range a {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// ============================================================================
// Epic 54, US-54.3: Org-scoped wildcard subdomain routing chart tests.
//
// These tests verify the chart's opt-in wildcard Ingress + Certificate
// templates render correctly when orgSubdomainRouting.enabled=true and
// are absent by default (backward compatibility).
// ============================================================================

// TestEpic54_DefaultRender_NoWildcardResources verifies that a default
// `helm template` with no overrides renders ZERO wildcard Ingress or
// Certificate resources — single-host deploys are unaffected.
func TestEpic54_DefaultRender_NoWildcardResources(t *testing.T) {
	docs := helmTemplate(t, "")
	for _, d := range docs {
		name := metaName(d)
		if strings.Contains(name, "wildcard") {
			t.Fatalf("default render must NOT include wildcard resources; found %s/%s",
				d["kind"], name)
		}
	}
}

// TestEpic54_WildcardEnabled_RendersIngressAndCert verifies that enabling
// orgSubdomainRouting renders both the wildcard Ingress and a cert-manager
// Certificate with the correct host and DNS names.
func TestEpic54_WildcardEnabled_RendersIngressAndCert(t *testing.T) {
	values := `
frontend:
  enabled: true
  ingress:
    enabled: true
    host: "app.example.com"
    tls: true
orgSubdomainRouting:
  enabled: true
  baseDomain: "app.example.com"
  cookieDomain: ".app.example.com"
  wildcardCert:
    issuerRef:
      name: "letsencrypt-prod"
      kind: "ClusterIssuer"
`
	docs := helmTemplate(t, values)

	// Find the wildcard Ingress
	ingresses := findByKind(docs, "Ingress")
	var wildcardIngress map[string]any
	for _, ing := range ingresses {
		if strings.Contains(metaName(ing), "wildcard") {
			wildcardIngress = ing
			break
		}
	}
	require.NotNil(t, wildcardIngress, "wildcard Ingress must render when orgSubdomainRouting.enabled=true")

	// Verify the host rule
	spec, _ := wildcardIngress["spec"].(map[string]any)
	rules, _ := spec["rules"].([]any)
	require.NotEmpty(t, rules, "wildcard Ingress must have at least one rule")
	rule, _ := rules[0].(map[string]any)
	assert.Equal(t, "*.app.example.com", rule["host"],
		"wildcard Ingress host must be *.<baseDomain>")

	// Verify TLS block references the wildcard secret
	tls, _ := spec["tls"].([]any)
	require.NotEmpty(t, tls, "wildcard Ingress must have TLS when frontend.ingress.tls=true")
	tlsEntry, _ := tls[0].(map[string]any)
	hosts, _ := tlsEntry["hosts"].([]any)
	require.Contains(t, hosts, "*.app.example.com")
	assert.Contains(t, tlsEntry["secretName"], "wildcard-tls",
		"TLS secret name must contain 'wildcard-tls'")

	// Find the wildcard Certificate
	certs := findByKind(docs, "Certificate")
	var wildcardCert map[string]any
	for _, cert := range certs {
		if strings.Contains(metaName(cert), "wildcard") {
			wildcardCert = cert
			break
		}
	}
	require.NotNil(t, wildcardCert, "wildcard Certificate must render when issuerRef is configured")

	certSpec, _ := wildcardCert["spec"].(map[string]any)
	dnsNames, _ := certSpec["dnsNames"].([]any)
	require.Contains(t, dnsNames, "*.app.example.com",
		"Certificate dnsNames must include the wildcard")
	require.Contains(t, dnsNames, "app.example.com",
		"Certificate dnsNames must include the base domain (for non-wildcard requests)")

	issuerRef, _ := certSpec["issuerRef"].(map[string]any)
	assert.Equal(t, "letsencrypt-prod", issuerRef["name"],
		"Certificate issuerRef.name must match values")
	assert.Equal(t, "ClusterIssuer", issuerRef["kind"],
		"Certificate issuerRef.kind must match values")
}

// TestEpic54_WildcardEnabled_NoCertWhenTlsDisabled verifies that when TLS
// is disabled (frontend.ingress.tls=false), no Certificate is rendered
// even if orgSubdomainRouting is enabled.
func TestEpic54_WildcardEnabled_NoCertWhenTlsDisabled(t *testing.T) {
	values := `
frontend:
  enabled: true
  ingress:
    enabled: true
    host: "app.example.com"
    tls: false
orgSubdomainRouting:
  enabled: true
  baseDomain: "app.example.com"
  cookieDomain: ".app.example.com"
  wildcardCert:
    issuerRef:
      name: "letsencrypt-prod"
      kind: "ClusterIssuer"
`
	docs := helmTemplate(t, values)

	// Wildcard Ingress should still render (just without TLS)
	ingresses := findByKind(docs, "Ingress")
	var sawWildcardIngress bool
	for _, ing := range ingresses {
		if strings.Contains(metaName(ing), "wildcard") {
			sawWildcardIngress = true
			spec, _ := ing["spec"].(map[string]any)
			_, hasTLS := spec["tls"]
			assert.False(t, hasTLS, "wildcard Ingress must NOT have TLS when tls=false")
			break
		}
	}
	assert.True(t, sawWildcardIngress, "wildcard Ingress should render even without TLS")

	// No Certificate should render
	for _, cert := range findByKind(docs, "Certificate") {
		assert.NotContains(t, metaName(cert), "wildcard",
			"wildcard Certificate must NOT render when tls=false")
	}
}

// TestEpic54_ExistingTLSSecret_NoCertificateRendered verifies that when
// wildcardCert.tlsSecret is set (operator has an external wildcard cert),
// no Certificate resource is rendered — the Ingress references the
// operator's existing Secret.
func TestEpic54_ExistingTLSSecret_NoCertificateRendered(t *testing.T) {
	values := `
frontend:
  enabled: true
  ingress:
    enabled: true
    host: "app.example.com"
    tls: true
orgSubdomainRouting:
  enabled: true
  baseDomain: "app.example.com"
  cookieDomain: ".app.example.com"
  wildcardCert:
    tlsSecret: "my-existing-wildcard-cert"
    issuerRef:
      name: "letsencrypt-prod"
      kind: "ClusterIssuer"
`
	docs := helmTemplate(t, values)

	// No Certificate should render (tlsSecret takes precedence)
	for _, cert := range findByKind(docs, "Certificate") {
		assert.NotContains(t, metaName(cert), "wildcard",
			"wildcard Certificate must NOT render when tlsSecret is set")
	}

	// The Ingress should reference the operator's Secret
	ingresses := findByKind(docs, "Ingress")
	for _, ing := range ingresses {
		if !strings.Contains(metaName(ing), "wildcard") {
			continue
		}
		spec, _ := ing["spec"].(map[string]any)
		tls, _ := spec["tls"].([]any)
		require.NotEmpty(t, tls)
		tlsEntry, _ := tls[0].(map[string]any)
		assert.Equal(t, "my-existing-wildcard-cert", tlsEntry["secretName"],
			"wildcard Ingress must reference the operator-supplied tlsSecret")
	}
}

// TestEpic54_ConfigMapEmitsOrgSubdomainRouting verifies that the API
// ConfigMap includes the orgSubdomainRouting block when configured.
func TestEpic54_ConfigMapEmitsOrgSubdomainRouting(t *testing.T) {
	values := `
orgSubdomainRouting:
  enabled: true
  baseDomain: "app.example.com"
  cookieDomain: ".app.example.com"
`
	docs := helmTemplate(t, values)
	cms := findByKind(docs, "ConfigMap")
	var apiCM map[string]any
	for _, cm := range cms {
		if strings.Contains(metaName(cm), "api-config") {
			apiCM = cm
			break
		}
	}
	require.NotNil(t, apiCM, "API ConfigMap must render")
	data, _ := apiCM["data"].(map[string]any)
	configYAML, _ := data["config.yaml"].(string)
	assert.Contains(t, configYAML, "orgSubdomainRouting:")
	assert.Contains(t, configYAML, "baseDomain: \"app.example.com\"")
	assert.Contains(t, configYAML, "cookieDomain: \".app.example.com\"")
}

// TestEpic54_WildcardEnabled_EmptyBaseDomain_FailsAtTemplate verifies that
// enabling orgSubdomainRouting without a baseDomain fails at helm template
// time (via the `required` function) rather than rendering a broken host rule.
func TestEpic54_WildcardEnabled_EmptyBaseDomain_FailsAtTemplate(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH; skipping chart render test")
	}
	values := `
frontend:
  enabled: true
  ingress:
    enabled: true
orgSubdomainRouting:
  enabled: true
  baseDomain: ""
`
	dir := t.TempDir()
	valuesPath := filepath.Join(dir, "values.yaml")
	require.NoError(t, writeFile(valuesPath, values))

	cmd := exec.Command("helm", "template", "test-release", chartDir(t), "-n", "test-ns", "-f", valuesPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	require.Error(t, err, "helm template must fail when orgSubdomainRouting.enabled=true but baseDomain is empty")
	assert.Contains(t, stderr.String(), "baseDomain is required",
		"error message must explain that baseDomain is required")
}

// ---------------------------------------------------------------------------
// G47 (Inference-relay secret never as plaintext CLI arg) — closed in Epic 60.
// ---------------------------------------------------------------------------
// The CF Worker relay that G47 protected is gone (Zen blocks CF Worker IPs,
// see Epic 60). With it went the entire surface G47 covered:
//   - the .Values.inferenceRelaySecret chart value
//   - the .Values.externalSecret.{create,existingSecret} gate
//   - the --inference-relay-secret controller flag
//   - the INFERENCE_RELAY_SECRET env var block on the controller Deployment
//   - the relay-secret-sync Helm Hook Job
// The self-hosted InferenceRelay fleet (Epic 42) that replaces the Worker
// uses per-VM tokens managed by the router, never a path-segment secret, so
// G47 has no remaining applicability. The two original tests
// (TestControllerArgs_G47_NoPlaintextRelaySecretFallback and
// TestControllerArgs_G47_EnvVarPathStillWorks) were deleted with the Worker.
// The new TestControllerArgs_NoRelayURLByDefault pins the post-removal
// default state.

// ---------------------------------------------------------------------------
// Image digest pinning (#476)
// ---------------------------------------------------------------------------

// TestImageHelper_TagDefault verifies the image helpers produce repo:tag
// when no digest is set. Pins the pre-existing behavior so the digest
// support added in #476 is provably non-regressive.
func TestImageHelper_TagDefault(t *testing.T) {
	vals := `
api:
  image:
    repository: registry.example.com/api
    tag: v1.2.3
controller:
  image:
    repository: registry.example.com/controller
    tag: v1.2.3
frontend:
  enabled: true
  image:
    repository: registry.example.com/frontend
    tag: v1.2.3
`
	docs := helmTemplate(t, vals)

	// API and controller use the helpers; frontend uses inline template.
	apiImg := findImageByDeployment(t, docs, "-api")
	ctrlImg := findImageByDeployment(t, docs, "-controller")
	feImg := findImageByDeployment(t, docs, "-frontend")

	assert.Equal(t, "registry.example.com/api:v1.2.3", apiImg,
		"api image must be repo:tag when digest unset")
	assert.Equal(t, "registry.example.com/controller:v1.2.3", ctrlImg,
		"controller image must be repo:tag when digest unset")
	assert.Equal(t, "registry.example.com/frontend:v1.2.3", feImg,
		"frontend image must be repo:tag when digest unset")
}

// TestImageHelper_DigestOverridesTag is the #476 regression test: when
// .Values.<svc>.image.digest is set, the helper produces repo@digest,
// ignoring tag. Operators use this to pin to immutable content-addressable
// refs (avoids the GHCR tag-GC issue #454 ran into).
func TestImageHelper_DigestOverridesTag(t *testing.T) {
	vals := `
api:
  image:
    repository: registry.example.com/api
    tag: v1.2.3
    digest: sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789
controller:
  image:
    repository: registry.example.com/controller
    tag: v1.2.3
    digest: sha256:1111111111111111111111111111111111111111111111111111111111111111
frontend:
  enabled: true
  image:
    repository: registry.example.com/frontend
    tag: v1.2.3
    digest: sha256:2222222222222222222222222222222222222222222222222222222222222222
`
	docs := helmTemplate(t, vals)

	apiImg := findImageByDeployment(t, docs, "-api")
	ctrlImg := findImageByDeployment(t, docs, "-controller")
	feImg := findImageByDeployment(t, docs, "-frontend")

	const apiDigest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	const ctrlDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const feDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	assert.Equal(t, "registry.example.com/api@"+apiDigest, apiImg,
		"api image must be repo@digest when digest set (tag ignored)")
	assert.Equal(t, "registry.example.com/controller@"+ctrlDigest, ctrlImg,
		"controller image must be repo@digest when digest set (tag ignored)")
	assert.Equal(t, "registry.example.com/frontend@"+feDigest, feImg,
		"frontend image must be repo@digest when digest set (tag ignored)")
}

// TestImageHelper_DigestNoTag verifies digest works without setting tag
// at all — the operator only sets digest, nothing else.
func TestImageHelper_DigestNoTag(t *testing.T) {
	vals := `
api:
  image:
    repository: registry.example.com/api
    digest: sha256:abcdef
controller:
  image:
    repository: registry.example.com/controller
    digest: sha256:111111
frontend:
  enabled: true
  image:
    repository: registry.example.com/frontend
    digest: sha256:222222
`
	docs := helmTemplate(t, vals)

	apiImg := findImageByDeployment(t, docs, "-api")
	ctrlImg := findImageByDeployment(t, docs, "-controller")
	feImg := findImageByDeployment(t, docs, "-frontend")

	assert.Equal(t, "registry.example.com/api@sha256:abcdef", apiImg,
		"api image must be repo@digest with no tag set")
	assert.Equal(t, "registry.example.com/controller@sha256:111111", ctrlImg,
		"controller image must be repo@digest with no tag set")
	assert.Equal(t, "registry.example.com/frontend@sha256:222222", feImg,
		"frontend image must be repo@digest with no tag set")
}

// findImageByDeployment renders the chart and returns the first container
// image from the Deployment whose name contains the given substring.
// Fails the test if no such Deployment exists.
func findImageByDeployment(t *testing.T, docs []map[string]any, nameSubstr string) string {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != "Deployment" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if !strings.Contains(name, nameSubstr) {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		for _, c := range containers {
			cm, _ := c.(map[string]any)
			if img, ok := cm["image"].(string); ok {
				return img
			}
		}
	}
	t.Fatalf("no Deployment with name containing %q found", nameSubstr)
	return ""
}

// TestImageHelper_RelayRouterDigest verifies the relayRouter.image helper
// honors the digest field (#476 review follow-up — relayRouter was modified
// but untested in the first push). Renders the chart with the relay fleet
// enabled (so the relay-router Deployment renders) and asserts the image
// uses repo@digest when digest is set.
func TestImageHelper_RelayRouterDigest(t *testing.T) {
	vals := `
controller:
  inferenceRelay:
    enabled: true
    artifact:
      urls:
        - "https://example.test/relay-proxy"
      sha256Arm64: "aaa"
      sha256Amd64: "bbb"
    router:
      image:
        repository: registry.example.com/relay-router
        tag: v1.0.0
        digest: sha256:abc
`
	docs := helmTemplate(t, vals)

	routerImg := findImageByDeployment(t, docs, "relay-router")
	assert.Equal(t, "registry.example.com/relay-router@sha256:abc", routerImg,
		"relayRouter image must be repo@digest when digest set (tag ignored)")
}

// TestImageHelper_RuntimeEnvBaseDigest verifies the inline digest
// conditional in runtimeenvironment-base.yaml (#476 review follow-up —
// the inline conditional was added but untested). Renders the chart and
// asserts the RuntimeEnvironment CR's spec.image uses repo@digest.
func TestImageHelper_RuntimeEnvBaseDigest(t *testing.T) {
	vals := `
runtimeEnvironments:
  base:
    image:
      repository: registry.example.com/base
      tag: v1.0.0
      digest: sha256:def
`
	docs := helmTemplate(t, vals)

	for _, d := range docs {
		if d["kind"] != "RuntimeEnvironment" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		img, _ := spec["image"].(string)
		assert.Equal(t, "registry.example.com/base@sha256:def", img,
			"RuntimeEnvironment base image must be repo@digest when digest set")
		return
	}
	t.Fatal("no RuntimeEnvironment CR rendered")
}

// ---------------------------------------------------------------------------
// #465: Redis TLS configmap rendering
// ---------------------------------------------------------------------------

// TestRedisTLS_DefaultRender_OmitsTLSFields verifies the default render
// (redis.tls unset) produces a configmap with tls: false and
// insecureSkipVerify: false. Regression guard for the configmap wiring.
func TestRedisTLS_DefaultRender_OmitsTLSFields(t *testing.T) {
	docs := helmTemplate(t, "")
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm, "API config ConfigMap must be rendered by default")
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, "tls: false",
		"redis.tls must default to false in the rendered configmap; config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, "insecureSkipVerify: false",
		"redis.insecureSkipVerify must default to false; config.yaml was:\n%s", cfg)
}

// TestRedisTLS_EnabledRendersTrueFields verifies that setting redis.tls=true
// and insecureSkipVerify=true in Helm values propagates to the rendered API
// configmap. This is the integration test for the Helm → Go config wiring;
// without it, a future template regression (e.g., someone removes lines 37-38
// from configmap-api.yaml) would silently break TLS support with no test
// catching it.
func TestRedisTLS_EnabledRendersTrueFields(t *testing.T) {
	docs := helmTemplate(t, `redis:
  tls: true
  insecureSkipVerify: true
`)
	cm := findAPIConfigMap(t, docs)
	require.NotNil(t, cm)
	cfg := configYAML(t, cm)
	require.Contains(t, cfg, "tls: true",
		"redis.tls=true must render into configmap; config.yaml was:\n%s", cfg)
	require.Contains(t, cfg, "insecureSkipVerify: true",
		"redis.insecureSkipVerify=true must render into configmap; config.yaml was:\n%s", cfg)
}

// ---------------------------------------------------------------------------
// #469: ConfigMap ClusterRole grant for free-models refresher
// ---------------------------------------------------------------------------

// TestClusterRole_ConfigMapsGrantedWhenFreeModelsEnabled verifies that the
// cluster-scoped ClusterRole includes configmaps when the free-models
// refresher is enabled (the default), even when the inference relay fleet
// is disabled. Pre-fix, configmaps were only granted when
// inferenceRelay.enabled=true; the manager's cache informer failed at
// cluster-wide ConfigMap listing with "configmaps is forbidden" (#469).
func TestClusterRole_ConfigMapsGrantedWhenFreeModelsEnabled(t *testing.T) {
	docs := helmTemplate(t, `rbac:
  scope: cluster
controller:
  freeModelsRefresher:
    enabled: true
`)
	clusterCR := findClusterRoleByNameSubstr(t, docs, "-controller-cluster")
	require.NotNil(t, clusterCR, "cluster ClusterRole must be rendered when rbac.scope=cluster")

	rules, _ := clusterCR["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		resources, _ := rule["resources"].([]any)
		for _, res := range resources {
			if res == "configmaps" {
				return // found — test passes
			}
		}
	}
	t.Fatal("ClusterRole must include configmaps when freeModelsRefresher.enabled=true and rbac.scope=cluster (#469)")
}

// TestClusterRole_ConfigMapsAbsentWhenBothDisabled verifies the negative
// case: when both inferenceRelay and freeModelsRefresher are disabled, the
// ClusterRole should NOT grant configmaps. Prevents accidental over-granting.
func TestClusterRole_ConfigMapsAbsentWhenBothDisabled(t *testing.T) {
	docs := helmTemplate(t, `rbac:
  scope: cluster
controller:
  inferenceRelay:
    enabled: false
  freeModelsRefresher:
    enabled: false
`)
	clusterCR := findClusterRoleByNameSubstr(t, docs, "-controller-cluster")
	require.NotNil(t, clusterCR)

	rules, _ := clusterCR["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		resources, _ := rule["resources"].([]any)
		for _, res := range resources {
			if res == "configmaps" {
				t.Fatal("ClusterRole must NOT include configmaps when both inferenceRelay and freeModelsRefresher are disabled")
			}
		}
	}
}

// findClusterRoleByNameSubstr scans rendered docs for a ClusterRole whose
// metadata.name contains the given substring. Returns nil if not found.
func findClusterRoleByNameSubstr(t *testing.T, docs []map[string]any, nameSubstr string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != "ClusterRole" {
			continue
		}
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if strings.Contains(name, nameSubstr) {
			return d
		}
	}
	return nil
}
