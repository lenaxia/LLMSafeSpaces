// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// posture_gate_workflow_test.go — design 0061 §5 (M3): structural pins
// for the posture gate workflow. The gate cold-installs the chart under
// its OWN shipped default posture on kind (rbac.scope=namespace +
// relayOnlyKeyDelivery.enabled=true are the SHIPPED values — the install
// sets NEITHER) and asserts, in order: (1) every Deployment in every
// rendered namespace Ready, (2) the controller's armed line
// (`relay-only key delivery enabled`, M1's boot contract), (3) zero
// `forbidden` lines in any pod log (the silent-RBAC-starvation class),
// (4) the running controller's commit stamp equals the sha the image
// was built with in this run (the wrong-artifact class — §5's r1
// envelope: what this CAN catch is running something other than this
// run's built artifact; wrong-bits-with-right-stamp stays owner-side).
//
// The pins hold: the three-path trigger breadth (§11 ruling 3: helm/**
// + controller/** + api/**), the four assertions present and ordered,
// the default-posture install command (environmental overrides ONLY —
// image refs, delivery pins, mcp off (issue #28, no image exists),
// test DB/Redis — with the posture levers BANNED from the install:
// a gate that pins rbac.scope or relayOnlyKeyDelivery to any value is
// testing an override, not the shipped posture, and must fail here),
// and the provenance basis (the controller image build stamps the SAME
// `${{ github.sha }}` literal assertion 4 compares against).

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

var postureGateWorkflow = filepath.Join("..", ".github", "workflows", "posture-gate.yml")

// gateStep/gateWorkflow mirror the nightly pins' parse shape.
type gateStep struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	If   string `json:"if"`
	Run  string `json:"run"`
}

type gateWorkflow struct {
	Jobs map[string]struct {
		Steps []gateStep `json:"steps"`
	} `json:"jobs"`
}

func parsePostureGate(t *testing.T) []gateStep {
	t.Helper()
	raw := []byte(mustRead(t, postureGateWorkflow))
	var wf gateWorkflow
	require.NoError(t, yaml.Unmarshal(raw, &wf), "posture-gate.yml must parse")
	var steps []gateStep
	for _, j := range wf.Jobs {
		steps = append(steps, j.Steps...)
	}
	require.NotEmpty(t, steps)
	return steps
}

