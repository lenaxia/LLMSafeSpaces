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
// an ALLOWLIST over the --set channel, the values channels banned
// outright), and the provenance basis (the controller image build
// stamps the SAME `${{ github.sha }}` literal assertion 4 compares
// against).
//
// Residual threat model (stated, r5 — so the pins' claims stop
// outrunning their mechanism): these pins deter ACCIDENTAL DRIFT on
// maintainer PRs — a renamed flag, a dropped namespace, a pasted
// override, a refactor that amputates a check. They are NOT an
// adversarial-shell-evasion defense: eval strings, function overrides,
// PATH-shimmed kubectl, or a workflow step that rewrites the chart
// itself can defeat any text-level pin and are out of scope (the
// workflow diff is the reviewed artifact; r2's ruling on the
// chart-mutation class stands). Five rounds of spelling-list closes
// were each falsified within one round — the allowlist/channel-ban/
// shape-pin structure is the close for the drift classes; what remains
// beyond it is adversarial, and the review of the diff itself is the
// control for that.

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

// valuesFileFlagRe matches ANY -f flag spelling against the install
// run block — attached (`-fov.yaml`), delimited (`-f file`, `-f=file`,
// tab), or continuation (`-f\` + newline). r5's ruling: the shell
// cannot be enumerated by spelling lists; any `-f` after whitespace
// is a values channel, period.
var valuesFileFlagRe = regexp.MustCompile(`(?m)(^|\s)-f`)

// installSetAllowlist is the COMPLETE environmental override surface —
// the only keys the install may carry. This is the structural close
// (r5): an allowlist over the --set channel ends the spelling war —
// quote-split (`--set rbac.scope"=cluster"`), variable indirection
// (`--set "${KEY}=cluster"`), and every future lever fail HERE, in one
// check, regardless of how they are spelled.
var installSetAllowlist = map[string]bool{
	"api.image.repository": true, "api.image.tag": true, "api.image.pullPolicy": true,
	"controller.image.repository": true, "controller.image.tag": true, "controller.image.pullPolicy": true,
	"mcp.enabled":     true,
	"postgresql.host": true, "postgresql.port": true, "postgresql.user": true, "postgresql.database": true,
	"redis.host": true, "redis.port": true,
	"externalSecret.create": true, "externalSecret.postgresPassword": true, "externalSecret.redisPassword": true,
	"api.config.logging.development":              true,
	"controller.agentdDelivery.image":             true,
	"controller.agentdDelivery.binarySHA256Amd64": true, "controller.agentdDelivery.binarySHA256Arm64": true,
	"controller.opencodeDelivery.image":             true,
	"controller.opencodeDelivery.binarySHA256Amd64": true, "controller.opencodeDelivery.binarySHA256Arm64": true,
	"controller.inferenceRelay.router.image.repository": true, "controller.inferenceRelay.router.image.tag": true,
}

// extractSetKeys parses the install run block and returns the KEY of
// every --set argument: continuation lines joined, whitespace-tokenized,
// `--set <value>` and `--set=<value>` forms, surrounding quotes
// stripped, and — helm's own parsing rule (r6's root cause B) — each
// value SPLIT ON COMMAS with every k=v segment's key returned (the
// comma multi-set channel `--set mcp.enabled=false,rbac.scope=cluster`
// reaches rendering; a key-before-first-= parse is blind to the tail).
// A quote-split or indirected key arrives mangled (`rbac.scope"`,
// `${KEY}`) and fails the allowlist.
func extractSetKeys(t *testing.T, run string) []string {
	t.Helper()
	joined := strings.ReplaceAll(run, "\\\n", " ")
	var keys []string
	fields := strings.Fields(joined)
	for i, tok := range fields {
		var value string
		switch {
		case tok == "--set":
			require.True(t, i+1 < len(fields), "--set must be followed by a value token")
			value = fields[i+1]
		case strings.HasPrefix(tok, "--set="):
			value = strings.TrimPrefix(tok, "--set=")
		default:
			continue
		}
		for _, seg := range strings.Split(value, ",") {
			seg = strings.Trim(seg, `"`)
			keys = append(keys, strings.SplitN(seg, "=", 2)[0])
		}
	}
	return keys
}

