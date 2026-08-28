// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// opencode_overlay_contract_test.go — the controller↔agentd seam contract
// guard for opencode overlay delivery (design 0053 §4.2).
//
// The contract literals (env key names, mount path + binary rel path
// composition, exit codes 83/84, termination-log message prefixes) are
// defined INDEPENDENTLY on the two sides: here in
// controller/internal/workspace/opencode_overlay.go and in
// cmd/workspace-agentd/opencode_overlay.go. Nothing at compile time fails
// if one side drifts — a renamed env or renumbered exit code ships green
// and only surfaces as unverifiable pods (exit 83 "no pin for arch") or
// unattributed crashloops in a live cluster.
//
// This test closes that gap mechanically, the same way
// TestAgentdAnnotationKeys_MatchCIWorkflow guards the CI↔Go annotation
// seam: it parses the agentd source off disk and asserts the agentd-side
// constant DECLARATIONS equal the controller-side constants. Deliberately
// regex-over-const-blocks, not a type import — cmd/workspace-agentd is a
// main package and cannot be linked from here.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// agentdOverlaySourcePath is the supervisor-side half of the seam,
// relative to this package's directory.
func agentdOverlaySourcePath() string {
	return filepath.Join("..", "..", "..", "cmd", "workspace-agentd", "opencode_overlay.go")
}

// extractAgentdConst returns the string value of a top-level const
// declaration in the agentd overlay source, failing the test with a
// rename-drift message when the declaration is gone.
func extractAgentdConst(t *testing.T, body, constName string) string {
	t.Helper()
	m := regexp.MustCompile(constName + `\s*=\s*"([^"]+)"`).FindStringSubmatch(body)
	require.NotNil(t, m,
		"const %s not found in cmd/workspace-agentd/opencode_overlay.go — the agentd half of the seam changed; update this test AND the controller constants together", constName)
	return m[1]
}

// extractAgentdExitCode returns the integer value of an exit-code const
// declaration on the agentd side.
func extractAgentdExitCode(t *testing.T, body, constName string) int32 {
	t.Helper()
	m := regexp.MustCompile(constName + `\s*=\s*(\d+)`).FindStringSubmatch(body)
	require.NotNil(t, m,
		"const %s not found in cmd/workspace-agentd/opencode_overlay.go — exit-code contract changed; update this test AND the controller constants together", constName)
	n, err := strconv.ParseInt(m[1], 10, 32)
	require.NoError(t, err)
	return int32(n)
}

// TestOpencodeOverlayContract_MatchesAgentdSupervisor asserts every
// seam literal the controller wires/detects equals the literal the
// supervisor consumes/reports. If this test fails, one side of the
// opencode overlay contract drifted — fix BOTH files together, never
// just the test.
func TestOpencodeOverlayContract_MatchesAgentdSupervisor(t *testing.T) {
	agentdRaw, err := os.ReadFile(agentdOverlaySourcePath())
	require.NoError(t, err, "the supervisor half of the seam must exist on disk")
	agentdSrc := string(agentdRaw)

	controllerRaw, err := os.ReadFile("opencode_overlay.go")
	require.NoError(t, err, "the controller half of the seam must exist on disk")
	controllerSrc := string(controllerRaw)

	// --- env key names -------------------------------------------------

	// Marker env: the controller stamps it (wireOpencodeOverlay) AND
	// gates on it (podHasOpencodeOverlay) via the ONE opencodeOverlayEnvKey
	// constant; the supervisor gates its whole verify path on it. Both the
	// wiring and the detection gate must keep using that same constant —
	// assert the constant usages, then equality with the agentd const.
	require.Regexp(t, regexp.MustCompile(`EnvVar\{Name: opencodeOverlayEnvKey, Value: "1"\}`),
		controllerSrc, "wireOpencodeOverlay must stamp the marker via the opencodeOverlayEnvKey constant, not a literal")
	require.Regexp(t, regexp.MustCompile(`e\.Name == opencodeOverlayEnvKey && e\.Value == "1"`),
		controllerSrc, "podHasOpencodeOverlay's gate must key on the same opencodeOverlayEnvKey constant the wiring stamps")
	require.Equal(t, opencodeOverlayEnvKey, extractAgentdConst(t, agentdSrc, "envOpencodeImageVolume"),
		"marker env name drifted between controller and supervisor")

	// Pin env names: the controller wires them as literals in
	// wireOpencodeOverlay (no constants exist on this side); assert the
	// wiring declares exactly the names the supervisor reads.
	controllerPinEnv := regexp.MustCompile(`Name: "(LLMSAFESPACES_OPENCODE_[A-Z0-9_]+)"`).FindAllStringSubmatch(controllerSrc, -1)
	require.Len(t, controllerPinEnv, 3,
		"wireOpencodeOverlay must wire exactly the three LLMSAFESPACES_OPENCODE_* pin envs")
	require.Equal(t, extractAgentdConst(t, agentdSrc, "envOpencodeBinary"), controllerPinEnv[0][1],
		"binary-path env name drifted between controller wiring and supervisor reader")
	require.Equal(t, extractAgentdConst(t, agentdSrc, "envOpencodeSHA256AMD64"), controllerPinEnv[1][1],
		"amd64 pin env name drifted between controller wiring and supervisor reader")
	require.Equal(t, extractAgentdConst(t, agentdSrc, "envOpencodeSHA256ARM64"), controllerPinEnv[2][1],
		"arm64 pin env name drifted between controller wiring and supervisor reader")

	// --- default binary path composition --------------------------------

	// The supervisor's single default constant must equal the
	// controller's mount-path + rel-path composition.
	require.Equal(t, opencodeMountPath+opencodeBinaryRelPath,
		extractAgentdConst(t, agentdSrc, "opencodeOverlayBinaryDefault"),
		"default overlay binary path drifted: controller composes %s%s, supervisor defaults to a different path",
		opencodeMountPath, opencodeBinaryRelPath)

	// --- exit codes ------------------------------------------------------

	require.Equal(t, opencodeExitVerifyFailed, extractAgentdExitCode(t, agentdSrc, "opencodeExitVerifyFailed"),
		"verify-failed exit code drifted — controller attributes %d, supervisor exits something else",
		opencodeExitVerifyFailed)
	require.Equal(t, opencodeExitOverlayMissing, extractAgentdExitCode(t, agentdSrc, "opencodeExitOverlayMissing"),
		"overlay-missing exit code drifted — controller attributes %d, supervisor exits something else",
		opencodeExitOverlayMissing)

	// --- termination-log message prefixes ---------------------------------

	// The supervisor writes the failure reason to stderr + the pod
	// termination log; the controller surfaces it as the condition reason
	// and event reason. The prefixes are v1.Reason* here and fmt.Sprintf
	// literals there — assert the agentd message literals carry the
	// controller's reason strings verbatim.
	for _, prefix := range []string{
		string(v1.ReasonOpencodeVerificationFailed),
		string(v1.ReasonOpencodeOverlayMissing),
	} {
		require.Regexp(t, regexp.MustCompile(regexp.QuoteMeta(`"`+prefix)+`: `),
			agentdSrc, "supervisor failure messages must carry the controller reason prefix %q verbatim (termination-log attribution)", prefix)
	}
}
