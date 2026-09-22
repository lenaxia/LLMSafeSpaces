// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us72_nightly_lane_hardening_test.go — #1541: tonight's nightly failed
// at the design-0060 SR-6 upload-perf row (run 35737624754) and the
// default skip-on-failure semantics hostage-skipped the ENTIRE epic-72
// evidence lane (router build → drill shape → drill → sweep) with zero
// drill rows executed. The fix arms the lane's five steps with
// cancel-guarded prerequisite conditions (GitHub's cancel-guard
// expression composed with step-outcome equality — the exact literals
// live in the const block below) — never bare
// `always()`: a failed install (or a failed prerequisite) must SKIP the
// lane rather than produce infrastructure-failure rows that poison the
// executed-rollback / K1 evidence record.
//
// These pins hold the arming structurally AND behaviorally: the
// conditions are evaluated under simulated run scenarios (a tiny
// evaluator for exactly the emitted expression shapes — anything else
// fails the pin so the evaluator can never silently drift from the
// workflow), including the #1541 scenario itself: an unrelated row
// failure must leave the whole lane RUNNING.
//
// YAML gotcha this file encodes: a step name containing " #" (e.g.
// "…owner's #1534 script") has everything from the # parsed as a
// COMMENT — the name GitHub actually stores is the truncated prefix.
// The armed-step table below keys by those PARSED names (verified
// unique prefixes of the full names in the raw file by pin (d)).

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// nightStep is one parsed step of the nightly workflow (the fields the
// arming pins need).
type nightStep struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	If   string `json:"if"`
	Run  string `json:"run"`
}

type nightWorkflow struct {
	Jobs map[string]struct {
		Steps []nightStep `json:"steps"`
	} `json:"jobs"`
}

func parseNightly(t *testing.T) []nightStep {
	t.Helper()
	raw := []byte(mustRead(t, us70NightlyWorkflow))
	var wf nightWorkflow
	require.NoError(t, yaml.Unmarshal(raw, &wf), "e2e-nightly.yml must parse")
	var steps []nightStep
	for _, j := range wf.Jobs {
		steps = append(steps, j.Steps...)
	}
	require.NotEmpty(t, steps)
	return steps
}

