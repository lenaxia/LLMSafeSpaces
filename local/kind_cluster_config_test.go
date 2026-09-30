// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// kind_cluster_config_test.go — pins for the nightly's 2-node kind
// topology (run 35550849959 adjudication: three consecutive one-node
// capacity walls — AC-1c, AC-13 wave 2, AC-11 — is a class retirement).
// The nightly runs on a hosted runner whose single node tops out at
// ~10 standing workspace pods of requests; a second node doubles the
// allocatable ceiling ONLY with the control-plane untaint (kind strips
// that taint on single-node clusters only — see
// TestNightlyKindConfig_ControlPlaneUntainted). The SHARED
// local/kind-cluster.yaml stays single-node: the pool is calibrated on
// exactly that topology (dind nesting, #1244-class network
// sensitivities) and this change retires the nightly's wall class, not
// the pool's envelope.

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
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

// TestNightlyKindConfig_ControlPlaneUntainted pins the capacity
// completion for the 2-node retirement: kind only strips the
// control-plane taint on SINGLE-node clusters (kind v0.32.0
// pkg/cluster/internal/create/actions/kubeadminit/init.go:124-155 runs
// `kubectl taint nodes --all node-role.kubernetes.io/control-plane-`
// under "if we are only provisioning one node"). On the nightly's 2-node
// cluster the control-plane kept node-role.kubernetes.io/control-plane
// :NoSchedule, so every pod — workspaces and infra alike — piled onto
// the worker and the promised allocatable doubling never materialized
// (nightly runs 36135708380…36740521434: the M2/M4 leg's chain-end
// workspace pod pended forever on FailedScheduling "1 Insufficient cpu,
// 1 node(s) had untolerated taint(s)").
//
// The pin is YAML-parsed, not text-matched, because kind v0.32.0
// SILENTLY DISCARDS patches that do not match a generated document
// (pkg/internal/patch/kubeyaml.go drops the matches bool): a drifted
// patch — most plausibly one "corrected" to declare an explicit
// apiVersion that differs from the generated template's (kind selects
// ConfigTemplateBetaV3 for node images < v1.36.0, kubeadm/config.go:677)
// — would leave a text pin green while kind no-ops the patch and the
// original failure returns. The asserted document must therefore be
// EXACTLY {kind: InitConfiguration, nodeRegistration: {taints: []}}:
// apiVersion ABSENT (the missing-apiVersion wildcard is what makes the
// patch version-proof), and taints an explicit EMPTY LIST (`taints:
// null` re-defaults to the control-plane taint; `taints: []` is
// kubeadm's documented untaint form — v1beta3/v1beta4
// NodeRegistrationOptions.Taints carry textually identical guidance).
func TestNightlyKindConfig_ControlPlaneUntainted(t *testing.T) {
	wantUntaint := map[string]interface{}{
		"kind": "InitConfiguration",
		"nodeRegistration": map[string]interface{}{
			"taints": []interface{}{},
		},
	}
	found := false
	for _, doc := range parsedKubeadmPatches(t, kindConfigNightly) {
		if reflect.DeepEqual(doc, wantUntaint) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the nightly kind config must carry, under kubeadmConfigPatches, a document EXACTLY equal to %v — no apiVersion (kind wildcards the match on a missing one and silently discards non-matching patches) and an explicit empty taints list (null re-defaults to the control-plane taint). Without a matching untaint patch kind keeps multi-node control-planes tainted and the second node contributes zero schedulable capacity", wantUntaint)
	}
	// Blast radius: the SHARED config must carry NO InitConfiguration
	// patch at all. It is single-node, where kind itself already strips
	// the taint at init — the pool's topology stays exactly as
	// calibrated.
	for _, doc := range parsedKubeadmPatches(t, kindConfigShared) {
		if doc["kind"] == "InitConfiguration" {
			t.Fatal("local/kind-cluster.yaml must stay single-node-stock: kind already untaints single-node control-planes, and the pool's calibrated topology must not drift")
		}
	}
}

type kindClusterConfigForPatches struct {
	KubeadmConfigPatches []string `json:"kubeadmConfigPatches"`
}

// parsedKubeadmPatches parses a kind cluster config and returns every
// kubeadmConfigPatches entry as a decoded document, failing the test on
// any parse error (a config that stops parsing is itself a regression —
// kind fails cluster creation on it).
func parsedKubeadmPatches(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	var cfg kindClusterConfigForPatches
	if err := yaml.Unmarshal([]byte(mustRead(t, path)), &cfg); err != nil {
		t.Fatalf("%s: kind config failed to parse as YAML: %v", path, err)
	}
	docs := make([]map[string]interface{}, 0, len(cfg.KubeadmConfigPatches))
	for i, patch := range cfg.KubeadmConfigPatches {
		var doc map[string]interface{}
		if err := yaml.Unmarshal([]byte(patch), &doc); err != nil {
			t.Fatalf("%s: kubeadmConfigPatches[%d] failed to parse: %v", path, i, err)
		}
		docs = append(docs, doc)
	}
	return docs
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
