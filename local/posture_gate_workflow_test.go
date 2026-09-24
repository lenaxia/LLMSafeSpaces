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
// prefix of its step, the literals its run block must contain, the
// verdict-bearing lines that must appear as EXACT trimmed lines (r9),
// and — r10's block-level close — a floor on `exit 1` carrier lines:
// the condition rows were pinned in r9, but a verdict is carried by
// its body's exit 1, which nothing required (five-for-five deletions
// stayed green).
var assertionSpecs = []struct {
	prefix     string
	literals   []string
	exactLines []string
	minExit1   int
	why        string
}{
	{"Assert 1", []string{
		"restartCount", // the stability window: helm --wait's rc=0 mid-crashloop (r1 live run) is not the verdict
		"sleep 45",     // r6's root cause D: the DURATION is the crashloop catch's teeth — `sleep 1` passed every pin
	}, []string{
		"kubectl wait --for=condition=available deployment --all -n $NS --timeout=300s",
		"kubectl wait --for=condition=available deployment --all -n llm-relay --timeout=300s",
		"kubectl wait --for=condition=available deployment --all -n $NS --timeout=60s",
		"kubectl wait --for=condition=available deployment --all -n llm-relay --timeout=60s",
		// r10 RC-A: the verdict's INPUTS — the four snapshot assignments
		// exact-pinned (r9 dropped the r8 anchor when moving the
		// comparison to an exact line; an `AFTER_NS="$BEFORE_NS"` alias
		// silently neutered the restart-diff).
		`BEFORE_NS=$(restarts_snapshot "$NS")`,
		`BEFORE_RELAY=$(restarts_snapshot llm-relay)`,
		`AFTER_NS=$(restarts_snapshot "$NS")`,
		`AFTER_RELAY=$(restarts_snapshot llm-relay)`,
		// r11 finding 1: the PRODUCER — a constant producer (a jsonpath
		// typo evaluating empty, an appended `| head -n 0`) makes BEFORE
		// and AFTER equal by construction; the comparison can never fire.
		`kubectl get pods -n "$1" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[*].restartCount}{"\n"}{/end}' | sort`,
		`if [[ "$BEFORE_NS" != "$AFTER_NS" || "$BEFORE_RELAY" != "$AFTER_RELAY" ]]; then`,
	}, 1,
		"all-Ready in BOTH rendered namespaces plus a no-new-restarts stability window (the #1546 Defect-1 catch; `rollout status --all` does not exist in kubectl — the wait idiom is the verified one)"},
	{"Assert 2", []string{
		"relay-only key delivery enabled",
	}, []string{
		"if ! kubectl -n $NS logs deployment/llmsafespaces-controller > /tmp/gate-armed.log 2>/tmp/gate-armed.err; then",
		"if ! grep -F 'relay-only key delivery enabled' /tmp/gate-armed.log; then",
	}, 2,
		"the armed line — M1's boot-time contract, cluster-side; the log FETCH is failure-checked (a pod whose logs cannot be read cannot be cleared)"},
	{"Assert 3", []string{
		`for GATE_NS in "$NS" "llm-relay"`,
	}, []string{
		`PODS=$(kubectl -n "$GATE_NS" get pods -o name)`,
		"while read -r POD; do",
		`done <<< "$PODS"`,
		`if ! kubectl -n "$GATE_NS" logs "$POD" --all-containers=true > /tmp/gate-pod.log 2>/tmp/gate-pod.err; then`,
		"if grep -i 'forbidden' /tmp/gate-pod.log; then",
		// r10 RC-C: the prev-fetch guard exact-pinned — a Contains
		// `--previous` passed for `--previous=false`, and the one
		// remaining unpinned fetch line accepted a `&& false` suffix.
		`if kubectl -n "$GATE_NS" logs "$POD" --all-containers=true --previous > /tmp/gate-pod-prev.log 2>/dev/null; then`,
		"if grep -i 'forbidden' /tmp/gate-pod-prev.log; then",
	}, 3,
		"zero forbidden: no RBAC-denial line in any pod log, any container, BOTH namespaces, prior crashed containers included; every fetch failure is red"},
	{"Assert 4", []string{
		"starting controller",
	}, []string{
		`if [[ -z "$RUNNING_COMMIT" ]]; then`,
		`if [[ "$RUNNING_COMMIT" != '${{ github.sha }}' ]]; then`,
		"if ! kubectl -n $NS logs deployment/llmsafespaces-controller > /tmp/gate-controller.log 2>/tmp/gate-controller.err; then",
		// r11 finding 1: the comparison's INPUT — the extraction's first
		// line (gutting the pipeline while retaining a no-op grep and a
		// pinned-sha assignment made the comparison unfireable with
		// every literal green).
		"RUNNING_COMMIT=$(grep -F 'starting controller' /tmp/gate-controller.log \\",
	}, 3,
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
		for _, exact := range spec.exactLines {
			requireExactLine(t, s.Run, exact,
				"%s must carry the exact line %%q (a suffix, flip, or partial gut keeps substring pins green) — %s", spec.prefix, spec.why)
		}
		// r10 RC-B: the verdict CARRIERS — each FAIL branch's exit 1.
		// r9 pinned the conditions; deleting the body's exit 1 turned
		// every FAIL branch into echo-and-continue, five for five. A
		// floor, not an equality: additional fail-closed branches are
		// legitimate drift; fewer is a neuter. r11 finding 2: count
		// NON-COMMENT lines only — `# exit 1` padding satisfied the
		// plain substring count.
		exit1s := 0
		for _, line := range strings.Split(s.Run, "\n") {
			if strings.TrimSpace(line) == "exit 1" {
				exit1s++
			}
		}
		require.GreaterOrEqual(t, exit1s, spec.minExit1,
			"%s must carry at least %d `exit 1` verdict carriers (found %d) — a FAIL branch without its exit is echo-and-continue", spec.prefix, spec.minExit1, exit1s)
		// r10 sub-agent (i) + r11 finding 4: no assertion may rebind NS
		// or GATE_NS — an in-block rebinding silently re-scopes the
		// verdict's targets (both loop iterations scanning llm-relay
		// left the release namespace's pods forever unchecked). The
		// env ban closed the parsed channel; assignment and export
		// rebinding are the same class one level down.
		require.NotRegexp(t, `(?m)^\s*NS=`, s.Run,
			"%s must not rebind NS — the namespace targets are the verdict's scope", spec.prefix)
		require.NotRegexp(t, `(?m)^\s*GATE_NS=`, s.Run,
			"%s must not rebind GATE_NS — both loop iterations must scan their own namespace", spec.prefix)
		require.NotContains(t, normalizeRunText(s.Run), "export NS=",
			"%s must not export-rebind NS", spec.prefix)
		idx := indexOfStep(t, steps, s)
		require.Greater(t, idx, last, "assertions must run in §5 order: %s", spec.prefix)
		last = idx
	}
	// r9 finding 4: exactly ONE PODS assignment in Assert 3 — a second
	// (blanket) assignment between the pinned pieces relocates the r2
	// silent-skip one line below every pin.
	a3 := gateStepByPrefix(t, steps, "Assert 3")
	podsAssigns := 0
	for _, line := range strings.Split(a3.Run, "\n") {
		if strings.Contains(strings.TrimSpace(line), "PODS=") {
			podsAssigns++
		}
	}
	require.Equal(t, 1, podsAssigns, "Assert 3 must carry exactly one PODS assignment (the pinned fetch) — a blanket reassignment feeds empty stdin")
	// r10 sub-agent (ii)+(iii): exactly one consumer of the pod list
	// (a stray `read -r _` swallows every other pod's line) and each
	// capture file written exactly once (a second truncate rewires the
	// primary grep onto an empty file).
	readers := 0
	podLogWrites, prevLogWrites := 0, 0
	for _, line := range strings.Split(a3.Run, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "read -r") {
			readers++
		}
		if strings.Contains(line, "> /tmp/gate-pod.log") {
			podLogWrites++
		}
		if strings.Contains(line, "> /tmp/gate-pod-prev.log") {
			prevLogWrites++
		}
	}
	require.Equal(t, 1, readers, "Assert 3 must carry exactly one `read -r` consumer — a stray reader swallows pods")
	require.Equal(t, 1, podLogWrites, "/tmp/gate-pod.log must be written exactly once — a second write truncates the primary capture")
	require.Equal(t, 1, prevLogWrites, "/tmp/gate-pod-prev.log must be written exactly once")
	// r11 finding 3: the write-count close extended to Assert 2/4's
	// capture files — an `echo <expected-line> > file` between fetch
	// and detector flips either detector to always-green.
	countWrites := func(run, file string) int {
		n := 0
		for _, line := range strings.Split(run, "\n") {
			if strings.Contains(line, "> "+file) {
				n++
			}
		}
		return n
	}
	a2 := gateStepByPrefix(t, steps, "Assert 2")
	a4 := gateStepByPrefix(t, steps, "Assert 4")
	require.Equal(t, 1, countWrites(a2.Run, "/tmp/gate-armed.log"),
		"/tmp/gate-armed.log must be written exactly once — injection between fetch and detector is always-green")
	require.Equal(t, 1, countWrites(a4.Run, "/tmp/gate-controller.log"),
		"/tmp/gate-controller.log must be written exactly once — injection before the extraction is always-green")
	// r11 finding 1: exactly-one counts on the four snapshot
	// assignments — a duplicate AFTER the window re-snapshots and the
	// restarts during the sleep become invisible.
	a1r := gateStepByPrefix(t, steps, "Assert 1").Run
	for _, assign := range []string{
		`BEFORE_NS=$(restarts_snapshot "$NS")`,
		`BEFORE_RELAY=$(restarts_snapshot llm-relay)`,
		`AFTER_NS=$(restarts_snapshot "$NS")`,
		`AFTER_RELAY=$(restarts_snapshot llm-relay)`,
	} {
		n := 0
		for _, line := range strings.Split(a1r, "\n") {
			if strings.TrimSpace(line) == assign {
				n++
			}
		}
		require.Equal(t, 1, n, "snapshot assignment must appear exactly once (a duplicate re-snapshots past the window): %q", assign)
	}
	// r9 finding 5: exactly FOUR kubectl wait lines in Assert 1 — the
	// 300s pair and the 60s settle-window re-assertion pair.
	a1 := gateStepByPrefix(t, steps, "Assert 1")
	waits := 0
	for _, line := range strings.Split(a1.Run, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "kubectl wait") {
			waits++
		}
	}
	require.Equal(t, 4, waits, "Assert 1 must carry exactly four kubectl wait lines (the 300s pair + the 60s settle-window re-assertion)")
	for _, s := range steps {
		if strings.HasPrefix(s.Name, "Assert ") {
			require.Contains(t, []string{"Assert 1", "Assert 2", "Assert 3", "Assert 4"},
				prefixWords(s.Name), "no extra Assert-named steps (found %q)", s.Name)
		}
	}
}