var postureGateWorkflow = filepath.Join("..", ".github", "workflows", "posture-gate.yml")

// gateStep/gateWorkflow mirror the nightly pins' parse shape.
// ContinueOnError is captured (as *bool) so pin (f) can ban it outright
// — `continue-on-error: true` on an assertion step is a failed
// assertion with a green job. Env is captured so the install step's
// env block can be required empty (an env-carried flag value evades
// every run-text ban — r6's minor finding).
type gateStep struct {
	Name            string            `json:"name"`
	ID              string            `json:"id"`
	If              string            `json:"if"`
	Run             string            `json:"run"`
	Env             map[string]string `json:"env"`
	ContinueOnError *bool             `json:"continue-on-error"`
}

type gateWorkflow struct {
	Jobs map[string]struct {
		If    string            `json:"if"`
		Env   map[string]string `json:"env"`
		Steps []gateStep        `json:"steps"`
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
		"sleep 45",     // r6's root cause D: the DURATION is the crashloop catch's teeth — `sleep 1` passed every pin
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
		// r6's root cause D + r7's inversion finding: the payload pinned
		// on BOTH greps in the POSITIVE `if grep` shape — a Contains-
		// anywhere pin passed a partial swap, and a `!`-inversion (green
		// on broken, red on healthy) kept every literal intact.
		"if grep -i 'forbidden' /tmp/gate-pod.log",
		"if grep -i 'forbidden' /tmp/gate-pod-prev.log",
		`if ! kubectl -n "$GATE_NS" logs`, // the fetch guard is negated: a fetch failure must be red
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
	// r7 finding 1 — the head-of-line shape pin (the regression pin for
	// the committed-mutation incident): the install's FIRST command
	// line must be exactly the nightly's head line. A left-in mutation
	// appending tokens to the head line defeats every ban view, the
	// allowlist parse, and the -f regex at once (the tokens never form
	// — six rounds of pins stayed green over a provably broken install).
	first := ""
	for _, line := range strings.Split(install.Run, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		first = trimmed
		break
	}
	require.Equal(t, "helm upgrade --install llmsafespaces helm \\", first,
		"the install's first command line must be exactly the nightly's head line — anything else is an unreviewed append (the r7 committed-mutation class)")
	// The two value-bearing environmental pins: mcp MUST be off (issue
	// #28 — no image exists to pull) and the install MUST wait (the
	// nightly's shape; the assertions still carry the verdict).
	require.Contains(t, install.Run, "--set mcp.enabled=false",
		"mcp.enabled must be OFF — the mcp image does not exist (issue #28)")
	require.Contains(t, install.Run, "--wait", "the install must wait")

	for _, banned := range []string{
		"rbac.scope=",
		"relayOnlyKeyDelivery.enabled=",
		"agentdSidecar.enabled=",
		"allowRelayRouterEgress=",
		// watchNamespaces: scoping the informer to a namespace would
		// silence the exact #1555-class crashloop (cluster-wide list →
		// forbidden → red gate) the gate exists to catch — tuning the
		// gate green while the shipped posture is broken (r2 finding 1).
		"watchNamespaces=",
		// The values channels beyond --set (r2–r5): the install takes
		// its posture from the chart alone. --set-string and
		// --reuse-values are value channels too; --post-renderer is a
		// whole-manifest rewrite channel (r5, helm-proven live).
		"--values",
		"--set-json",
		"--set-string",
		"--reuse-values",
		"--post-renderer",
	} {
		for _, view := range banViews(install.Run) {
			require.NotContains(t, view, banned,
				"the gate must never set %s — the shipped default IS the posture under test (checked in every bash-join view)", banned)
		}
	}
	require.Empty(t, install.Env,
		"the install step must carry no env block — an env-carried flag value evades every run-text ban (r6's carrier channel)")
	for _, view := range banViews(install.Run) {
		require.False(t, valuesFileFlagRe.MatchString(view),
			"the install must take no values files in ANY -f spelling — attached, delimited, tab, or continuation form (checked in every bash-join view)")
	}
	// THE structural close (r5): every --set key must be allowlisted,
	// every allowlist entry must be used (dead entries are drift), and
	// the parser must have found the full override set (a silently
	// empty parse would be a vacuous pass). Any spelling of any other
	// key — quote-split, indirected, or a future lever — fails here.
	keys := extractSetKeys(t, install.Run)
	require.GreaterOrEqual(t, len(keys), 20,
		"the --set parse must find the full environmental override set (found %d — a silent parse failure would vacuously pass)", len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		require.True(t, installSetAllowlist[k],
			"--set key %q is NOT on the environmental allowlist — the install may carry no posture lever in ANY spelling", k)
		seen[k] = true
	}
	for allowed := range installSetAllowlist {
		require.True(t, seen[allowed],
			"allowlist entry %q is unused — dead entries are drift; remove it or use it", allowed)
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
		// r7 (the sub-agent's job-level carrier): the r6 close pinned
		// only the STEP env; a job-level env block feeding $VARS into
		// the helm line smuggles past every run-text ban the same way.
		require.Empty(t, j.Env,
			"the gate job %q must carry no env block — job-level env is a value-carrier channel that evades the run-text bans", jobName)
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
		// r3–r6: the pinned prefix alone is presence, not persistence —
		// a later countermand neuters it under the pinned prefix. The
		// family is banned across EVERY bash-join view (r6's root cause
		// A: bash joins backslash-newline with NOTHING, so `se\<NL>t +e`
		// executes as `set +e` while every space-joined view stays
		// clean): `set +o` as a PREFIX (all long forms), bare
		// `+o errexit`/`+o pipefail`/`+o nounset` (the mixed form),
		// `set +e`, `trap ` (a trap 'exit 0' EXIT is a complete neuter
		// that is not a set spelling at all), and `exit 0` (r6's
		// finding 5: strictly larger blast radius than the trap — it
		// neuters the restart-diff too; failure paths use exit 1).
		for _, view := range banViews(s.Run) {
			for _, countermand := range []string{
				"set +e", "set +o", "+o errexit", "+o pipefail", "+o nounset", "trap ", "exit 0",
			} {
				require.NotContains(t, view, countermand,
					"%s must not countermand set -euo pipefail (`%s` neuters every check under the pinned prefix)", spec.prefix, countermand)
			}
		}
		// r6's root cause C: the shape checks' entry condition keys on
		// `kubectl wait` — a double-space `kubectl   wait` skipped them
		// entirely (the guard, not the ban, was the hole). Collapse
		// whitespace per line before matching. r7 (the sub-agent's
		// split escape): a continuation `kubectl \`+newline+`wait …`
		// never enters the guard either — Assert 1 carries no
		// legitimate continuations, so they are banned outright.
		if spec.prefix == "Assert 1" {
			require.NotContains(t, s.Run, "\\\n",
				"Assert 1 must carry no line continuations — a split command never enters the shape guard (r7)")
			collapse := regexp.MustCompile(`[ \t]+`)
			for _, line := range strings.Split(s.Run, "\n") {
				collapsed := collapse.ReplaceAllString(strings.TrimSpace(line), " ")
				if strings.Contains(collapsed, "kubectl wait") {
					require.True(t, strings.HasPrefix(collapsed, "kubectl wait"),
						"Assert 1's wait lines must start the line bare (no !/if/assignment prefix): %q", line)
					for _, op := range []string{"||", "&&", ";", "`"} {
						require.NotContains(t, collapsed, op,
							"Assert 1's wait lines must carry no shell operators (bare lines only — || :/&& :/; all escape errexit): %q", line)
					}
					require.False(t, strings.HasSuffix(collapsed, "\\"),
						"Assert 1's wait lines must not continue (a continuation hides what follows): %q", line)
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

// normalizeRunText collapses runs of spaces/tabs to one space AND joins
// backslash-newline continuations with a space.
func normalizeRunText(s string) string {
	s = strings.ReplaceAll(s, "\\\n", " ")
	return regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
}

// banViews returns every text view a substring ban must hold against:
// the raw text, the space-normalized view (double-space variants), and
// the BASH-JOIN view — bash removes backslash-newline with NO space, so
// `se\<NL>t +e` executes as `set +e` while raw and space-joined views
// stay clean (r6's root cause A).
func banViews(s string) []string {
	return []string{
		s,
		normalizeRunText(s),
		strings.ReplaceAll(s, "\\\n", ""),
	}
}

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
