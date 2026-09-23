// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// The #1546 posture RBAC fixes (design 0061 §5, the posture gate's first
// catch): each shipped fix is render-pinned here, plus the flag↔RBAC
// key-coupling pin (the Defect-3 replacement — the desync class becomes
// a test failure, not a posture question).

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routerSecretsRole(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == "Role" {
			if meta, ok := d["metadata"].(map[string]any); ok && meta["name"] == "llm-relay-router-secrets" {
				return d
			}
		}
	}
	return nil
}

func ruleFor(rules []any, predicate func(map[string]any) bool) map[string]any {
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if predicate(m) {
			return m
		}
	}
	return nil
}

func verbSet(rule map[string]any) map[string]bool {
	out := map[string]bool{}
	if rule == nil {
		return out
	}
	vs, _ := rule["verbs"].([]any)
	for _, v := range vs {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out
}

func hasResourceName(rule map[string]any, name string) bool {
	if rule == nil {
		return false
	}
	ns, _ := rule["resourceNames"].([]any)
	for _, n := range ns {
		if s, ok := n.(string); ok && s == name {
			return true
		}
	}
	return false
}

// Defect 1 (design 0061 §5): the apiserver IGNORES resourceNames for
// `create` — the keypair bootstrap rule must be SPLIT: unscoped create
// on secrets (the router is the llm-relay Secrets key owner with
// get/list/watch already — no meaningful surface added) + name-scoped
// update for exactly the two keypair Secrets (the #863/#890 agentd-pins
// precedent the chart already documents).
func TestRelayRouterKeypairRuleSplit(t *testing.T) {
	docs := helmTemplate(t, "relayOnlyKeyDelivery:\n  enabled: true\n")
	role := routerSecretsRole(t, docs)
	require.NotNil(t, role, "the router secrets Role must render under relay-only")

	rules, _ := role["rules"].([]any)

	// No rule may combine create with resourceNames (the dead letter).
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		vs := verbSet(m)
		if vs["create"] {
			assert.Empty(t, m["resourceNames"],
				"a create rule with resourceNames is dead RBAC — the apiserver ignores resourceNames for create (#1546 Defect 1)")
		}
	}

	// The unscoped create exists — on SECRETS in the core group (the
	// verbs-on-wrong-resource drift class, r1 finding 3).
	create := ruleFor(rules, func(m map[string]any) bool {
		vs := verbSet(m)
		return vs["create"] && !vs["update"] && onSecrets(m)
	})
	require.NotNil(t, create, "the split unscoped create rule must exist (on secrets)")

	// The name-scoped update covers exactly the two keypair Secrets.
	update := ruleFor(rules, func(m map[string]any) bool {
		vs := verbSet(m)
		return vs["update"] && !vs["create"] && onSecrets(m)
	})
	require.NotNil(t, update, "the split name-scoped update rule must exist (on secrets)")
	assert.True(t, hasResourceName(update, "llm-relay-hpke-key"))
	assert.True(t, hasResourceName(update, "llm-relay-hpke-pub"))
	ns, _ := update["resourceNames"].([]any)
	assert.Len(t, ns, 2, "the update rule covers exactly the two keypair Secrets")
}

// Defect 2: with delivery pins set, the first CACHED ConfigMap Get
// starts a reflector that LISTs configmaps in the release namespace —
// the pins Role's resourceNames-scoped get/update never granted
// list/watch. The pins blocks must carry the informer's actual verbs.
func TestPinsRoleGrantsInformerListWatch(t *testing.T) {
	for _, pin := range []string{"agentdDelivery", "opencodeDelivery"} {
		t.Run(pin, func(t *testing.T) {
			// freeModelsRefresher defaults ON and its grant would
			// vacuously satisfy the predicate — render it OFF (and the
			// fleet off) so ONLY the pins grants are in play.
			docs := helmTemplate(t, "controller:\n  "+pin+":\n    image: registry.test/x:tag\n    binarySHA256Amd64: \"aaa\"\n    binarySHA256Arm64: \"bbb\"\n  freeModelsRefresher:\n    enabled: false\n  inferenceRelay:\n    enabled: false\n")
			role := leaderElectionRole(t, docs)
			require.NotNil(t, role, "the leader-election Role (the pins Role) must render")

			rules, _ := role["rules"].([]any)
			lw := ruleFor(rules, func(m map[string]any) bool {
				vs := verbSet(m)
				res, _ := m["resources"].([]any)
				cm := false
				for _, r := range res {
					if s, ok := r.(string); ok && s == "configmaps" {
						cm = true
					}
				}
				return cm && vs["list"] && vs["watch"] && vs["get"]
			})
			if assert.NotNil(t, lw,
				"the %s pins block must grant get/list/watch on configmaps — the cached informer LISTs on first Get (#1546 Defect 2)", pin) {
				assert.Empty(t, lw["resourceNames"],
					"the informer grant must be UNSCOPED — an informer LIST cannot be resourceNames-scoped (the same dead-RBAC semantics as create, r1 finding 2)")
			}
		})
	}
}

func onSecrets(rule map[string]any) bool {
	if rule == nil {
		return false
	}
	res, _ := rule["resources"].([]any)
	secs := false
	for _, r := range res {
		if s, ok := r.(string); ok && s == "secrets" {
			secs = true
		}
	}
	ags, _ := rule["apiGroups"].([]any)
	core := false
	for _, a := range ags {
		if s, ok := a.(string); ok && s == "" {
			core = true
		}
	}
	return secs && core
}

