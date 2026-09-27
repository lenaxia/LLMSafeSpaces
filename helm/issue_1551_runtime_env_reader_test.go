// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// issue_1551_runtime_env_reader_test.go — #1551: the workspace
// runtime resolver (controller/internal/workspace/runtime_resolver.go)
// reads RuntimeEnvironment through the manager's CACHED client
// (Get at :40/:52, List at :66; called from pod_builder.go:24 on
// every workspace pod build). RuntimeEnvironment is a CLUSTER-scoped
// CRD (helm/crds/runtimeenvironment.yaml:13) — a namespaced Role
// cannot grant it, and F1.3.4 correctly removed the grant from the
// API Role — so under the MANDATED rbac.scope=namespace posture the
// read is granted NOWHERE: the runtimeenvironments rule exists only
// inside the $isCluster block (rbac.yaml's controller-cluster
// ClusterRole). The informer is lazy (no boot-time watch — which is
// exactly why the posture gate's cold-boot assertions all passed),
// so the first workspace create/update with a named runtime wedges
// the reconcile worker silently: the cached Get waits for an informer
// sync whose LIST is 403'd forever, the pod is never built, and
// nothing logs. rbac.yaml's own fleet guard names the class ("#1551
// family").
//
// The fix is the §8.1 precedent (the relay-safe-crd-watch
// ClusterRole, design 0061 §5): an ALWAYS-created (namespace-scope),
// read-only, no-Secrets ClusterRole + ClusterRoleBinding granting
// get/list/watch on runtimeenvironments ONLY. Cluster installs keep
// their broader block (which already grants it).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findRuntimeEnvReader locates the runtime-env-reader ClusterRole and
// its binding in a rendered doc set.
func findRuntimeEnvReader(docs []map[string]any) (cr map[string]any, binding map[string]any) {
	for _, d := range docs {
		meta, ok := d["metadata"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := meta["name"].(string)
		if !strings.HasSuffix(name, "controller-runtime-env-reader") {
			continue
		}
		if d["kind"] == "ClusterRole" {
			cr = d
		}
		if d["kind"] == "ClusterRoleBinding" {
			binding = d
		}
	}
	return cr, binding
}

// TestRuntimeEnvReaderClusterRoleNamespaceScope — RED against the
// unfixed chart (no grant anywhere under namespace scope), GREEN with
// the §8.1 reader: the wedge class becomes a render failure.
func TestRuntimeEnvReaderClusterRoleNamespaceScope(t *testing.T) {
	docs := helmTemplate(t, "") // default scope = namespace
	cr, binding := findRuntimeEnvReader(docs)
	require.NotNil(t, cr, "the runtime-env-reader ClusterRole must render under the DEFAULT (namespace) posture — without it the runtime resolver's cached Get wedges the first named-runtime reconcile (#1551: RuntimeEnvironment is cluster-scoped, so no namespaced Role can grant the read)")
	require.NotNil(t, binding, "and its ClusterRoleBinding (a ClusterRole grants nothing unbound)")

	rules, _ := cr["rules"].([]any)
	require.NotEmpty(t, rules, "the reader must carry the runtimeenvironments rule")
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		ags, _ := m["apiGroups"].([]any)
		require.Len(t, ags, 1)
		assert.Equal(t, "llmsafespaces.dev", ags[0], "the CRD's actual group (helm/crds/)")
		res, _ := m["resources"].([]any)
		require.NotEmpty(t, res)
		for _, rr := range res {
			s, _ := rr.(string)
			assert.Equal(t, "runtimeenvironments", s,
				"the reader touches ONLY runtimeenvironments — no Secrets (§4.3), no subresources, no other CRDs")
		}
		for v := range verbSet(m) {
			assert.Contains(t, []string{"get", "list", "watch"}, v,
				"read-only verbs only — the resolver does Get and List; writes stay wherever the posture put them")
		}
	}

	// The binding wires the CONTROLLER's service account in the release
	// namespace (the relay-safe precedent's subject shape).
	subjects, _ := binding["subjects"].([]any)
	require.Len(t, subjects, 1)
	subj, _ := subjects[0].(map[string]any)
	assert.Equal(t, "ServiceAccount", subj["kind"])
	saName, _ := subj["name"].(string)
	assert.True(t, strings.Contains(saName, "controller"),
		"the subject is the controller's service account: %v", saName)
	ns, _ := subj["namespace"].(string)
	assert.Equal(t, "test-ns", ns, "the release namespace")
}

// TestRuntimeEnvReaderAbsentClusterScope — cluster installs already
// grant the read inside the controller-cluster block; a second
// always-created reader there would be duplicate surface. Relay-only
// must be pinned off for the render (the §4.3 no-read guard refuses
// relay-on + cluster scope at render; the runbook's documented
// cluster-scope remedy).
func TestRuntimeEnvReaderAbsentClusterScope(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: cluster\nrelayOnlyKeyDelivery:\n  enabled: false\n")
	cr, binding := findRuntimeEnvReader(docs)
	assert.Nil(t, cr, "under cluster scope the broader controller-cluster block already grants runtimeenvironments — the reader is namespace-posture-only")
	assert.Nil(t, binding, "and its binding")
}
