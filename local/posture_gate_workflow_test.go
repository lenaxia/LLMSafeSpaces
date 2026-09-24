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
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// valuesFileFlagRe matches any -f flag spelling against the install
// run block: space- or tab-delimited, or =-joined, at line start or
// after any whitespace (r4's tab-form escape of the literal bans).
var valuesFileFlagRe = regexp.MustCompile(`(?m)(^|\s)-f[\s=]`)

var postureGateWorkflow = filepath.Join("..", ".github", "workflows", "posture-gate.yml")

// gateStep/gateWorkflow mirror the nightly pins' parse shape.
// ContinueOnError is captured (as *bool) so pin (f) can ban it outright
// — `continue-on-error: true` on an assertion step is a failed
// assertion with a green job.
type gateStep struct {
	Name            string `json:"name"`
	ID              string `json:"id"`
	If              string `json:"if"`
	Run             string `json:"run"`
	ContinueOnError *bool  `json:"continue-on-error"`
}

type gateWorkflow struct {
	Jobs map[string]struct {
		If    string     `json:"if"`
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

	// The pull_request block must carry ONLY the paths filter — a
	// `types:`/`branches:` filter silently narrows which PR events arm
	// the gate (e.g. types: [opened] stops re-runs on pushes).
	var prKeys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(onMap["pull_request"], &prKeys))
	require.ElementsMatch(t, []string{"paths"}, keysOf(prKeys),
		"pull_request must filter by paths only — types/branches filters silently disarm the gate")
	var prPaths struct {
		Paths []string `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(onMap["pull_request"], &prPaths))
	require.ElementsMatch(t, []string{"helm/**", "controller/**", "api/**"}, prPaths.Paths,
		"pull_request paths must be exactly the ruling-3 breadth: helm/** + controller/** + api/**")
}

// assertionSpec is one of the four §5 assertions: the parsed-name
// prefix of its step and the literals its run block must contain
// (multiple where one literal alone would let a mutation through —
// r1: deleting the llm-relay half of assertions 1/3 kept every pin
// green while gutting the both-namespace scope the why-text claims).
var assertionSpecs = []struct {
	prefix   string
	literals []string
	why      string
}{
	{"Assert 1", []string{
		"--for=condition=available deployment --all -n $NS",
		"--for=condition=available deployment --all -n llm-relay",
		"restartCount", // the stability window: helm --wait's rc=0 mid-crashloop (r1 live run) is not the verdict
	},
		"all-Ready in BOTH rendered namespaces plus a no-new-restarts stability window (the #1546 Defect-1 catch; `rollout status --all` does not exist in kubectl — the wait idiom is the verified one)"},
	{"Assert 2", []string{
		"relay-only key delivery enabled",
		"if ! kubectl -n $NS logs deployment/llmsafespaces-controller >",
	},
		"the armed line — M1's boot-time contract, cluster-side; the log FETCH is failure-checked (a pod whose logs cannot be read cannot be cleared)"},
	{"Assert 3", []string{
		`for GATE_NS in "$NS" "llm-relay"`,
		"--previous",
		`PODS=$(kubectl -n "$GATE_NS" get pods -o name)`,
	},
		"zero forbidden: no RBAC-denial line in any pod log, any container, BOTH namespaces, prior crashed containers included; the pod-LIST fetch is failure-checked too (a process-substitution feed is invisible to set -e — r2's silent-skip)"},
	{"Assert 4", []string{
		`!= '${{ github.sha }}'`, // the comparison shape, not the FAIL-echo's literal
		"starting controller",
		"if ! kubectl -n $NS logs deployment/llmsafespaces-controller >",
	},
		"provenance: the running commit stamp COMPARED against this run's build sha (r1 mutation: gutting the comparison while the echo retained the literal passed the old pin); the fetch is failure-checked"},
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
		for _, lit := range spec.literals {
			require.Contains(t, s.Run, lit, "%s run must contain %q — %s", spec.prefix, lit, spec.why)
		}
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
		// watchNamespaces: scoping the informer to a namespace would
		// silence the exact #1555 crashloop class (cluster-wide list →
		// forbidden → red gate) the gate exists to catch — tuning the
		// gate green while the shipped posture is broken (r2 finding 1).
		"watchNamespaces=",
		// Values files and --set-json smuggle whole posture overrides
		// past the --set ban list (r0 raised it, r2 re-demonstrated: a
		// posture-override.yaml passed via --values kept every pin
		// green). The install takes its posture from the chart alone.
		// -f is banned as a REGEX over the run block (r4): the literal
		// spellings missed the tab-delimited `-f<TAB>file` form — tab
		// is IFS whitespace to the shell, so the smuggle was live helm
		// behavior with every pin green.
		"--values",
		"--set-json",
	} {
		require.NotContains(t, install.Run, banned,
			"the gate must never set %s — the shipped default IS the posture under test", banned)
	}
	require.False(t, valuesFileFlagRe.MatchString(install.Run),
		"the install must take no values files in ANY flag spelling — the regex covers space, tab, and = delimiters (the r4 tab-form escape)")
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
	require.Contains(t, a4.Run, `!= '${{ github.sha }}'`,
		"assertion 4's COMPARISON must be against the same github.sha literal the build stamps (not merely an echo that mentions it — the r1 mutation)")
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
// test), no assertion step may carry `continue-on-error` (a failed
// assertion with a green job — the silent-disarm mutation the r1
// review enumerates), the job itself carries no `if:` (a job-level
// condition disarms the whole gate in one edit), and the failure dump
// + teardown keep the crash-loud culture (logs on failure, cluster
// disposed always).
func TestPostureGate_FailureSemantics(t *testing.T) {
	raw := []byte(mustRead(t, postureGateWorkflow))
	var wf gateWorkflow
	require.NoError(t, yaml.Unmarshal(raw, &wf))
	require.Len(t, wf.Jobs, 1, "the gate is ONE job (0061 §5)")
	for jobName, j := range wf.Jobs {
		require.Empty(t, strings.TrimSpace(j.If),
			"the gate job %q must carry no `if:` — one edit could disarm the whole gate", jobName)
	}
	steps := parsePostureGate(t)
	for _, spec := range assertionSpecs {
		s := gateStepByPrefix(t, steps, spec.prefix)
		require.Empty(t, strings.TrimSpace(s.If),
			"%s must be unconditional — a failed install must fail the gate, not skip its assertions", spec.prefix)
		require.Nil(t, s.ContinueOnError,
			"%s must not carry continue-on-error — a failed assertion must fail the job", spec.prefix)
		// r2 finding 2: without set -e a failed kubectl wait continues
		// silently and a stuck-not-ready deployment prints OK — the
		// literal surface is unchanged, so only a pin sees the neuter.
		require.True(t, strings.HasPrefix(strings.TrimSpace(s.Run), "set -euo pipefail"),
			"%s must begin with `set -euo pipefail` — deleting it neuters every check with zero literal drift", spec.prefix)
		// r3/r4: the pinned prefix alone is presence, not persistence —
		// a later countermand neuters it under the pinned prefix. The
		// family is banned with WHITESPACE NORMALIZED (collapse runs of
		// spaces/tabs to one space first): exact-spelling bans missed
		// `set +o errexit` and double-space `set  +e` (r4's escapes).
		norm := regexp.MustCompile(`[ \t]+`).ReplaceAllString(s.Run, " ")
		for _, countermand := range []string{
			"set +e", "set +o errexit", "set +o pipefail", "set +o nounset",
		} {
			require.NotContains(t, norm, countermand,
				"%s must not countermand set -euo pipefail (`%s` neuters every check under the pinned prefix)", spec.prefix, countermand)
		}
		// r4 finding 4: `|| true` appended to the Assert 1 waits neuters
		// the Ready checks with every literal intact — the wait lines
		// must be bare.
		if spec.prefix == "Assert 1" {
			for _, line := range strings.Split(s.Run, "\n") {
				if strings.Contains(line, "kubectl wait") {
					require.NotContains(t, line, "|| true",
						"Assert 1's wait lines must be bare — `|| true` on a kubectl wait neuters the Ready check with zero literal drift: %q", line)
				}
			}
		}
	}
	for _, s := range steps {
		require.Nil(t, s.ContinueOnError,
			"no step may carry continue-on-error (found on %q) — failures must propagate", s.Name)
	}
	dump := gateStepByPrefix(t, steps, "Dump cluster state on failure")
	require.Contains(t, dump.If, "failure()")
	teardown := gateStepByPrefix(t, steps, "Teardown")
	require.Contains(t, teardown.If, "always()")
}

// --- helpers ---------------------------------------------------------

// keysOf returns the sorted key set of a JSON object (for the
// exact-keys pins).
func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