// gateStepByPrefix finds the unique step whose parsed name starts with
// prefix (multi-match fails loudly, mirroring the nightly pins).
func gateStepByPrefix(t *testing.T, steps []gateStep, prefix string) gateStep {
	t.Helper()
	var found []gateStep
	for _, s := range steps {
		if strings.HasPrefix(s.Name, prefix) {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "prefix %q must match exactly one step (matched %d)", prefix, len(found))
	return found[0]
}

// Pin (a): the trigger surface — workflow_dispatch plus the §11
// ruling-3 breadth: pull_request paths EXACTLY helm/**, controller/**,
// api/**. A dropped path silently stops gating that surface; an added
// path gates surface the owner did not confirm.
func TestPostureGate_Triggers(t *testing.T) {
	raw := []byte(mustRead(t, postureGateWorkflow))
	var top map[string]json.RawMessage
	require.NoError(t, yaml.Unmarshal(raw, &top))
	// go-yaml resolves the bare `on` key as YAML-1.1 boolean true when
	// round-tripped through JSON — accept either key spelling.
	onBlock, ok := top["on"]
	if !ok {
		onBlock, ok = top["true"]
	}
	require.True(t, ok, "workflow must have an `on:` trigger block")

	var onMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(onBlock, &onMap))
	_, hasDispatch := onMap["workflow_dispatch"]
	require.True(t, hasDispatch, "workflow_dispatch must be a trigger (manual posture re-checks)")

	var pr struct {
		Paths []string `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(onMap["pull_request"], &pr))
	require.ElementsMatch(t, []string{"helm/**", "controller/**", "api/**"}, pr.Paths,
		"pull_request paths must be exactly the ruling-3 breadth: helm/** + controller/** + api/**")
}

// assertionSpec is one of the four §5 assertions: the parsed-name
// prefix of its step and a literal its run block must contain.
var assertionSpecs = []struct {
	prefix  string
	literal string
	why     string
}{
	{"Assert 1", "rollout status deployment --all",
		"all-Ready: every Deployment in every rendered namespace (the release ns AND llm-relay — a CrashLooping router is the #1546 Defect-1 catch)"},
	{"Assert 2", "relay-only key delivery enabled",
		"the armed line — M1's boot-time contract, asserted cluster-side"},
	{"Assert 3", "forbidden",
		"zero forbidden: no RBAC-denial line in any pod log (Defect 2's class, generically)"},
	{"Assert 4", "github.sha",
		"provenance: the running commit stamp compared against this run's build sha"},
}

// Pin (b): the four assertions are present, in §5's order, each in its
// own named step — and no OTHER step may claim an "Assert N" name
// (an extra assertion step would silently reorder or dilute the
// contract).
func TestPostureGate_FourAssertionsInOrder(t *testing.T) {
	steps := parsePostureGate(t)
	last := -1
	for _, spec := range assertionSpecs {
		s := gateStepByPrefix(t, steps, spec.prefix)
		require.NotEmpty(t, s.Run, "%s must carry a run block", spec.prefix)
		require.Contains(t, s.Run, spec.literal, "%s run must contain %q — %s", spec.prefix, spec.literal, spec.why)
		idx := indexOfStep(t, steps, s)
		require.Greater(t, idx, last, "assertions must run in §5 order: %s", spec.prefix)
		last = idx
	}
	for _, s := range steps {
		if strings.HasPrefix(s.Name, "Assert ") {
			require.Contains(t, []string{"Assert 1", "Assert 2", "Assert 3", "Assert 4"},
				prefixWords(s.Name), "no extra Assert-named steps (found %q)", s.Name)
		}
	}
}

// Pin (c): the install command's environmental overrides AND the
// posture-lever ban list. The shipped defaults (rbac.scope=namespace,
// relayOnlyKeyDelivery.enabled=true, agentdSidecar.enabled=false,
// networkPolicy.allowRelayRouterEgress=false) reach helm UNTOUCHED —
// the moment the gate pins any of these levers, it is asserting an
// override, not the shipped posture, and this pin goes red.
func TestPostureGate_InstallShippedPosture(t *testing.T) {
	steps := parsePostureGate(t)
	install := gateStepByPrefix(t, steps, "Helm install LLMSafeSpaces")
	require.Equal(t, "posture-install", install.ID, "install step must carry the id the run keys on")
	require.Contains(t, install.Run, "helm upgrade --install llmsafespaces helm",
		"the nightly's install command shape, verbatim")

	for _, env := range []string{
		"api.image.repository=llmsafespaces/api",
		"controller.image.repository=llmsafespaces/controller",
		"mcp.enabled=false", // issue #28: no mcp image exists to pull
		"controller.agentdDelivery.image=",
		"controller.opencodeDelivery.image=",                                           // delivery pins: #1546 Defect-2 posture
		"controller.inferenceRelay.router.image.repository=llmsafespaces/relay-router", // kind cannot pull the ghcr default (drill-shape precedent)
		"externalSecret.create=true",
		"--wait",
	} {
		require.Contains(t, install.Run, env, "install must keep the environmental override %q", env)
	}
	for _, banned := range []string{
		"rbac.scope=",
		"relayOnlyKeyDelivery.enabled=",
		"agentdSidecar.enabled=",
		"allowRelayRouterEgress=",
	} {
		require.NotContains(t, install.Run, banned,
			"the gate must never set %s — the shipped default IS the posture under test", banned)
	}
}

// Pin (d): the provenance basis is ONE sha — the controller image
// build stamps COMMIT_SHA from the same `${{ github.sha }}` literal
// assertion 4 compares against. If the stamp and the assertion ever
// drift apart, the gate reds on every run (or worse, silently passes
// against a pinned sha) — this pin makes the drift a test failure
// first.
func TestPostureGate_ProvenanceSameShaBasis(t *testing.T) {
	raw := mustRead(t, postureGateWorkflow)
	require.Contains(t, raw, `--build-arg COMMIT_SHA="${{ github.sha }}" -f controller/Dockerfile`,
		"the controller image must be stamped from this run's github.sha")
	a4 := gateStepByPrefix(t, parsePostureGate(t), "Assert 4")
	require.Contains(t, a4.Run, `'${{ github.sha }}'`,
		"assertion 4 must compare the running stamp against the same github.sha literal the build stamps")
	require.Contains(t, a4.Run, "starting controller",
		"assertion 4 reads the running binary's own startup line (the label channel §5 r1 names)")
}

// Pin (e): the bootstrap reuses the e2e-nightly sequence verbatim —
// the proven kind topology + registry + image-build + cert-manager +
// test DB/Redis chain. A silently divergent bootstrap turns gate
// failures into bootstrap flake hunts.
func TestPostureGate_BootstrapReusesNightlySequence(t *testing.T) {
	steps := parsePostureGate(t)
	joined := stepRunsJoined(t, steps)
	for _, literal := range []string{
		"local/kind-cluster-nightly.yaml", // the nightly's 2-node topology, verbatim
		"lss-e2e-registry",                // digest-pinned delivery refs resolve in-node
		"cert-manager",                    // the validating webhook's CA chain
		"local/postgres-redis.yaml",       // test DB/Redis (§5's install shape)
		"llmsafespaces-credentials",       // the credentials Secret pre-Postgres
	} {
		require.Contains(t, joined, literal, "the nightly bootstrap must contribute %q", literal)
	}
}

// Pin (f): a failed cold install IS a red gate — the assertions carry
// NO skip conditions (the #1541 arming exists to keep EVIDENCE lanes
// running past unrelated failures; here the install is the thing under
// test), and the failure dump + teardown keep the crash-loud culture
// (logs on failure, cluster disposed always).
func TestPostureGate_FailureSemantics(t *testing.T) {
	steps := parsePostureGate(t)
	for _, spec := range assertionSpecs {
		s := gateStepByPrefix(t, steps, spec.prefix)
		require.Empty(t, strings.TrimSpace(s.If),
			"%s must be unconditional — a failed install must fail the gate, not skip its assertions", spec.prefix)
	}
	dump := gateStepByPrefix(t, steps, "Dump cluster state on failure")
	require.Contains(t, dump.If, "failure()")
	teardown := gateStepByPrefix(t, steps, "Teardown")
	require.Contains(t, teardown.If, "always()")
}

// --- helpers ---------------------------------------------------------

func indexOfStep(t *testing.T, steps []gateStep, want gateStep) int {
	t.Helper()
	for i, s := range steps {
		if s.Name == want.Name {
			return i
		}
	}
	t.Fatalf("step %q not in list", want.Name)
	return -1
}

// prefixWords keeps the first two words ("Assert 1" of "Assert 1 — …")
// for the exact-set check in pin (b).
func prefixWords(name string) string {
	f := strings.Fields(name)
	if len(f) >= 2 {
		return f[0] + " " + f[1]
	}
	return name
}

func stepRunsJoined(t *testing.T, steps []gateStep) string {
	t.Helper()
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Name)
		b.WriteString("\n")
		b.WriteString(s.Run)
		b.WriteString("\n")
	}
	return b.String()
}
