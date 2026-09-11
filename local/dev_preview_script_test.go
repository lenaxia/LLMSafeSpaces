// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// dev_preview_script_test.go — structure pins for
// local/dev-preview-tunnel-e2e.sh (#1332/#1333), same philosophy as
// s5_kind_script_test.go: the script is CI glue on a real kind cluster;
// what is pinnable deterministically is the structure past failures
// actually broke — bash syntax, the assertion rows that must exist so
// they cannot be silently dropped, the UUID workspace-name contract, and
// the nightly workflow wiring.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const devPreviewScript = "dev-preview-tunnel-e2e.sh"

func TestDevPreviewScript_BashSyntax(t *testing.T) {
	cmd := exec.Command("bash", "-n", devPreviewScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// TestDevPreviewScript_AssertionRows pins the load-bearing rows: each
// incident leg must keep its assertion, or the nightly silently stops
// testing the regression it exists for.
func TestDevPreviewScript_AssertionRows(t *testing.T) {
	raw, err := os.ReadFile(devPreviewScript)
	if err != nil {
		t.Fatalf("read %s: %v", devPreviewScript, err)
	}
	s := string(raw)

	rows := []struct {
		fragment string
		why      string
	}{
		{`== "308"`, "#1333-A: bare port must assert the 308"},
		{`?v=3&x=1`, "#1333-A2: redirect must preserve the query string"},
		{`dpv-e2e-marker`, "#1333-B: HTML marker must be asserted after following the redirect"},
		{`style.css?v=3`, "#1333-B: the relative stylesheet must be fetched through the tunnel"},
		{`== "400"`, "#1333-C: mis-resolved asset path and denied port must stay 400"},
		{`LLMSAFESPACE_API_PUBLIC_URL`, "#1332-A: the fail-loud error must name the env var"},
		{`!= *".svc"*`, "#1332: svc origins must never leak into tool output"},
		{`--api-public-url`, "#1332-B: the controller flag patch drives the wiring-chain leg"},
		{`workspace-pw-`, "the MCP call must authenticate with the workspace password secret (§D1)"},
		{`devPreview: true`, "the seeded workspace must enable dev preview (the 503 gate)"},
	}
	for _, r := range rows {
		if !strings.Contains(s, r.fragment) {
			t.Errorf("script lost its %s — fragment %q not found", r.why, r.fragment)
		}
	}

	// The controller patch is restored so later nightly suites see the
	// unmodified deployment.
	if !strings.Contains(s, `"op":"remove","path":"/spec/template/spec/containers/0/args/-"`) {
		t.Error("script must restore the controller args after the #1332-B leg")
	}
}

// TestDevPreviewScript_WorkspaceNameIsUUID pins the API's workspace-lookup
// contract (workspaces.id is a uuid column; the CR name IS the id — the
// us-70 pool's run-4 lesson). The script must derive names via ws_id.
func TestDevPreviewScript_WorkspaceNameIsUUID(t *testing.T) {
	raw, err := os.ReadFile(devPreviewScript)
	if err != nil {
		t.Fatalf("read %s: %v", devPreviewScript, err)
	}
	if !regexp.MustCompile(`WS="\$\(ws_id 90\)"`).Match(raw) {
		t.Error("workspace name must come from ws_id (UUID contract), not a literal")
	}
}

// TestDevPreviewScript_NightlyWiring pins the nightly workflow running
// the script — the e2e legs exist only if CI executes them.
func TestDevPreviewScript_NightlyWiring(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "e2e-nightly.yml"))
	if err != nil {
		t.Fatalf("read e2e-nightly.yml: %v", err)
	}
	if !strings.Contains(string(raw), "bash local/dev-preview-tunnel-e2e.sh") {
		t.Error("e2e-nightly.yml must run local/dev-preview-tunnel-e2e.sh")
	}
}
