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
	"strconv"
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

// --- behavioral regression: the binary-extraction block ----------------------
//
// extractBlock pulls the sentinel-marked block VERBATIM out of the script;
// runBlock executes those exact lines under a stubbed PATH. This is the
// red/green regression for run 32412056256 (silent set -e death) and for
// the scratch-image no-command docker create that caused it: the stub
// mimics docker's real behavior — `create` FAILS when an entrypoint-less
// image gets no command arg. Remove the `no-start` arg or the guards and
// these tests go red exactly the way CI did.

func extractBlock(t *testing.T) string {
	t.Helper()
	src := scriptSource(t)
	const begin, end = ">>> extract-binary-sha", "<<< extract-binary-sha"
	var lines []string
	inBlock := false
	for _, line := range strings.Split(src, "\n") {
		switch {
		case strings.Contains(line, begin):
			inBlock = true // marker sits at END of its comment line — start AFTER it
			continue
		case strings.Contains(line, end):
			return strings.Join(lines, "\n")
		case inBlock:
			lines = append(lines, line)
		}
	}
	require.Fail(t, "sentinels missing or unbalanced", "need both %q and %q", begin, end)
	return ""
}

// runBlock executes the extracted block with: a stub docker whose create
// rejects no-command invocations (and whose cp/rm succeed or fail per
// mode), a stub sha256sum, REG/TMPDIR set, and a log() function. Returns
// combined output and exit code.
func runBlock(t *testing.T, block, dockerMode, shaOut string, shaRC int) (string, int) {
	t.Helper()
	stubDir := t.TempDir()
	dockerStub := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  create)\n" +
		"    # Mimic docker: entrypoint-less image + no command arg → fail.\n" +
		"    case \"$#\" in 2) echo 'docker: no command specified' >&2; exit 1;; esac\n" +
		"    [ \"" + dockerMode + "\" = create-fail ] && { echo 'docker: simulated create failure' >&2; exit 1; }\n" +
		"    echo stub-cid; exit 0;;\n" +
		"  cp)\n" +
		"    [ \"" + dockerMode + "\" = cp-fail ] && exit 1\n" +
		"    echo fake-binary >\"$TMPDIR/workspace-agentd\"; exit 0;;\n" +
		"  rm) exit 0;;\n" +
		"esac\nexit 0\n"
	require.NoError(t, os.WriteFile(stubDir+"/docker", []byte(dockerStub), 0o755))
	shaStub := "#!/bin/sh\ncat <<'EOF'\n" + shaOut + "\nEOF\nexit " + strconv.Itoa(shaRC) + "\n"
	require.NoError(t, os.WriteFile(stubDir+"/sha256sum", []byte(shaStub), 0o755))

	out, err := exec.Command("bash", "-c", "set -euo pipefail\n"+
		"PATH=\""+stubDir+":$PATH\"\n"+
		"REG=localhost:5001/llmsafespaces\n"+
		"TMPDIR=\""+stubDir+"\"\n"+
		"log() { echo \"[log] $*\"; }\n"+
		block).CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		code = -1
	}
	return string(out), code
}

// TestUS2KindScript_ExtractBlock_HappyPath: with the script's real
// invocation (command arg present, tools succeeding) the hash is logged.
func TestUS2KindScript_ExtractBlock_HappyPath(t *testing.T) {
	out, code := runBlock(t, extractBlock(t), "ok", "deadbeef  file", 0)
	require.Equal(t, 0, code, "output: %s", out)
	require.Contains(t, out, "[log] agentd binary sha256 (amd64): deadbeef", out)
}

// TestUS2KindScript_ExtractBlock_NoCommandArgIsTheRegression: if the
// `no-start` arg is removed (the pre-fix line), the stub's create fails
// the way real docker did — and the block must DIE LOUDLY (message +
// nonzero exit), not silently. Simulates the pre-fix line by rewriting
// the create invocation, proving the guard is what converts the failure
// into a diagnosable one.
func TestUS2KindScript_ExtractBlock_NoCommandArgIsTheRegression(t *testing.T) {
	block := strings.Replace(extractBlock(t), `" no-start 2>/dev/null)`, `" 2>/dev/null)`, 1)
	require.NotEqual(t, extractBlock(t), block, "rewrite must apply (no-start arg present in script)")
	out, code := runBlock(t, block, "ok", "deadbeef  file", 0)
	require.NotEqual(t, 0, code, "no-command create must fail: %s", out)
	require.Contains(t, out, "[log] docker create failed for binary extraction",
		"the guard must print its message — silent death is the run-32412056256 regression; got: %s", out)
}

// TestUS2KindScript_ExtractBlock_CreateFailureLoud: explicit create
// failure surfaces the message (the guard, independent of the arg).
func TestUS2KindScript_ExtractBlock_CreateFailureLoud(t *testing.T) {
	out, code := runBlock(t, extractBlock(t), "create-fail", "x  y", 0)
	require.Equal(t, 1, code, "output: %s", out)
	require.Contains(t, out, "[log] docker create failed for binary extraction", out)
}

// TestUS2KindScript_ExtractBlock_CpFailureLoud: cp failure prints its
// message (the path that also rm's the cid).
func TestUS2KindScript_ExtractBlock_CpFailureLoud(t *testing.T) {
	out, code := runBlock(t, extractBlock(t), "cp-fail", "x  y", 0)
	require.Equal(t, 1, code, "output: %s", out)
	require.Contains(t, out, "[log] docker cp failed to extract agentd binary", out)
}

// TestUS2KindScript_ExtractBlock_EmptyHashRejected: a sha256sum that
// yields nothing (missing file) is rejected with the hash message, not
// an empty pin.
func TestUS2KindScript_ExtractBlock_EmptyHashRejected(t *testing.T) {
	out, code := runBlock(t, extractBlock(t), "ok", "", 1)
	require.Equal(t, 1, code, "output: %s", out)
	require.Contains(t, out, "[log] sha256sum failed on extracted binary", out)
}
