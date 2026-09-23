// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// rbac_cache_scope_chart_test.go — the namespace-scope → cache-scoping
// derivation (nightly run 35872827066, the drill-shape step's first-ever
// execution).
//
// The latent bug this pins shut: under rbac.scope=namespace the chart
// deletes the ClusterRole/Binding (the workspace-namespace Role covers
// one namespace), but the controller's cache scoping came ONLY from
// --watch-namespaces, which the chart emitted only when the operator
// hand-set controller.watchNamespaces. Unset (the default) → the
// manager starts a CLUSTER-WIDE cache whose Secret/ServiceAccount/Pod/
// PVC/Workspace informers need list/watch across ALL namespaces →
// Forbidden outside the workspace namespace → "Could not wait for Cache
// to sync" → CrashLoopBackOff (three restarts inside the 10m helm
// --wait; run 35872827066's failure dump). The runbook's own prescribed
// cluster→namespace migration path therefore crashed on its first-ever
// real exercise — and the chart's DEFAULT posture (namespace scope
// since G5) carries the same defect. The derivation: namespace scope +
// watchNamespaces unset → default --watch-namespaces to the workspace
// namespace (the workspaceNamespace helper).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// controllerWatchNamespacesArg extracts the --watch-namespaces value
// from the controller Deployment's args ("" when absent).
func controllerWatchNamespacesArg(t *testing.T, docs []map[string]any) string {
	t.Helper()
	dep := findDeploymentByNameSubstr(docs, "llmsafespaces-controller")
	require.NotNil(t, dep, "controller Deployment must render")
	podSpec, ok := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	require.True(t, ok)
	containers, ok := podSpec["containers"].([]any)
	require.True(t, ok && len(containers) > 0)
	args, ok := containers[0].(map[string]any)["args"].([]any)
	require.True(t, ok)
	for _, a := range args {
		s, isStr := a.(string)
		if isStr && strings.HasPrefix(s, "--watch-namespaces=") {
			return strings.TrimPrefix(s, "--watch-namespaces=")
		}
	}
	return ""
}

// TestRBACNamespaceScope_DerivesWatchNamespaces (the fix): namespace
// scope (the chart default) + watchNamespaces unset → the controller
// args MUST carry --watch-namespaces=<workspaceNamespace> (the release
// namespace by default). Without the derivation the namespace-scope
// posture crashloops on informer-sync — nightly run 35872827066.
func TestRBACNamespaceScope_DerivesWatchNamespaces(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: namespace\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Equal(t, "test-ns", got,
		"rbac.scope=namespace MUST derive --watch-namespaces to the workspace namespace — without it the manager runs a cluster-wide cache against namespaced RBAC and crashloops on informer sync (run 35872827066)")
}

// TestRBACNamespaceScope_CustomWorkspaceNamespace: the derivation
// follows the workspaceNamespace helper's contract — an explicit
// api.config.kubernetes.namespace wins over the release namespace.
func TestRBACNamespaceScope_CustomWorkspaceNamespace(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: namespace\napi:\n  config:\n    kubernetes:\n      namespace: sandboxes\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Equal(t, "sandboxes", got,
		"the derived --watch-namespaces must follow the workspaceNamespace helper (api.config.kubernetes.namespace override)")
}

// TestRBACNamespaceScope_ExplicitWatchNamespacesWins: an operator-set
// controller.watchNamespaces passes through verbatim — the derivation
// is a DEFAULT, never a clobber.
func TestRBACNamespaceScope_ExplicitWatchNamespacesWins(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: namespace\ncontroller:\n  watchNamespaces: \"tenant-a,tenant-b\"\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Equal(t, "tenant-a,tenant-b", got,
		"explicit controller.watchNamespaces must win over the namespace-scope derivation")
}

// TestRBACClusterScope_NoDerivedWatchNamespaces (the off-path
// regression pin — byte-identical cluster-scope renders): under
// rbac.scope=cluster with watchNamespaces unset, NO --watch-namespaces
// arg may appear — the cluster posture keeps its cluster-wide cache by
// design and the derivation must not leak across the scope boundary.
func TestRBACClusterScope_NoDerivedWatchNamespaces(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: cluster\nrelayOnlyKeyDelivery:\n  enabled: false\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Empty(t, got,
		"rbac.scope=cluster with watchNamespaces unset must render WITHOUT --watch-namespaces — the cluster posture's cache is cluster-wide by design; the namespace-scope derivation must not leak (byte-identical off-path)")
}

// TestRBACClusterScope_ExplicitWatchNamespacesStillHonored: the
// cluster-scope path keeps its pre-existing behavior for EXPLICIT
// watchNamespaces (opt-in cache scoping inside cluster scope) —
// unchanged by the derivation.
func TestRBACClusterScope_ExplicitWatchNamespacesStillHonored(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: cluster\nrelayOnlyKeyDelivery:\n  enabled: false\ncontroller:\n  watchNamespaces: \"only-this-one\"\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Equal(t, "only-this-one", got,
		"explicit watchNamespaces under cluster scope must keep passing through (pre-existing behavior, unchanged)")
}

// TestRBACClusterScope_WatchAllStillValid (r1): "*" under CLUSTER scope
// stays the valid explicit watch-all spelling — the r1 fail guard must
// scope to the namespace posture only.
func TestRBACClusterScope_WatchAllStillValid(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  scope: cluster\nrelayOnlyKeyDelivery:\n  enabled: false\ncontroller:\n  watchNamespaces: \"*\"\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Equal(t, "*", got,
		"\"*\" remains the valid watch-all spelling under cluster scope (the controller parses it to a cluster-wide cache, which the ClusterRole there covers)")
}

