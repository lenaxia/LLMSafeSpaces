// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us72_reconciliation_pins_test.go — structural + executable pins for the
// US-72.5 reconciliation (PR #1536): the nightly wiring of the owner's
// drill (#1534) and the seven cluster-scope consumer relay-OFF pins.
// The TestUS70AC1D_MockEgressLeverInNightly precedents: a backslash
// continuation broken mid-step amputates every flag after it, and
// bash -n / YAML parsing / substring pins are ALL blind to that class —
// executing the real step body against a fake helm is not.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stepBody extracts a named step's `run: |` block from a workflow,
// strips the YAML indentation, and substitutes ${{ env.X }} refs with
// synthetic values (the AC-1D extraction, factored for reuse).
func stepBody(t *testing.T, wf, stepName string) string {
	t.Helper()
	src := mustRead(t, wf)
	at := strings.Index(src, stepName)
	if at < 0 {
		t.Fatalf("step %q not found in %s", stepName, wf)
	}
	step := src[at:]
	if end := strings.Index(step, "\n      - name: "); end >= 0 {
		step = step[:end]
	}
	runAt := strings.Index(step, "run: |")
	if runAt < 0 {
		t.Fatalf("step %q has no run: | block", stepName)
	}
	body := regexp.MustCompile(`(?m)^ {10}`).ReplaceAllString(step[runAt+len("run: |"):], "")
	return regexp.MustCompile(`\$\{\{ env\.([A-Za-z_]+) \}\}`).ReplaceAllString(body, "synthetic-$1")
}

// runWithFakes executes body under bash with fake helm/kubectl on PATH,
// recording their argv into files under dir. Returns the concatenated
// captured argv of BOTH binaries.
func runWithFakes(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	var argvs []string
	for _, bin := range []string{"helm", "kubectl"} {
		argsFile := filepath.Join(dir, bin+"-args")
		fake := "#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >> " + shQuote(argsFile) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, bin), []byte(fake), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := "set -euo pipefail; export PATH=" + shQuote(dir) + ":$PATH NS=llmsafespaces CLUSTER_NAME=ci IMAGE_TAG=ci\n" + body
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("step body must execute cleanly under bash -e — a broken backslash continuation makes the next flag a fresh command (exit %v):\n%s", err, out)
	}
	for _, bin := range []string{"helm", "kubectl"} {
		if raw, err := os.ReadFile(filepath.Join(dir, bin+"-args")); err == nil {
			argvs = append(argvs, string(raw))
		} else {
			t.Fatalf("%s was never invoked — the step body no longer calls it", bin)
		}
	}
	return strings.Join(argvs, "")
}

// Pin (a): the drill pre-step must hand helm EXACTLY the drill-shape
// flags (relay on, namespaced scope, the local router image) — the
// owner's drill flips only the relay flag, so the workflow must put the
// release into the valid shape first — and wait out both rollouts.
// EXECUTABLE: a mid-continuation comment or broken backslash silently
// drops flags after it (the AC-1D amputation class).
func TestUS72DrillPreStep_ExecutableFlagSet(t *testing.T) {
	argv := runWithFakes(t, stepBody(t, us70NightlyWorkflow, "Put the release into the drill's valid shape"))
	for _, want := range []string{
		"--reuse-values",
		"relayOnlyKeyDelivery.enabled=true",
		"rbac.scope=namespace",
		"controller.inferenceRelay.router.image.repository=llmsafespaces/relay-router",
		"controller.inferenceRelay.router.image.tag=ci", // $IMAGE_TAG from the step env (set to ci by the harness)
		"--wait",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("pre-step helm must receive %q — a continuation break silently drops it (captured argv: %s)", want, argv)
		}
	}
	for _, rollout := range []string{
		"rollout status deployment/llmsafespaces-controller",
		"rollout status deployment/llm-relay-router",
	} {
		if !strings.Contains(argv, rollout) {
			t.Errorf("pre-step must wait out %q (captured argv: %s)", rollout, argv)
		}
	}
}

// Pin (b): ordering — the router-image build precedes the drill; the
// drill runs AFTER the us-70 suite (its mock-llm Service dependency is
// created there) and BEFORE the failure-state dump.
func TestUS72Drill_NightlyOrdering(t *testing.T) {
	src := mustRead(t, us70NightlyWorkflow)
	idx := func(marker, what string) int {
		t.Helper()
		i := strings.Index(src, marker)
		if i < 0 {
			t.Fatalf("%s (%q) not found in the nightly workflow", what, marker)
		}
		return i
	}
	us70 := idx("Run secret-delivery e2e rows", "the us-70 suite step (creates mock-llm)")
	routerBuild := idx("Build + load the llm-relay router image", "the router image build step")
	drill := idx("bash local/us-72-relay-only-flip-drill.sh", "the drill step")
	dump := idx("Dump cluster state on failure", "the failure-dump step")
	if us70 >= drill || routerBuild >= drill || drill >= dump {
		t.Fatalf("ordering violated: us70=%d routerBuild=%d drill=%d dump=%d — the drill needs the mock-llm dependency and the router image BEFORE it, and must precede the dump",
			us70, routerBuild, drill, dump)
	}
}

