// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local

// Pins for local/issue-1632-agentz-liveness-e2e.sh — the #1632 fix #3
// happy-path e2e (probe shape + live 401/200 + zero restarts). The
// script runs on the harness/pool cluster; these pins hold its shape
// (TestIssue1507Script_BashSyntax precedent; the sustained-episode
// unhappy loop is unit/integration-pinned in cmd/workspace-agentd
// pending an env-tunable sustain bound).

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const issue1632Script = "issue-1632-agentz-liveness-e2e.sh"

func TestIssue1632Script_BashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	out, err := exec.Command(bash, "-n", issue1632Script).CombinedOutput()
	if err != nil {
		t.Fatalf("bash -n failed: %s", out)
	}
}

func mustRead1632(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(issue1632Script)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The probe-shape row must assert ALL THREE spec properties: the
// HTTPGet path, the admin port, and the absence of the old
// kernel-accept-blind tcpSocket — plus the bearer header (the F1.4.2
// failure class on the liveness path).
func TestIssue1632Script_PinsProbeShape(t *testing.T) {
	src := mustRead1632(t)
	for _, marker := range []string{
		`livenessProbe.httpGet.path`,
		`/v1/agentz`,
		`livenessProbe.httpGet.port`,
		`livenessProbe.tcpSocket`,
		`httpHeaders`,
		`Authorization`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must contain %q (probe-shape row)", marker)
		}
	}
}

// The live-pod row must exercise BOTH auth outcomes against the real
// admin mux — 401 without the bearer, ok:true with it — fetched via
// the shared netns from inside the pod (kubectl exec), with the token
// read from the workspace Secret.
func TestIssue1632Script_LiveAuthRows(t *testing.T) {
	src := mustRead1632(t)
	for _, marker := range []string{
		`data.admin-token`,
		`4098/v1/agentz`,
		`401`,
		`Bearer`,
		`"ok":true`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must contain %q (live auth row)", marker)
		}
	}
}

// The happy-path row must assert the workspace container did NOT
// restart — a probe that fires blind on a healthy pod is the
// regression this row exists to catch.
func TestIssue1632Script_ZeroRestartsRow(t *testing.T) {
	src := mustRead1632(t)
	for _, marker := range []string{
		`containerStatuses[0].restartCount`,
		`restartCount=0`,
	} {
		if !strings.Contains(src, marker) {
			t.Errorf("script must contain %q (zero-restart row)", marker)
		}
	}
}
