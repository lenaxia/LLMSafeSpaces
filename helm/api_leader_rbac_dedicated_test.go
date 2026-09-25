// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

// The #1566 peel's move (a): the api-leader-election Role+RoleBinding
// live in a DEDICATED template file because, in rbac.yaml's multi-doc
// tail, the Binding rendered and was stored in helm's manifest but
// NEVER reached the cluster (sibling docs from the same file applied;
// version-independent 3.21.4/3.22.0). The posture gate now REQUIRES the
// binding live post-install; this pin is the render-layer half of that
// guard — if the pair leaves api-leader-rbac.yaml (or the render drops
// either doc), this fails here instead of waiting for the kind gate.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func findByKindAndSuffix(t *testing.T, docs []map[string]any, kind, nameSuffix string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if d["kind"] != kind {
			continue
		}
		if meta, ok := d["metadata"].(map[string]any); ok {
			if name, _ := meta["name"].(string); strings.HasSuffix(name, nameSuffix) {
				return d
			}
		}
	}
	return nil
}

func TestAPILeaderElectionRBAC_RenderedInDedicatedFile(t *testing.T) {
	docs := helmTemplate(t, "")

	role := findByKindAndSuffix(t, docs, "Role", "-api-leader-election")
	require.NotNil(t, role, "the api-leader-election Role must render (the dedicated file, #1566 move a)")

	binding := findByKindAndSuffix(t, docs, "RoleBinding", "-api-leader-election")
	require.NotNil(t, binding,
		"the api-leader-election RoleBinding must render — this exact doc was the apply-loss victim in rbac.yaml's tail (manifest-carried, cluster-never-received)")

	// The binding must actually bind the API SA to THIS Role — a
	// roleRef drift is a dead grant that renders green.
	roleRef, _ := binding["roleRef"].(map[string]any)
	require.NotNil(t, roleRef)
	assert.Equal(t, "Role", roleRef["kind"])
	roleName, _ := role["metadata"].(map[string]any)["name"].(string)
	assert.Equal(t, roleName, roleRef["name"], "the binding must reference the rendered api-leader-election Role by name")

	subj, _ := binding["subjects"].([]any)
	require.NotEmpty(t, subj)
	first, _ := subj[0].(map[string]any)
	require.NotNil(t, first)
	if assert.Equal(t, "ServiceAccount", first["kind"]) {
		assert.Contains(t, first["name"], "api", "the subject is the API service account (the lease-starved SA of the peel)")
	}

	// The Role's lease verbs must survive the file move intact.
	rules, _ := role["rules"].([]any)
	lease := ruleFor(rules, func(m map[string]any) bool {
		vs := verbSet(m)
		res, _ := m["resources"].([]any)
		for _, r := range res {
			if s, ok := r.(string); ok && s == "leases" && vs["get"] && vs["update"] {
				return true
			}
		}
		return false
	})
	assert.NotNil(t, lease, "the lease get/update rule must render — leader election dies without it")
}

// The dedicated file must honor the same rbac.create gate as the rest
// of the RBAC surface (an always-rendering file would drift from the
// chart's posture contract).
func TestAPILeaderElectionRBAC_AbsentWhenRbacCreateFalse(t *testing.T) {
	docs := helmTemplate(t, "rbac:\n  create: false\n")
	assert.Nil(t, findByKindAndSuffix(t, docs, "Role", "-api-leader-election"),
		"rbac.create=false must suppress the dedicated api-leader-election Role")
	assert.Nil(t, findByKindAndSuffix(t, docs, "RoleBinding", "-api-leader-election"),
		"rbac.create=false must suppress the dedicated api-leader-election RoleBinding")
}