// Pin (c): the router image the pre-step deploys must be the locally
// built one (the ghcr.io default is unreachable on kind) — built from
// the router Dockerfile and kind-loaded under the name the pre-step
// sets.
func TestUS72Drill_RouterImageBuildStep(t *testing.T) {
	src := mustRead(t, us70NightlyWorkflow)
	for _, want := range []string{
		"-f cmd/relay-router/Dockerfile -t llmsafespaces/relay-router:$IMAGE_TAG",
		"kind load docker-image llmsafespaces/relay-router:$IMAGE_TAG",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the nightly must contain %q — without the local build+load the drill's flipped release pulls an unreachable ghcr.io image", want)
		}
	}
}

// Pin (d): the hard regression gate — EVERY cluster-scope consumer
// install must carry the relay-OFF pin in the same command context (the
// #1534 flip made relay-on the chart default, and the chart refuses
// relay-on + rbac.scope=cluster at render; a silently dropped pin
// reintroduces the identical next-run install failure undetected).
// Table-driven over all seven sites: occurrences of the cluster-scope
// set must EQUAL occurrences of the relay pin — no site may keep a
// cluster install without its pin, and no orphan pins may hide a
// second unpinned install.
func TestUS72ClusterScopeConsumers_CarryRelayOffPin(t *testing.T) {
	type site struct {
		file    string
		cluster string // the cluster-scope marker syntax in this file
		relay   string // the relay-off pin syntax in this file
	}
	sites := []site{
		{filepath.Join("..", ".github", "workflows", "e2e-nightly.yml"), "--set rbac.scope=cluster", "--set relayOnlyKeyDelivery.enabled=false"},
		{filepath.Join("..", ".github", "workflows", "us-70-delivery-pool.yml"), "--set rbac.scope=cluster", "--set relayOnlyKeyDelivery.enabled=false"},
		{filepath.Join("..", ".github", "workflows", "e2e-attachments-single-container.yml"), "--set rbac.scope=cluster", "--set relayOnlyKeyDelivery.enabled=false"},
		{"../local/bootstrap.sh", "--set \"rbac.scope=cluster\"", "--set relayOnlyKeyDelivery.enabled=false"},
		{"../local/s5-overlay-validation.sh", "--set rbac.scope=cluster", "--set relayOnlyKeyDelivery.enabled=false"},
		{"../scripts/us2-kind-integration.sh", "--set rbac.scope=cluster", "--set relayOnlyKeyDelivery.enabled=false"},
		// values-cluster.yaml is a values FILE, not a --set command line.
		{"../values-cluster.yaml", "scope: cluster", "relayOnlyKeyDelivery:"},
	}
	for _, s := range sites {
		s := s
		t.Run(filepath.Base(s.file), func(t *testing.T) {
			raw, err := os.ReadFile(s.file)
			if err != nil {
				t.Fatal(err)
			}
			src := string(raw)
			clusterN := strings.Count(src, s.cluster)
			relayN := strings.Count(src, s.relay)
			if clusterN == 0 {
				t.Fatalf("%s no longer carries a cluster-scope install (%q absent) — update this pin table if the site's shape changed", s.file, s.cluster)
			}
			if relayN < clusterN {
				t.Fatalf("%s has %d cluster-scope install(s) but only %d relay-off pin(s) — the US-72.5 flip made relay-on the default and the chart REFUSES relay-on + cluster scope at render: the next install of this site fails (runbook 'Known interactions' remedy: pin relayOnlyKeyDelivery.enabled=false at every cluster-scope install)",
					s.file, clusterN, relayN)
			}
			if s.file == "../values-cluster.yaml" {
				// The values-file shape: the relayOnlyKeyDelivery block
				// must actually disable it (the marker alone could sit in
				// a comment).
				if !strings.Contains(src, "relayOnlyKeyDelivery:\n  enabled: false") {
					t.Fatalf("values-cluster.yaml must carry the explicit block:\nrelayOnlyKeyDelivery:\n  enabled: false")
				}
			}
		})
	}
}
