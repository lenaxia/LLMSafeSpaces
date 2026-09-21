// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// kind_cluster_config_test.go — pins for the nightly's 2-node kind
// topology (run 35550849959 adjudication: three consecutive one-node
// capacity walls — AC-1c, AC-13 wave 2, AC-11 — is a class retirement).
// The nightly runs on a hosted runner whose single node tops out at
// ~10 standing workspace pods of requests; a second node doubles the
// allocatable ceiling. The SHARED local/kind-cluster.yaml stays
// single-node: the pool is calibrated on exactly that topology (dind
// nesting, #1244-class network sensitivities) and this change retires
// the nightly's wall class, not the pool's envelope.

import (
	"regexp"
	"strings"
	"testing"
)

const (
	kindConfigShared  = "kind-cluster.yaml"
	kindConfigNightly = "kind-cluster-nightly.yaml"
)

func kindNodeRoles(t *testing.T, path string) []string {
	t.Helper()
	src := mustRead(t, path)
	roles := regexp.MustCompile(`(?m)^\s*- role: (\S+)`).FindAllStringSubmatch(src, -1)
	if len(roles) == 0 {
		t.Fatalf("%s defines no kind nodes", path)
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, r[1])
	}
	return out
}

// TestNightlyKindConfig_TwoNodes pins the class retirement: the nightly
// cluster carries a second (worker) node alongside the control plane,
// both on the same digest-pinned node image (the kubeVersion floor
// coupling), and keeps the dind-escape-prevention settings the shared
// config carries (serviceSubnet 10.217, iptables kube-proxy) — the
// nightly's config is the shared shape PLUS a worker, never a fork.
func TestNightlyKindConfig_TwoNodes(t *testing.T) {
	roles := kindNodeRoles(t, kindConfigNightly)
	if len(roles) < 2 {
		t.Fatalf("the nightly kind config must define ≥2 nodes (class retirement for the AC-1c/AC-13/AC-11 capacity walls), got %d: %v", len(roles), roles)
	}
	hasControlPlane, hasWorker := false, false
	for _, r := range roles {
		if r == "control-plane" {
			hasControlPlane = true
		}
		if r == "worker" {
			hasWorker = true
		}
	}
	if !hasControlPlane || !hasWorker {
		t.Fatalf("the nightly config needs a control-plane and a worker node, got %v", roles)
	}
	// Both nodes on the SAME digest-pinned image — a floating worker
	// image would decouple the nightly's cluster version from the
	// chart's kubeVersion floor.
	imgs := regexp.MustCompile(`(?m)^\s*image: (\S+)`).FindAllStringSubmatch(mustRead(t, kindConfigNightly), -1)
	if len(imgs) < 2 {
		t.Fatalf("every node must pin its image explicitly, found %d", len(imgs))
	}
	for _, m := range imgs {
		if !strings.Contains(m[1], "@sha256:") {
			t.Fatalf("node image must stay digest-pinned, got %q", m[1])
		}
		if m[1] != imgs[0][1] {
			t.Fatalf("all nodes must use the same pinned image (worker %q != control-plane %q)", m[1], imgs[0][1])
		}
	}
	for _, pin := range []string{
		`serviceSubnet: "10.217.0.0/16"`,
		"mode: iptables",
		"config_path = \"/etc/containerd/certs.d\"",
	} {
		if !strings.Contains(mustRead(t, kindConfigNightly), pin) {
			t.Fatalf("the nightly config must keep the shared shape's %q — the nightly cluster is the shared shape plus a worker, never a fork", pin)
		}
	}
}

// TestSharedKindConfig_StaysSingleNode is the BLAST-RADIUS pin: the
// shared config serves the pool (calibrated single-node dind topology,
// #1244 sensitivities) and the weekly attachments run — this class
// retirement must not silently re-topologize them.
func TestSharedKindConfig_StaysSingleNode(t *testing.T) {
	roles := kindNodeRoles(t, kindConfigShared)
	if len(roles) != 1 || roles[0] != "control-plane" {
		t.Fatalf("local/kind-cluster.yaml must stay single-node (pool + weekly consumers are calibrated on it) — to give the nightly more nodes, edit kind-cluster-nightly.yaml; got roles %v", roles)
	}
}

// TestNightlyWorkflow_UsesNightlyKindConfig pins the wiring plus the
// node-generalizing loops: the nightly must create its cluster from the
// nightly config, and the registry wiring must iterate ALL nodes (a
// hardcoded single node would leave the worker unable to resolve the
// digest-pinned delivery refs).
func TestNightlyWorkflow_UsesNightlyKindConfig(t *testing.T) {
	src := mustRead(t, us70NightlyWorkflow)
	if !strings.Contains(src, "--config local/kind-cluster-nightly.yaml") {
		t.Fatal("the nightly must create its cluster from local/kind-cluster-nightly.yaml (the 2-node class retirement)")
	}
	if strings.Contains(src, "--config local/kind-cluster.yaml ") {
		t.Fatal("the nightly must not create from the shared single-node config")
	}
	if !strings.Contains(src, "for node in $(kind get nodes --name $CLUSTER_NAME)") {
		t.Fatal("the registry certs.d wiring must iterate every node (the worker needs the hosts.toml alias for digest-pinned delivery refs)")
	}
}
