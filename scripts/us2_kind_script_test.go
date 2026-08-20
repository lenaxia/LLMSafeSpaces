// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package scripts_test

// us2_kind_script_test.go — regression harness for
// scripts/us2-kind-integration.sh (design 0051 L3).
//
// The script is CI glue executing a real kind cluster; mocking docker/kind
// to "unit test" its internals would test the mocks, not the script. What
// IS pinnable deterministically, and what past failures actually broke:
//
//   - bash syntax validity (bash -n);
//   - the STRUCTURE of the wiring the two production incidents regressed:
//     run 32404366824 — the digest splice dropped the "@" separator;
//     run 32407515619 — image-only agentdDelivery made the controller
//     resolve pins from inside its pod (localhost:5001 unresolvable) and
//     the EXIT-trap teardown deleted the cluster before diagnostics.
//
// So: the agentd reference must be tag@sha256, BOTH binarySHA256 flags
// must be passed together (the both-or-neither controller contract — the
// validation itself is pinned in controller's TestAgentdOverlay_* suite),
// teardown must be success-gated, and setup_diagnostics must exist and be
// wired to the helm failure path.
//
// The controller-side both-or-neither contract lives at
// controller/internal/workspace/agentd_overlay_test.go
// (TestAgentdOverlay_Validation) — not duplicated here.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const scriptPath = "us2-kind-integration.sh"

func scriptSource(t *testing.T) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), scriptPath))
	require.NoError(t, err, "script must exist beside this test")
	return string(data)
}

// TestUS2KindScript_BashSyntax: the script must parse under bash — the
// cheapest possible gate against glue rot between executions (the
// workflow only runs weekly).
func TestUS2KindScript_BashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	//nolint:gosec // fixed script path beside this test, constant argv
	out, err := exec.Command("bash", "-n", filepath.Join(filepath.Dir(thisFile), scriptPath)).CombinedOutput()
	require.NoError(t, err, "bash -n must parse: %s", out)
}

// TestUS2KindScript_DigestReferenceWellFormed: the constructed agentd
// reference must splice the digest with an "@" separator — the run
// 32404366824 regression (`:cisha256:…` — missing @ — silently produced
// an unpullable reference).
func TestUS2KindScript_DigestReferenceWellFormed(t *testing.T) {
	src := scriptSource(t)
	require.Regexp(t, regexp.MustCompile(`AGENTD_REF="\$REG/llmsafespaces/agentd:ci@\$\{AGENTD_REF##\*@\}"`),
		src, "digest splice must keep the '@' separator (run 32404366824)")
}

// TestUS2KindScript_BinaryPinsPassedTogether: BOTH binarySHA256 flags at
// the helm call site — the run 32407515619 fix (image-only makes the
// controller query the registry from inside its pod, where the local
// registry hostname does not resolve). Both-or-neither is the controller
// startup contract; passing one alone fails validation and boots nothing.
func TestUS2KindScript_BinaryPinsPassedTogether(t *testing.T) {
	src := scriptSource(t)
	amd64 := strings.Contains(src, `--set "controller.agentdDelivery.binarySHA256Amd64=${BINARY_SHA}"`)
	arm64 := strings.Contains(src, `--set "controller.agentdDelivery.binarySHA256Arm64=${BINARY_SHA}"`)
	require.True(t, amd64 && arm64,
		"both binary sha pins must be passed together (run 32407515619; both-or-neither validation)")
}

// TestUS2KindScript_TeardownSuccessGated: the finish trap must delete the
// cluster ONLY on success (or explicit --keep) — deleting on failure
// destroyed the evidence the diagnostics steps exist to collect (run
// 32407515619's blind diagnostics).
func TestUS2KindScript_TeardownSuccessGated(t *testing.T) {
	src := scriptSource(t)
	require.Contains(t, src, `if [ "$KEEP" -ne 1 ] && [ "$fails" -eq 0 ] && [ "$code" -eq 0 ]; then`,
		"teardown must be gated on success + no fails + not --keep")
}

// TestUS2KindScript_DiagnosticsWiredToHelmFailure: setup_diagnostics must
// exist and run on the helm failure path while the cluster is still up.
func TestUS2KindScript_DiagnosticsWiredToHelmFailure(t *testing.T) {
	src := scriptSource(t)
	require.Contains(t, src, "setup_diagnostics() {", "diagnostics function must be defined")
	require.Contains(t, src, `|| { setup_diagnostics; exit 1; }`,
		"helm failure must invoke in-script diagnostics before exiting")
}