// TestRBACNamespaceScope_WatchAllFailsRender (r1): "*" (or a
// whitespace-only value) under namespace scope is INCOHERENT — it
// parses to a cluster-wide cache that cannot sync informers against
// namespaced RBAC (the exact run-35872827066 crashloop) — so the render
// must FAIL LOUDLY (the llm-relay-rbac convention) naming both
// remedies. The migration population uses precisely this spelling; a
// verbatim pass-through would deploy a controller that cannot start.
func TestRBACNamespaceScope_WatchAllFailsRender(t *testing.T) {
	for _, value := range []string{"*", "  ", "\\t"} {
		t.Run("value="+value, func(t *testing.T) {
			err := helmTemplateErr(t, "rbac:\n  scope: namespace\ncontroller:\n  watchNamespaces: \""+value+"\"\n")
			require.Error(t, err,
				"watchNamespaces=%q under namespace scope must fail the render — a watch-all cache crashloops against namespaced RBAC", value)
			msg := err.Error()
			assert.Contains(t, msg, "rbac.scope=namespace", "the failure must name the incoherent combination")
			assert.Contains(t, msg, "CrashLoopBackOff", "the failure must name the consequence")
			assert.Contains(t, msg, "controller.watchNamespaces=", "the failure must name the setting to change")
			assert.Contains(t, msg, "rbac.scope=cluster", "the failure must name the keep-cluster remedy")
		})
	}
}

// TestRBACNamespaceScope_WatchAllGuardLeakShapes (r2): the guard
// mirrors the controller's parseWatchNamespaces — embedded "*"
// ("*,"/"*,ns1"/"**") parses to cluster-wide or a bogus "*" NAMESPACE,
// and Go-TrimSpace whitespace classes beyond ASCII space/tab
// (newline, NBSP) trim to cluster-wide. Every shape crashloops
// identically under namespace scope; every one must fail the render.
//
// r4: subtest names are INDEX-BASED — raw operator input (commas!)
// must never reach t.Run names: the name lands in t.TempDir()'s path,
// and helm's -f is a pflag StringSlice that splits on the comma — the
// truncated path then errors as a HARNESS failure and require.Error
// passes vacuously without evaluating any template (the r3 pins were
// exactly that; mutation-verified red after the fix).
func TestRBACNamespaceScope_WatchAllGuardLeakShapes(t *testing.T) {
	for i, value := range []string{"*,", "*,ns1", "**", "\\n", "\\u00a0"} {
		t.Run(fmt.Sprintf("case%02d", i), func(t *testing.T) {
			err := helmTemplateErr(t, "rbac:\n  scope: namespace\ncontroller:\n  watchNamespaces: \""+value+"\"\n")
			require.Error(t, err,
				"watchNamespaces=%q under namespace scope must fail the render (the run-35872827066 death class)", value)
			require.Contains(t, err.Error(), "CrashLoopBackOff",
				"the error must be the guard's template fail, not a harness failure (the r4 vacuity class)")
		})
	}
}

// TestRBACNamespaceScope_CommaCollapseFailsRender (r3, de-vacuumed r4):
// the nil branch of parseWatchNamespaces (watch_namespaces.go:38-40,
// pinned by the repo's own TestParseWatchNamespaces_AllEmptyEntriesReturnsNil)
// — a value whose comma-split yields ZERO non-empty entries collapses
// to nil = cluster-wide cache. No "*", not whitespace-only: the r2
// guard passed them verbatim into the crashloop. All must fail the
// render — and the subtest names are index-based because the raw
// values contain commas (see the r4 note above).
func TestRBACNamespaceScope_CommaCollapseFailsRender(t *testing.T) {
	for i, value := range []string{",", " , ", ",,,"} {
		t.Run(fmt.Sprintf("case%02d", i), func(t *testing.T) {
			err := helmTemplateErr(t, "rbac:\n  scope: namespace\ncontroller:\n  watchNamespaces: \""+value+"\"\n")
			require.Error(t, err,
				"watchNamespaces=%q collapses to zero namespaces (parseWatchNamespaces nil branch) — cluster-wide cache, the run-35872827066 death", value)
			require.Contains(t, err.Error(), "CrashLoopBackOff",
				"the error must be the guard's template fail, not a harness failure (the r4 vacuity class)")
		})
	}
	// The TrimSpace edge the r2 class missed: U+2028/U+2029 (Zl/Zp) are
	// trimmed by Go TrimSpace → nil → cluster-wide.
	for i, value := range []string{"\\u2028", "\\u2029"} {
		t.Run(fmt.Sprintf("unicode%02d", i), func(t *testing.T) {
			err := helmTemplateErr(t, "rbac:\n  scope: namespace\ncontroller:\n  watchNamespaces: \""+value+"\"\n")
			require.Error(t, err, "watchNamespaces=%q (Zl/Zp) trims to empty under Go TrimSpace — must fail the render", value)
			require.Contains(t, err.Error(), "CrashLoopBackOff",
				"the error must be the guard's template fail, not a harness failure (the r4 vacuity class)")
		})
	}
}

// TestRBACDefaultRender_DerivesWatchNamespaces (r3, the pure-defaults
// pin): NO rbac.scope key in the values at all — the `| default
// "namespace"` fallback must flow through the derivation identically.
// Every other pin sets scope explicitly, so a silent values-default
// flip would otherwise change the default render uncaught.
func TestRBACDefaultRender_DerivesWatchNamespaces(t *testing.T) {
	docs := helmTemplate(t, "controller:\n  watchNamespaces: \"\"\n")
	got := controllerWatchNamespacesArg(t, docs)
	assert.Equal(t, "test-ns", got,
		"the pure-defaults render (no rbac.scope key) must carry the derived --watch-namespaces — the | default \"namespace\" fallback is the path real default installs take")
}