// The ClusterRole renders UNCONDITIONALLY under namespace scope (the
// design §8.1 letter: the posture never depends on the fleet flag).
func TestInferenceRelayClusterRoleUnconditional(t *testing.T) {
	docs := helmTemplate(t, "controller:\n  inferenceRelay:\n    enabled: false\n") // default scope = namespace
	for _, d := range docs {
		if d["kind"] == "ClusterRole" {
			if meta, ok := d["metadata"].(map[string]any); ok {
				if name, _ := meta["name"].(string); strings.HasSuffix(name, "relay-safe-crd-watch") {
					return // present with the fleet OFF — unconditional
				}
			}
		}
	}
	t.Fatal("the relay-safe ClusterRole must render with the fleet DISABLED (unconditional — the posture never depends on the fleet flag)")
}

func leaderElectionRole(t *testing.T, docs []map[string]any) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] == "Role" {
			if meta, ok := d["metadata"].(map[string]any); ok {
				name, _ := meta["name"].(string)
				if strings.HasSuffix(name, "controller-leader-election") {
					return d
				}
			}
		}
	}
	return nil
}

// The design gap (§8.1): inferenceRelay (a cluster-scoped CRD watch)
// cannot coexist with the namespace posture — the grant exists only
// inside the opt-in cluster block, which the relay-only guard refuses.
// The fix: an always-created, relay-only-safe ClusterRole (inferencerelays
// get/list/watch ONLY — a CRD read, no Secrets, no §4.3 conflict) that
// renders under NAMESPACE scope whenever inferenceRelay is enabled.
func TestInferenceRelayNamespaceScopeClusterRole(t *testing.T) {
	docs := helmTemplate(t, "controller:\n  inferenceRelay:\n    enabled: true\n") // default scope = namespace
	var cr map[string]any
	var binding map[string]any
	for _, d := range docs {
		if meta, ok := d["metadata"].(map[string]any); ok {
			name, _ := meta["name"].(string)
			if !strings.HasSuffix(name, "relay-safe-crd-watch") {
				continue
			}
			if d["kind"] == "ClusterRole" {
				cr = d
			}
			if d["kind"] == "ClusterRoleBinding" {
				binding = d
			}
		}
	}
	require.NotNil(t, cr, "the relay-safe inferencerelays ClusterRole must render under namespace scope with the fleet enabled (the #1546 design gap)")
	require.NotNil(t, binding, "and its binding")

	rules, _ := cr["rules"].([]any)
	require.NotEmpty(t, rules)
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		// apiGroups pinned EXACTLY (r1 finding 1: a wrong apiGroup renders
		// a dead grant — the forbidden flood returns — and nothing else
		// catches it; this is the CRD's actual group, helm/crds/).
		ags, _ := m["apiGroups"].([]any)
		require.Len(t, ags, 1)
		assert.Equal(t, "llmsafespaces.dev", ags[0])
		res, _ := m["resources"].([]any)
		for _, rr := range res {
			if s, ok := rr.(string); ok {
				assert.Contains(t, []string{"inferencerelays", "inferencerelays/status", "inferencerelays/finalizers"}, s,
					"the relay-safe ClusterRole touches ONLY the CRD and its subresources — never Secrets (§4.3)")
			}
		}
		vs := verbSet(m)
		for v := range vs {
			assert.Contains(t, []string{"get", "list", "watch", "update"}, v,
				"the reconciler's CRD-lifecycle verbs only (watch + finalizer/status writes)")
		}
	}
	// The binding's subject is the CONTROLLER SA (r1 finding 4, the
	// api-inferencerelay precedent).
	subj, _ := binding["subjects"].([]any)
	require.NotEmpty(t, subj)
	first, _ := subj[0].(map[string]any)
	require.NotNil(t, first)
	assert.Equal(t, "ServiceAccount", first["kind"])
	assert.Contains(t, first["name"], "controller")
}

// The flag↔RBAC key-coupling pin (design 0061 §5's Defect-3
// replacement): the refresher flag and its RBAC render from the SAME
// values key — for BOTH settings, the configmap-write grant and the pod
// arg correlate. A desync (grant without flag, flag without grant)
// becomes this test's failure, not a live posture question.
func TestFreeModelsRefresherFlagRBACKeyCoupling(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		val := "false"
		if enabled {
			val = "true"
		}
		t.Run("enabled="+val, func(t *testing.T) {
			docs := helmTemplate(t, "controller:\n  freeModelsRefresher:\n    enabled: "+val+"\n  inferenceRelay:\n    enabled: false\n")
			role := leaderElectionRole(t, docs)
			require.NotNil(t, role)
			rules, _ := role["rules"].([]any)
			grant := ruleFor(rules, func(m map[string]any) bool {
				vs := verbSet(m)
				res, _ := m["resources"].([]any)
				cm := false
				for _, r := range res {
					if s, ok := r.(string); ok && s == "configmaps" {
						cm = true
					}
				}
				return cm && vs["patch"] && vs["update"] // the refresher's write-grant shape (patch+update on CONFIGMAPS — the leases rule also carries both, on leases)
			})
			if enabled {
				assert.NotNil(t, grant, "flag on ⇒ the write grant renders")
			} else {
				assert.Nil(t, grant, "flag off ⇒ NO write grant (the key coupling)")
			}
			dep := controllerDeployment(t, docs)
			args := controllerArgs(t, dep)
			sawArg := false
			for _, a := range args {
				if a == "--enable-free-models-refresher=false" {
					sawArg = true
				}
			}
			if enabled {
				assert.False(t, sawArg, "flag on ⇒ no disable arg (the default is on)")
			} else {
				assert.True(t, sawArg, "flag off ⇒ the disable arg renders (the same key)")
			}
		})
	}
}