// requireExactLine fails unless `want` appears as an exact (trimmed)
// line of `run` — the r9 uniform close: exact lines have no sibling
// spellings.
func requireExactLine(t *testing.T, run, want, msg string, args ...interface{}) {
	t.Helper()
	for _, line := range strings.Split(run, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Fatalf(msg+` (want exact line %q)`, append(args, want)...)
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
	// r8 finding 7: the comparison must be on the RAW parsed line —
	// TrimSpace defeated the pin for the trailing-space spelling
	// (`helm … \ ` — an escaped space, not a continuation: helm gets a
	// positional arg and the install fails on every tree with all pins
	// green, the r6 dead-gate class through the pin's own normalization).
	first := ""
	for _, line := range strings.Split(install.Run, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		first = line
		break
	}
	require.Equal(t, "helm upgrade --install llmsafespaces helm \\", first,
		"the install's first command line must be exactly the nightly's head line, RAW — trailing/leading whitespace drift is an unreviewed change (the r7 committed-mutation class; the r8 escaped-space class)")
	// r8 finding 6: the install is one operator-free command (the
	// nightly's shape) — any shell operator opens a post-install
	// channel (`… && kubectl set env …`) that no allowlist or -f regex
	// sees. False-positive-free against the current block.
	for _, op := range []string{"&&", ";", "|", "`", "$("} {
		require.NotContains(t, install.Run, op,
			"the install run block must carry no shell operators — the nightly's shape is one operator-free command; `%s` opens an unreviewed channel", op)
	}
	// r9 finding 1: the TAIL — a newline-separated second command has
	// no operator and escaped the operator ban (live-demonstrated RBAC
	// patch post-install). The block must END with the wait line, and
	// every non-comment line except the last must end with a
	// continuation backslash.
	lines := strings.Split(install.Run, "\n")
	var last string
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		last = trimmed
		break
	}
	require.Equal(t, "--wait --timeout 10m", last,
		"the install's last command line must be exactly the wait line — a newline-separated second command is an unreviewed channel")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.TrimSpace(line) == last {
			break
		}
		require.True(t, strings.HasSuffix(trimmed, "\\"),
			"install line %d must continue (end with backslash) — the install is ONE command, no newline-separated seconds: %q", i+1, line)
	}
	// r8 finding 3: the WORKFLOW-level env block is a carrier channel
	// one tier above the step/job closes — its values reach helm as
	// unquoted ${VARS} past every run-text ban. It legitimately exists
	// (the three cluster-identity vars); anything else is drift.
	var top struct {
		Env map[string]string `json:"env"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(mustRead(t, postureGateWorkflow)), &top))
	require.Equal(t, map[string]string{
		"CLUSTER_NAME": "llmsafespaces-posture",
		"IMAGE_TAG":    "posture",
		"NS":           "llmsafespaces",
	}, top.Env,
		"the workflow-level env must be exactly the three cluster-identity vars — any added entry is a value-carrier channel that evades the run-text bans")
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
				// r8 finding 5: `break`/`continue` abandon the loop and
				// print OK with every literal intact (exit 1 → break in
				// the detector branch fires the grep, drops the verdict).
				// No assertion block legitimately uses either.
				"break", "continue",
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
		// r9 finding 6: the carrier channel closed at workflow, job, and
		// install-step levels was open on every OTHER step — an env
		// block on an assertion step can re-scope its $NS-consuming
		// checks.
		require.Empty(t, s.Env,
			"no step may carry an env block (found on %q) — step env is a value-carrier channel", s.Name)
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
