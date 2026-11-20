// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package scripts_test

// workspace_lifecycle_script_test.go — regression harness for
// scripts/workspace-lifecycle-skew-e2e.sh (the kind-tier leg of the
// PR #2088 bug-class coverage, owner direction 2026-11-17).
//
// Same philosophy as us2_kind_script_test.go: the script is CI glue
// executing a real kind cluster; mocking docker/kind would test the
// mocks. What IS pinnable deterministically — and what past incidents
// of this script class actually broke:
//
//   - bash syntax validity (bash -n);
//   - THE SKEW WIRED THE RIGHT WAY: the pre-.15 CRD must be applied
//     BEFORE helm, and helm must run --skip-crds (a plain install would
//     install the current CRDs and silently destroy the skew under
//     test — the run would go green proving nothing);
//   - the fixture itself must stay harness-less (fixture drift guard:
//     a regenerated fixture from a post-.15 tag would silently
//     un-skew every execution);
//   - the digest splice + both-or-neither binary pins (the us2
//     incidents: the missing "@" separator; image-only
//     agentdDelivery made the controller resolve pins from inside its
//     pod where the registry alias does not resolve);
//   - teardown success-gated + diagnostics wired to the helm failure
//     path (the run 32407515619 lesson: the EXIT trap deleted the
//     cluster before diagnostics could inspect it);
//   - every check leg reachable: each LC-K* assertion must have a
//     matching wait_phase call, and the convergence leg must restart
//     the controller (the crash-safe attempt-map reset that makes the
//     10-minute backfill window converge in seconds).

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lifecycleScript = "workspace-lifecycle-skew-e2e.sh"

func lifecycleScriptBody(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	//nolint:gosec // test fixture read of a checked-in script
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), lifecycleScript))
	require.NoError(t, err)
	return string(data)
}

func TestWorkspaceLifecycleScript_BashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	//nolint:gosec // fixed script path beside this test, constant argv
	out, err := exec.Command("bash", "-n", filepath.Join(filepath.Dir(thisFile), lifecycleScript)).CombinedOutput()
	require.NoError(t, err, "bash -n must parse: %s", out)
}

// The skew is the point of the script: the legacy CRD applies BEFORE
// helm, and helm installs with --skip-crds — otherwise the chart's
// current CRDs would overwrite the skew and the whole run proves
// nothing while going green.
func TestWorkspaceLifecycleScript_SkewWiring(t *testing.T) {
	body := lifecycleScriptBody(t)

	applyIdx := strings.Index(body, `kubectl apply -f "$LEGACY_CRD"`)
	require.GreaterOrEqual(t, applyIdx, 0, "the legacy CRD apply must exist")
	helmIdx := strings.Index(body, `helm upgrade --install "$RELEASE" helm`)
	require.GreaterOrEqual(t, helmIdx, 0, "the helm install must exist")
	assert.Less(t, applyIdx, helmIdx,
		"the pre-.15 CRD must be applied BEFORE helm installs (otherwise the chart serves current CRDs)")

	waitIdx := strings.Index(body[helmIdx:], "--wait --timeout 420s")
	require.GreaterOrEqual(t, waitIdx, 0, "the helm install must carry its wait/timeout tail")
	helmCmd := body[helmIdx : helmIdx+waitIdx]
	assert.Contains(t, helmCmd, "--skip-crds",
		"helm must install with --skip-crds — a fresh install would install the current CRDs and destroy the skew")
}

// The r2 fix, pinned: the chart ships a RuntimeEnvironment "base"
// resource, and helm builds the release manifest against the cluster's
// discovery — so the CURRENT non-Workspace platform CRDs must be
// pre-applied beside the legacy Workspace one (the incident shape:
// Workspace CRD stale, the rest of the cluster healthy). The current
// workspace.yaml must NEVER be in that apply set.
func TestWorkspaceLifecycleScript_NonWorkspaceCRDsPreApplied(t *testing.T) {
	body := lifecycleScriptBody(t)
	assert.Contains(t, body, `for crd in runtimeenvironment.yaml inferencerelay.yaml; do`,
		"the current non-Workspace platform CRDs must be pre-applied (helm's manifest build needs the kinds)")
	assert.Contains(t, body, `kubectl apply -f "helm/crds/$crd"`,
		"the pre-apply loop must apply from helm/crds/")
	// The current Workspace CRD applies ONLY in the LC-K4 convergence
	// leg (after the checks) — never in setup, which would destroy the
	// skew under test.
	k4Idx := strings.Index(body, `log "LC-K4: operator applies the current CRD`)
	require.GreaterOrEqual(t, k4Idx, 0, "the LC-K4 convergence leg must exist")
	curIdx := strings.Index(body, `kubectl apply -f "$CURRENT_CRD"`)
	require.GreaterOrEqual(t, curIdx, 0, "the current CRD apply must exist (LC-K4)")
	assert.Greater(t, curIdx, k4Idx,
		"the current Workspace CRD applies only inside LC-K4 — an apply before the legs would destroy the skew")
	assert.Equal(t, strings.Count(body, `kubectl apply -f "$CURRENT_CRD"`), 1,
		"exactly ONE apply of the current Workspace CRD (the LC-K4 convergence leg)")
}

// Fixture drift guard: the pinned CRD the script (and the envtest
// lifecycle suite) serves must stay the PRE-.15 harness-less shape —
// regenerating it from a later tag silently un-skews every execution.
func TestWorkspaceLifecycleScript_FixtureStaysHarnessLess(t *testing.T) {
	body := lifecycleScriptBody(t)
	assert.Contains(t, body, `if grep -q "harness" "$LEGACY_CRD"`,
		"the preflight must refuse to run against a fixture that drifted post-.15")
}