// stepByNamePrefix finds the step whose PARSED name starts with prefix
// (unique-match enforced — a prefix matching two steps fails loudly).
func stepByNamePrefix(t *testing.T, steps []nightStep, prefix string) nightStep {
	t.Helper()
	var found []nightStep
	for _, s := range steps {
		if strings.HasPrefix(s.Name, prefix) {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "prefix %q must match exactly one step (matched %d)", prefix, len(found))
	return found[0]
}

// GitHub's expression function is the British-spelled cancel guard
// (NOT a typo of the American spelling — it is the platform's own
// function name; the misspell lint is exempted on the literal lines).
const (
	installOK  = "${{ !cancelled() && steps.helm-install.outcome == 'success' }}"                                                                                                                              //nolint:misspell
	drillChain = "${{ !cancelled() && steps.helm-install.outcome == 'success' && steps.us70-suite.outcome == 'success' && steps.drill-shape.outcome == 'success' }}"                                           //nolint:misspell
	sweepChain = "${{ !cancelled() && steps.helm-install.outcome == 'success' && steps.us70-suite.outcome == 'success' && steps.drill-shape.outcome == 'success' && steps.relay-drill.outcome == 'success' }}" //nolint:misspell
)

// armedStep names the five #1541-armed steps and the exact condition
// each must carry (the arming ladder: build/shape/us70 need the
// install; the drill adds its two prerequisites; the sweep adds the
// drill itself — sweeping a flag-off release would read the raw canary
// as a K1 violation and poison the record). `name` is the step's
// PARSED name (YAML truncates at " #"); `fullName` is checked against
// the raw file by pin (d) so the prefix can never silently drift to a
// different step.
var armedSteps = []struct {
	name      string
	fullName  string
	id        string
	condition string
}{
	{"Run secret-delivery e2e rows (Epic 70 US-70.1 AC-1/2/13/17 + chaos)",
		"Run secret-delivery e2e rows (Epic 70 US-70.1 AC-1/2/13/17 + chaos)", "us70-suite", installOK},
	{"Build + load the llm-relay router image (US-72.5 drill)",
		"Build + load the llm-relay router image (US-72.5 drill)", "", installOK},
	{"Put the release into the drill's valid shape (relay on, namespaced scope)",
		"Put the release into the drill's valid shape (relay on, namespaced scope)", "drill-shape", installOK},
	{"Run the relay-only flip + rollback drill (US-72.5, owner's",
		"Run the relay-only flip + rollback drill (US-72.5, owner's #1534 script)", "relay-drill", drillChain},
	{"Run the rogue-agent sweep (US-72.6,",
		"Run the rogue-agent sweep (US-72.6, #820 exit criterion)", "", sweepChain},
}

// Pin (a): every armed step carries its EXACT condition (string-exact —
// a dropped conjunct, a renamed step id, or a reworded expression all
// fail here), the referenced ids exist, and no armed step uses bare
// always()/failure() (the poisoned-evidence trap #1541 rules out).
func TestUS72LaneHardening_ArmedConditionsExact(t *testing.T) {
	steps := parseNightly(t)
	ids := map[string]bool{}
	for _, s := range steps {
		if s.ID != "" {
			ids[s.ID] = true
		}
	}
	for _, want := range []string{"helm-install", "us70-suite", "drill-shape", "relay-drill"} {
		require.True(t, ids[want], "step id %q must exist — every condition referencing it silently evaluates false (the lane skips forever) if it is renamed", want)
	}
	for _, a := range armedSteps {
		t.Run(a.name, func(t *testing.T) {
			s := stepByNamePrefix(t, steps, a.name)
			require.Equal(t, a.condition, strings.TrimSpace(s.If),
				"the #1541 arming condition must be exactly this — a dropped conjunct re-hostages the lane (the #1541 bug) or re-poisons the evidence (the always() trap)")
			if a.id != "" {
				require.Equal(t, a.id, s.ID, "step must carry id %q for downstream conditions", a.id)
			}
			require.NotContains(t, s.If, "always()", "bare always() runs the lane against failed infrastructure — the poisoned-evidence trap")
		})
	}
	// The install step itself carries the id everything references.
	install := stepByNamePrefix(t, steps, "Helm install LLMSafeSpaces")
	require.Equal(t, "helm-install", install.ID)
}

// Pin (b): the armed set is EXACTLY the five steps — no other step may
// silently gain a cancel-guard arming (scope creep changes unrelated
// lanes' skip semantics) and none of the five may lose theirs.
// lanes' skip semantics) and none of the five may lose theirs.
func TestUS72LaneHardening_ArmedSetIsExact(t *testing.T) {
	steps := parseNightly(t)
	want := map[string]bool{}
	for _, a := range armedSteps {
		want[a.name] = true
	}
	got := map[string]bool{}
	for _, s := range steps {
		if strings.Contains(s.If, "!cancelled()") { //nolint:misspell // GitHub's expression function name
			got[s.Name] = true
		}
	}
	require.Equal(t, want, got,
		"exactly the five #1541-armed steps may carry the cancel-guard arming — extra armed steps change other lanes' failure semantics; missing ones re-hostage the epic-72 evidence lane")
}

// Pin (d): each armed step's parsed-name prefix must belong to the
// intended FULL name in the raw workflow text — the belt for the
// YAML-truncation keying above.
func TestUS72LaneHardening_ParsedNamesBelongToFullNames(t *testing.T) {
	raw := mustRead(t, us70NightlyWorkflow)
	for _, a := range armedSteps {
		require.Contains(t, raw, a.fullName,
			"the full step name %q must exist in the raw workflow — the parsed prefix %q keyed a different/renamed step", a.fullName, a.name)
	}
}

// evalCond evaluates EXACTLY the emitted expression shapes — the
// cancel-guard expression plus `steps.<id>.outcome == 'success'`
// conjuncts (see the const block for the literal forms) —
// under a simulated run (outcomes by step id; skipped steps report
// "skipped", matching GitHub). Any other syntax fails the pin: the
// evaluator must never guess at expressions it was not built for.
func evalCond(t *testing.T, expr string, outcomes map[string]string) bool {
	t.Helper()
	e := strings.TrimSpace(expr)
	e = strings.TrimPrefix(strings.TrimSuffix(e, "}}"), "${{")
	e = strings.TrimSpace(e)
	require.True(t, strings.HasPrefix(e, "!cancelled()"), //nolint:misspell // GitHub's expression function name
		"unsupported condition shape (must start with the cancel guard): %q", expr)
	for _, conjunct := range strings.Split(strings.TrimPrefix(e, "!cancelled()"), "&&") { //nolint:misspell // GitHub's expression function name
		c := strings.TrimSpace(conjunct)
		if c == "" {
			continue
		}
		m := regexp.MustCompile(`^steps\.([a-z0-9-]+)\.outcome == 'success'$`).FindStringSubmatch(c)
		require.NotNil(t, m, "unsupported conjunct %q — extend the evaluator deliberately or fix the workflow expression", c)
		if outcomes[m[1]] != "success" {
			return false
		}
	}
	return true
}

// Pin (c): the simulated-run scenarios — the behavioral heart of #1541.
//
//	scenario           | install | us70 | shape | drill | UNRELATED fail | lane
//	#1541 (tonight)    |   ok    |  ok  |  ok   |  ok   |     yes        | RUNS (the fix)
//	install failed     |   X     |  —   |  —    |  —    |      —         | skips (no poison)
//	us70 failed        |   ok    |  X   |  ok   | skip  |      —         | build/shape run; drill+sweep skip
//	shape failed       |   ok    |  ok  |  X    | skip  |      —         | drill+sweep skip
//	drill failed       |   ok    |  ok  |  ok   |   X   |      —         | sweep skips (flag state unknown)
//
// "skip" outcomes propagate exactly as GitHub's would (a step whose
// `if` evaluates false is skipped, and downstream `== 'success'` gates
// read skipped ≠ success).
func TestUS72LaneHardening_Scenarios(t *testing.T) {
	steps := parseNightly(t)
	cond := map[string]string{}
	for _, a := range armedSteps {
		cond[a.name] = stepByNamePrefix(t, steps, a.name).If
	}

	run := func(t *testing.T, scenario map[string]string) map[string]bool {
		t.Helper()
		// Propagate skips: a step gated on X is skipped when X is not
		// success (its outcome becomes "skipped" for further gating).
		out := map[string]string{}
		for k, v := range scenario {
			out[k] = v
		}
		for range armedSteps { // fixpoint: ids are topologically ordered anyway
			for _, a := range armedSteps {
				key := a.name
				if a.id != "" {
					key = a.id
				}
				if _, seen := out[key]; !seen {
					if evalCond(t, cond[a.name], out) {
						out[key] = "success"
					} else {
						out[key] = "skipped"
					}
				}
			}
		}
		ran := map[string]bool{}
		for _, a := range armedSteps {
			key := a.name
			if a.id != "" {
				key = a.id
			}
			ran[a.name] = out[key] == "success"
		}
		return ran
	}

	t.Run("#1541 unrelated-row failure (tonight's SR-6 class)", func(t *testing.T) {
		ran := run(t, map[string]string{"helm-install": "success"})
		for _, a := range armedSteps {
			require.True(t, ran[a.name], "%q must RUN after an unrelated row failure — the #1541 hostage bug", a.name)
		}
	})
	t.Run("failed install skips the whole lane", func(t *testing.T) {
		ran := run(t, map[string]string{"helm-install": "failure"})
		for _, a := range armedSteps {
			require.False(t, ran[a.name], "%q must SKIP on a failed install — running it would fail rows against missing infrastructure and poison the evidence record", a.name)
		}
	})
	t.Run("failed us70: build+shape run, drill+sweep skip", func(t *testing.T) {
		ran := run(t, map[string]string{"helm-install": "success", "us70-suite": "failure"})
		require.True(t, ran["Build + load the llm-relay router image (US-72.5 drill)"], "the image build has no us70 dependency")
		require.True(t, ran["Put the release into the drill's valid shape (relay on, namespaced scope)"], "the shape step has no us70 dependency")
		require.False(t, ran["Run the relay-only flip + rollback drill (US-72.5, owner's"], "the drill needs mock-llm (us70) — skipping beats a poisoned run")
		require.False(t, ran["Run the rogue-agent sweep (US-72.6,"], "the sweep rides the drill's end state")
	})
	t.Run("failed shape: drill+sweep skip", func(t *testing.T) {
		ran := run(t, map[string]string{"helm-install": "success", "us70-suite": "success", "drill-shape": "failure"})
		require.False(t, ran["Run the relay-only flip + rollback drill (US-72.5, owner's"], "the drill against an un-shaped (flag-off, cluster-scope) release fails structurally")
		require.False(t, ran["Run the rogue-agent sweep (US-72.6,"], "the sweep needs the shaped release")
	})
	t.Run("failed drill: sweep skips (flag state unknown)", func(t *testing.T) {
		ran := run(t, map[string]string{"helm-install": "success", "us70-suite": "success", "drill-shape": "success", "relay-drill": "failure"})
		require.False(t, ran["Run the rogue-agent sweep (US-72.6,"],
			"a drill dead mid-rollback leaves the release flag-off — sweeping it reads the raw canary as a K1 violation (false epic-criterion failure)")
	})
}