// The us2 incidents, re-pinned for this script: the digest reference
// splices with "@" and BOTH binarySHA256 flags ride together.
func TestWorkspaceLifecycleScript_AgentdReferenceAndPins(t *testing.T) {
	body := lifecycleScriptBody(t)
	assert.Regexp(t, regexp.MustCompile(`AGENTD_REF="\$REG/llmsafespaces/agentd:ci@\$\{AGENTD_REF##\*@\}"`), body,
		"the digest splice must keep the @ separator")
	assert.Contains(t, body, `--set "controller.agentdDelivery.binarySHA256Amd64=${BINARY_SHA}"`)
	assert.Contains(t, body, `--set "controller.agentdDelivery.binarySHA256Arm64=${BINARY_SHA}"`)
}

// The r1 review finding, pinned forever: the chart REFUSES to render
// without all three design-0053 delivery artifacts (§4.5 agentd +
// opencode "no baked fallback"; §5.4 pi) — a helm install that omits
// any of the nine --set flags dies in SETUP before a single check
// runs (controller-deployment.yaml's mandatory gates). The mirrored
// us2 script never installs the controller, which is why the pattern
// didn't carry them.
func TestWorkspaceLifecycleScript_AllDeliveryPinsMandatory(t *testing.T) {
	body := lifecycleScriptBody(t)
	for _, art := range []struct{ name, ref string }{
		{"opencode", `OPENCODE_REF="$REG/llmsafespaces/opencode:ci@${OPENCODE_REF##*@}"`},
		{"pi", `PI_REF="$REG/llmsafespaces/pi:ci@${PI_REF##*@}"`},
	} {
		assert.Contains(t, body, art.ref,
			"the %s digest splice must exist and keep the @ separator", art.name)
	}
	for _, flag := range []string{
		`--set "controller.opencodeDelivery.image=${OPENCODE_REF}"`,
		`--set "controller.opencodeDelivery.binarySHA256Amd64=${OPENCODE_BINARY_SHA}"`,
		`--set "controller.opencodeDelivery.binarySHA256Arm64=${OPENCODE_BINARY_SHA}"`,
		`--set "controller.piDelivery.image=${PI_REF}"`,
		`--set "controller.piDelivery.binarySHA256Amd64=${PI_BINARY_SHA}"`,
		`--set "controller.piDelivery.binarySHA256Arm64=${PI_BINARY_SHA}"`,
	} {
		assert.Contains(t, body, flag,
			"the chart-mandatory delivery pin is missing — helm dies in SETUP at the render gate")
	}
	// The artifacts must actually be BUILT and pushed (a pin without a
	// registry image fails the digest extraction).
	assert.Contains(t, body, `-f runtimes/opencode/Dockerfile -t "$REG/llmsafespaces/opencode:ci"`)
	assert.Contains(t, body, `-f runtimes/pi/Dockerfile -t "$REG/llmsafespaces/pi:ci" runtimes/pi`)
}

// Teardown must stay success-gated and the helm failure path must dump
// diagnostics BEFORE the EXIT trap can tear the cluster down.
func TestWorkspaceLifecycleScript_TeardownAndDiagnostics(t *testing.T) {
	body := lifecycleScriptBody(t)
	assert.Contains(t, body, `--wait --timeout 420s || { setup_diagnostics; exit 1; }`,
		"the helm failure path must dump diagnostics")
	assert.Contains(t, body, `[ "$KEEP" -ne 1 ] && [ "$fails" -eq 0 ] && [ "$code" -eq 0 ]`,
		"teardown stays success-gated (failures keep the cluster for inspection)")
}

// Every owner-visible leg must be driven: the four LC-K assertions
// exist as wait_phase/verdict arms, and convergence restarts the
// controller (the crash-safe attempt-map reset).
func TestWorkspaceLifecycleScript_LegsPresent(t *testing.T) {
	body := lifecycleScriptBody(t)
	for _, leg := range []string{
		`wait_phase Active 300 "LC-K1 start"`,
		`wait_phase Creating 90 "LC-K2 recycle"`,
		`wait_phase Suspended 120 "LC-K3 suspend"`,
		`wait_phase Active 300 "LC-K3 resume"`,
		`pass LC-K4 "CRD apply converged`,
	} {
		assert.Contains(t, body, leg, "leg missing from the script: %s", leg)
	}
	assert.Contains(t, body, `rollout restart deployment "${RELEASE}-llmsafespaces-controller"`,
		"convergence must restart the controller — the crash-safe attempt-map reset that collapses the 10m backfill window")
	// The refresh write shape: generation bump + the #1505 force marker.
	assert.Contains(t, body, `llmsafespaces.dev/force-recycle=`)
	assert.Contains(t, body, `"{\"spec\":{\"restartGeneration\":$RUNTIME_GEN_NEXT}}"`)
	// The suspend pod-gone check must be a BOUNDED WAIT, not an instant
	// existence check — pod teardown is async on a real cluster (the r2
	// CI finding: the phase flips the moment the delete is issued, the
	// pod stays listed while Terminating).
	assert.Contains(t, body, `wait_pod_gone 180 "llmsafespaces.dev/workspace=$WORKSPACE_NAME"`,
		"the suspend leg must poll for pod teardown, never check instantly")
}
